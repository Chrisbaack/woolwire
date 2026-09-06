package m0

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
)

func newTestRoom(t *testing.T) (RoomState, string) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "room.json")
	state, err := LoadRoom(statePath, "room-m0-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureServerCertificate(&state); err != nil {
		t.Fatal(err)
	}
	return state, statePath
}

func TestPeerCredentialSigningAndVerification(t *testing.T) {
	room, _ := newTestRoom(t)
	authority := room.AuthorityPrivate.Public().(ed25519.PublicKey)

	device, err := LoadDevice(filepath.Join(t.TempDir(), "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	devicePublic, err := DevicePublic(device)
	if err != nil {
		t.Fatal(err)
	}

	testAddr := tailcat.NewPrivateKey().Public.Addr()
	cred, err := SignPeerCredential(room.AuthorityPrivate, room.RoomID, devicePublic, testAddr)
	if err != nil {
		t.Fatalf("sign peer credential: %v", err)
	}

	if err := VerifyPeerCredential(cred, authority); err != nil {
		t.Fatalf("verify with valid authority: %v", err)
	}

	// Substituted authority must fail verification.
	_, otherAuthorityPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherAuthority := otherAuthorityPrivate.Public().(ed25519.PublicKey)
	if err := VerifyPeerCredential(cred, otherAuthority); err == nil {
		t.Fatal("expected substituted authority to fail verification")
	}

	// Tampered address must fail verification.
	tamperedCred := cred
	tamperedCred.TailcatAddr = string(tailcat.NewPrivateKey().Public.Addr())
	if err := VerifyPeerCredential(tamperedCred, authority); err == nil {
		t.Fatal("expected tampered address to fail verification")
	}

	// Tampered device public key must fail verification.
	tamperedCred = cred
	_, otherDeviceKey, _ := ed25519.GenerateKey(rand.Reader)
	tamperedCred.DevicePublic = encodeToken(otherDeviceKey)
	if err := VerifyPeerCredential(tamperedCred, authority); err == nil {
		t.Fatal("expected tampered device public key to fail verification")
	}
}

func TestSubstitutedPeerRejectedByClientTLS(t *testing.T) {
	room, _ := newTestRoom(t)
	authority := room.AuthorityPrivate.Public().(ed25519.PublicKey)

	// An attacker sets up a substituted room with its own authority key.
	substitutedRoom, _ := newTestRoom(t)

	device, err := LoadDevice(filepath.Join(t.TempDir(), "client.json"))
	if err != nil {
		t.Fatal(err)
	}

	invitation, err := NewInvitation(room.RoomID, authority, tailcat.NewPrivateKey().Public.Addr())
	if err != nil {
		t.Fatal(err)
	}

	// Client expects certificates signed by room authority.
	clientConfig, err := ClientTLSConfig(invitation, device)
	if err != nil {
		t.Fatal(err)
	}

	// Substituted server presents certificate signed by attacker authority.
	substitutedServerConfig, err := ServerTLSConfig(substitutedRoom)
	if err != nil {
		t.Fatal(err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	serverDone := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		_, sErr := AcceptTLS(conn, substitutedServerConfig)
		serverDone <- sErr
	}()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	tlsClient := tls.Client(conn, clientConfig)
	clientErr := tlsClient.Handshake()
	if clientErr == nil {
		t.Fatal("expected client handshake to fail against substituted peer")
	}
	if !errors.Is(clientErr, errors.New("room server certificate is not signed by the pinned room authority")) &&
		clientErr.Error() != "room server certificate is not signed by the pinned room authority" {
		t.Logf("client rejected substituted peer with: %v", clientErr)
	}
}

func TestAdmittedPeersExchangeDataWithCreatorStopped(t *testing.T) {
	room, _ := newTestRoom(t)
	authority := room.AuthorityPrivate.Public().(ed25519.PublicKey)

	// Enroll Member A.
	deviceA, err := LoadDevice(filepath.Join(t.TempDir(), "device-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	addrA := tailcat.NewPrivateKey().Public.Addr()
	peerStateA, err := EnrollPeer(&room, deviceA, addrA)
	if err != nil {
		t.Fatalf("enroll peer A: %v", err)
	}

	// Enroll Member B.
	deviceB, err := LoadDevice(filepath.Join(t.TempDir(), "device-b.json"))
	if err != nil {
		t.Fatal(err)
	}
	addrB := tailcat.NewPrivateKey().Public.Addr()
	peerStateB, err := EnrollPeer(&room, deviceB, addrB)
	if err != nil {
		t.Fatalf("enroll peer B: %v", err)
	}

	// THE CREATOR IS STOPPED / OFFLINE.
	// Peer A serves incoming connections from other members.
	receivedMessages := make(chan string, 1)
	peerAEndpoint, err := NewPeerEndpoint(
		filepath.Join(t.TempDir(), "peer-a.json"),
		peerStateA,
		authority,
		func(conn *tls.Conn, cred PeerCredential) {
			var msg ProbeMessage
			if err := ReadFrame(conn, &msg); err != nil {
				return
			}
			receivedMessages <- msg.Body
			_ = WriteFrame(conn, ProbeReply{Type: "echo", Body: "reply:" + msg.Body})
		},
	)
	if err != nil {
		t.Fatalf("create peer A endpoint: %v", err)
	}

	// Pipe represents transport connection between Member B and Member A.
	serverConn, clientConn := net.Pipe()
	serverDone := make(chan struct{})
	go func() {
		peerAEndpoint.handleTCP(ProtocolPort)(serverConn)
		close(serverDone)
	}()

	// Member B dials Member A using Member A's credential and room authority.
	clientTLSConfig, err := PeerClientTLSConfig(*peerStateA.Credential, deviceB, authority)
	if err != nil {
		t.Fatal(err)
	}
	tlsClient := tls.Client(clientConn, clientTLSConfig)
	if err := tlsClient.Handshake(); err != nil {
		t.Fatalf("member B handshake with member A: %v", err)
	}
	defer tlsClient.Close()

	// Member B sends PeerHello containing its own signed credential.
	if err := WriteFrame(tlsClient, PeerHello{
		Version:    protocolVersion,
		Credential: *peerStateB.Credential,
	}); err != nil {
		t.Fatalf("write peer hello: %v", err)
	}

	var ack HelloAck
	if err := ReadFrame(tlsClient, &ack); err != nil {
		t.Fatalf("read peer hello ack: %v", err)
	}
	if !ack.Accepted {
		t.Fatalf("peer admission rejected: %s", ack.Reason)
	}

	// Member B sends message to Member A while creator is stopped.
	testPayload := "hello from member B with creator stopped"
	if err := WriteFrame(tlsClient, ProbeMessage{Type: "echo", Body: testPayload}); err != nil {
		t.Fatalf("write probe message: %v", err)
	}

	var reply ProbeReply
	if err := ReadFrame(tlsClient, &reply); err != nil {
		t.Fatalf("read probe reply: %v", err)
	}
	if reply.Body != "reply:"+testPayload {
		t.Fatalf("got reply %q, want %q", reply.Body, "reply:"+testPayload)
	}

	select {
	case got := <-receivedMessages:
		if got != testPayload {
			t.Fatalf("peer A received %q, want %q", got, testPayload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for peer A to process message")
	}

	_ = clientConn.Close()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("server handler did not exit")
	}
}

func TestUnauthorizedPeerRejectedByPeerEndpoint(t *testing.T) {
	room, _ := newTestRoom(t)
	authority := room.AuthorityPrivate.Public().(ed25519.PublicKey)

	// An outsider room authority.
	outsiderRoom, _ := newTestRoom(t)

	// Enroll Member A in legitimate room.
	deviceA, _ := LoadDevice(filepath.Join(t.TempDir(), "device-a.json"))
	addrA := tailcat.NewPrivateKey().Public.Addr()
	peerStateA, _ := EnrollPeer(&room, deviceA, addrA)

	// Attacker device enrolled in outsider room.
	attackerDevice, _ := LoadDevice(filepath.Join(t.TempDir(), "device-atk.json"))
	attackerAddr := tailcat.NewPrivateKey().Public.Addr()
	attackerPeerState, _ := EnrollPeer(&outsiderRoom, attackerDevice, attackerAddr)

	peerAEndpoint, err := NewPeerEndpoint(
		filepath.Join(t.TempDir(), "peer-a.json"),
		peerStateA,
		authority,
		func(*tls.Conn, PeerCredential) {},
	)
	if err != nil {
		t.Fatal(err)
	}

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	go peerAEndpoint.handleTCP(ProtocolPort)(serverConn)

	// Attacker dials Member A with legitimate Member A's credential but attacker's device.
	attackerClientConfig, err := PeerClientTLSConfig(*peerStateA.Credential, attackerDevice, authority)
	if err != nil {
		t.Fatal(err)
	}
	tlsClient := tls.Client(clientConn, attackerClientConfig)
	if err := tlsClient.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	// Attacker sends hello with its outsider credential.
	_ = WriteFrame(tlsClient, PeerHello{
		Version:    protocolVersion,
		Credential: *attackerPeerState.Credential,
	})

	var ack HelloAck
	if err := ReadFrame(tlsClient, &ack); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack.Accepted {
		t.Fatal("expected outsider peer to be rejected by peer endpoint")
	}
}

func TestRelayRegionChangeAddressRegeneration(t *testing.T) {
	nodeKey := key.NewNode()
	psk := tailcat.NewPresharedKey()

	region1 := &tailcfg.DERPRegion{
		RegionID:   901,
		RegionCode: "reg-a",
		Nodes: []*tailcfg.DERPNode{
			{Name: "901a", RegionID: 901, HostName: "derp-a.example.com", DERPPort: 443},
		},
	}
	region2 := &tailcfg.DERPRegion{
		RegionID:   902,
		RegionCode: "reg-b",
		Nodes: []*tailcfg.DERPNode{
			{Name: "902a", RegionID: 902, HostName: "derp-b.example.com", DERPPort: 443},
		},
	}

	serverA := &tailcat.Server{
		Key:          nodeKey,
		PresharedKey: psk,
		Region:       region1,
		Logf:         logger.Discard,
	}
	if err := serverA.Start(); err != nil {
		t.Fatalf("start tailcat server on region 1: %v", err)
	}
	addr1 := serverA.TailcatAddr()
	_ = serverA.Close()

	serverB := &tailcat.Server{
		Key:          nodeKey,
		PresharedKey: psk,
		Region:       region2,
		Logf:         logger.Discard,
	}
	if err := serverB.Start(); err != nil {
		t.Fatalf("start tailcat server on region 2: %v", err)
	}
	addr2 := serverB.TailcatAddr()
	_ = serverB.Close()

	if addr1 == addr2 {
		t.Fatal("expected different addresses across distinct DERP regions")
	}
	parsed1, err := tailcat.ParseAddr(addr1)
	if err != nil || len(parsed1.Region) != 1 {
		t.Fatalf("expected 1 region in parsed address, got err=%v, regions=%d", err, len(parsed1.Region))
	}
	if parsed1.Region[0].Nodes[0].HostName != "derp-a.example.com" {
		t.Fatalf("expected derp-a.example.com in parsed address, got %q", parsed1.Region[0].Nodes[0].HostName)
	}
	parsed2, err := tailcat.ParseAddr(addr2)
	if err != nil || len(parsed2.Region) != 1 {
		t.Fatalf("expected 1 region in parsed address 2, got err=%v", err)
	}
	if parsed2.Region[0].Nodes[0].HostName != "derp-b.example.com" {
		t.Fatalf("expected derp-b.example.com in parsed address 2, got %q", parsed2.Region[0].Nodes[0].HostName)
	}
}
