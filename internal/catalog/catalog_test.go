package catalog

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/Chrisbaack/woolwire/internal/identity"
)

func TestModelAdSigningAndFreshness(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	ad := ModelAd{
		RoomID:        "room-1",
		HostMemberID:  "m-alice",
		ModelID:       "qwen-3.8b",
		Revision:      1,
		Name:          "Qwen 3.8B",
		ContextLimit:  4096,
		Availability:  "ready",
		QueueEstimate: 0,
		IsManaged:     false,
	}

	if err := ad.Sign(priv); err != nil {
		t.Fatalf("sign ad: %v", err)
	}
	if err := ad.Verify(pub); err != nil {
		t.Fatalf("verify ad: %v", err)
	}

	// Tampered ad fails
	tampered := ad
	tampered.Name = "Malicious Model"
	if err := tampered.Verify(pub); err == nil {
		t.Fatal("tampered ad should fail verification")
	}

	// Catalog operations
	cat := NewCatalog()
	if err := cat.Upsert(ad, pub); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	avail := cat.ListAvailable()
	if len(avail) != 1 || avail[0].Name != "Qwen 3.8B" {
		t.Fatalf("unexpected available ads: %#v", avail)
	}

	// Stale ad (>90s) check
	staleAd := ad
	staleAd.Timestamp = time.Now().Unix() - 100
	// Re-sign with older timestamp
	sig := ed25519.Sign(priv, staleAd.Payload())
	staleAd.Signature = identityEncode(sig)

	catStale := NewCatalog()
	_ = catStale.Upsert(staleAd, pub)
	if len(catStale.ListAvailable()) != 0 {
		t.Fatal("stale ad older than 90s should be excluded from available list")
	}
}

func identityEncode(b []byte) string {
	return identity.EncodeToken(b)
}
