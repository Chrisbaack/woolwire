package peerapi

import (
	"strings"
	"testing"
	"time"
)

func TestMemberIDForDevicePublic(t *testing.T) {
	// A 32-byte Ed25519 key encodes to 43 base64url characters.
	const validKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	got, err := MemberIDForDevicePublic(validKey)
	if err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	if got != "m-"+validKey[:16] {
		t.Fatalf("unexpected member id %q", got)
	}

	// The old code sliced [:16] unconditionally, so anything shorter panicked
	// the handler. Every short input must now be an error instead.
	for _, short := range []string{"", "a", "ab", strings.Repeat("x", 15)} {
		if _, err := MemberIDForDevicePublic(short); err == nil {
			t.Fatalf("a %d-character key should not yield a member id", len(short))
		}
	}
}

func TestWithinRetentionWindow(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		desc string
		ts   time.Time
		want bool
	}{
		{"now", now, true},
		{"one minute ahead", now.Add(time.Minute), true},
		{"just inside the skew", now.Add(eventFutureSkew - time.Second), true},
		{"just past the skew", now.Add(eventFutureSkew + time.Second), false},
		{"far future", now.Add(365 * 24 * time.Hour), false},
		{"inside the retention window", now.Add(-29 * 24 * time.Hour), true},
		{"past the retention horizon", now.Add(-31 * 24 * time.Hour), false},
	}

	for _, tc := range tests {
		if got := withinRetentionWindow(tc.ts.Unix(), now); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.desc, got, tc.want)
		}
	}

	if withinRetentionWindow(0, now) {
		t.Error("a zero timestamp should be refused")
	}
	if withinRetentionWindow(-1, now) {
		t.Error("a negative timestamp should be refused")
	}
}

func TestBootstrapPortIsDistinct(t *testing.T) {
	// Bootstrap and the peer API have different trust rules, so they must not
	// share a listener. The offset is what keeps them apart.
	if BootstrapPortOffset == 0 {
		t.Fatal("bootstrap must not share the peer port")
	}
}
