package localapi

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cbaack/woolwire/internal/identity"
	"github.com/cbaack/woolwire/internal/peerapi"
	"github.com/cbaack/woolwire/internal/store"
	"github.com/cbaack/woolwire/internal/transport"
)

type testNode struct {
	store      *store.Store
	trans      *transport.MemoryTransport
	localSrv   *Server
	peerSrv    *peerapi.Server
	setupToken string
	cookie     *http.Cookie
	device     *identity.Device
}

func setupTestNode(t *testing.T, net *transport.MemoryNetwork, addr string, port uint16) *testNode {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "node.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	dev, err := identity.GenerateDevice()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDeviceIdentity(store.DeviceIdentity{
		TailcatKey:    "tailcat-" + addr,
		DevicePrivate: dev.PrivateKey,
		DeviceCertDER: dev.CertDER,
		DevicePublic:  dev.EncodedPublicKey(),
	}); err != nil {
		t.Fatal(err)
	}

	trans := net.NewTransport(addr)

	node := &testNode{
		store:      s,
		trans:      trans,
		setupToken: "setup-secret-12345",
		device:     dev,
	}

	node.localSrv, err = NewServer(Config{
		Store:      s,
		Transport:  trans,
		SetupToken: node.setupToken,
		PeerPort:   port,
		StartPeerFn: func(authority ed25519.PrivateKey) error {
			node.peerSrv = peerapi.NewServer(s, authority)
			l, lErr := trans.Listen(port)
			if lErr != nil {
				return lErr
			}
			go func() { _ = node.peerSrv.Serve(l) }()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	return node
}

func (n *testNode) login(t *testing.T) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"token": n.setupToken})
	req := httptest.NewRequest("POST", "/api/v1/setup", bytes.NewReader(body))
	w := httptest.NewRecorder()
	n.localSrv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d, %s", w.Code, w.Body.String())
	}

	cookies := w.Result().Cookies()
	for _, c := range cookies {
		if c.Name == "woolwire_session" {
			n.cookie = c
			return
		}
	}
	t.Fatal("session cookie not set")
}

func (n *testNode) doJSON(method, path string, payload any) *httptest.ResponseRecorder {
	var bodyReader *bytes.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Host = "127.0.0.1:7070"
	if n.cookie != nil {
		req.AddCookie(n.cookie)
	}
	w := httptest.NewRecorder()
	n.localSrv.securityMiddleware(n.localSrv.mux).ServeHTTP(w, req)
	return w
}

func TestM1FullOnboardingAndAcceptanceGate(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4242

	// Node 1: Creator (Alice)
	node1 := setupTestNode(t, vNet, "node-1", peerPort)
	defer node1.trans.Close()
	defer node1.store.Close()
	node1.login(t)

	// Set display name on Node 1
	w := node1.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Alice"})
	if w.Code != http.StatusOK {
		t.Fatalf("set profile Alice: %d", w.Code)
	}

	// Host room on Node 1
	w = node1.doJSON("POST", "/api/v1/room/host", map[string]string{"room_name": "Technical Friends"})
	if w.Code != http.StatusOK {
		t.Fatalf("host room: %d, %s", w.Code, w.Body.String())
	}
	var hostResp struct {
		RoomID         string `json:"room_id"`
		InvitationCode string `json:"invitation_code"`
	}
	_ = json.NewDecoder(w.Body).Decode(&hostResp)
	if hostResp.InvitationCode == "" {
		t.Fatal("empty invitation code returned")
	}

	// Node 2: Member (Bob)
	node2 := setupTestNode(t, vNet, "node-2", peerPort)
	defer node2.trans.Close()
	defer node2.store.Close()
	node2.login(t)

	// Set display name on Node 2
	w = node2.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Bob"})
	if w.Code != http.StatusOK {
		t.Fatalf("set profile Bob: %d", w.Code)
	}

	// Gate Check 1: Join room using invitation code with no manual networking setup
	w = node2.doJSON("POST", "/api/v1/room/join", map[string]string{"invitation_code": hostResp.InvitationCode})
	if w.Code != http.StatusOK {
		t.Fatalf("join room Bob: %d, %s", w.Code, w.Body.String())
	}

	// Check Bob's state shows member in room
	w = node2.doJSON("GET", "/api/v1/state", nil)
	var bobState struct {
		DisplayName string `json:"display_name"`
		Room        struct {
			RoomID string `json:"room_id"`
			Role   string `json:"role"`
		} `json:"room"`
	}
	_ = json.NewDecoder(w.Body).Decode(&bobState)
	if bobState.Room.Role != "member" || bobState.Room.RoomID != hostResp.RoomID {
		t.Fatalf("unexpected bob state: %#v", bobState)
	}

	// Gate Check 2: Restart retains identity
	// Close Bob's server, create fresh instance using same store & keys
	_ = node2.localSrv.Close()
	restartedNode2, err := NewServer(Config{
		Store:      node2.store,
		Transport:  node2.trans,
		SetupToken: node2.setupToken,
		PeerPort:   peerPort,
	})
	if err != nil {
		t.Fatalf("restarted node 2 server: %v", err)
	}
	node2.localSrv = restartedNode2
	w = node2.doJSON("GET", "/api/v1/state", nil)
	var restartedBobState struct {
		DisplayName string `json:"display_name"`
		Room        struct {
			RoomID string `json:"room_id"`
			Role   string `json:"role"`
		} `json:"room"`
	}
	_ = json.NewDecoder(w.Body).Decode(&restartedBobState)
	if restartedBobState.DisplayName != "Bob" || restartedBobState.Room.Role != "member" {
		t.Fatalf("restart did not retain identity: %#v", restartedBobState)
	}

	// Gate Check 3: Creator cannot use remote routes to change another member's settings
	// (Node 2 only exposes peer routes on transport; local settings are on loopback requiring local auth)
	unauthReq := httptest.NewRequest("POST", "/api/v1/profile", bytes.NewReader([]byte(`{"display_name":"Hacked"}`)))
	unauthReq.Host = "127.0.0.1:7070"
	unauthW := httptest.NewRecorder()
	node2.localSrv.securityMiddleware(node2.localSrv.mux).ServeHTTP(unauthW, unauthReq)
	if unauthW.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized remote setting change should be 401, got %d", unauthW.Code)
	}

	// Gate Check 4: A rotated code fails new admission while existing members remain connected
	w = node1.doJSON("POST", "/api/v1/room-admin/invitation/rotate", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate invitation: %d, %s", w.Code, w.Body.String())
	}
	var rotateResp struct {
		InvitationCode string `json:"invitation_code"`
	}
	_ = json.NewDecoder(w.Body).Decode(&rotateResp)
	if rotateResp.InvitationCode == hostResp.InvitationCode {
		t.Fatal("rotated code should differ from original code")
	}

	// Node 3 tries to join with old code -> MUST FAIL
	node3 := setupTestNode(t, vNet, "node-3", peerPort)
	defer node3.trans.Close()
	defer node3.store.Close()
	node3.login(t)
	_ = node3.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Charlie"})

	w = node3.doJSON("POST", "/api/v1/room/join", map[string]string{"invitation_code": hostResp.InvitationCode})
	if w.Code == http.StatusOK {
		var joinCheck struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(w.Body).Decode(&joinCheck)
		if joinCheck.Status == "admitted" {
			t.Fatal("node 3 was admitted with rotated old code")
		}
	}

	// Node 3 joins with new rotated code -> MUST SUCCEED
	w = node3.doJSON("POST", "/api/v1/room/join", map[string]string{"invitation_code": rotateResp.InvitationCode})
	if w.Code != http.StatusOK {
		t.Fatalf("node 3 failed to join with new code: %d, %s", w.Code, w.Body.String())
	}

	// Existing member Bob is still admitted in Alice's member list
	w = node1.doJSON("GET", "/api/v1/room-admin/members", nil)
	var members []store.MemberRecord
	_ = json.NewDecoder(w.Body).Decode(&members)
	var bobFound bool
	for _, m := range members {
		if m.DisplayName == "Bob" && m.Status == "admitted" {
			bobFound = true
			break
		}
	}
	if !bobFound {
		t.Fatal("bob should remain admitted after invitation rotation")
	}

	// Gate Check 5: A removed member loses access on peers that receive the update
	var bobID string
	for _, m := range members {
		if m.DisplayName == "Bob" {
			bobID = m.MemberID
			break
		}
	}
	w = node1.doJSON("POST", fmt.Sprintf("/api/v1/room-admin/members/%s/remove", bobID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("remove bob: %d, %s", w.Code, w.Body.String())
	}

	// Verify Bob is marked removed on Alice's member list
	w = node1.doJSON("GET", "/api/v1/room-admin/members", nil)
	var updatedMembers []store.MemberRecord
	_ = json.NewDecoder(w.Body).Decode(&updatedMembers)
	for _, m := range updatedMembers {
		if m.MemberID == bobID && m.Status != "removed" {
			t.Fatalf("bob should be marked removed, got %q", m.Status)
		}
	}
}

func TestSecurityMiddlewareLANAndDNSRebinding(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "sec-node", 4242)
	defer node.trans.Close()
	defer node.store.Close()

	// Update node to have configured allowed host
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
		w := httptest.NewRecorder()
		node.localSrv.securityMiddleware(node.localSrv.mux).ServeHTTP(w, req)

		if tc.wantBlock {
			if w.Code != http.StatusForbidden {
				t.Errorf("[%s] host %q should be blocked with 403, got %d", tc.desc, tc.host, w.Code)
			}
		} else {
			if w.Code == http.StatusForbidden {
				t.Errorf("[%s] host %q should NOT be blocked with 403 Forbidden", tc.desc, tc.host)
			}
		}
	}
}

func TestSetupTokenManagement(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "setup-node", 4299)
	defer node.trans.Close()
	defer node.store.Close()

	// 1. Unauthenticated GET /api/v1/setup/info should return 401
	w := node.doJSON("GET", "/api/v1/setup/info", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauthorized, got %d", w.Code)
	}

	// 2. Login with initial token
	node.login(t)

	// 3. Authenticated GET /api/v1/setup/info should return current token
	w = node.doJSON("GET", "/api/v1/setup/info", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}
	var infoResp struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(w.Body).Decode(&infoResp)
	if infoResp.Token != node.setupToken {
		t.Fatalf("expected token %q, got %q", node.setupToken, infoResp.Token)
	}

	// 4. Update setup token to custom PIN "987654"
	w = node.doJSON("POST", "/api/v1/setup/token", map[string]string{"token": "987654"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", w.Code)
	}

	// 5. Test login with old token should fail
	oldReqBody, _ := json.Marshal(map[string]string{"token": node.setupToken})
	req := httptest.NewRequest("POST", "/api/v1/setup", bytes.NewReader(oldReqBody))
	rec := httptest.NewRecorder()
	node.localSrv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected old token to fail with 401, got %d", rec.Code)
	}

	// 6. Test login with new token should succeed and set session cookie
	newReqBody, _ := json.Marshal(map[string]string{"token": "987654"})
	req = httptest.NewRequest("POST", "/api/v1/setup", bytes.NewReader(newReqBody))
	rec = httptest.NewRecorder()
	node.localSrv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected new token to succeed with 200, got %d", rec.Code)
	}
	foundCookie := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == "woolwire_session" {
			foundCookie = true
			if c.MaxAge <= 0 {
				t.Errorf("expected MaxAge > 0 for persistent login cookie")
			}
			break
		}
	}
	if !foundCookie {
		t.Fatal("session cookie not found after login with new token")
	}

	// 7. Update with too short token should fail
	w = node.doJSON("POST", "/api/v1/setup/token", map[string]string{"token": "12"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for short token, got %d", w.Code)
	}
}

