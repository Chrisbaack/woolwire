package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEngine stands in for llama-server. It records the arguments it was
// launched with, becomes healthy only after a delay, and streams a canned
// completion, which is what lets the load, restart, and isolation behavior be
// tested without a real inference engine.
type fakeEngine struct {
	server     *httptest.Server
	port       uint16
	readyAfter time.Duration
	started    time.Time
	launches   atomic.Int32
	lastArgs   atomic.Pointer[EngineArgs]
	lastBody   atomic.Pointer[map[string]any]
}

func newFakeEngine(t *testing.T, readyAfter time.Duration) *fakeEngine {
	t.Helper()

	fe := &fakeEngine{readyAfter: readyAfter}

	// The engine listens on a fixed loopback port that the controller probes.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fe.port = uint16(listener.Addr().(*net.TCPAddr).Port)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if time.Since(fe.started) < fe.readyAfter {
			http.Error(w, "loading", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fe.lastBody.Store(&body)

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, chunk := range []string{"hello", " world"} {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	})

	fe.server = &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: mux},
	}
	fe.server.Start()
	t.Cleanup(fe.server.Close)

	return fe
}

// command produces a child process that lives until it is killed, standing in
// for a long-running engine. The HTTP surface is served in-process; only the
// lifecycle is a real child.
func (fe *fakeEngine) command(ctx context.Context, args EngineArgs) *exec.Cmd {
	fe.launches.Add(1)
	copied := args
	fe.lastArgs.Store(&copied)
	fe.started = time.Now()

	cmd := exec.CommandContext(ctx, "sleep", "600")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd
}

func newTestController(t *testing.T, fe *fakeEngine, modelDir string) (*Controller, *http.Client, string) {
	t.Helper()

	ctrl, err := NewController(Config{
		ModelDir:      modelDir,
		RunnerToken:   "runner-token",
		EnginePort:    fe.port,
		EngineCommand: fe.command,
		HealthCheck: func(ctx context.Context, port uint16) error {
			return defaultHealthCheck(ctx, port)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctrl.Close() })

	srv := httptest.NewServer(ctrl.Handler())
	t.Cleanup(srv.Close)

	return ctrl, srv.Client(), srv.URL
}

func do(t *testing.T, client *http.Client, method, url string, body any) *http.Response {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer runner-token")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func writeDummyModel(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tiny.gguf"), []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestLoadWaitsForEngineHealth is the regression for marking a model ready at
// cmd.Start(): the first request always failed because llama-server was still
// reading weights.
func TestLoadWaitsForEngineHealth(t *testing.T) {
	fe := newFakeEngine(t, 300*time.Millisecond)
	modelDir := writeDummyModel(t)
	_, client, base := newTestController(t, fe, modelDir)

	start := time.Now()
	resp := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "m1", "filename": "tiny.gguf", "context_limit": 8192, "threads": 3, "gpu_layers": 7,
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("load returned %d", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("load returned after %s, before the engine reported healthy", elapsed)
	}

	health := do(t, client, "GET", base+"/runner/v1/health", nil)
	defer health.Body.Close()
	var h map[string]any
	_ = json.NewDecoder(health.Body).Decode(&h)
	if h["status"] != string(StatusReady) {
		t.Fatalf("expected ready, got %v (%v)", h["status"], h["error"])
	}
}

// TestRestartReusesLoadArguments is the regression for a restart that dropped
// the context size, thread count, and GPU-layer settings.
func TestRestartReusesLoadArguments(t *testing.T) {
	fe := newFakeEngine(t, 0)
	modelDir := writeDummyModel(t)
	_, client, base := newTestController(t, fe, modelDir)

	resp := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "m1", "filename": "tiny.gguf", "context_limit": 8192, "threads": 3, "gpu_layers": 7,
	})
	resp.Body.Close()

	loadArgs := fe.lastArgs.Load()
	if loadArgs == nil || loadArgs.ContextLimit != 8192 || loadArgs.Threads != 3 || loadArgs.GPULayers != 7 {
		t.Fatalf("load used unexpected arguments: %#v", loadArgs)
	}

	restart := do(t, client, "POST", base+"/runner/v1/engine/restart", nil)
	restart.Body.Close()

	restartArgs := fe.lastArgs.Load()
	if restartArgs == nil {
		t.Fatal("restart launched no engine")
	}
	if restartArgs.ContextLimit != 8192 || restartArgs.Threads != 3 || restartArgs.GPULayers != 7 {
		t.Fatalf("restart dropped load arguments: %#v", restartArgs)
	}
	if fe.launches.Load() != 2 {
		t.Fatalf("expected 2 engine launches, got %d", fe.launches.Load())
	}
}

// TestCrashedEngineReportsError is the regression for a crashed engine leaving
// status "ready" and an unreaped zombie.
func TestCrashedEngineReportsError(t *testing.T) {
	fe := newFakeEngine(t, 0)
	modelDir := writeDummyModel(t)

	ctrl, client, base := newTestController(t, fe, modelDir)

	resp := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "m1", "filename": "tiny.gguf",
	})
	resp.Body.Close()

	// Kill the engine out from under the controller.
	ctrl.mu.RLock()
	cmd := ctrl.engineCmd
	ctrl.mu.RUnlock()
	if cmd == nil || cmd.Process == nil {
		t.Fatal("no engine process was started")
	}
	_ = cmd.Process.Kill()

	deadline := time.Now().Add(5 * time.Second)
	for {
		health := do(t, client, "GET", base+"/runner/v1/health", nil)
		var h map[string]any
		_ = json.NewDecoder(health.Body).Decode(&h)
		health.Body.Close()

		if h["status"] == string(StatusError) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine death was never reflected in status, still %v", h["status"])
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestInferenceRejectsUnmodelledFields is the regression for proxying the raw
// client body to llama-server: the engine request is now built from a typed
// struct, so an injected engine parameter cannot ride along.
func TestInferenceBuildsTypedEngineRequest(t *testing.T) {
	fe := newFakeEngine(t, 0)
	modelDir := writeDummyModel(t)
	_, client, base := newTestController(t, fe, modelDir)

	load := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "m1", "filename": "tiny.gguf",
	})
	load.Body.Close()

	resp := do(t, client, "POST", base+"/runner/v1/inference", map[string]any{
		"model":      "tiny",
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 64,
		// Fields the runner does not model must not reach the engine.
		"n_predict":   999999,
		"grammar":     "root ::= anything",
		"cache_reuse": true,
	})
	defer resp.Body.Close()

	body := fe.lastBody.Load()
	if body == nil {
		t.Fatal("engine received no request")
	}
	for _, injected := range []string{"n_predict", "grammar", "cache_reuse"} {
		if _, ok := (*body)[injected]; ok {
			t.Fatalf("unmodelled field %q was forwarded to the engine", injected)
		}
	}
	if (*body)["max_tokens"] != float64(64) {
		t.Fatalf("max_tokens not forwarded: %v", (*body)["max_tokens"])
	}
}

func TestInferenceRejectsUnknownRole(t *testing.T) {
	fe := newFakeEngine(t, 0)
	modelDir := writeDummyModel(t)
	_, client, base := newTestController(t, fe, modelDir)

	load := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "m1", "filename": "tiny.gguf",
	})
	load.Body.Close()

	resp := do(t, client, "POST", base+"/runner/v1/inference", map[string]any{
		"model":    "tiny",
		"messages": []map[string]string{{"role": "root", "content": "hi"}},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unsupported role, got %d", resp.StatusCode)
	}
}

// TestEngineRestartsBetweenMembers covers the architecture requirement that
// one member's session state never reaches the next member.
func TestEngineRestartsBetweenMembers(t *testing.T) {
	fe := newFakeEngine(t, 0)
	modelDir := writeDummyModel(t)
	_, client, base := newTestController(t, fe, modelDir)

	load := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "m1", "filename": "tiny.gguf",
	})
	load.Body.Close()

	launchesAfterLoad := fe.launches.Load()

	first := do(t, client, "POST", base+"/runner/v1/inference", map[string]any{
		"model":               "tiny",
		"messages":            []map[string]string{{"role": "user", "content": "hi"}},
		"requester_member_id": "m-alice",
	})
	first.Body.Close()

	if fe.launches.Load() != launchesAfterLoad {
		t.Fatal("the first request should not have restarted the engine")
	}

	second := do(t, client, "POST", base+"/runner/v1/inference", map[string]any{
		"model":               "tiny",
		"messages":            []map[string]string{{"role": "user", "content": "hi"}},
		"requester_member_id": "m-bob",
	})
	second.Body.Close()

	if fe.launches.Load() != launchesAfterLoad+1 {
		t.Fatalf("expected a restart when the requesting member changed, launches went %d -> %d",
			launchesAfterLoad, fe.launches.Load())
	}

	third := do(t, client, "POST", base+"/runner/v1/inference", map[string]any{
		"model":               "tiny",
		"messages":            []map[string]string{{"role": "user", "content": "hi"}},
		"requester_member_id": "m-bob",
	})
	third.Body.Close()

	if fe.launches.Load() != launchesAfterLoad+1 {
		t.Fatal("a repeat request from the same member should not restart the engine")
	}
}

func TestRunnerRequiresToken(t *testing.T) {
	fe := newFakeEngine(t, 0)
	modelDir := writeDummyModel(t)
	_, client, base := newTestController(t, fe, modelDir)

	req, _ := http.NewRequest("GET", base+"/runner/v1/health", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a token, got %d", resp.StatusCode)
	}
}

func TestLoadRejectsPathTraversal(t *testing.T) {
	fe := newFakeEngine(t, 0)
	modelDir := writeDummyModel(t)
	_, client, base := newTestController(t, fe, modelDir)

	for _, name := range []string{"../etc/passwd", "sub/tiny.gguf", "..\\windows"} {
		resp := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
			"model_id": "m1", "filename": name,
		})
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusBadRequest {
			t.Fatalf("filename %q returned %d, want 400", name, status)
		}
	}

	resp := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "m1", "filename": "ghost.gguf",
	})
	status := resp.StatusCode
	resp.Body.Close()
	if status != http.StatusNotFound {
		t.Fatalf("missing weight file returned %d, want 404", status)
	}
}

// TestConcurrentInferenceIsSerialized is the regression for concurrent calls
// overwriting cancelActive, which made a cancel stop an unrelated request.
func TestConcurrentInferenceIsSerialized(t *testing.T) {
	fe := newFakeEngine(t, 0)
	modelDir := writeDummyModel(t)
	_, client, base := newTestController(t, fe, modelDir)

	load := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "m1", "filename": "tiny.gguf",
	})
	load.Body.Close()

	const parallel = 8
	done := make(chan string, parallel)
	for i := 0; i < parallel; i++ {
		go func() {
			resp := do(t, client, "POST", base+"/runner/v1/inference", map[string]any{
				"model":               "tiny",
				"messages":            []map[string]string{{"role": "user", "content": "hi"}},
				"requester_member_id": "m-alice",
			})
			out, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			done <- string(out)
		}()
	}

	for i := 0; i < parallel; i++ {
		select {
		case out := <-done:
			if !strings.Contains(out, "[DONE]") {
				t.Fatalf("concurrent request produced a truncated stream: %q", out)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("concurrent inference deadlocked")
		}
	}
}
