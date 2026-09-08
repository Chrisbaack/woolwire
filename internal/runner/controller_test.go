package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		// The controller asks for include_usage, so a real engine closes the
		// stream with a usage frame. llama.cpp attaches its own rate
		// measurement alongside it in "timings".
		_, _ = fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5},\"timings\":{\"predicted_per_second\":42.5}}\n\n")
		if flusher != nil {
			flusher.Flush()
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
		// Pinned so engine arguments do not depend on whether the machine
		// running the tests happens to have a GPU.
		DetectGPU: func() (bool, string) { return false, "" },
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
	if loadArgs == nil || loadArgs.ContextLimit != 8192 || loadArgs.Threads != 3 || loadArgs.GPULayers == nil || *loadArgs.GPULayers != 7 {
		t.Fatalf("load used unexpected arguments: %#v", loadArgs)
	}

	restart := do(t, client, "POST", base+"/runner/v1/engine/restart", nil)
	restart.Body.Close()

	restartArgs := fe.lastArgs.Load()
	if restartArgs == nil {
		t.Fatal("restart launched no engine")
	}
	if restartArgs.ContextLimit != 8192 || restartArgs.Threads != 3 || restartArgs.GPULayers == nil || *restartArgs.GPULayers != 7 {
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

func TestValidateExtraArgs(t *testing.T) {
	for _, args := range [][]string{
		{"--batch-size", "512", "--flash-attn"},
		{"--batch-size=512", "--no-mmap"},
		// llama.cpp spells the toggle both ways across versions; the inline
		// form is how a value reaches it unambiguously.
		{"--flash-attn=on"},
		{"--cache-type-k", "q8_0", "--cache-type-v", "q8_0"},
	} {
		if err := ValidateExtraArgs(args); err != nil {
			t.Errorf("supported tuning flags rejected %#v: %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"--model", "/tmp/secret"},
		{"--host", "0.0.0.0"},
		{"--batch-size"},
		// A bare value after an optional-value flag is indistinguishable from
		// a stray positional, so it is refused rather than guessed at.
		{"--flash-attn", "on"},
		{"--batch-size", "--host"},
		{"--no-mmap=true"},
		{"-ngl", "99"},
	} {
		if err := ValidateExtraArgs(args); err == nil {
			t.Errorf("unsafe or malformed args accepted: %#v", args)
		}
	}
}

func TestLoadRejectsPathTraversal(t *testing.T) {
	fe := newFakeEngine(t, 0)
	modelDir := writeDummyModel(t)
	_, client, base := newTestController(t, fe, modelDir)

	for _, name := range []string{"../etc/passwd", "sub/../../etc/passwd", "/etc/passwd", "..\\windows", ""} {
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

	// A subdirectory is in bounds: it is where a Hugging Face cache keeps
	// every weight file. Only a reference that leaves the volume is refused.
	resp = do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "m1", "filename": "sub/missing.gguf",
	})
	status = resp.StatusCode
	resp.Body.Close()
	if status != http.StatusNotFound {
		t.Fatalf("missing nested weight file returned %d, want 404", status)
	}
}

// TestLoadAcceptsNestedModelPath covers pointing the runner straight at a
// Hugging Face cache, where no weight file sits at the top level.
func TestLoadAcceptsNestedModelPath(t *testing.T) {
	fe := newFakeEngine(t, 0)
	modelDir := t.TempDir()

	blobDir := filepath.Join(modelDir, "models--org--repo", "blobs")
	snapshotDir := filepath.Join(modelDir, "models--org--repo", "snapshots", "rev1")
	for _, dir := range []string{blobDir, snapshotDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	blob := filepath.Join(blobDir, "abcdef")
	if err := os.WriteFile(blob, []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The cache stores the snapshot entry as a symlink into blobs.
	if err := os.Symlink(blob, filepath.Join(snapshotDir, "tiny.gguf")); err != nil {
		t.Fatal(err)
	}

	ctrl, client, base := newTestController(t, fe, modelDir)
	ref := "models--org--repo/snapshots/rev1/tiny.gguf"

	resp := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "m1", "filename": ref,
	})
	status := resp.StatusCode
	resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("nested load returned %d, want 200", status)
	}

	if got := ctrl.loaded.ModelPath; got != filepath.Join(snapshotDir, "tiny.gguf") {
		t.Fatalf("engine was given %q", got)
	}
	// The runner reports the reference it was given, so the app can match the
	// loaded model against the one it listed.
	if got := ctrl.loaded.Filename; got != ref {
		t.Fatalf("loaded file = %q, want %q", got, ref)
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

func TestLoadedModelIdentityEnforced(t *testing.T) {
	fe := newFakeEngine(t, 0)
	modelDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modelDir, "model-a.gguf"), []byte("weights-a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model-b.gguf"), []byte("weights-b"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, client, base := newTestController(t, fe, modelDir)

	// 1. Load model A
	loadA := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "id-a", "filename": "model-a.gguf",
	})
	if loadA.StatusCode != http.StatusOK {
		t.Fatalf("load A failed: %d", loadA.StatusCode)
	}
	loadA.Body.Close()

	// Inference for A should succeed
	infA := do(t, client, "POST", base+"/runner/v1/inference", map[string]any{
		"model_id": "id-a",
		"model":    "model-a",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if infA.StatusCode != http.StatusOK {
		t.Fatalf("inference for A failed: %d", infA.StatusCode)
	}
	infA.Body.Close()

	// 2. Replace with model B
	loadB := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "id-b", "filename": "model-b.gguf",
	})
	if loadB.StatusCode != http.StatusOK {
		t.Fatalf("load B failed: %d", loadB.StatusCode)
	}
	loadB.Body.Close()

	// 3. Request for A (by model_id) must be rejected with 409 Conflict
	infAOld := do(t, client, "POST", base+"/runner/v1/inference", map[string]any{
		"model_id": "id-a",
		"model":    "model-a",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if infAOld.StatusCode != http.StatusConflict {
		t.Fatalf("inference for replaced model A should return 409 Conflict, got %d", infAOld.StatusCode)
	}
	infAOld.Body.Close()

	// Request for A (by model name only) must also be rejected
	infAOldName := do(t, client, "POST", base+"/runner/v1/inference", map[string]any{
		"model":    "model-a",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if infAOldName.StatusCode != http.StatusConflict {
		t.Fatalf("inference for replaced model A by name should return 409 Conflict, got %d", infAOldName.StatusCode)
	}
	infAOldName.Body.Close()

	// 4. Request for B must succeed
	infB := do(t, client, "POST", base+"/runner/v1/inference", map[string]any{
		"model_id": "id-b",
		"model":    "model-b",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if infB.StatusCode != http.StatusOK {
		t.Fatalf("inference for B failed: %d", infB.StatusCode)
	}
	infB.Body.Close()

	// 5. Failed load (missing file)
	loadFail := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "id-c", "filename": "nonexistent.gguf",
	})
	if loadFail.StatusCode != http.StatusNotFound {
		t.Fatalf("load nonexistent should return 404, got %d", loadFail.StatusCode)
	}
	loadFail.Body.Close()
}

// TestGPUOffloadIsTheRunnersDecision covers the split the managed profile
// creates: the GPU is passed to the runner container, so the app asking for a
// layer count would be guessing about hardware it cannot see.
func TestGPUOffloadIsTheRunnersDecision(t *testing.T) {
	newCtrl := func(t *testing.T, hasGPU bool) (*Controller, *http.Client, string) {
		t.Helper()
		fe := newFakeEngine(t, 0)
		ctrl, err := NewController(Config{
			ModelDir:      writeDummyModel(t),
			RunnerToken:   "runner-token",
			EnginePort:    fe.port,
			EngineCommand: fe.command,
			HealthCheck: func(ctx context.Context, port uint16) error {
				return defaultHealthCheck(ctx, port)
			},
			DetectGPU: func() (bool, string) { return hasGPU, "Test GPU" },
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ctrl.Close() })
		srv := httptest.NewServer(ctrl.Handler())
		t.Cleanup(srv.Close)
		return ctrl, srv.Client(), srv.URL
	}

	load := func(t *testing.T, client *http.Client, base string, body map[string]any) {
		t.Helper()
		resp := do(t, client, "POST", base+"/runner/v1/models/load", body)
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusOK {
			t.Fatalf("load returned %d", status)
		}
	}

	// With a GPU and no preference expressed, the engine is left to size the
	// offload against the VRAM that is actually free.
	ctrl, client, base := newCtrl(t, true)
	load(t, client, base, map[string]any{"model_id": "m1", "filename": "tiny.gguf"})
	if got := ctrl.loaded.GPULayers; got != nil {
		t.Fatalf("gpu layers = %d, want the engine's own choice", *got)
	}

	// An explicit 0 still means the CPU: it is a choice, not an omission.
	load(t, client, base, map[string]any{"model_id": "m2", "filename": "tiny.gguf", "gpu_layers": 0})
	if got := ctrl.loaded.GPULayers; got == nil || *got != 0 {
		t.Fatalf("explicit gpu_layers 0 became %v", got)
	}

	// An explicit count is honoured.
	load(t, client, base, map[string]any{"model_id": "m3", "filename": "tiny.gguf", "gpu_layers": 12})
	if got := ctrl.loaded.GPULayers; got == nil || *got != 12 {
		t.Fatalf("explicit gpu_layers 12 became %v", got)
	}

	// Without a GPU there is nothing to offload to, and the runner says so
	// rather than leaving it to an engine that would probe for one.
	ctrlNoGPU, clientNoGPU, baseNoGPU := newCtrl(t, false)
	load(t, clientNoGPU, baseNoGPU, map[string]any{"model_id": "m1", "filename": "tiny.gguf"})
	if got := ctrlNoGPU.loaded.GPULayers; got == nil || *got != 0 {
		t.Fatalf("gpu layers without a GPU = %v, want 0", got)
	}

	// Health reports the runner's hardware, since the app cannot see it.
	resp := do(t, client, "GET", base+"/runner/v1/health", nil)
	var health struct {
		HasGPU  bool   `json:"has_gpu"`
		GPUName string `json:"gpu_name"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&health)
	resp.Body.Close()
	if !health.HasGPU || health.GPUName != "Test GPU" {
		t.Fatalf("health reported has_gpu=%v gpu_name=%q", health.HasGPU, health.GPUName)
	}
}

// TestLoadFailureQuotesTheEngine is the regression for an unloadable model
// surfacing as nothing but "runner error (500)". llama-server explains itself
// before exiting — "unknown model architecture", a missing file, an
// out-of-memory — and that explanation only existed in the runner container's
// own stdout, where nobody using the UI can see it.
func TestLoadFailureQuotesTheEngine(t *testing.T) {
	modelDir := writeDummyModel(t)

	// An engine that prints why it is giving up, then exits without ever
	// serving health, exactly as llama-server does for a model it cannot read.
	ctrl, err := NewController(Config{
		ModelDir:    modelDir,
		RunnerToken: "runner-token",
		EnginePort:  1, // nothing listens here
		EngineCommand: func(ctx context.Context, args EngineArgs) *exec.Cmd {
			// The real shape of the failure: the diagnosis comes first, is
			// repeated because the engine probes the model twice, and is then
			// buried under the lines it prints while giving up.
			return exec.CommandContext(ctx, "sh", "-c",
				`echo "loading model '/models/tiny.gguf'";`+
					`echo "error loading model: unknown model architecture: 'k2-horizon'" >&2;`+
					`echo "error loading model: unknown model architecture: 'k2-horizon'" >&2;`+
					`echo "cleaning up before exit..." >&2;`+
					`echo "exiting due to model loading error" >&2; exit 1`)
		},
		HealthCheck: func(ctx context.Context, port uint16) error {
			return errors.New("connection refused")
		},
		DetectGPU: func() (bool, string) { return false, "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctrl.Close() })

	srv := httptest.NewServer(ctrl.Handler())
	t.Cleanup(srv.Close)

	resp := do(t, srv.Client(), "POST", srv.URL+"/runner/v1/models/load", map[string]any{
		"model_id": "m1", "filename": "tiny.gguf",
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("load returned %d, want 500", resp.StatusCode)
	}
	if !strings.Contains(string(body), "unknown model architecture") {
		t.Fatalf("the load failure did not say why:\n%s", body)
	}
	// The diagnosis appears once, not once per probe.
	if n := strings.Count(string(body), "unknown model architecture"); n != 1 {
		t.Fatalf("the diagnosis was repeated %d times:\n%s", n, body)
	}

	// Health carries the same reason, so the reason survives the request that
	// triggered it.
	health := do(t, srv.Client(), "GET", srv.URL+"/runner/v1/health", nil)
	var h struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = json.NewDecoder(health.Body).Decode(&h)
	health.Body.Close()

	if h.Status != string(StatusError) {
		t.Fatalf("status = %q, want %q", h.Status, StatusError)
	}
	if !strings.Contains(h.Error, "unknown model architecture") {
		t.Fatalf("health error does not say why: %q", h.Error)
	}
}

// TestLoadPassesCompanionsToTheEngine covers a vision model arriving with its
// projector: without --mmproj the engine loads it as text-only, which looks
// like the model is broken rather than like it is missing a file.
func TestLoadPassesCompanionsToTheEngine(t *testing.T) {
	fe := newFakeEngine(t, 0)
	modelDir := writeDummyModel(t)
	for _, name := range []string{"mmproj-F16.gguf", "mtp-tiny.gguf"} {
		if err := os.WriteFile(filepath.Join(modelDir, name), []byte("companion"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ctrl, client, base := newTestController(t, fe, modelDir)
	resp := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "m1", "filename": "tiny.gguf",
		"mmproj": "mmproj-F16.gguf", "draft_model": "mtp-tiny.gguf",
	})
	status := resp.StatusCode
	resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("load returned %d", status)
	}

	if got := ctrl.loaded.ProjectorPath; got != filepath.Join(modelDir, "mmproj-F16.gguf") {
		t.Fatalf("projector path = %q", got)
	}
	if got := ctrl.loaded.DraftPath; got != filepath.Join(modelDir, "mtp-tiny.gguf") {
		t.Fatalf("draft path = %q", got)
	}

	// A companion that is not there is reported, not ignored.
	resp = do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "m2", "filename": "tiny.gguf", "mmproj": "mmproj-missing.gguf",
	})
	status = resp.StatusCode
	resp.Body.Close()
	if status != http.StatusNotFound {
		t.Fatalf("missing projector returned %d, want 404", status)
	}

	// And a companion cannot be used to reach outside the model volume.
	resp = do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "m3", "filename": "tiny.gguf", "mmproj": "../../etc/passwd",
	})
	status = resp.StatusCode
	resp.Body.Close()
	if status != http.StatusNotFound && status != http.StatusBadRequest {
		t.Fatalf("traversing projector path returned %d", status)
	}
}

// TestDraftModelFallsBackWhenTheEngineRefusesIt covers speculative decoding
// being an optimization: support for a given module is newer than the models
// shipping them, and losing the speed-up beats refusing to serve the model.
func TestDraftModelFallsBackWhenTheEngineRefusesIt(t *testing.T) {
	fe := newFakeEngine(t, 0)
	modelDir := writeDummyModel(t)
	if err := os.WriteFile(filepath.Join(modelDir, "mtp-tiny.gguf"), []byte("companion"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The fake engine's health endpoint stays up across launches, so the
	// probe has to follow the process that was actually started.
	var draftAttempt atomic.Bool
	ctrl, err := NewController(Config{
		ModelDir:    modelDir,
		RunnerToken: "runner-token",
		EnginePort:  fe.port,
		EngineCommand: func(ctx context.Context, args EngineArgs) *exec.Cmd {
			draftAttempt.Store(args.DraftPath != "")
			if args.DraftPath != "" {
				// An engine that does not know this speculation type.
				return exec.CommandContext(ctx, "sh", "-c",
					`echo "error: unknown spec type draft-mtp" >&2; exit 1`)
			}
			return fe.command(ctx, args)
		},
		HealthCheck: func(ctx context.Context, port uint16) error {
			if draftAttempt.Load() {
				return errors.New("engine is not listening")
			}
			return defaultHealthCheck(ctx, port)
		},
		DetectGPU: func() (bool, string) { return false, "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctrl.Close() })

	srv := httptest.NewServer(ctrl.Handler())
	t.Cleanup(srv.Close)

	resp := do(t, srv.Client(), "POST", srv.URL+"/runner/v1/models/load", map[string]any{
		"model_id": "m1", "filename": "tiny.gguf", "draft_model": "mtp-tiny.gguf",
	})
	status := resp.StatusCode
	resp.Body.Close()
	if status != http.StatusOK {
		t.Fatalf("load returned %d, want the model served without the draft module", status)
	}

	health := do(t, srv.Client(), "GET", srv.URL+"/runner/v1/health", nil)
	var h struct {
		Status string `json:"status"`
		Notice string `json:"notice"`
	}
	_ = json.NewDecoder(health.Body).Decode(&h)
	health.Body.Close()

	if h.Status != string(StatusReady) {
		t.Fatalf("status = %q, want ready", h.Status)
	}
	if !strings.Contains(h.Notice, "speculative decoding disabled") {
		t.Fatalf("the downgrade was not reported: %q", h.Notice)
	}
}

// TestHealthReportsInferenceStats covers the data behind the runner status
// page: the counters and token totals only mean anything if a completed
// request actually moves them.
func TestHealthReportsInferenceStats(t *testing.T) {
	fe := newFakeEngine(t, 0)
	modelDir := writeDummyModel(t)
	_, client, base := newTestController(t, fe, modelDir)

	load := do(t, client, "POST", base+"/runner/v1/models/load", map[string]any{
		"model_id": "m1", "filename": "tiny.gguf", "context_limit": 8192, "threads": 6,
	})
	load.Body.Close()

	resp := do(t, client, "POST", base+"/runner/v1/inference", map[string]any{
		"model":    "tiny",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	health := do(t, client, "GET", base+"/runner/v1/health", nil)
	defer health.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(health.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}

	for field, want := range map[string]float64{
		"requests_completed": 1,
		"requests_failed":    0,
		"queued_requests":    0,
		"prompt_tokens":      12,
		"completion_tokens":  5,
		// The engine's own measurement is preferred over the wall clock.
		"last_tokens_per_second": 42.5,
		"current_context_tokens": 12,
		"context_limit":          8192,
		"threads":                6,
	} {
		if got[field] != want {
			t.Errorf("health field %q = %v, want %v", field, got[field], want)
		}
	}
	if got["processing"] != false {
		t.Errorf("processing = %v, want false once the request finished", got["processing"])
	}
}
