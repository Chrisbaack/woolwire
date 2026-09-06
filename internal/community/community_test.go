package community

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/cbaack/woolwire/internal/identity"
)

func TestCommunityEventSigningAndVerification(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	memberID := "m-" + identity.EncodeToken(pub)[:16]

	event := Event{
		RoomID:         "room-123",
		ChannelID:      "chan-general",
		AuthorMemberID: memberID,
		AuthorSeq:      1,
		EventType:      EventMessage,
		Content:        "Hello test message",
		Timestamp:      time.Now().Unix(),
	}

	if err := event.Sign(priv); err != nil {
		t.Fatalf("sign failed: %v", err)
	}

	if err := event.Verify(pub); err != nil {
		t.Fatalf("verification failed: %v", err)
	}

	// Tampered content must fail verification
	tampered := event
	tampered.Content = "Malicious content injection"
	if err := tampered.Verify(pub); err == nil {
		t.Fatal("expected verification failure for tampered content")
	}

	// Wrong public key must fail
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := event.Verify(otherPub); err == nil {
		t.Fatal("expected verification failure for wrong public key")
	}
}

func TestCommunityMaterializationQuarantineAndOrdering(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	memberID := "m-" + identity.EncodeToken(pub)[:16]

	now := time.Now().Unix()

	// Event 1: seq 1
	e1 := Event{
		RoomID:         "room-1",
		ChannelID:      "chan-1",
		AuthorMemberID: memberID,
		AuthorSeq:      1,
		EventType:      EventMessage,
		Content:        "Message 1",
		Timestamp:      now,
	}
	_ = e1.Sign(priv)

	// Event 2: conflicting seq 1 (split-brain or forged replay)
	e2Conflict := Event{
		RoomID:         "room-1",
		ChannelID:      "chan-1",
		AuthorMemberID: memberID,
		AuthorSeq:      1,
		EventType:      EventMessage,
		Content:        "Conflicting message with same seq",
		Timestamp:      now + 1,
	}
	_ = e2Conflict.Sign(priv)

	// Event 3: seq 2
	e3 := Event{
		RoomID:         "room-1",
		ChannelID:      "chan-1",
		AuthorMemberID: memberID,
		AuthorSeq:      2,
		EventType:      EventMessage,
		Content:        "Message 2",
		Timestamp:      now + 2,
	}
	_ = e3.Sign(priv)

	// Materialize with both e1, e2Conflict, e3
	materialized := MaterializeEvents([]Event{e1, e2Conflict, e3})

	// Conflicting event must be quarantined (only e1 and e3 appear)
	if len(materialized) != 2 {
		t.Fatalf("expected 2 materialized messages after quarantine, got %d", len(materialized))
	}
	if materialized[0].Content != "Message 1" || materialized[1].Content != "Message 2" {
		t.Fatalf("unexpected content: %+v", materialized)
	}
}
