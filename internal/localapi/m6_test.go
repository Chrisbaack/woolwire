package localapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cbaack/woolwire/internal/store"
	"github.com/cbaack/woolwire/internal/transport"
)

func TestM6MetricsEndpoint(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4260

	node := setupTestNode(t, vNet, "m6-metrics-1", peerPort)
	node.login(t)

	w := node.doJSON("GET", "/api/v1/metrics", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get metrics failed: %d, %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	var m MetricsResponse
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&m); err != nil {
		t.Fatalf("decode metrics response failed: %v", err)
	}

	if m.TelemetryEnabled {
		t.Fatalf("telemetry must be strictly disabled")
	}

	// The reported address is the transport's public address.
	if m.Connectivity.TailcatAddr != "m6-metrics-1" {
		t.Fatalf("unexpected tailcat addr: %s", m.Connectivity.TailcatAddr)
	}

	// Regression for leaking the persisted node private key through metrics:
	// the stored key is "tailcat-<addr>" in tests and "privkey:..." in
	// production, and neither may appear anywhere in the response.
	dev, _ := node.store.GetDeviceIdentity()
	if strings.Contains(body, "privkey:") {
		t.Fatalf("metrics response contains a private key: %s", body)
	}
	if strings.Contains(body, dev.TailcatKey) {
		t.Fatalf("metrics response contains the persisted node key: %s", body)
	}

	if m.Storage.DatabaseSizeBytes < 0 {
		t.Fatalf("invalid database size: %d", m.Storage.DatabaseSizeBytes)
	}
}

// TestM6BackupIsEncryptedAndUnderStateDir covers task 22.
func TestM6BackupIsEncryptedAndUnderStateDir(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4261

	node := setupTestNode(t, vNet, "m6-backup-1", peerPort)
	node.login(t)
	node.hostRoom(t, "BackupUser", "Pre-Backup Room")

	// A backup without a passphrase is refused: the plaintext database holds
	// the device private key, the session token, and every endpoint API key.
	if code := node.doJSON("POST", "/api/v1/backup/export", map[string]string{}).Code; code != http.StatusBadRequest {
		t.Fatalf("expected 400 without a passphrase, got %d", code)
	}

	const passphrase = "correct horse battery staple"
	w := node.doJSON("POST", "/api/v1/backup/export", map[string]string{"passphrase": passphrase})
	if w.Code != http.StatusOK {
		t.Fatalf("backup export failed: %d, %s", w.Code, w.Body.String())
	}

	var resp struct {
		OK         bool   `json:"ok"`
		BackupPath string `json:"backup_path"`
		SizeBytes  int64  `json:"size_bytes"`
		Encrypted  bool   `json:"encrypted"`
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if !resp.OK || resp.BackupPath == "" || resp.SizeBytes <= 0 || !resp.Encrypted {
		t.Fatalf("invalid backup response: %+v", resp)
	}

	// The export lives under the state directory, not the working directory,
	// so a container backup lands inside the mounted volume.
	if !strings.HasPrefix(resp.BackupPath, filepath.Join(node.localSrv.stateDir, "backups")) {
		t.Fatalf("backup written outside the state dir: %s", resp.BackupPath)
	}

	blob, err := os.ReadFile(resp.BackupPath)
	if err != nil {
		t.Fatalf("backup file not on disk: %v", err)
	}

	// It must not be a readable SQLite database.
	if bytes.HasPrefix(blob, []byte("SQLite format 3")) {
		t.Fatal("backup is a plaintext SQLite database")
	}
	if bytes.Contains(blob, node.device.PrivateKey) {
		t.Fatal("backup contains the device private key in the clear")
	}

	// The wrong passphrase must not decrypt it.
	if _, err := DecryptBackup(bytes.NewReader(blob), "wrong passphrase"); err == nil {
		t.Fatal("backup decrypted with the wrong passphrase")
	}

	plaintext, err := DecryptBackup(bytes.NewReader(blob), passphrase)
	if err != nil {
		t.Fatalf("decrypt backup: %v", err)
	}

	restorePath := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(restorePath, plaintext, 0o600); err != nil {
		t.Fatal(err)
	}

	restoredStore, err := store.Open(restorePath)
	if err != nil {
		t.Fatalf("open restored store failed: %v", err)
	}
	defer restoredStore.Close()

	restoredName, err := restoredStore.GetSetting("display_name")
	if err != nil || restoredName != "BackupUser" {
		t.Fatalf("restored name mismatch: got %q, err %v", restoredName, err)
	}

	restoredRoom, err := restoredStore.GetRoomState()
	if err != nil || restoredRoom == nil || restoredRoom.RoomName != "Pre-Backup Room" {
		t.Fatalf("restored room state mismatch: %+v, err %v", restoredRoom, err)
	}
}

func TestDatabaseFileIsOwnerOnly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "perm.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("database mode is %04o, want 0600", perm)
	}
}

func TestM6SecurityBoundariesReview(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4262

	node := setupTestNode(t, vNet, "m6-sec-1", peerPort)

	// 1. Unauthenticated request to /api/v1/state -> 401
	req := httptest.NewRequest("GET", "/api/v1/state", nil)
	req.Host = "127.0.0.1:7070"
	if code := node.doRequest(req).Code; code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated state request, got %d", code)
	}

	// 2. Invalid setup token -> 401
	if code := node.doJSON("POST", "/api/v1/setup",
		map[string]string{"token": "wrong-token"}).Code; code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong setup token, got %d", code)
	}

	// 3. Login with the correct setup secret
	node.login(t)
	if code := node.doJSON("GET", "/api/v1/state", nil).Code; code != http.StatusOK {
		t.Fatalf("expected 200 after authentication, got %d", code)
	}
}

// TestOpenAIRoutesRequireToken covers task 8.
func TestOpenAIRoutesRequireToken(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "oai-node", 4263)
	node.login(t)

	// No bearer token: refused even though local_api_token was never set by
	// hand. It is generated on first boot precisely so "unset" cannot mean
	// "open".
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Host = "127.0.0.1:7070"
	if code := node.doRequest(req).Code; code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a bearer token, got %d", code)
	}

	// A wrong token is refused.
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Host = "127.0.0.1:7070"
	req.Header.Set("Authorization", "Bearer not-the-token")
	if code := node.doRequest(req).Code; code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a wrong bearer token, got %d", code)
	}

	// The real token works.
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Host = "127.0.0.1:7070"
	req.Header.Set("Authorization", node.localAPIBearer(t))
	if code := node.doRequest(req).Code; code != http.StatusOK {
		t.Fatalf("expected 200 with the local API token, got %d", code)
	}

	// A cross-origin simple POST to chat completions is refused before auth.
	post := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"x","messages":[]}`)))
	post.Host = "127.0.0.1:7070"
	post.Header.Set("Content-Type", "text/plain")
	post.Header.Set("Origin", "https://evil.example")
	if code := node.doRequest(post).Code; code != http.StatusForbidden {
		t.Fatalf("expected 403 for a cross-origin simple POST, got %d", code)
	}

	// Regenerating replaces the token.
	before := node.localAPIBearer(t)
	w := node.doJSON("POST", "/api/v1/local-api-token/regenerate", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("regenerate failed: %d", w.Code)
	}
	if node.localAPIBearer(t) == before {
		t.Fatal("regenerate did not change the token")
	}
}
