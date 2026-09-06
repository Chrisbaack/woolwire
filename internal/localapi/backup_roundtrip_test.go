package localapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cbaack/woolwire/internal/store"
	"github.com/cbaack/woolwire/internal/transport"
)

// TestBackupRoundTripThroughHelper proves the documented recovery procedure
// works: export encrypted, decrypt with hack/backup-decrypt, open the result.
func TestBackupRoundTripThroughHelper(t *testing.T) {
	helper := filepath.Join(t.TempDir(), "backup-decrypt")
	build := exec.Command("go", "build", "-o", helper, "github.com/cbaack/woolwire/hack/backup-decrypt")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build helper: %v", err)
	}

	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "roundtrip", 4290)
	node.login(t)
	node.hostRoom(t, "Alice", "Round Trip Room")

	const passphrase = "a passphrase of at least 12 characters"
	w := node.doJSON("POST", "/api/v1/backup/export", map[string]string{"passphrase": passphrase})
	if w.Code != http.StatusOK {
		t.Fatalf("export: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		BackupPath string `json:"backup_path"`
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)

	restored := filepath.Join(t.TempDir(), "restored.db")
	cmd := exec.Command(helper, "-in", resp.BackupPath, "-out", restored)
	cmd.Stdin = bytes.NewBufferString(passphrase + "\n")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("decrypt helper: %v", err)
	}

	info, err := os.Stat(restored)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("restored database mode is %04o, want 0600", perm)
	}

	s, err := store.Open(restored)
	if err != nil {
		t.Fatalf("open restored database: %v", err)
	}
	defer s.Close()

	roomRec, err := s.GetRoomState()
	if err != nil || roomRec == nil || roomRec.RoomName != "Round Trip Room" {
		t.Fatalf("restored room state: %+v (%v)", roomRec, err)
	}
	// The transport identity must survive, or the node comes back at a new
	// address and every stored peer address and the invitation code break.
	dev, err := s.GetDeviceIdentity()
	if err != nil || dev == nil || dev.TailcatKey == "" {
		t.Fatalf("restored device identity: %+v (%v)", dev, err)
	}
}
