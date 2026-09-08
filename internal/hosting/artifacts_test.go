package hosting

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestArtifactManagerSharedDiskBudgetConcurrentUnknownSize(t *testing.T) {
	tempDir := t.TempDir()
	modelsDir := filepath.Join(tempDir, "models")
	if err := os.MkdirAll(modelsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// 1000-byte budget
	const budget int64 = 1000
	mgr, err := NewArtifactManager(modelsDir, budget)
	if err != nil {
		t.Fatal(err)
	}

	// Create 800-byte payload
	payload := make([]byte, 800)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	// Server that streams chunks slowly so both transfers overlap
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		chunkSize := 100
		for i := 0; i < len(payload); i += chunkSize {
			end := i + chunkSize
			if end > len(payload) {
				end = len(payload)
			}
			_, _ = w.Write(payload[i:end])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	srv := httptest.NewTLSServer(handler)
	defer srv.Close()
	mgr.httpClient = srv.Client()

	var wg sync.WaitGroup
	var successCount int32
	var budgetErrorCount int32

	wg.Add(2)
	runDL := func(name string) {
		defer wg.Done()
		_, dlErr := mgr.DownloadArtifact(context.Background(), srv.URL, name, "", 0, nil)
		if dlErr != nil {
			if errors.Is(dlErr, ErrDiskBudgetExceeded) {
				atomic.AddInt32(&budgetErrorCount, 1)
			} else {
				t.Errorf("unexpected error on %s: %v", name, dlErr)
			}
		} else {
			atomic.AddInt32(&successCount, 1)
		}
	}

	go runDL("model-a.gguf")
	go runDL("model-b.gguf")
	wg.Wait()

	if budgetErrorCount == 0 {
		t.Fatalf("expected at least one download to fail with ErrDiskBudgetExceeded, successes=%d errors=%d", successCount, budgetErrorCount)
	}

	// Check total disk usage on disk
	used, err := mgr.GetUsedDiskSpace()
	if err != nil {
		t.Fatal(err)
	}
	if used > budget {
		t.Fatalf("used space %d exceeded budget %d", used, budget)
	}
}

func TestArtifactManagerSharedDiskBudgetMixedAndCancellation(t *testing.T) {
	tempDir := t.TempDir()
	modelsDir := filepath.Join(tempDir, "models")
	if err := os.MkdirAll(modelsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	const budget int64 = 1000
	mgr, err := NewArtifactManager(modelsDir, budget)
	if err != nil {
		t.Fatal(err)
	}

	payload := make([]byte, 500)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		for i := 0; i < len(payload); i += 50 {
			end := i + 50
			if end > len(payload) {
				end = len(payload)
			}
			_, _ = w.Write(payload[i:end])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(15 * time.Millisecond)
		}
	}))
	defer srv.Close()
	mgr.httpClient = srv.Client()

	// 1. Start a known-size download of 600 bytes that stays in flight
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()

	dlStarted := make(chan struct{})
	go func() {
		close(dlStarted)
		_, _ = mgr.DownloadArtifact(ctxA, srv.URL, "known-600.gguf", "", 600, nil)
	}()
	<-dlStarted
	// Give it a moment to reserve
	time.Sleep(20 * time.Millisecond)

	// 2. Unknown-size download of 500 bytes should fail because 600 + 500 > 1000
	_, err = mgr.DownloadArtifact(context.Background(), srv.URL, "unknown-500.gguf", "", 0, nil)
	if !errors.Is(err, ErrDiskBudgetExceeded) {
		t.Fatalf("expected ErrDiskBudgetExceeded for mixed unknown-size download, got: %v", err)
	}

	// 3. Cancel the first download and assert budget recovers
	cancelA()
	time.Sleep(50 * time.Millisecond)

	// Now that known-600 was cancelled, budget should be fully recovered.
	// A 900-byte download should succeed.
	payload900 := make([]byte, 900)
	srv900 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(payload900)
	}))
	defer srv900.Close()
	mgr.httpClient = srv900.Client()

	manifest, err := mgr.DownloadArtifact(context.Background(), srv900.URL, "success-900.gguf", "", 0, nil)
	if err != nil {
		t.Fatalf("expected download to succeed after budget recovery, got: %v", err)
	}
	if manifest.SizeBytes != 900 {
		t.Fatalf("expected 900 bytes, got %d", manifest.SizeBytes)
	}

	used, err := mgr.GetUsedDiskSpace()
	if err != nil {
		t.Fatal(err)
	}
	if used != 900 {
		t.Fatalf("expected 900 bytes used, got %d", used)
	}
}
