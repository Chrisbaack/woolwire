package room

import (
	"bytes"
	"compress/flate"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/cbaack/woolwire/internal/identity"
)

const (
	InvitationVersion    = 1
	InvitationMaxSize    = 4096
	InvitationSecretSize = 32
	InvitationIDSize     = 16
	compressedMagicByte  = 0x01
)

type Invitation struct {
	Version         uint8  `json:"v"`
	RoomID          string `json:"room"`
	AuthorityPublic string `json:"authority"`
	BootstrapAddr   string `json:"bootstrap"`
	InvitationID    string `json:"id"`
	AdmissionSecret string `json:"secret"`
}

func NewInvitation(roomID string, authority ed25519.PublicKey, bootstrapAddr string) (Invitation, error) {
	if roomID == "" || len(authority) != ed25519.PublicKeySize || bootstrapAddr == "" {
		return Invitation{}, errors.New("room id, authority public key, and bootstrap address are required")
	}

	idBytes := make([]byte, InvitationIDSize)
	if _, err := rand.Read(idBytes); err != nil {
		return Invitation{}, fmt.Errorf("generate invitation id: %w", err)
	}

	secretBytes := make([]byte, InvitationSecretSize)
	if _, err := rand.Read(secretBytes); err != nil {
		return Invitation{}, fmt.Errorf("generate admission secret: %w", err)
	}

	return Invitation{
		Version:         InvitationVersion,
		RoomID:          roomID,
		AuthorityPublic: identity.EncodeToken(authority),
		BootstrapAddr:   bootstrapAddr,
		InvitationID:    identity.EncodeToken(idBytes),
		AdmissionSecret: identity.EncodeToken(secretBytes),
	}, nil
}

func (i Invitation) Encode() (string, error) {
	if err := i.Validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(i)
	if err != nil {
		return "", fmt.Errorf("marshal invitation: %w", err)
	}

	// Compress payload using flate (best compression) with magic prefix byte
	var buf bytes.Buffer
	buf.WriteByte(compressedMagicByte)
	zw, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return "", fmt.Errorf("compress invitation: %w", err)
	}
	if _, err := zw.Write(payload); err != nil {
		return "", fmt.Errorf("write compressed invitation: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("flush compressed invitation: %w", err)
	}

	code := base64.RawURLEncoding.EncodeToString(buf.Bytes())
	if len(code) > InvitationMaxSize {
		return "", errors.New("encoded invitation code exceeds size limit")
	}
	return code, nil
}

func ParseInvitation(code string) (Invitation, error) {
	if len(code) == 0 || len(code) > InvitationMaxSize {
		return Invitation{}, errors.New("invalid invitation code length")
	}
	payload, err := base64.RawURLEncoding.DecodeString(code)
	if err != nil {
		return Invitation{}, errors.New("invitation code is not valid URL-safe base64")
	}

	var jsonBytes []byte
	if len(payload) > 0 && payload[0] == compressedMagicByte {
		zr := flate.NewReader(bytes.NewReader(payload[1:]))
		decompressed, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			return Invitation{}, fmt.Errorf("decompress invitation: %w", err)
		}
		jsonBytes = decompressed
	} else {
		// Backward compatible: legacy uncompressed JSON format
		jsonBytes = payload
	}

	var inv Invitation
	decoder := json.NewDecoder(bytes.NewReader(jsonBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inv); err != nil {
		return Invitation{}, fmt.Errorf("decode invitation: %w", err)
	}

	if err := inv.Validate(); err != nil {
		return Invitation{}, err
	}
	return inv, nil
}

func (i Invitation) Validate() error {
	if i.Version != InvitationVersion {
		return fmt.Errorf("unsupported invitation version %d", i.Version)
	}
	if i.RoomID == "" || len(i.RoomID) > 128 {
		return errors.New("invalid room id")
	}
	if _, err := identity.DecodeToken(i.AuthorityPublic, ed25519.PublicKeySize); err != nil {
		return fmt.Errorf("invalid authority key: %w", err)
	}
	if i.BootstrapAddr == "" || len(i.BootstrapAddr) > 8192 {
		return errors.New("invalid bootstrap address")
	}
	if _, err := identity.DecodeToken(i.InvitationID, InvitationIDSize); err != nil {
		return fmt.Errorf("invalid invitation id: %w", err)
	}
	if _, err := identity.DecodeToken(i.AdmissionSecret, InvitationSecretSize); err != nil {
		return fmt.Errorf("invalid admission secret: %w", err)
	}
	return nil
}

func (i Invitation) VerifySecret(secret string) bool {
	return subtle.ConstantTimeCompare([]byte(i.AdmissionSecret), []byte(secret)) == 1
}
