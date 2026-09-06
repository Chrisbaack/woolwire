package localapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cbaack/woolwire/internal/identity"
	"github.com/cbaack/woolwire/internal/peerapi"
	"github.com/cbaack/woolwire/internal/store"
	"github.com/cbaack/woolwire/internal/transport"
)

const testSetupSecret = "setup-secret-12345"

// testNode is one Woolwire instance on the in-memory transport. It mirrors
// what cmd/woolwire wires up, including the mutually authenticated peer
// listener, so tests exercise the real trust boundary rather than a plaintext
// stand-in.
type testNode struct {
	t          *testing.T
	store      *store.Store
	trans      *transport.MemoryTransport
	localSrv   *Server
	setupToken string
	cookie     *http.Cookie
	device     *identity.Device
	memberID   string
	peerPort   uint16

	mu      sync.Mutex
	peerSrv *peerapi.Server
}

func setupTestNode(t *testing.T, vnet *transport.MemoryNetwork, addr string, port uint16) *testNode {
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

	memberID, err := peerapi.MemberIDForDevicePublic(dev.EncodedPublicKey())
	if err != nil {
		t.Fatal(err)
	}

	trans := vnet.NewTransport(addr)

	node := &testNode{
		t:          t,
		store:      s,
		trans:      trans,
		setupToken: testSetupSecret,
		device:     dev,
		memberID:   memberID,
		peerPort:   port,
	}

	node.localSrv, err = NewServer(Config{
		Store:       s,
		Transport:   trans,
		SetupToken:  node.setupToken,
		PeerPort:    port,
		StateDir:    t.TempDir(),
		StartPeerFn: node.startPeer,
		StopPeerFn:  node.stopPeer,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = node.stopPeer()
		_ = node.localSrv.Close()
		_ = trans.Close()
		_ = s.Close()
	})

	return node
}

func (n *testNode) startPeer(authority ed25519.PrivateKey) error {
	dev, err := n.store.GetDeviceIdentity()
	if err != nil {
		return err
	}
	deviceCert := tls.Certificate{
		Certificate: [][]byte{dev.DeviceCertDER},
		PrivateKey:  ed25519.PrivateKey(dev.DevicePrivate),
	}

	var roomCert *tls.Certificate
	if roomRec, err := n.store.GetRoomState(); err == nil && roomRec != nil && len(roomRec.RoomCertDER) > 0 {
		roomCert = &tls.Certificate{
			Certificate: [][]byte{roomRec.RoomCertDER},
			PrivateKey:  ed25519.PrivateKey(dev.DevicePrivate),
		}
	}

	srv := peerapi.NewServer(peerapi.Config{
		Store:      n.store,
		Authority:  authority,
		DeviceCert: deviceCert,
		RoomCert:   roomCert,
		Inference:  n.localSrv.Inference(),
	})

	n.mu.Lock()
	defer n.mu.Unlock()
	n.stopPeerLocked()
	if err := srv.Start(n.trans, n.peerPort); err != nil {
		return err
	}
	n.peerSrv = srv
	return nil
}

func (n *testNode) stopPeer() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.stopPeerLocked()
	return nil
}

func (n *testNode) stopPeerLocked() {
	if n.peerSrv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = n.peerSrv.Stop(ctx)
	cancel()
	n.peerSrv = nil
}

func (n *testNode) peerServer() *peerapi.Server {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.peerSrv
}

func (n *testNode) login(t *testing.T) {
	t.Helper()

	w := n.doJSON("POST", "/api/v1/setup", map[string]string{"token": n.setupToken})
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d, %s", w.Code, w.Body.String())
	}

	for _, c := range w.Result().Cookies() {
		if c.Name == "woolwire_session" {
			n.cookie = c
			return
		}
	}
	t.Fatal("session cookie not set")
}

// newRequest builds a request the SPA would send: correct host, JSON content
// type, and the custom header that satisfies the CSRF check.
func (n *testNode) newRequest(method, path string, payload any) *http.Request {
	var bodyReader *bytes.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, bodyReader)
	req.Host = "127.0.0.1:7070"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, "1")
	if n.cookie != nil {
		req.AddCookie(n.cookie)
	}
	return req
}

func (n *testNode) doJSON(method, path string, payload any) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	n.localSrv.securityMiddleware(n.localSrv.mux).ServeHTTP(w, n.newRequest(method, path, payload))
	return w
}

// doRequest sends a fully custom request through the middleware chain.
func (n *testNode) doRequest(req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	n.localSrv.securityMiddleware(n.localSrv.mux).ServeHTTP(w, req)
	return w
}

// hostRoom sets a display name and creates a room, returning the invitation.
func (n *testNode) hostRoom(t *testing.T, displayName, roomName string) (roomID, invitation string) {
	t.Helper()

	if w := n.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": displayName}); w.Code != http.StatusOK {
		t.Fatalf("set profile %s: %d %s", displayName, w.Code, w.Body.String())
	}

	w := n.doJSON("POST", "/api/v1/room/host", map[string]string{"room_name": roomName})
	if w.Code != http.StatusOK {
		t.Fatalf("host room: %d, %s", w.Code, w.Body.String())
	}
	var resp struct {
		RoomID         string `json:"room_id"`
		InvitationCode string `json:"invitation_code"`
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.InvitationCode == "" {
		t.Fatal("empty invitation code returned")
	}
	return resp.RoomID, resp.InvitationCode
}

// joinRoom sets a display name and joins with the given invitation.
func (n *testNode) joinRoom(t *testing.T, displayName, invitation string) *httptest.ResponseRecorder {
	t.Helper()

	if w := n.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": displayName}); w.Code != http.StatusOK {
		t.Fatalf("set profile %s: %d %s", displayName, w.Code, w.Body.String())
	}
	return n.doJSON("POST", "/api/v1/room/join", map[string]string{"invitation_code": invitation})
}

// localAPIBearer returns the token the OpenAI-compatible routes require.
func (n *testNode) localAPIBearer(t *testing.T) string {
	t.Helper()
	token, err := n.localSrv.localAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + token
}
