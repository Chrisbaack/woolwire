package m0

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

// PeerState is the disposable M0 state needed for a member to serve another
// member after the creator has gone away. It contains only this member's
// Tailcat identity and its authority-signed application certificate and
// credential. In particular, it never contains the room authority private
// key.
type PeerState struct {
	Version             int             `json:"version"`
	RoomID              string          `json:"room_id"`
	TailcatKey          key.NodePrivate `json:"tailcat_key"`
	TailcatPresharedKey tailcat.PresharedKey `json:"tailcat_preshared_key"`
	TailcatAddr         tailcat.Addr   `json:"tailcat_addr"`
	Device              DeviceState     `json:"device"`
	PeerCertDER         []byte          `json:"peer_certificate_der,omitempty"`
	Credential          *PeerCredential `json:"credential,omitempty"`
}

// LoadPeer creates or loads a member's persistent identity. Enrollment fills
// in PeerCertDER and Credential later; the address is retained independently
// so an endpoint restart does not silently create a new Tailcat identity.
func LoadPeer(path, roomID string) (PeerState, error) {
	var state PeerState
	if err := loadJSON(path, &state); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return PeerState{}, err
		}
		if roomID == "" {
			return PeerState{}, errors.New("room id is required when creating peer state")
		}
		state = PeerState{
			Version:             stateVersion,
			RoomID:              roomID,
			TailcatKey:          key.NewNode(),
			TailcatPresharedKey: tailcat.NewPresharedKey(),
			Device: DeviceState{
				Version:    stateVersion,
				TailcatKey: key.NewNode(),
			},
		}
		if err := ensureDeviceCertificate(&state.Device); err != nil {
			return PeerState{}, err
		}
		if err := SavePeer(path, state); err != nil {
			return PeerState{}, err
		}
	}
	if roomID != "" && state.RoomID != roomID {
		return PeerState{}, fmt.Errorf("state belongs to room %q, not %q", state.RoomID, roomID)
	}
	if err := validatePeer(state); err != nil {
		return PeerState{}, fmt.Errorf("peer state: %w", err)
	}
	return state, nil
}

// SavePeer atomically persists a member identity and its M0 enrollment.
func SavePeer(path string, state PeerState) error { return saveJSON(path, state) }

// PreparePeerAddress starts a Tailcat transport briefly to obtain and persist
// the address for a member that has not yet enrolled. The Tailcat key and
// pre-shared key remain the same when the authenticated application
// certificate arrives and the serving endpoint is restarted.
func PreparePeerAddress(path string, state *PeerState) error {
	if state == nil {
		return errors.New("peer state is required")
	}
	if state.TailcatAddr != "" {
		address, err := tailcat.ParseAddr(state.TailcatAddr)
		if err != nil {
			return fmt.Errorf("parse persisted peer address: %w", err)
		}
		if len(address.Region) != 1 {
			return errors.New("persisted peer address does not pin one DERP region")
		}
		return nil
	}
	server := &tailcat.Server{
		Key:            state.TailcatKey,
		PresharedKey:   state.TailcatPresharedKey,
		ServedTCPPorts: []filter.PortRange{{First: ProtocolPort, Last: ProtocolPort}},
		Logf:           logger.Discard,
	}
	if err := server.Start(); err != nil {
		return fmt.Errorf("start peer transport: %w", err)
	}
	address := server.TailcatAddr()
	closeErr := server.Close()
	if closeErr != nil {
		return fmt.Errorf("close peer transport: %w", closeErr)
	}
	state.TailcatAddr = address
	if err := SavePeer(path, *state); err != nil {
		return fmt.Errorf("persist peer address: %w", err)
	}
	return nil
}

func validatePeer(state PeerState) error {
	if state.Version != stateVersion || state.RoomID == "" || state.TailcatKey.IsZero() || state.TailcatPresharedKey.IsZero() {
		return errors.New("unsupported or incomplete peer state")
	}
	if err := validateDevice(state.Device); err != nil {
		return err
	}
	if state.TailcatAddr != "" {
		address, err := tailcat.ParseAddr(state.TailcatAddr)
		if err != nil || len(address.Region) != 1 {
			return errors.New("peer Tailcat address is invalid or does not pin one DERP region")
		}
	}
	if len(state.PeerCertDER) != 0 && state.Credential == nil {
		return errors.New("peer certificate has no credential")
	}
	if state.Credential != nil {
		if err := state.Credential.Validate(); err != nil {
			return err
		}
		if state.Credential.RoomID != state.RoomID || state.Credential.TailcatAddr != string(state.TailcatAddr) {
			return errors.New("peer credential does not match state")
		}
	}
	return nil
}

// PeerEndpoint serves the fixed M0 protocol port using the member's
// authority-signed certificate. It deliberately has no forwarding callback.
type PeerEndpoint struct {
	mu        sync.RWMutex
	statePath string
	state     PeerState
	authority ed25519.PublicKey
	server    *tailcat.Server
	tlsConfig *tls.Config
	onAccept  func(*tls.Conn, PeerCredential)
	closeOnce sync.Once
}

func NewPeerEndpoint(statePath string, state PeerState, authority ed25519.PublicKey, onAccept func(*tls.Conn, PeerCredential)) (*PeerEndpoint, error) {
	if len(authority) != ed25519.PublicKeySize {
		return nil, errors.New("room authority key is required")
	}
	if state.Credential == nil || len(state.PeerCertDER) == 0 {
		return nil, errors.New("peer enrollment is required before serving")
	}
	if err := VerifyPeerCredential(*state.Credential, authority); err != nil {
		return nil, err
	}
	if onAccept == nil {
		return nil, errors.New("peer accept handler is required")
	}
	tlsConfig, err := PeerServerTLSConfig(state, authority)
	if err != nil {
		return nil, err
	}
	return &PeerEndpoint{
		statePath: statePath,
		state:     state,
		authority: append(ed25519.PublicKey(nil), authority...),
		tlsConfig: tlsConfig,
		onAccept:  onAccept,
	}, nil
}

func (e *PeerEndpoint) Start() error {
	e.mu.RLock()
	state := e.state
	e.mu.RUnlock()
	server := &tailcat.Server{
		Key:            state.TailcatKey,
		PresharedKey:   state.TailcatPresharedKey,
		ServedTCPPorts: []filter.PortRange{{First: ProtocolPort, Last: ProtocolPort}},
		OnTCP:          e.handleTCP,
		Logf:           logger.Discard,
	}
	if state.TailcatAddr != "" {
		address, err := tailcat.ParseAddr(state.TailcatAddr)
		if err != nil {
			return fmt.Errorf("parse persisted peer address: %w", err)
		}
		if len(address.Region) != 1 {
			return errors.New("persisted peer address does not pin one DERP region")
		}
		server.Region = address.Region[0]
	}
	if err := server.Start(); err != nil {
		return fmt.Errorf("start Tailcat peer endpoint: %w", err)
	}
	e.mu.Lock()
	e.server = server
	if e.state.TailcatAddr == "" {
		updated := e.state
		updated.TailcatAddr = server.TailcatAddr()
		if err := SavePeer(e.statePath, updated); err != nil {
			e.mu.Unlock()
			_ = server.Close()
			e.mu.Lock()
			e.server = nil
			e.mu.Unlock()
			return fmt.Errorf("persist peer Tailcat address: %w", err)
		}
		e.state = updated
	}
	e.mu.Unlock()
	return nil
}

func (e *PeerEndpoint) Address() tailcat.Addr {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.state.TailcatAddr
}

func (e *PeerEndpoint) Close() error {
	var err error
	e.closeOnce.Do(func() {
		e.mu.RLock()
		server := e.server
		e.mu.RUnlock()
		if server != nil {
			err = server.Close()
		}
	})
	return err
}

func (e *PeerEndpoint) handleTCP(port uint16) func(net.Conn) {
	if port != ProtocolPort {
		return nil
	}
	return func(conn net.Conn) {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		tlsConn, err := AcceptTLS(conn, e.tlsConfig)
		if err != nil {
			return
		}
		defer tlsConn.Close()
		_ = tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
		var hello PeerHello
		if err := ReadFrame(tlsConn, &hello); err != nil || VerifyPeerHello(tlsConn, hello, e.authority) != nil {
			_ = WriteFrame(tlsConn, HelloAck{Accepted: false, Reason: "invalid peer hello"})
			return
		}
		if err := WriteFrame(tlsConn, HelloAck{Accepted: true}); err != nil {
			return
		}
		_ = tlsConn.SetDeadline(time.Time{})
		e.onAccept(tlsConn, hello.Credential)
	}
}

// NewPeerClient returns a Tailcat client for the signed peer address. It is
// kept separate from NewClient so a peer can never accidentally use the
// creator invitation's bearer credential as a peer authorization.
func NewPeerClient(credential PeerCredential, device DeviceState) (*tailcat.Client, error) {
	if err := credential.Validate(); err != nil {
		return nil, err
	}
	return &tailcat.Client{Server: tailcat.Addr(credential.TailcatAddr), Key: device.TailcatKey, Logf: logger.Discard}, nil
}

func DialPeerTLS(ctx context.Context, client *tailcat.Client, credential PeerCredential, device DeviceState, authority ed25519.PublicKey) (*tls.Conn, error) {
	if err := VerifyPeerCredential(credential, authority); err != nil {
		return nil, err
	}
	conn, err := client.DialTCPPort(ctx, ProtocolPort)
	if err != nil {
		return nil, err
	}
	config, err := PeerClientTLSConfig(credential, device, authority)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	tlsConn := tls.Client(conn, config)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	return tlsConn, nil
}

// EnrollPeer creates authority-signed peer credentials and a peer TLS
// certificate for a member device, recording the credential in room state.
func EnrollPeer(roomState *RoomState, device DeviceState, addr tailcat.Addr) (PeerState, error) {
	if roomState == nil {
		return PeerState{}, errors.New("room state is required")
	}
	devicePublic, err := DevicePublic(device)
	if err != nil {
		return PeerState{}, err
	}
	publicBytes, err := decodeToken(devicePublic, ed25519.PublicKeySize)
	if err != nil {
		return PeerState{}, err
	}
	certDER, err := IssuePeerCertificate(roomState.AuthorityPrivate, ed25519.PublicKey(publicBytes))
	if err != nil {
		return PeerState{}, err
	}
	cred, err := SignPeerCredential(roomState.AuthorityPrivate, roomState.RoomID, devicePublic, addr)
	if err != nil {
		return PeerState{}, err
	}
	return PeerState{
		Version:             stateVersion,
		RoomID:              roomState.RoomID,
		TailcatKey:          device.TailcatKey,
		TailcatPresharedKey: roomState.TailcatPresharedKey,
		TailcatAddr:         addr,
		Device:              device,
		PeerCertDER:         certDER,
		Credential:          &cred,
	}, nil
}

