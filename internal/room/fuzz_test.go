package room

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

// FuzzParseInvitation checks that no input panics and that any code which
// parses successfully round-trips to the same invitation.
func FuzzParseInvitation(f *testing.F) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.Fatal(err)
	}
	inv, err := NewInvitation("room-1", pub, "tailcat-addr")
	if err != nil {
		f.Fatal(err)
	}
	code, err := inv.Encode()
	if err != nil {
		f.Fatal(err)
	}

	seeds := []string{
		code,
		"",
		"!!!!",
		"AQ",
		strings.Repeat("A", InvitationMaxSize+1),
		strings.Repeat("A", 100),
		"eyJ2IjoxfQ",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		parsed, err := ParseInvitation(raw)
		if err != nil {
			return
		}

		// A parsed invitation is fully validated, so re-encoding and
		// re-parsing must reproduce it exactly.
		if err := parsed.Validate(); err != nil {
			t.Fatalf("ParseInvitation accepted an invalid invitation: %v", err)
		}
		reencoded, err := parsed.Encode()
		if err != nil {
			t.Fatalf("a parsed invitation failed to re-encode: %v", err)
		}
		again, err := ParseInvitation(reencoded)
		if err != nil {
			t.Fatalf("a re-encoded invitation failed to parse: %v", err)
		}
		if again != parsed {
			t.Fatalf("round trip changed the invitation: %#v vs %#v", again, parsed)
		}
	})
}

// FuzzMembershipVerify checks that verification never panics and never accepts
// a record whose signature was not made over its own payload.
func FuzzMembershipVerify(f *testing.F) {
	f.Add("m-1", "room-1", "dev", "Alice", "admitted", int64(1), "sig", uint8(1))
	f.Add("", "", "", "", "", int64(0), "", uint8(0))
	f.Add("m-1", "room-1", "dev", "Alice:admitted:1", "admitted", int64(1), "sig", uint8(0))

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, memberID, roomID, devicePublic, displayName, status string,
		version int64, signature string, sigVersion uint8) {
		m := Membership{
			MemberID:      memberID,
			RoomID:        roomID,
			DevicePublic:  devicePublic,
			DisplayName:   displayName,
			Status:        MemberStatus(status),
			RosterVersion: version,
			Signature:     signature,
			SigVersion:    sigVersion,
		}
		// Must not panic; an arbitrary signature must not verify.
		if err := m.Verify(pub); err == nil {
			t.Fatalf("a fuzzed membership verified: %#v", m)
		}

		// Signing the same struct always produces a record that verifies.
		if memberID != "" && roomID != "" && devicePublic != "" && displayName != "" {
			signed := m
			signed.Status = StatusAdmitted
			if err := signed.Sign(priv); err != nil {
				t.Fatalf("sign failed: %v", err)
			}
			if err := signed.Verify(pub); err != nil {
				t.Fatalf("a freshly signed membership failed to verify: %v", err)
			}
		}
	})
}
