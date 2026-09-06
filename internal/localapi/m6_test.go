package localapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cbaack/woolwire/internal/store"
	"github.com/cbaack/woolwire/internal/transport"
)

func TestM6MetricsEndpoint(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4260

	node := setupTestNode(t, vNet, "m6-metrics-1", peerPort)
	defer node.trans.Close()
	defer node.store.Close()
	node.login(t)

	w := node.doJSON("GET", "/api/v1/metrics", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get metrics failed: %d, %s", w.Code, w.Body.String())
	}

	var m MetricsResponse
	if err := json.NewDecoder(w.Body).Decode(&m); err != nil {
		t.Fatalf("decode metrics response failed: %v", err)
	}

	if m.TelemetryEnabled {
		t.Fatalf("telemetry must be strictly disabled")
	}

	if m.Connectivity.TailcatAddr != "tailcat-m6-metrics-1" {
		t.Fatalf("unexpected tailcat addr: %s", m.Connectivity.TailcatAddr)
	}

	if m.Storage.DatabaseSizeBytes < 0 {
		t.Fatalf("invalid database size: %d", m.Storage.DatabaseSizeBytes)
	}
}

func TestM6BackupAndRestoreFlow(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4261

	node := setupTestNode(t, vNet, "m6-backup-1", peerPort)
	defer node.trans.Close()
	defer node.store.Close()
	node.login(t)

	// Set display name and host room
	_ = node.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "BackupUser"})
	w := node.doJSON("POST", "/api/v1/room/host", map[string]string{"room_name": "Pre-Backup Room"})
	if w.Code != http.StatusOK {
		t.Fatalf("host room failed: %d", w.Code)
	}

	// Trigger backup export
	w = node.doJSON("POST", "/api/v1/backup/export", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("backup export failed: %d, %s", w.Code, w.Body.String())
	}

	var resp struct {
		OK         bool   `json:"ok"`
		BackupPath string `json:"backup_path"`
		SizeBytes  int64  `json:"size_bytes"`
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if !resp.OK || resp.BackupPath == "" || resp.SizeBytes <= 0 {
		t.Fatalf("invalid backup response: %+v", resp)
	}
	defer os.Remove(resp.BackupPath)

	// Verify backup file exists on disk
	info, err := os.Stat(resp.BackupPath)
	if err != nil || info.Size() <= 0 {
		t.Fatalf("backup file not on disk: %v", err)
	}

	// Restore from backup file into a fresh store
	restoreDir := filepath.Join(t.TempDir(), "restored.db")
	backupBytes, err := os.ReadFile(resp.BackupPath)
	if err != nil {
		t.Fatalf("read backup file failed: %v", err)
	}
	if err := os.WriteFile(restoreDir, backupBytes, 0o600); err != nil {
		t.Fatalf("write restore file failed: %v", err)
	}

	restoredStore, err := store.Open(restoreDir)
	if err != nil {
		t.Fatalf("open restored store failed: %v", err)
	}
	defer restoredStore.Close()

	// Verify restored room and identity
	restoredName, err := restoredStore.GetSetting("display_name")
	if err != nil || restoredName != "BackupUser" {
		t.Fatalf("restored name mismatch: got %q, err %v", restoredName, err)
	}

	restoredRoom, err := restoredStore.GetRoomState()
	if err != nil || restoredRoom == nil || restoredRoom.RoomName != "Pre-Backup Room" {
		t.Fatalf("restored room state mismatch: %+v, err %v", restoredRoom, err)
	}
}

func TestM6SecurityBoundariesReview(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4262

	node := setupTestNode(t, vNet, "m6-sec-1", peerPort)
	defer node.trans.Close()
	defer node.store.Close()

	// 1. Unauthenticated request to /api/v1/state without cookie or setup token -> 401 Unauthorized
	req, _ := http.NewRequest("GET", "/api/v1/state", nil)
	w := node.localSrv.mux
	rec := node.doRequest(req)
	_ = w
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated state request, got %d", rec.Code)
	}

	// 2. Invalid setup token -> 401 Unauthorized
	wLogin := node.doJSON("POST", "/api/v1/setup", map[string]string{"token": "wrong-token"})
	if wLogin.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong setup token, got %d", wLogin.Code)
	}

	// 3. Login with correct setup token
	node.login(t)
	wAuth := node.doJSON("GET", "/api/v1/state", nil)
	if wAuth.Code != http.StatusOK {
		t.Fatalf("expected 200 after authentication, got %d", wAuth.Code)
	}
}

func (n *testNode) doRequest(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	n.localSrv.mux.ServeHTTP(rec, req)
	return rec
}
