package m0

import (
	"crypto/ed25519"
	"testing"

	"github.com/tailscale/tailcat"
)

func TestInvitationRoundTripAndRotation(t *testing.T) {
	_, authorityPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	authority := authorityPrivate.Public().(ed25519.PublicKey)
	address := tailcat.NewPrivateKey().Public.Addr()
	invitation, err := NewInvitation("room-test", authority, address)
	if err != nil {
		t.Fatal(err)
	}
	code, err := invitation.Encode()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseInvitation(code)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewInviteRegistry(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !registry.Accept(parsed.InvitationID, parsed.AdmissionSecret) {
		t.Fatal("active invitation was rejected")
	}
	rotated, err := registry.Rotate(parsed.RoomID, authority, address)
	if err != nil {
		t.Fatal(err)
	}
	if registry.Accept(parsed.InvitationID, parsed.AdmissionSecret) {
		t.Fatal("rotated invitation remained valid")
	}
	if !registry.Accept(rotated.InvitationID, rotated.AdmissionSecret) {
		t.Fatal("new invitation was rejected")
	}
}

func TestInvitationRejectsMalformedInputs(t *testing.T) {
	if _, err := ParseInvitation("not-an-invitation"); err == nil {
		t.Fatal("expected malformed invitation to fail")
	}
	if _, err := decodeToken("!", invitationSecretSize); err == nil {
		t.Fatal("expected malformed token to fail")
	}
}
