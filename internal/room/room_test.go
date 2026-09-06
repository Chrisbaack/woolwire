package room

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestInvitationLifecycle(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	inv, err := NewInvitation("room-test-1", pub, "tailcat://addr-1")
	if err != nil {
		t.Fatalf("new invitation: %v", err)
	}

	code, err := inv.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	parsed, err := ParseInvitation(code)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if parsed.RoomID != inv.RoomID || parsed.AuthorityPublic != inv.AuthorityPublic || parsed.BootstrapAddr != inv.BootstrapAddr {
		t.Fatalf("parsed invitation mismatch: got %#v, want %#v", parsed, inv)
	}

	if !parsed.VerifySecret(inv.AdmissionSecret) {
		t.Fatal("secret verification failed")
	}

	if parsed.VerifySecret("wrong-secret") {
		t.Fatal("wrong secret unexpectedly verified")
	}
}

func TestMembershipAndRoster(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	m1 := Membership{
		MemberID:      "member-alice",
		RoomID:        "room-test-1",
		DevicePublic:  "alice-pub-key",
		DisplayName:   "Alice",
		Status:        StatusAdmitted,
		RosterVersion: 1,
	}

	if err := m1.Sign(priv); err != nil {
		t.Fatalf("sign membership: %v", err)
	}

	if err := m1.Verify(pub); err != nil {
		t.Fatalf("verify membership: %v", err)
	}

	// Verify with wrong authority fails
	wrongPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := m1.Verify(wrongPub); err == nil {
		t.Fatal("expected wrong authority to fail verification")
	}

	// Roster
	roster, err := NewRoster("room-test-1", pub)
	if err != nil {
		t.Fatalf("new roster: %v", err)
	}

	if err := roster.Apply(m1); err != nil {
		t.Fatalf("apply membership: %v", err)
	}

	if !roster.IsAdmitted("member-alice", "alice-pub-key") {
		t.Fatal("alice should be admitted")
	}

	// Remove Alice with higher roster version
	m1Removed := m1
	m1Removed.Status = StatusRemoved
	m1Removed.RosterVersion = 2
	if err := m1Removed.Sign(priv); err != nil {
		t.Fatalf("sign removal: %v", err)
	}

	if err := roster.Apply(m1Removed); err != nil {
		t.Fatalf("apply removal: %v", err)
	}

	if roster.IsAdmitted("member-alice", "alice-pub-key") {
		t.Fatal("alice should no longer be admitted")
	}
	if !roster.IsRemoved("member-alice") {
		t.Fatal("alice should be marked removed")
	}

	// Stale update (version 1) must not revert the removal (version 2)
	if err := roster.Apply(m1); err != nil {
		t.Fatalf("apply stale: %v", err)
	}
	if roster.IsAdmitted("member-alice", "alice-pub-key") {
		t.Fatal("stale update must not resurrect removed member")
	}
}
