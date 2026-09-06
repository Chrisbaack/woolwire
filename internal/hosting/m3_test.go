package hosting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cbaack/woolwire/internal/runner"
)

func TestM3RunnerControllerAndArtifactsGate(t *testing.T) {
	tempDir := t.TempDir()
	modelsDir := filepath.Join(tempDir, "models")
	if err := os.MkdirAll(modelsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Create a dummy GGUF weight file
	dummyWeightContent := []byte("dummy GGUF model weight content for testing")
	dummyGGUFName := "tiny-model.gguf"
	dummyPath := filepath.Join(modelsDir, dummyGGUFName)
	if err := os.WriteFile(dummyPath, dummyWeightContent, 0o600); err != nil {
		t.Fatal(err)
	}

	hasher := sha256.New()
	hasher.Write(dummyWeightContent)
	dummySHA256 := hex.EncodeToString(hasher.Sum(nil))

	// ---------------------------------------------------------------------------------
	// Part 1: Artifact Manager, Bounded Downloader, SSRF & Security
	// ---------------------------------------------------------------------------------
	t.Run("ArtifactManager: download, SSRF, budget and cleanup", func(t *testing.T) {
		// Mock download server
		downloadSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(dummyWeightContent)
		}))
		defer downloadSrv.Close()

		mgr, err := NewArtifactManager(modelsDir, 10*1024*1024) // 10 MB budget
		if err != nil {
			t.Fatal(err)
		}
		// Use TLS test server client to trust self-signed cert
		mgr.httpClient = downloadSrv.Client()

		// 1. Path traversal rejection
		_, err = mgr.DownloadArtifact(context.Background(), downloadSrv.URL, "../evil.gguf", "", 0, nil)
		if err == nil {
			t.Fatal("expected error on path traversal filename, got nil")
		}

		// 2. Non-gguf extension rejection
		_, err = mgr.DownloadArtifact(context.Background(), downloadSrv.URL, "script.sh", "", 0, nil)
		if err == nil {
			t.Fatal("expected error on non-gguf file, got nil")
		}

		// 3. SSRF / non-HTTPS rejection
		_, err = mgr.DownloadArtifact(context.Background(), "http://169.254.169.254/latest/meta-data", "test.gguf", "", 0, nil)
		if err == nil {
			t.Fatal("expected SSRF error on metadata IP, got nil")
		}

		// 4. Successful download with hash validation
		manifest, err := mgr.DownloadArtifact(
			context.Background(),
			downloadSrv.URL,
			"downloaded.gguf",
			dummySHA256,
			1024*1024,
			nil,
		)
		if err != nil {
			t.Fatalf("download failed: %v", err)
		}
		if manifest.SHA256 != dummySHA256 {
			t.Fatalf("unexpected hash: %s", manifest.SHA256)
		}

		// Verify staging directory is clean
		stagingEntries, _ := os.ReadDir(filepath.Join(modelsDir, ".staging"))
		if len(stagingEntries) != 0 {
			t.Fatalf("staging dir should be clean, found: %d entries", len(stagingEntries))
		}

		// 5. Hash mismatch failure and immediate cleanup
		_, err = mgr.DownloadArtifact(
			context.Background(),
			downloadSrv.URL,
			"bad-hash.gguf",
			"0000000000000000000000000000000000000000000000000000000000000000",
			1024*1024,
			nil,
		)
		if err == nil || !strings.Contains(err.Error(), "mismatch") {
			t.Fatalf("expected hash mismatch error, got: %v", err)
		}
		// File must NOT exist in finalized directory
		if _, statErr := os.Stat(filepath.Join(modelsDir, "bad-hash.gguf")); statErr == nil {
			t.Fatal("bad-hash file should not exist in models dir")
		}

		// 6. Disk budget enforcement
		tinyBudgetMgr, _ := NewArtifactManager(modelsDir, 10) // 10 bytes budget
		tinyBudgetMgr.httpClient = downloadSrv.Client()
		_, err = tinyBudgetMgr.DownloadArtifact(context.Background(), downloadSrv.URL, "over-budget.gguf", "", 0, nil)
		if err == nil || !strings.Contains(err.Error(), "budget") {
			t.Fatalf("expected budget exceeded error, got: %v", err)
		}

		// 7. Local import & Manifest listing
		imported, err := mgr.ImportLocalArtifact(dummyPath, "imported.gguf")
		if err != nil {
			t.Fatalf("import local artifact failed: %v", err)
		}
		if imported.SizeBytes != int64(len(dummyWeightContent)) {
			t.Fatalf("unexpected imported size: %d", imported.SizeBytes)
		}

		artifacts, err := mgr.ListArtifacts()
		if err != nil || len(artifacts) < 2 {
			t.Fatalf("expected >=2 artifacts listed, got: %d", len(artifacts))
		}

		// 8. Delete artifact
		if err := mgr.DeleteArtifact("downloaded.gguf"); err != nil {
			t.Fatalf("delete artifact failed: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(modelsDir, "downloaded.gguf")); statErr == nil {
			t.Fatal("downloaded.gguf was not deleted")
		}
	})

	// ---------------------------------------------------------------------------------
	// Part 2: Isolated Runner Controller & Protocol
	// ---------------------------------------------------------------------------------
	t.Run("Runner Controller: argument validation, isolation and engine lifecycle", func(t *testing.T) {
		token := "secret-runner-auth-token"
		ctrl, err := runner.NewController(runner.Config{
			ModelDir:    modelsDir,
			RunnerToken: token,
			EnginePath:  "echo", // use echo as dummy engine command
			EnginePort:  9099,
		})
		if err != nil {
			t.Fatal(err)
		}

		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()

		go func() { _ = ctrl.Serve(l) }()

		baseURL := fmt.Sprintf("http://%s", l.Addr().String())
		client := NewRunnerClient(baseURL, token)

		// 1. Health check
		h, err := client.Health(context.Background())
		if err != nil {
			t.Fatalf("runner health failed: %v", err)
		}
		if h.Status != string(runner.StatusIdle) {
			t.Fatalf("expected idle status, got: %s", h.Status)
		}

		// 2. Authentication check: wrong token fails
		badClient := NewRunnerClient(baseURL, "wrong-token")
		_, err = badClient.Health(context.Background())
		if err == nil {
			t.Fatal("expected unauthorized error with wrong token, got nil")
		}

		// 3. Path traversal in load model rejected
		err = client.LoadModel(context.Background(), "m1", "../etc/passwd", 4096, 4, 0)
		if err == nil {
			t.Fatal("expected path traversal rejection, got nil")
		}

		// 4. Non-existent file rejected
		err = client.LoadModel(context.Background(), "m1", "ghost.gguf", 4096, 4, 0)
		if err == nil {
			t.Fatal("expected missing file rejection, got nil")
		}

		// 5. Successful load of approved model
		err = client.LoadModel(context.Background(), "tiny-model", dummyGGUFName, 4096, 2, 0)
		if err != nil {
			t.Fatalf("load approved model failed: %v", err)
		}

		// 6. Restart engine (used between requesting members to release prompt state)
		err = client.RestartEngine(context.Background())
		if err != nil {
			t.Fatalf("restart engine failed: %v", err)
		}

		// 7. Unload model
		err = client.UnloadModel(context.Background())
		if err != nil {
			t.Fatalf("unload model failed: %v", err)
		}

		h, err = client.Health(context.Background())
		if err != nil || h.Status != string(runner.StatusIdle) {
			t.Fatalf("expected idle after unload, got: %s", h.Status)
		}
	})

	// ---------------------------------------------------------------------------------
	// Part 3: Hardware Detection
	// ---------------------------------------------------------------------------------
	t.Run("Hardware Detection", func(t *testing.T) {
		hw := DetectHardware()
		if hw.CPUCores <= 0 {
			t.Fatalf("expected CPU cores > 0, got: %d", hw.CPUCores)
		}
		if hw.Arch == "" || hw.OS == "" {
			t.Fatalf("empty arch or OS: %#v", hw)
		}
	})
}
