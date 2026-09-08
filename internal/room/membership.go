package room

import (
	"crypto/ed25519"
	"errors"
	"fmt"

	"github.com/Chrisbaack/woolwire/internal/identity"
)

type MemberStatus string

const (
	StatusAdmitted MemberStatus = "admitted"
	StatusPending  MemberStatus = "pending"
	StatusRemoved  MemberStatus = "removed"
)

type Membership struct {
	MemberID      string       `json:"member_id"`
	RoomID        string       `json:"room_id"`
	DevicePublic  string       `json:"device_public"`
	DisplayName   string       `json:"display_name"`
	Status        MemberStatus `json:"status"`
	RosterVersion int64        `json:"roster_version"`
	Signature     string       `json:"signature"`
	// SigVersion selects the signing payload format. Zero means a record
	// written before payloads were canonicalized; it is still verifiable but
	// never produced.
	SigVersion uint8 `json:"sig_version,omitempty"`
}

func (m Membership) Payload() []byte {
	if m.SigVersion == 0 {
		// Legacy colon-joined payload. Display names may contain colons, so
		// this form is ambiguous and is kept only to verify existing records.
		return []byte(fmt.Sprintf("%s:%s:%s:%s:%s:%d",
			m.RoomID, m.MemberID, m.DevicePublic, m.DisplayName, m.Status, m.RosterVersion))
	}
	return identity.SigningPayload(m.SigVersion, "woolwire/membership",
		m.RoomID, m.MemberID, m.DevicePublic, m.DisplayName, string(m.Status), m.RosterVersion)
}

func (m *Membership) Sign(authorityPrivate ed25519.PrivateKey) error {
	if len(authorityPrivate) != ed25519.PrivateKeySize {
		return errors.New("invalid authority private key")
	}
	m.SigVersion = identity.SigVersionCanonical
	sig := ed25519.Sign(authorityPrivate, m.Payload())
	m.Signature = identity.EncodeToken(sig)
	return nil
}

func (m Membership) Verify(authorityPublic ed25519.PublicKey) error {
	if m.MemberID == "" || m.RoomID == "" || m.DevicePublic == "" || m.DisplayName == "" {
		return errors.New("incomplete membership record")
	}
	if m.Status != StatusAdmitted && m.Status != StatusPending && m.Status != StatusRemoved {
		return fmt.Errorf("invalid member status %q", m.Status)
	}
	if len(authorityPublic) != ed25519.PublicKeySize {
		return errors.New("invalid authority public key")
	}
	sig, err := identity.DecodeToken(m.Signature, ed25519.SignatureSize)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	if !ed25519.Verify(authorityPublic, m.Payload(), sig) {
		return errors.New("membership signature verification failed")
	}
	return nil
}
