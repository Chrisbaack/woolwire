package m0

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/tailscale/tailcat"
)

const (
	invitationVersion    = 1
	invitationMaxSize    = 4096
	invitationSecretSize = 32
	invitationIDSize     = 16
)

// Invitation is the self-contained bootstrap capability used by the M0
// probe. The secret is never written to logs by the probe; callers should
// treat the encoded value like a password.
type Invitation struct {
	Version         uint8  `json:"v"`
	RoomID          string `json:"room"`
	AuthorityPublic string `json:"authority"`
	BootstrapAddr   string `json:"bootstrap"`
	InvitationID    string `json:"id"`
	AdmissionSecret string `json:"secret"`
}

// NewInvitation creates a reusable invitation with a cryptographically
// random admission secret. Reuse is intentional; rotation is explicit.
func NewInvitation(roomID string, authority ed25519.PublicKey, address tailcat.Addr) (Invitation, error) {
	if roomID == "" || len(authority) != ed25519.PublicKeySize || address == "" {
		return Invitation{}, errors.New("room id, authority key, and bootstrap address are required")
	}
	if _, err := tailcat.ParseAddr(address); err != nil {
		return Invitation{}, fmt.Errorf("invalid bootstrap address: %w", err)
	}
	id, err := randomToken(invitationIDSize)
	if err != nil {
		return Invitation{}, err
	}
	secret, err := randomToken(invitationSecretSize)
	if err != nil {
		return Invitation{}, err
	}
	return Invitation{
		Version:         invitationVersion,
		RoomID:          roomID,
		AuthorityPublic: encodeToken(authority),
		BootstrapAddr:   string(address),
		InvitationID:    encodeToken(id),
		AdmissionSecret: encodeToken(secret),
	}, nil
}

// Encode returns a URL-safe, copy/paste-friendly invitation code.
func (i Invitation) Encode() (string, error) {
	if err := i.Validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(i)
	if err != nil {
		return "", fmt.Errorf("encode invitation: %w", err)
	}
	code := base64.RawURLEncoding.EncodeToString(payload)
	if len(code) > invitationMaxSize {
		return "", errors.New("encoded invitation exceeds size limit")
	}
	return code, nil
}

// Parse validates bounds and all cryptographic field lengths before the code
// is accepted by a caller that is about to initiate a network connection.
func ParseInvitation(code string) (Invitation, error) {
	if len(code) == 0 || len(code) > invitationMaxSize {
		return Invitation{}, errors.New("invitation size is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(code)
	if err != nil || len(payload) > invitationMaxSize {
		return Invitation{}, errors.New("invitation is not valid URL-safe base64")
	}
	var invitation Invitation
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&invitation); err != nil {
		return Invitation{}, fmt.Errorf("decode invitation: %w", err)
	}
	if err := invitation.Validate(); err != nil {
		return Invitation{}, err
	}
	return invitation, nil
}

func (i Invitation) Validate() error {
	if i.Version != invitationVersion {
		return fmt.Errorf("unsupported invitation version %d", i.Version)
	}
	if i.RoomID == "" || len(i.RoomID) > 128 {
		return errors.New("room id is invalid")
	}
	authority, err := decodeToken(i.AuthorityPublic, ed25519.PublicKeySize)
	if err != nil {
		return fmt.Errorf("authority key: %w", err)
	}
	if len(authority) != ed25519.PublicKeySize {
		return errors.New("authority key has invalid length")
	}
	if i.BootstrapAddr == "" || len(i.BootstrapAddr) > 8192 {
		return errors.New("bootstrap address is invalid")
	}
	if _, err := tailcat.ParseAddr(tailcat.Addr(i.BootstrapAddr)); err != nil {
		return fmt.Errorf("bootstrap address: %w", err)
	}
	if _, err := decodeToken(i.InvitationID, invitationIDSize); err != nil {
		return fmt.Errorf("invitation id: %w", err)
	}
	if _, err := decodeToken(i.AdmissionSecret, invitationSecretSize); err != nil {
		return fmt.Errorf("admission secret: %w", err)
	}
	return nil
}

// InviteRegistry models explicit invitation rotation. Existing admitted
// device identities are tracked separately and are not invalidated by this
// registry, matching the architecture's rotation semantics.
type InviteRegistry struct {
	mu     sync.RWMutex
	active Invitation
}

func NewInviteRegistry(invitation Invitation) (*InviteRegistry, error) {
	if err := invitation.Validate(); err != nil {
		return nil, err
	}
	return &InviteRegistry{active: invitation}, nil
}

func (r *InviteRegistry) Active() Invitation {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active
}

func (r *InviteRegistry) Rotate(roomID string, authority ed25519.PublicKey, address tailcat.Addr) (Invitation, error) {
	next, err := NewInvitation(roomID, authority, address)
	if err != nil {
		return Invitation{}, err
	}
	r.mu.Lock()
	r.active = next
	r.mu.Unlock()
	return next, nil
}

func (r *InviteRegistry) Accept(invitationID, secret string) bool {
	r.mu.RLock()
	active := r.active
	r.mu.RUnlock()
	if subtle.ConstantTimeCompare([]byte(active.InvitationID), []byte(invitationID)) != 1 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(active.AdmissionSecret), []byte(secret)) == 1
}

func randomToken(size int) ([]byte, error) {
	token := make([]byte, size)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("generate random token: %w", err)
	}
	return token, nil
}

func encodeToken(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func decodeToken(value string, expected int) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(b) != expected {
		return nil, errors.New("invalid token")
	}
	return b, nil
}
