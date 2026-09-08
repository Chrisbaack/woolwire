package community

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Chrisbaack/woolwire/internal/identity"
)

type EventType string

const (
	EventMessage   EventType = "message"
	EventEdit      EventType = "edit"
	EventDelete    EventType = "delete"
	EventTombstone EventType = "tombstone"
	// EventChannel announces a channel so the channel list replicates through
	// the same signed log as messages instead of existing only on the node
	// that created it.
	EventChannel EventType = "channel"
)

type Event struct {
	ID               string    `json:"id"`
	RoomID           string    `json:"room_id"`
	ChannelID        string    `json:"channel_id"`
	AuthorMemberID   string    `json:"author_member_id"`
	AuthorSeq        int64     `json:"author_seq"`
	EventType        EventType `json:"event_type"`
	TargetEventID    string    `json:"target_event_id,omitempty"`
	Content          string    `json:"content"`
	Timestamp        int64     `json:"timestamp"`
	Signature        string    `json:"signature"`
	ReplicatedStatus string    `json:"replicated_status,omitempty"`
	// SigVersion selects the signing payload format; see identity.SigningPayload.
	SigVersion uint8 `json:"sig_version,omitempty"`
}

func (e *Event) Payload() []byte {
	if e.SigVersion == 0 {
		// Legacy colon-joined payload. Message content contains colons freely,
		// so this form is ambiguous and is kept only to verify existing events.
		return []byte(fmt.Sprintf("%s:%s:%s:%d:%s:%s:%s:%d",
			e.RoomID, e.ChannelID, e.AuthorMemberID, e.AuthorSeq,
			e.EventType, e.TargetEventID, e.Content, e.Timestamp))
	}
	return identity.SigningPayload(e.SigVersion, "woolwire/community-event",
		e.RoomID, e.ChannelID, e.AuthorMemberID, e.AuthorSeq,
		string(e.EventType), e.TargetEventID, e.Content, e.Timestamp)
}

func (e *Event) Sign(privateKey ed25519.PrivateKey) error {
	if len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("invalid private key size")
	}
	if e.Timestamp == 0 {
		e.Timestamp = time.Now().Unix()
	}
	e.SigVersion = identity.SigVersionCanonical

	sig := ed25519.Sign(privateKey, e.Payload())
	e.Signature = identity.EncodeToken(sig)

	// Compute canonical Event ID: sha256(payload + signature)
	h := sha256.New()
	h.Write(e.Payload())
	h.Write(sig)
	e.ID = "evt-" + hex.EncodeToString(h.Sum(nil))[:24]

	return nil
}

func (e *Event) Verify(publicKey ed25519.PublicKey) error {
	if e.ID == "" || e.RoomID == "" || e.ChannelID == "" || e.AuthorMemberID == "" || e.AuthorSeq <= 0 {
		return errors.New("incomplete community event fields")
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return errors.New("invalid public key size")
	}

	sig, err := identity.DecodeToken(e.Signature, ed25519.SignatureSize)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}

	if !ed25519.Verify(publicKey, e.Payload(), sig) {
		return errors.New("event signature verification failed")
	}

	// Verify ID matches hash
	h := sha256.New()
	h.Write(e.Payload())
	h.Write(sig)
	expectedID := "evt-" + hex.EncodeToString(h.Sum(nil))[:24]
	if e.ID != expectedID {
		return errors.New("event ID mismatch")
	}

	return nil
}
