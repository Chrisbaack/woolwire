// Package m0 contains the disposable transport-feasibility probe. It is
// intentionally not the v1 persistence layer; it exists to test the
// assumptions that later seams will depend on.
package m0

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

const stateVersion = 1

// DeviceState is the identity material a client must preserve across
// restarts. The Tailcat key is deliberately distinct from the application
// identity certificate key.
type DeviceState struct {
	Version       int                `json:"version"`
	TailcatKey    key.NodePrivate    `json:"tailcat_key"`
	DevicePrivate ed25519.PrivateKey `json:"device_private"`
	DeviceCertDER []byte             `json:"device_certificate_der"`
}

// RoomState is the owner-side state needed to restart a Tailcat server with
// the same address. The address includes the DERP region and PSK, so all
// three values must be retained together.
type RoomState struct {
	Version             int                  `json:"version"`
	RoomID              string               `json:"room_id"`
	TailcatKey          key.NodePrivate      `json:"tailcat_key"`
	TailcatPresharedKey tailcat.PresharedKey `json:"tailcat_preshared_key"`
	TailcatAddr         tailcat.Addr         `json:"tailcat_addr"`
	AuthorityPrivate    ed25519.PrivateKey   `json:"authority_private"`
	ServerPrivatePKCS8  []byte               `json:"server_private_pkcs8"`
	ServerCertDER       []byte               `json:"server_certificate_der"`
	Device              DeviceState          `json:"device"`
	// AdmittedDevices contains the encoded Ed25519 public keys that have
	// successfully joined this room. An omitted field in older state files
	// decodes as an empty slice and therefore remains fully compatible.
	AdmittedDevices []string `json:"admitted_devices,omitempty"`
	// PeerCredentials is the bounded M0 handoff registry. It is deliberately
	// not a roster: M1 will replace this with signed membership state and
	// propagation. Keeping the small registry in the creator state lets a
	// member refresh an address after a restart or relay-region migration.
	PeerCredentials []PeerCredential `json:"peer_credentials,omitempty"`
}

// LoadDevice reads an existing device identity or creates one with fresh
// keys. It uses an atomic replacement and restrictive file permissions.
func LoadDevice(path string) (DeviceState, error) {
	var state DeviceState
	if err := loadJSON(path, &state); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return DeviceState{}, err
		}
		state = DeviceState{
			Version:    stateVersion,
			TailcatKey: key.NewNode(),
		}
		if err := ensureDeviceCertificate(&state); err != nil {
			return DeviceState{}, err
		}
		if err := saveJSON(path, state); err != nil {
			return DeviceState{}, err
		}
	}
	if err := validateDevice(state); err != nil {
		return DeviceState{}, fmt.Errorf("device state: %w", err)
	}
	return state, nil
}

// LoadRoom reads an existing room identity or creates one. The caller must
// call EnsureServerCertificate before exposing the room endpoint.
func LoadRoom(path, roomID string) (RoomState, error) {
	var state RoomState
	if err := loadJSON(path, &state); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return RoomState{}, err
		}
		if roomID == "" {
			return RoomState{}, errors.New("room id is required when creating room state")
		}
		_, authorityPrivate, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return RoomState{}, fmt.Errorf("generate room authority: %w", err)
		}
		state = RoomState{
			Version:             stateVersion,
			RoomID:              roomID,
			TailcatKey:          key.NewNode(),
			TailcatPresharedKey: tailcat.NewPresharedKey(),
			AuthorityPrivate:    authorityPrivate,
			Device: DeviceState{
				Version:    stateVersion,
				TailcatKey: key.NewNode(),
			},
		}
		if err := ensureDeviceCertificate(&state.Device); err != nil {
			return RoomState{}, err
		}
		if err := saveJSON(path, state); err != nil {
			return RoomState{}, err
		}
	}
	if roomID != "" && state.RoomID != roomID {
		return RoomState{}, fmt.Errorf("state belongs to room %q, not %q", state.RoomID, roomID)
	}
	if err := validateRoom(state); err != nil {
		return RoomState{}, fmt.Errorf("room state: %w", err)
	}
	return state, nil
}

// EnsureServerCertificate creates the room's TLS server certificate once.
// The certificate is signed by the room authority, while the private key is
// stored separately from the Tailcat key in the same protected state file.
func EnsureServerCertificate(state *RoomState) error {
	if len(state.ServerCertDER) > 0 || len(state.ServerPrivatePKCS8) > 0 {
		if len(state.ServerCertDER) == 0 || len(state.ServerPrivatePKCS8) == 0 {
			return errors.New("server certificate state is incomplete")
		}
		return nil
	}
	authorityCert, err := authorityCertificate(state.AuthorityPrivate)
	if err != nil {
		return err
	}
	serverPublic, serverPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate server certificate key: %w", err)
	}
	certTemplate, err := certificateTemplate("woolwire-room", x509.ExtKeyUsageServerAuth)
	if err != nil {
		return err
	}
	certDER, err := x509.CreateCertificate(rand.Reader, certTemplate, authorityCert, serverPublic, state.AuthorityPrivate)
	if err != nil {
		return fmt.Errorf("create server certificate: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(serverPrivate)
	if err != nil {
		return fmt.Errorf("marshal server certificate key: %w", err)
	}
	state.ServerCertDER = certDER
	state.ServerPrivatePKCS8 = privateDER
	return nil
}

func ensureDeviceCertificate(state *DeviceState) error {
	if len(state.DeviceCertDER) > 0 {
		return nil
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate device certificate key: %w", err)
	}
	template, err := certificateTemplate("woolwire-device", x509.ExtKeyUsageClientAuth)
	if err != nil {
		return err
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		return fmt.Errorf("create device certificate: %w", err)
	}
	state.DevicePrivate = private
	state.DeviceCertDER = certDER
	return nil
}

func certificateTemplate(commonName string, usages ...x509.ExtKeyUsage) (*x509.Certificate, error) {
	serialBytes := make([]byte, 16)
	if _, err := rand.Read(serialBytes); err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	serial := new(big.Int).SetBytes(serialBytes)
	return &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(3650 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           usages,
		BasicConstraintsValid: true,
		DNSNames:              []string{commonName},
	}, nil
}

func authorityCertificate(private ed25519.PrivateKey) (*x509.Certificate, error) {
	if len(private) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid room authority private key")
	}
	public := private.Public().(ed25519.PublicKey)
	template, err := certificateTemplate("woolwire-room-authority")
	if err != nil {
		return nil, err
	}
	template.IsCA = true
	template.KeyUsage |= x509.KeyUsageCertSign
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		return nil, fmt.Errorf("create authority certificate: %w", err)
	}
	return x509.ParseCertificate(der)
}

// IssuePeerCertificate creates an application TLS certificate for a member peer,
// signed by the room authority.
func IssuePeerCertificate(authorityPrivate ed25519.PrivateKey, devicePublic ed25519.PublicKey) ([]byte, error) {
	authorityCert, err := authorityCertificate(authorityPrivate)
	if err != nil {
		return nil, err
	}
	template, err := certificateTemplate("woolwire-peer", x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth)
	if err != nil {
		return nil, err
	}
	return x509.CreateCertificate(rand.Reader, template, authorityCert, devicePublic, authorityPrivate)
}

func validateDevice(state DeviceState) error {
	if state.Version != stateVersion || state.TailcatKey.IsZero() || len(state.DevicePrivate) != ed25519.PrivateKeySize || len(state.DeviceCertDER) == 0 {
		return errors.New("unsupported or incomplete device state")
	}
	return nil
}

func validateRoom(state RoomState) error {
	if state.Version != stateVersion || state.RoomID == "" || state.TailcatKey.IsZero() || state.TailcatPresharedKey.IsZero() || len(state.AuthorityPrivate) != ed25519.PrivateKeySize {
		return errors.New("unsupported or incomplete room state")
	}
	return validateDevice(state.Device)
}

func loadJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) > 1<<20 {
		return errors.New("state file exceeds 1 MiB")
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func saveJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return fmt.Errorf("create state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("protect state temp file: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

// SaveRoom persists the room after the first Tailcat start or after a
// certificate is generated.
func SaveRoom(path string, state RoomState) error { return saveJSON(path, state) }

// SaveDevice persists a client identity after creation or rotation.
func SaveDevice(path string, state DeviceState) error { return saveJSON(path, state) }
