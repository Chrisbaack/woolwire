package localapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Chrisbaack/woolwire/internal/peerapi"
	"github.com/Chrisbaack/woolwire/internal/peerauth"
	"github.com/Chrisbaack/woolwire/internal/room"
	"github.com/Chrisbaack/woolwire/internal/store"
	"github.com/Chrisbaack/woolwire/internal/transport"
)

func TestM1FullOnboardingAndAcceptanceGate(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4242

	node1 := setupTestNode(t, vNet, "node-1", peerPort)
	node1.login(t)
	roomID, invitation := node1.hostRoom(t, "Alice", "Technical Friends")

	node2 := setupTestNode(t, vNet, "node-2", peerPort)
	node2.login(t)

	// Gate Check 1: Join room using invitation code with no manual networking setup
	if w := node2.joinRoom(t, "Bob", invitation); w.Code != http.StatusOK {
		t.Fatalf("join room Bob: %d, %s", w.Code, w.Body.String())
	}

	w := node2.doJSON("GET", "/api/v1/state", nil)
	var bobState struct {
		DisplayName string `json:"display_name"`
		Room        struct {
			RoomID         string `json:"room_id"`
			RoomName       string `json:"room_name"`
			Role           string `json:"role"`
			InvitationCode string `json:"invitation_code"`
		} `json:"room"`
	}
	_ = json.NewDecoder(w.Body).Decode(&bobState)
	if bobState.Room.Role != "member" || bobState.Room.RoomID != roomID {
		t.Fatalf("unexpected bob state: %#v", bobState)
	}
	// The creator sends the real room name; members no longer show a hardcoded one.
	if bobState.Room.RoomName != "Technical Friends" {
		t.Fatalf("member shows room name %q, want %q", bobState.Room.RoomName, "Technical Friends")
	}
	// A member must not hold the invitation code at all.
	if bobState.Room.InvitationCode != "" {
		t.Fatal("member state exposed an invitation code")
	}
	bobRoom, _ := node2.store.GetRoomState()
	if bobRoom.InvitationCode != "" {
		t.Fatal("member persisted the invitation code, which would let a leaked backup admit devices")
	}

	// The creator's address must be stored under its real member id, not one
	// derived from the authority key.
	if addr := node2.localSrv.peerAddress(node1.memberID); addr == "" {
		t.Fatal("member did not learn the creator's address under the creator's member id")
	}

	// Gate Check 2: Restart retains identity
	_ = node2.localSrv.Close()
	restarted, err := NewServer(Config{
		Store:      node2.store,
		Transport:  node2.trans,
		SetupToken: node2.setupToken,
		PeerPort:   peerPort,
	})
	if err != nil {
		t.Fatalf("restarted node 2 server: %v", err)
	}
	node2.localSrv = restarted
	w = node2.doJSON("GET", "/api/v1/state", nil)
	var restartedBobState struct {
		DisplayName string `json:"display_name"`
		Room        struct {
			Role string `json:"role"`
		} `json:"room"`
	}
	_ = json.NewDecoder(w.Body).Decode(&restartedBobState)
	if restartedBobState.DisplayName != "Bob" || restartedBobState.Room.Role != "member" {
		t.Fatalf("restart did not retain identity: %#v", restartedBobState)
	}

	// Gate Check 3: A remote caller cannot change another member's settings
	unauthReq := httptest.NewRequest("POST", "/api/v1/profile", bytes.NewReader([]byte(`{"display_name":"Hacked"}`)))
	unauthReq.Host = "127.0.0.1:7070"
	unauthReq.Header.Set("Content-Type", "application/json")
	unauthReq.Header.Set(csrfHeader, "1")
	if code := node2.doRequest(unauthReq).Code; code != http.StatusUnauthorized {
		t.Fatalf("unauthorized remote setting change should be 401, got %d", code)
	}

	// Gate Check 4: A rotated code fails new admission while existing members remain
	w = node1.doJSON("POST", "/api/v1/room-admin/invitation/rotate", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate invitation: %d, %s", w.Code, w.Body.String())
	}
	var rotateResp struct {
		InvitationCode string `json:"invitation_code"`
	}
	_ = json.NewDecoder(w.Body).Decode(&rotateResp)
	if rotateResp.InvitationCode == invitation {
		t.Fatal("rotated code should differ from original code")
	}

	node3 := setupTestNode(t, vNet, "node-3", peerPort)
	node3.login(t)

	w = node3.joinRoom(t, "Charlie", invitation)
	if w.Code == http.StatusOK {
		var joinCheck struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(w.Body).Decode(&joinCheck)
		if joinCheck.Status == "admitted" {
			t.Fatal("node 3 was admitted with rotated old code")
		}
	}

	if w := node3.doJSON("POST", "/api/v1/room/join", map[string]string{
		"invitation_code": rotateResp.InvitationCode,
	}); w.Code != http.StatusOK {
		t.Fatalf("node 3 failed to join with new code: %d, %s", w.Code, w.Body.String())
	}

	w = node1.doJSON("GET", "/api/v1/room-admin/members", nil)
	var members []store.MemberRecord
	_ = json.NewDecoder(w.Body).Decode(&members)
	var bobFound bool
	for _, m := range members {
		if m.DisplayName == "Bob" && m.Status == "admitted" {
			bobFound = true
		}
	}
	if !bobFound {
		t.Fatal("bob should remain admitted after invitation rotation")
	}

	// Gate Check 5: a removal propagates to other members and takes effect.
	// Alice removes Bob; Charlie must learn it and refuse to serve Bob.
	if w := node1.doJSON("POST", fmt.Sprintf("/api/v1/room-admin/members/%s/remove", node2.memberID), nil); w.Code != http.StatusOK {
		t.Fatalf("remove bob: %d, %s", w.Code, w.Body.String())
	}

	w = node1.doJSON("GET", "/api/v1/room-admin/members", nil)
	var updatedMembers []store.MemberRecord
	_ = json.NewDecoder(w.Body).Decode(&updatedMembers)
	for _, m := range updatedMembers {
		if m.MemberID == node2.memberID && m.Status != "removed" {
			t.Fatalf("bob should be marked removed, got %q", m.Status)
		}
	}

	// Charlie polls membership and must apply the signed removal.
	node3.localSrv.SyncMembership(context.Background())
	bobOnCharlie, err := node3.store.GetMember(node2.memberID)
	if err != nil || bobOnCharlie == nil {
		t.Fatalf("charlie has no record of bob: %v", err)
	}
	if bobOnCharlie.Status != string(room.StatusRemoved) {
		t.Fatalf("removal did not propagate to charlie: status %q", bobOnCharlie.Status)
	}

	// Bob's peer connection to Charlie must now fail at the handshake.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := node2.localSrv.dialPeer(ctx, node3.memberID, "node-3"); err == nil {
		t.Fatal("a removed member still completed a peer handshake")
	}
}

// TestUnadmittedPeerCannotReachPeerRoutes is the core task-1 check: holding a
// node's transport address is not sufficient to talk to its peer API.
//
// Under TLS 1.3 the client finishes its own handshake before the server has
// validated the client certificate, so the refusal surfaces on the first
// request rather than in HandshakeContext. The test therefore asserts that no
// route ever produces a usable response.
func TestUnadmittedPeerCannotReachPeerRoutes(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4242

	creator := setupTestNode(t, vNet, "creator", peerPort)
	creator.login(t)
	_, invitation := creator.hostRoom(t, "Alice", "Room")

	member := setupTestNode(t, vNet, "member", peerPort)
	member.login(t)
	if w := member.joinRoom(t, "Bob", invitation); w.Code != http.StatusOK {
		t.Fatalf("member join failed: %d %s", w.Code, w.Body.String())
	}

	// Node C holds a valid transport address for the member but no membership.
	outsider := setupTestNode(t, vNet, "outsider", peerPort)
	outsider.login(t)

	outsiderTLS := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{outsider.device.CertDER},
			PrivateKey:  outsider.device.PrivateKey,
		}},
	}

	for _, path := range []string{
		"/peer/v1/membership/sync",
		"/peer/v1/catalog",
		"/peer/v1/inference",
		"/peer/v1/inference/cancel",
		"/peer/v1/community/sync",
		"/peer/v1/contributions/ack",
		"/peer/v1/contributions/sync",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

		raw, err := outsider.trans.Dial(ctx, "member", peerPort)
		if err != nil {
			cancel()
			continue // the transport refused outright, which is also a refusal
		}

		conn := tls.Client(raw, outsiderTLS)
		method := "POST"
		if path == "/peer/v1/catalog" {
			method = "GET"
		}
		resp, err := peerRoundTrip(ctx, conn, method, path, map[string]any{})
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			_ = conn.Close()
			cancel()
			t.Fatalf("route %s answered an unadmitted node: %d %s", path, resp.StatusCode, body)
		}
		if resp != nil {
			resp.Body.Close()
		}
		_ = conn.Close()
		cancel()
	}

	// The same is true through the product's own dialer, which additionally
	// refuses because the outsider has no roster to name the peer with.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := outsider.localSrv.dialPeer(ctx, member.memberID, "member"); err == nil {
		t.Fatal("an unadmitted node opened an authenticated peer connection")
	}
}

// TestAdmittedMemberCannotImpersonateAnother covers the second half of task 1:
// there is no member_id field left to lie in, and the queue attributes work to
// the authenticated identity.
func TestAdmittedMemberCannotImpersonateAnother(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4242

	creator := setupTestNode(t, vNet, "creator", peerPort)
	creator.login(t)
	_, invitation := creator.hostRoom(t, "Alice", "Room")

	bob := setupTestNode(t, vNet, "bob", peerPort)
	bob.login(t)
	if w := bob.joinRoom(t, "Bob", invitation); w.Code != http.StatusOK {
		t.Fatalf("bob join: %d %s", w.Code, w.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := bob.localSrv.dialPeer(ctx, creator.memberID, "creator")
	if err != nil {
		t.Fatalf("bob should reach the creator: %v", err)
	}
	defer conn.Close()

	// Bob announces an address while claiming to be the creator. The body has
	// no member field, so the update can only ever apply to Bob.
	body := map[string]any{
		"known_version": 0,
		"member_id":     creator.memberID, // ignored: the field no longer exists
		"tailcat_addr":  "attacker-node",
	}
	resp, err := peerRoundTrip(ctx, conn, "POST", "/peer/v1/membership/sync", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sync returned %d", resp.StatusCode)
	}

	addrs, _ := creator.store.ListPeerAddresses()
	for _, pa := range addrs {
		if pa.MemberID == creator.memberID && pa.TailcatAddr == "attacker-node" {
			t.Fatal("a member rebound the creator's address")
		}
		if pa.MemberID == bob.memberID && pa.TailcatAddr != "attacker-node" {
			t.Fatalf("the caller's own address was not applied: %q", pa.TailcatAddr)
		}
	}
}

// TestJoinAdmissionRules covers task 2.
func TestJoinAdmissionRules(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4242

	creator := setupTestNode(t, vNet, "creator", peerPort)
	creator.login(t)
	_, invitation := creator.hostRoom(t, "Alice", "Room")

	bob := setupTestNode(t, vNet, "bob", peerPort)
	bob.login(t)
	if w := bob.joinRoom(t, "Bob", invitation); w.Code != http.StatusOK {
		t.Fatalf("bob join: %d %s", w.Code, w.Body.String())
	}

	t.Run("join via a member node is refused", func(t *testing.T) {
		// A member serves no bootstrap listener at all, so the port is dead.
		charlie := setupTestNode(t, vNet, "charlie-a", peerPort)
		charlie.login(t)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		bobRoom, _ := bob.store.GetRoomState()
		if len(bobRoom.RoomCertDER) != 0 {
			t.Fatal("a member must not hold a room certificate")
		}
		if _, err := charlie.trans.Dial(ctx, "bob", peerPort+peerapi.BootstrapPortOffset); err == nil {
			t.Fatal("a member node is serving a bootstrap listener")
		}
	})

	t.Run("rejoin by an admitted device is refused", func(t *testing.T) {
		// Bob's device is already admitted; presenting the code again must not
		// overwrite his roster row.
		w := bob.doJSON("POST", "/api/v1/room/join", map[string]string{"invitation_code": invitation})
		if w.Code == http.StatusOK {
			var resp struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(w.Body).Decode(&resp)
			if resp.Status == "admitted" {
				t.Fatal("an already-admitted device was re-admitted")
			}
		}
	})

	t.Run("rejoin by a removed device is refused", func(t *testing.T) {
		charlie := setupTestNode(t, vNet, "charlie-b", peerPort)
		charlie.login(t)
		if w := charlie.joinRoom(t, "Charlie", invitation); w.Code != http.StatusOK {
			t.Fatalf("charlie join: %d %s", w.Code, w.Body.String())
		}

		// The creator removes Charlie but does not rotate the code.
		w := creator.doJSON("POST",
			fmt.Sprintf("/api/v1/room-admin/members/%s/remove", charlie.memberID),
			map[string]any{"rotate_invitation": false})
		if w.Code != http.StatusOK {
			t.Fatalf("remove charlie: %d %s", w.Code, w.Body.String())
		}

		_ = charlie.store.ClearRoom()
		w = charlie.doJSON("POST", "/api/v1/room/join", map[string]string{"invitation_code": invitation})
		if w.Code == http.StatusOK {
			var resp struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(w.Body).Decode(&resp)
			if resp.Status == "admitted" {
				t.Fatal("a removed device rejoined on an unrotated code")
			}
		}

		onCreator, _ := creator.store.GetMember(charlie.memberID)
		if onCreator == nil || onCreator.Status != string(room.StatusRemoved) {
			t.Fatalf("removed member row was overwritten by a rejoin: %#v", onCreator)
		}
	})

	t.Run("malformed device key cannot reach admission", func(t *testing.T) {
		// device_public is derived from the certificate, so a short or absent
		// key in the body has nowhere to land: there is no field to send one
		// in, and a connection with no client certificate never reaches the
		// handler. This is the regression for slicing a caller-supplied string
		// to sixteen characters without decoding it.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		raw, err := bob.trans.Dial(ctx, "creator", peerPort+peerapi.BootstrapPortOffset)
		if err != nil {
			t.Fatalf("dial bootstrap: %v", err)
		}
		conn := tls.Client(raw, &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true})
		defer conn.Close()

		resp, err := peerRoundTrip(ctx, conn, "POST", "/bootstrap/v1/join", map[string]any{
			"room_id":       "whatever",
			"device_public": "ab", // no such field exists any more
		})
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatal("bootstrap answered a client with no device certificate")
			}
		}

		// And a short device_public in the body is simply ignored rather than
		// sliced, so nothing panics.
		if _, err := peerapi.MemberIDForDevicePublic("ab"); err == nil {
			t.Fatal("a 2-character device key should not yield a member id")
		}
	})
}

// TestBootstrapPinsRoomAuthority covers the joiner side of task 2: the
// bootstrap endpoint must be signed by the authority in the invitation.
func TestBootstrapPinsRoomAuthority(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4242

	creator := setupTestNode(t, vNet, "creator", peerPort)
	creator.login(t)
	_, invitation := creator.hostRoom(t, "Alice", "Room")

	joiner := setupTestNode(t, vNet, "joiner", peerPort)
	joiner.login(t)

	inv, err := room.ParseInvitation(invitation)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// A different authority key must not verify the creator's certificate.
	wrongAuthority := make([]byte, 32)
	wrongAuthority[0] = 1
	if _, err := joiner.localSrv.dialBootstrap(ctx, inv.BootstrapAddr, wrongAuthority); err == nil {
		t.Fatal("bootstrap accepted a certificate not signed by the pinned authority")
	}
}

func TestSecurityMiddlewareLANAndDNSRebinding(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "sec-node", 4242)
	node.localSrv.allowedHosts = []string{"woolwire.local", "my-nas.lan"}

	tests := []struct {
		host      string
		wantBlock bool
		desc      string
	}{
		{"127.0.0.1:7070", false, "standard IPv4 loopback"},
		{"localhost:7070", false, "standard localhost"},
		{"[::1]:7070", false, "bracketed IPv6 loopback"},
		{"::1", false, "raw IPv6 loopback"},
		{"192.168.1.100:7070", false, "RFC1918 Class C private LAN IP"},
		{"10.0.0.5:7070", false, "RFC1918 Class A private LAN IP"},
		{"172.16.5.2:7070", false, "RFC1918 Class B private LAN IP"},
		{"[fe80::1]:7070", false, "IPv6 link-local address"},
		{"[fc00::1]:7070", false, "IPv6 unique local address"},
		{"woolwire.local:7070", false, "configured mDNS allowed host"},
		{"my-nas.lan", false, "configured domain without port"},
		{"evil-attacker.com:7070", true, "public domain (DNS rebinding attempt)"},
		{"subdomain.rebind.attacker.io", true, "subdomain attack"},
		{"8.8.8.8:7070", true, "public IP address literal"},
		{"1.1.1.1", true, "public IPv4"},
	}

	for _, tc := range tests {
		req := httptest.NewRequest("GET", "/api/v1/state", nil)
		req.Host = tc.host
		w := node.doRequest(req)

		if tc.wantBlock && w.Code != http.StatusForbidden {
			t.Errorf("[%s] host %q should be blocked with 403, got %d", tc.desc, tc.host, w.Code)
		}
		if !tc.wantBlock && w.Code == http.StatusForbidden {
			t.Errorf("[%s] host %q should NOT be blocked with 403 Forbidden", tc.desc, tc.host)
		}
	}
}

// TestCSRFProtection covers task 7. A page on another 127.0.0.1 port is
// same-site to the browser, so SameSite alone never blocked it.
func TestCSRFProtection(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "csrf-node", 4242)
	node.login(t)

	t.Run("cross-origin POST with a valid cookie is rejected", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/profile",
			bytes.NewReader([]byte(`{"display_name":"Hacked"}`)))
		req.Host = "127.0.0.1:7070"
		req.Header.Set("Origin", "http://127.0.0.1:3000")
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(node.cookie)

		if code := node.doRequest(req).Code; code != http.StatusForbidden {
			t.Fatalf("expected 403 for a cross-origin POST, got %d", code)
		}
	})

	t.Run("simple cross-origin POST with text/plain is rejected", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/room/leave", bytes.NewReader([]byte(`{}`)))
		req.Host = "127.0.0.1:7070"
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		req.Header.Set("Origin", "http://127.0.0.1:7070")
		req.AddCookie(node.cookie)

		if code := node.doRequest(req).Code; code != http.StatusForbidden {
			t.Fatalf("expected 403 for a text/plain POST, got %d", code)
		}
	})

	t.Run("same-origin POST is accepted", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/v1/profile",
			bytes.NewReader([]byte(`{"display_name":"Alice"}`)))
		req.Host = "127.0.0.1:7070"
		req.Header.Set("Origin", "http://127.0.0.1:7070")
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(node.cookie)

		if code := node.doRequest(req).Code; code != http.StatusOK {
			t.Fatalf("expected 200 for a same-origin POST, got %d", code)
		}
	})

	t.Run("the SPA header alone is accepted", func(t *testing.T) {
		if code := node.doJSON("POST", "/api/v1/profile",
			map[string]string{"display_name": "Alice"}).Code; code != http.StatusOK {
			t.Fatalf("expected 200 with the SPA header, got %d", code)
		}
	})

	t.Run("session cookie is SameSite=Strict", func(t *testing.T) {
		if node.cookie.SameSite != http.SameSiteStrictMode {
			t.Fatalf("session cookie SameSite is %v, want Strict", node.cookie.SameSite)
		}
	})
}

// TestSetupSecretIsOneTimeAndRateLimited covers task 6.
func TestSetupSecretIsOneTimeAndRateLimited(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "setup-node", 4299)

	// Unauthenticated setup info is refused.
	if code := node.doJSON("GET", "/api/v1/setup/info", nil).Code; code != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauthorized, got %d", code)
	}

	node.login(t)

	// The secret is never echoed back, only its state.
	w := node.doJSON("GET", "/api/v1/setup/info", nil)
	var info map[string]any
	_ = json.NewDecoder(w.Body).Decode(&info)
	if _, leaked := info["token"]; leaked {
		t.Fatal("setup info returned the secret itself")
	}
	if info["redeemed"] != true {
		t.Fatalf("expected the secret to be marked redeemed, got %#v", info)
	}

	// Redeeming again fails: the secret is one-time.
	replay := node.doJSON("POST", "/api/v1/setup", map[string]string{"token": node.setupToken})
	if replay.Code != http.StatusGone {
		t.Fatalf("expected 410 on a redeemed secret, got %d: %s", replay.Code, replay.Body.String())
	}

	// A short custom secret is refused.
	if code := node.doJSON("POST", "/api/v1/setup/token", map[string]string{"token": "987654"}).Code; code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a 6-character secret, got %d", code)
	}

	// Resetting with no body mints a fresh 128-bit secret, returned once.
	w = node.doJSON("POST", "/api/v1/setup/token", map[string]any{})
	if w.Code != http.StatusOK {
		t.Fatalf("reset setup secret: %d %s", w.Code, w.Body.String())
	}
	var reset struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(w.Body).Decode(&reset)
	if len(reset.Token) != 32 {
		t.Fatalf("expected a 128-bit hex secret, got %q", reset.Token)
	}

	// Ten wrong guesses must be delayed.
	var sawBackoff bool
	for i := 0; i < 10; i++ {
		req := node.newRequest("POST", "/api/v1/setup", map[string]string{"token": "wrong"})
		req.RemoteAddr = "192.0.2.10:5555"
		if node.doRequest(req).Code == http.StatusTooManyRequests {
			sawBackoff = true
			break
		}
	}
	if !sawBackoff {
		t.Fatal("ten failed setup attempts were not rate limited")
	}

	// The freshly minted secret still works from a different address.
	req := node.newRequest("POST", "/api/v1/setup", map[string]string{"token": reset.Token})
	req.RemoteAddr = "192.0.2.20:5555"
	if code := node.doRequest(req).Code; code != http.StatusOK {
		t.Fatalf("fresh secret should be redeemable, got %d", code)
	}
}

// TestRoomStateEdgeCases covers task 27.
func TestRoomStateEdgeCases(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4242

	t.Run("hosting requires a display name", func(t *testing.T) {
		node := setupTestNode(t, vNet, "no-name", peerPort)
		node.login(t)
		if code := node.doJSON("POST", "/api/v1/room/host",
			map[string]string{"room_name": "Room"}).Code; code != http.StatusBadRequest {
			t.Fatalf("expected 400 without a display name, got %d", code)
		}
	})

	t.Run("hosting twice is refused", func(t *testing.T) {
		node := setupTestNode(t, vNet, "double-host", peerPort)
		node.login(t)
		node.hostRoom(t, "Alice", "First")

		if code := node.doJSON("POST", "/api/v1/room/host",
			map[string]string{"room_name": "Second"}).Code; code != http.StatusConflict {
			t.Fatalf("expected 409 for a second room, got %d", code)
		}
		if n, _ := node.store.CountRoomStates(); n != 1 {
			t.Fatalf("expected exactly one room row, got %d", n)
		}
	})

	t.Run("leaving stops the peer server", func(t *testing.T) {
		node := setupTestNode(t, vNet, "leaver", peerPort)
		node.login(t)
		node.hostRoom(t, "Alice", "Room")

		if node.peerServer() == nil {
			t.Fatal("hosting did not start a peer server")
		}
		if code := node.doJSON("POST", "/api/v1/room/leave", nil).Code; code != http.StatusOK {
			t.Fatal("leave failed")
		}
		if node.peerServer() != nil {
			t.Fatal("leaving a room left the peer server running")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		other := setupTestNode(t, vNet, "leaver-probe", peerPort)
		if _, err := other.trans.Dial(ctx, "leaver", peerPort); err == nil {
			t.Fatal("the peer listener is still accepting connections after leave")
		}
	})

	t.Run("approval mode admits through the creator", func(t *testing.T) {
		creator := setupTestNode(t, vNet, "approval-creator", peerPort)
		creator.login(t)
		_, invitation := creator.hostRoom(t, "Alice", "Room")

		if code := creator.doJSON("POST", "/api/v1/room-admin/approval-mode",
			map[string]any{"approval_mode": true}).Code; code != http.StatusOK {
			t.Fatal("failed to enable approval mode")
		}

		joiner := setupTestNode(t, vNet, "approval-joiner", peerPort)
		joiner.login(t)
		w := joiner.joinRoom(t, "Bob", invitation)
		if w.Code != http.StatusOK {
			t.Fatalf("join under approval mode: %d %s", w.Code, w.Body.String())
		}
		var joinResp struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(w.Body).Decode(&joinResp)
		if joinResp.Status != string(room.StatusPending) {
			t.Fatalf("expected pending, got %q", joinResp.Status)
		}

		w = creator.doJSON("GET", "/api/v1/room-admin/pending", nil)
		var pending []store.MemberRecord
		_ = json.NewDecoder(w.Body).Decode(&pending)
		if len(pending) != 1 || pending[0].MemberID != joiner.memberID {
			t.Fatalf("unexpected pending list: %#v", pending)
		}

		if code := creator.doJSON("POST",
			fmt.Sprintf("/api/v1/room-admin/members/%s/approve", joiner.memberID), nil).Code; code != http.StatusOK {
			t.Fatal("approve failed")
		}

		approved, _ := creator.store.GetMember(joiner.memberID)
		if approved == nil || approved.Status != string(room.StatusAdmitted) {
			t.Fatalf("approval did not admit the member: %#v", approved)
		}
		// The approval must be verifiable by every peer.
		authority, err := peerauth.NewRoster(creator.store).Authority()
		if err != nil {
			t.Fatal(err)
		}
		m := room.Membership{
			MemberID:      approved.MemberID,
			RoomID:        approved.RoomID,
			DevicePublic:  approved.DevicePublic,
			DisplayName:   approved.DisplayName,
			Status:        room.MemberStatus(approved.Status),
			RosterVersion: approved.RosterVersion,
			Signature:     approved.Signature,
			SigVersion:    approved.SigVersion,
		}
		if err := m.Verify(authority); err != nil {
			t.Fatalf("approved membership does not verify: %v", err)
		}

		// Task 34: Approved joiner retries join to complete onboarding
		retryW := joiner.joinRoom(t, "Bob", invitation)
		if retryW.Code != http.StatusOK {
			t.Fatalf("joiner retry after approval failed: %d %s", retryW.Code, retryW.Body.String())
		}
		var retryResp struct {
			Status string `json:"status"`
			RoomID string `json:"room_id"`
		}
		_ = json.NewDecoder(retryW.Body).Decode(&retryResp)
		if retryResp.Status != string(room.StatusAdmitted) {
			t.Fatalf("expected admitted on retry, got %q", retryResp.Status)
		}

		// Verify joiner saved room state and started peer server
		joinerRoom, err := joiner.store.GetRoomState()
		if err != nil || joinerRoom == nil {
			t.Fatalf("joiner room state not saved: %v", err)
		}
		if joiner.peerServer() == nil {
			t.Fatal("joiner peer server was not started after onboarding")
		}

		// Authenticated peer request from joiner to creator
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var creatorCatalog []any
		err = joiner.localSrv.peerJSON(ctx, creator.memberID, "approval-creator", "GET", "/peer/v1/catalog", nil, &creatorCatalog)
		if err != nil {
			t.Fatalf("authenticated peer request failed: %v", err)
		}

		// Creator removes joiner
		removeCode := creator.doJSON("POST", fmt.Sprintf("/api/v1/room-admin/members/%s/remove", joiner.memberID), map[string]any{
			"rotate_invitation": false,
		}).Code
		if removeCode != http.StatusOK {
			t.Fatalf("remove member failed: %d", removeCode)
		}

		// Joiner leaves room locally
		if leaveCode := joiner.doJSON("POST", "/api/v1/room/leave", nil).Code; leaveCode != http.StatusOK {
			t.Fatalf("joiner leave room failed: %d", leaveCode)
		}

		// Removed member cannot rejoin: bootstrap response must be rejected
		rejoinW := joiner.joinRoom(t, "Bob", invitation)
		var rejoinResp struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(rejoinW.Body).Decode(&rejoinResp)
		if rejoinResp.Status != "rejected" {
			t.Fatalf("rejoin by removed member should be rejected, got %s: %s", rejoinResp.Status, rejoinW.Body.String())
		}
		if joinerRoomAfter, _ := joiner.store.GetRoomState(); joinerRoomAfter != nil {
			t.Fatal("removed member should not have saved room state")
		}
	})
}

// TestPartialRosterUpdateDoesNotHideRemovals covers task 30.
func TestPartialRosterUpdateDoesNotHideRemovals(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4280

	creator := setupTestNode(t, vNet, "roster-creator", peerPort)
	creator.login(t)
	_, invitation := creator.hostRoom(t, "Alice", "Roster Room")

	bob := setupTestNode(t, vNet, "roster-bob", peerPort)
	bob.login(t)
	if w := bob.joinRoom(t, "Bob", invitation); w.Code != http.StatusOK {
		t.Fatalf("bob join: %d %s", w.Code, w.Body.String())
	}

	charlie := setupTestNode(t, vNet, "roster-charlie", peerPort)
	charlie.login(t)
	if w := charlie.joinRoom(t, "Charlie", invitation); w.Code != http.StatusOK {
		t.Fatalf("charlie join: %d %s", w.Code, w.Body.String())
	}

	// Remove Bob on creator (generates Bob's removal record at higher roster version)
	if code := creator.doJSON("POST", fmt.Sprintf("/api/v1/room-admin/members/%s/remove", bob.memberID), map[string]any{
		"rotate_invitation": false,
	}).Code; code != http.StatusOK {
		t.Fatalf("remove bob failed: %d", code)
	}

	// Creator admits Dave (generates Dave's record at even higher roster version)
	dave := setupTestNode(t, vNet, "roster-dave", peerPort)
	dave.login(t)
	if w := dave.joinRoom(t, "Dave", invitation); w.Code != http.StatusOK {
		t.Fatalf("dave join: %d %s", w.Code, w.Body.String())
	}

	// Get Dave's signed membership from creator
	daveMem, err := creator.store.GetMember(dave.memberID)
	if err != nil || daveMem == nil {
		t.Fatal(err)
	}

	// Deliver Dave's newer record to Charlie directly (withholding Bob's removal)
	daveMembership := room.Membership{
		MemberID:      daveMem.MemberID,
		RoomID:        daveMem.RoomID,
		DevicePublic:  daveMem.DevicePublic,
		DisplayName:   daveMem.DisplayName,
		Status:        room.MemberStatus(daveMem.Status),
		RosterVersion: daveMem.RosterVersion,
		Signature:     daveMem.Signature,
		SigVersion:    daveMem.SigVersion,
	}
	charlieRoom, _ := charlie.store.GetRoomState()
	authority, err := creatorAuthority(creator.store)
	if err != nil {
		t.Fatal(err)
	}
	charlie.localSrv.applyMembershipUpdates(charlieRoom, authority, []room.Membership{daveMembership})

	// Charlie now knows Dave, but still thinks Bob is admitted
	bobOnCharlie, _ := charlie.store.GetMember(bob.memberID)
	if bobOnCharlie.Status != string(room.StatusAdmitted) {
		t.Fatalf("expected Bob still admitted before sync, got %s", bobOnCharlie.Status)
	}

	// Now Charlie syncs membership with honest peers (creator)
	charlie.localSrv.SyncMembership(context.Background())

	// Charlie must now have learned Bob's removal!
	bobAfterSync, err := charlie.store.GetMember(bob.memberID)
	if err != nil {
		t.Fatal(err)
	}
	if bobAfterSync.Status != string(room.StatusRemoved) {
		t.Fatalf("expected Bob to be removed on Charlie after sync, got %s", bobAfterSync.Status)
	}
}
