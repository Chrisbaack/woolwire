package m0

import (
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
)

func newTestEndpoint(t *testing.T, statePath string) (*RoomEndpoint, RoomState, Invitation) {
	t.Helper()
	state, err := LoadRoom(statePath, "room-test")
	if err != nil {
		t.Fatal(err)
	}
	authority := state.AuthorityPrivate.Public().(ed25519.PublicKey)
	address := tailcat.NewPrivateKey().Public.Addr()
	invitation, err := NewInvitation(state.RoomID, authority, address)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewInviteRegistry(invitation)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := NewRoomEndpoint(statePath, state, registry, func(*tls.Conn, Hello) {})
	if err != nil {
		t.Fatal(err)
	}
	return endpoint, state, invitation
}

func exchangeHelloResult(endpoint *RoomEndpoint, invitation Invitation, device DeviceState) (HelloAck, error) {
	serverConn, clientConn := net.Pipe()
	serverDone := make(chan struct{})
	go func() {
		endpoint.handleTCP(ProtocolPort)(serverConn)
		close(serverDone)
	}()

	config, err := ClientTLSConfig(invitation, device)
	if err != nil {
		_ = clientConn.Close()
		return HelloAck{}, err
	}
	tlsConn := tls.Client(clientConn, config)
	if err := tlsConn.Handshake(); err != nil {
		_ = clientConn.Close()
		return HelloAck{}, err
	}
	devicePublic, err := DevicePublic(device)
	if err != nil {
		_ = clientConn.Close()
		return HelloAck{}, err
	}
	err = WriteFrame(tlsConn, Hello{
		Version:         protocolVersion,
		RoomID:          invitation.RoomID,
		InvitationID:    invitation.InvitationID,
		AdmissionSecret: invitation.AdmissionSecret,
		DevicePublic:    devicePublic,
	})
	if err != nil {
		_ = clientConn.Close()
		return HelloAck{}, err
	}
	var ack HelloAck
	if err := ReadFrame(tlsConn, &ack); err != nil {
		_ = clientConn.Close()
		return HelloAck{}, err
	}
	// Closing the underlying in-memory transport avoids waiting for a TLS
	// close-notify exchange after the endpoint has already acknowledged the
	// hello. The endpoint still exercises a real TLS handshake and framing.
	_ = clientConn.Close()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		return HelloAck{}, errors.New("endpoint did not finish the connection")
	}
	return ack, nil
}

func exchangeHello(t *testing.T, endpoint *RoomEndpoint, invitation Invitation, device DeviceState) HelloAck {
	t.Helper()
	ack, err := exchangeHelloResult(endpoint, invitation, device)
	if err != nil {
		t.Fatal(err)
	}
	return ack
}

func TestAdmittedDeviceReconnectsAfterRotationAndRoomReload(t *testing.T) {
	roomPath := filepath.Join(t.TempDir(), "room.json")
	endpoint, state, invitation := newTestEndpoint(t, roomPath)
	first := exchangeHello(t, endpoint, invitation, state.Device)
	if !first.Accepted {
		t.Fatalf("first admission rejected: %#v", first)
	}
	devicePublic, err := DevicePublic(state.Device)
	if err != nil {
		t.Fatal(err)
	}
	if got := endpoint.State().AdmittedDevices; len(got) != 1 || got[0] != devicePublic {
		t.Fatalf("admitted devices after first join = %#v", got)
	}

	authority := state.AuthorityPrivate.Public().(ed25519.PublicKey)
	rotated, err := endpoint.registry.Rotate(state.RoomID, authority, tailcat.NewPrivateKey().Public.Addr())
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadRoom(roomPath, state.RoomID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.AdmittedDevices) != 1 || reloaded.AdmittedDevices[0] != devicePublic {
		t.Fatalf("reloaded admitted devices = %#v", reloaded.AdmittedDevices)
	}
	registry, err := NewInviteRegistry(rotated)
	if err != nil {
		t.Fatal(err)
	}
	reloadedEndpoint, err := NewRoomEndpoint(roomPath, reloaded, registry, func(*tls.Conn, Hello) {})
	if err != nil {
		t.Fatal(err)
	}
	if ack := exchangeHello(t, reloadedEndpoint, invitation, reloaded.Device); !ack.Accepted {
		t.Fatalf("admitted device could not reconnect after rotation: %#v", ack)
	}

	newDevice, err := LoadDevice(filepath.Join(t.TempDir(), "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	if ack := exchangeHello(t, reloadedEndpoint, invitation, newDevice); ack.Accepted {
		t.Fatal("new device was admitted with rotated invitation")
	}
}

func TestAdmissionStorageFailureDoesNotGrantDevice(t *testing.T) {
	roomDir := t.TempDir()
	parentFile := filepath.Join(roomDir, "parent")
	if err := os.WriteFile(parentFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(parentFile, "room.json")
	endpoint, state, invitation := newTestEndpoint(t, filepath.Join(roomDir, "room.json"))
	endpoint.statePath = statePath
	ack := exchangeHello(t, endpoint, invitation, state.Device)
	if ack.Accepted {
		t.Fatal("device was admitted despite room-state persistence failure")
	}
	if got := endpoint.State().AdmittedDevices; len(got) != 0 {
		t.Fatalf("storage failure changed in-memory admission state: %#v", got)
	}
}

func TestConcurrentAdmissionsAreAllPersisted(t *testing.T) {
	roomPath := filepath.Join(t.TempDir(), "room.json")
	endpoint, state, invitation := newTestEndpoint(t, roomPath)
	deviceA, err := LoadDevice(filepath.Join(t.TempDir(), "device-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	deviceB, err := LoadDevice(filepath.Join(t.TempDir(), "device-b.json"))
	if err != nil {
		t.Fatal(err)
	}
	acks := make(chan HelloAck, 2)
	errs := make(chan error, 2)
	for _, device := range []DeviceState{deviceA, deviceB} {
		go func(device DeviceState) {
			ack, err := exchangeHelloResult(endpoint, invitation, device)
			if err != nil {
				errs <- err
				return
			}
			acks <- ack
		}(device)
	}
	for range 2 {
		select {
		case err := <-errs:
			t.Fatal(err)
		case ack := <-acks:
			if !ack.Accepted {
				t.Fatalf("concurrent admission rejected: %#v", ack)
			}
		}
	}
	reloaded, err := LoadRoom(roomPath, state.RoomID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.AdmittedDevices) != 2 {
		t.Fatalf("persisted %d concurrent admissions, want 2: %#v", len(reloaded.AdmittedDevices), reloaded.AdmittedDevices)
	}
}
