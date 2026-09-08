package runner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cbaack/woolwire/internal/modelpath"
)

type EngineStatus string

const (
	StatusIdle    EngineStatus = "idle"
	StatusLoading EngineStatus = "loading"
	StatusReady   EngineStatus = "ready"
	StatusBusy    EngineStatus = "busy"
	StatusError   EngineStatus = "error"
)

// healthPollTimeout bounds how long a load waits for llama-server to finish
// reading weights. Flipping to ready at cmd.Start() reported a model as
// available while it was still loading, so the first request always failed.
const (
	healthPollTimeout  = 10 * time.Minute
	healthPollInterval = 500 * time.Millisecond
)

type Config struct {
	ModelDir    string
	RunnerToken string
	EnginePath  string
	EnginePort  uint16
	// HealthCheck overrides the engine readiness probe. Tests supply a fake,
	// and a deployment using an engine without /health can point it elsewhere.
	HealthCheck func(ctx context.Context, port uint16) error
	// EngineCommand builds the process for one load. It defaults to
	// llama-server with the flags below; a deployment needing extra engine
	// flags, or a test using a stand-in engine, replaces it wholesale.
	EngineCommand func(ctx context.Context, args EngineArgs) *exec.Cmd
	// DetectGPU reports whether this container can offload to a GPU, and what
	// it is called. It defaults to looking for the device node.
	DetectGPU func() (bool, string)
}

// detectGPU looks for the device the container was given. Only the device node
// proves the GPU is usable from in here: /proc/driver/nvidia is visible to
// every container on the host, whether or not it was passed a device.
func detectGPU() (bool, string) {
	if _, err := os.Stat("/dev/nvidia0"); err != nil {
		return false, ""
	}
	name := "NVIDIA CUDA Compatible Device"
	if out, err := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader").Output(); err == nil {
		if first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n"); first != "" {
			name = first
		}
	}
	return true, name
}

// EngineArgs is the resolved launch configuration for one model.
type EngineArgs struct {
	ModelID      string
	Filename     string
	ModelPath    string
	ContextLimit int
	Threads      int
	// ProjectorPath is a multimodal projector to load with the model. Without
	// it a vision model answers as a text-only one.
	ProjectorPath string
	// DraftPath is a multi-token-prediction module run as a speculative
	// draft. It only ever affects speed.
	DraftPath string
	// GPULayers is how many layers to offload. A nil value leaves the choice
	// to the engine, whose own default sizes the offload to the VRAM actually
	// free — better than any number this process could pick, and it degrades
	// to a partial offload instead of failing when a model does not fit.
	GPULayers *int
	Port      uint16
}

type Controller struct {
	modelDir    string
	runnerToken string
	enginePath  string
	enginePort  uint16
	hasGPU      bool
	gpuName     string
	// notice explains a load that succeeded on different terms than asked,
	// such as one that had to drop speculative decoding.
	notice        string
	healthCheck   func(ctx context.Context, port uint16) error
	engineCommand func(ctx context.Context, args EngineArgs) *exec.Cmd

	mu            sync.RWMutex
	loaded        EngineArgs
	status        EngineStatus
	lastError     string
	engineCmd     *exec.Cmd
	engineCancel  context.CancelFunc
	engineDone    chan struct{}
	cancelActive  context.CancelFunc
	activeRequest string
	// lastRequester is the member the engine last served. The architecture
	// requires a restart between different requesting members so one member's
	// cached state never reaches the next.
	lastRequester string

	// inferenceMu serializes inference. The engine holds one KV cache, and
	// concurrent calls previously overwrote each other's cancel function, so
	// cancelling one request stopped an unrelated one.
	inferenceMu sync.Mutex

	mux *http.ServeMux
}

func NewController(cfg Config) (*Controller, error) {
	if cfg.ModelDir == "" {
		return nil, errors.New("model directory is required")
	}
	if cfg.EnginePort == 0 {
		cfg.EnginePort = 8081
	}
	if cfg.HealthCheck == nil {
		cfg.HealthCheck = defaultHealthCheck
	}
	if cfg.DetectGPU == nil {
		cfg.DetectGPU = detectGPU
	}
	if cfg.EngineCommand == nil {
		enginePath := cfg.EnginePath
		if enginePath == "" {
			enginePath = "llama-server"
		}
		cfg.EngineCommand = func(ctx context.Context, args EngineArgs) *exec.Cmd {
			argv := []string{
				"-m", args.ModelPath,
				"-c", fmt.Sprintf("%d", args.ContextLimit),
				"-t", fmt.Sprintf("%d", args.Threads),
				"--port", fmt.Sprintf("%d", args.Port),
				"--host", "127.0.0.1",
			}
			if args.ProjectorPath != "" {
				argv = append(argv, "--mmproj", args.ProjectorPath)
			}
			if args.DraftPath != "" {
				argv = append(argv, "-md", args.DraftPath, "--spec-type", "draft-mtp")
			}
			// Omitting -ngl entirely is what asks the engine to decide, and it
			// works on builds that predate the flag's "auto" value.
			if args.GPULayers != nil {
				argv = append(argv, "-ngl", fmt.Sprintf("%d", *args.GPULayers))
			}
			return exec.CommandContext(ctx, enginePath, argv...)
		}
	}

	hasGPU, gpuName := cfg.DetectGPU()

	c := &Controller{
		hasGPU:        hasGPU,
		gpuName:       gpuName,
		modelDir:      cfg.ModelDir,
		runnerToken:   cfg.RunnerToken,
		enginePath:    cfg.EnginePath,
		enginePort:    cfg.EnginePort,
		healthCheck:   cfg.HealthCheck,
		engineCommand: cfg.EngineCommand,
		status:        StatusIdle,
		mux:           http.NewServeMux(),
	}

	c.routes()
	return c, nil
}

// engineTailBytes bounds what we keep of the engine's output. Only the last
// lines matter: llama-server prints why it is giving up just before it exits.
const engineTailBytes = 8 << 10

// outputTail keeps the end of the engine's output so a failure can say what
// the engine said. Without it the only record was the runner container's own
// stdout, which nobody looking at the UI can see: a model the engine cannot
// load surfaced as "runner error (500)" and nothing else.
type outputTail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *outputTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.buf = append(t.buf, p...)
	if len(t.buf) > engineTailBytes {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-engineTailBytes:]...)
	}
	return len(p), nil
}

// causePattern picks out the lines that say what went wrong, as opposed to
// the several lines an engine prints while giving up afterwards.
var causePattern = regexp.MustCompile(`(?i)\b(error|failed|cannot|unable|unsupported|unknown|out of memory|no such)\b`)

// summarize returns up to n lines explaining a failure.
//
// The first failing lines are the useful ones: llama.cpp names the actual
// problem ("unknown model architecture: 'k2-horizon'") and then prints
// several lines of cleanup, so quoting the tail reported only that it was
// exiting — true, and no help at all. Lines are deduplicated because the
// engine probes a model twice before giving up, and the diagnosis appears
// once per attempt.
func (t *outputTail) summarize(n int) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	var lines []string
	for _, raw := range strings.Split(string(t.buf), "\n") {
		if line := strings.TrimSpace(raw); line != "" {
			lines = append(lines, line)
		}
	}

	pick := func(candidates []string) []string {
		seen := make(map[string]bool, len(candidates))
		var kept []string
		for _, line := range candidates {
			if seen[line] {
				continue
			}
			seen[line] = true
			kept = append(kept, line)
			if len(kept) == n {
				break
			}
		}
		return kept
	}

	var causes []string
	for _, line := range lines {
		if causePattern.MatchString(line) {
			causes = append(causes, line)
		}
	}
	if len(causes) > 0 {
		return strings.Join(pick(causes), "; ")
	}

	// Nothing looked like a diagnosis, so the end of the output is the best
	// available account.
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(pick(lines), "; ")
}

// withEngineOutput reports an engine failure together with what the engine
// printed, so the reason reaches whoever asked for the load.
func withEngineOutput(err error, tail *outputTail) error {
	detail := tail.summarize(3)
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

// defaultHealthCheck polls llama-server's /health until it answers.
func defaultHealthCheck(ctx context.Context, port uint16) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("engine health returned %d", resp.StatusCode)
	}
	return nil
}

func (c *Controller) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c.runnerToken != "" {
			authHeader := r.Header.Get("Authorization")
			expected := "Bearer " + c.runnerToken
			if subtle.ConstantTimeCompare([]byte(authHeader), []byte(expected)) != 1 {
				http.Error(w, "unauthorized runner access", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (c *Controller) routes() {
	c.mux.HandleFunc("GET /runner/v1/health", c.authMiddleware(c.handleHealth))
	c.mux.HandleFunc("POST /runner/v1/models/load", c.authMiddleware(c.handleLoadModel))
	c.mux.HandleFunc("POST /runner/v1/models/unload", c.authMiddleware(c.handleUnloadModel))
	c.mux.HandleFunc("POST /runner/v1/inference", c.authMiddleware(c.handleInference))
	c.mux.HandleFunc("POST /runner/v1/inference/cancel", c.authMiddleware(c.handleCancelInference))
	c.mux.HandleFunc("POST /runner/v1/engine/restart", c.authMiddleware(c.handleRestartEngine))
}

func (c *Controller) Handler() http.Handler {
	return c.mux
}

func (c *Controller) handleHealth(w http.ResponseWriter, r *http.Request) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var pid int
	if c.engineCmd != nil && c.engineCmd.Process != nil {
		pid = c.engineCmd.Process.Pid
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":          c.status,
		"loaded_model_id": c.loaded.ModelID,
		"loaded_file":     c.loaded.Filename,
		"engine_pid":      pid,
		"model_dir":       c.modelDir,
		"error":           c.lastError,
		// The GPU is attached to this container, not to the one asking. The
		// app has no way to see it, so the runner reports it.
		"has_gpu":  c.hasGPU,
		"gpu_name": c.gpuName,
		"notice":   c.notice,
	})
}

type LoadModelRequest struct {
	ModelID      string `json:"model_id"`
	Filename     string `json:"filename"`
	ContextLimit int    `json:"context_limit"`
	Threads      int    `json:"threads"`
	// Projector and DraftModel locate the model's companions, relative to the
	// models directory and validated the same way the weights are.
	Projector  string `json:"mmproj"`
	DraftModel string `json:"draft_model"`
	// GPULayers is how many layers to offload. Omit it to let the runner
	// decide: it is the process with the GPU attached, and the caller is in
	// another container that cannot see one. 0 still means "run on the CPU".
	GPULayers *int `json:"gpu_layers"`
}

// defaultGPULayers resolves an omitted gpu_layers. With a GPU attached the
// engine picks the split itself; without one there is nothing to offload to,
// and saying so keeps a GPU-less runner off a code path it cannot serve.
func (c *Controller) defaultGPULayers() *int {
	if c.hasGPU {
		return nil
	}
	cpuOnly := 0
	return &cpuOnly
}

func (c *Controller) handleLoadModel(w http.ResponseWriter, r *http.Request) {
	var req LoadModelRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// The reference may name a file in a subdirectory — a Hugging Face cache
	// nests every weight file — but it must stay inside the model volume.
	cleanName, modelPath, err := modelpath.Resolve(c.modelDir, req.Filename)
	if err != nil {
		if errors.Is(err, modelpath.ErrEmpty) || errors.Is(err, modelpath.ErrEscapes) || errors.Is(err, modelpath.ErrTooDeep) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, fmt.Sprintf("model weight file %q not found in model volume", req.Filename), http.StatusNotFound)
		return
	}

	// A companion the runner cannot find is worth reporting: silently serving
	// a vision model without its projector looks like the model is broken.
	var projectorPath, draftPath string
	if req.Projector != "" {
		if _, resolved, err := modelpath.Resolve(c.modelDir, req.Projector); err != nil {
			http.Error(w, fmt.Sprintf("projector %q not found in model volume", req.Projector), http.StatusNotFound)
			return
		} else {
			projectorPath = resolved
		}
	}
	if req.DraftModel != "" {
		if _, resolved, err := modelpath.Resolve(c.modelDir, req.DraftModel); err != nil {
			http.Error(w, fmt.Sprintf("draft model %q not found in model volume", req.DraftModel), http.StatusNotFound)
			return
		} else {
			draftPath = resolved
		}
	}

	args := EngineArgs{
		ModelID:       req.ModelID,
		Filename:      cleanName,
		ModelPath:     modelPath,
		ProjectorPath: projectorPath,
		DraftPath:     draftPath,
		ContextLimit:  req.ContextLimit,
		Threads:       req.Threads,
		GPULayers:     c.defaultGPULayers(),
		Port:          c.enginePort,
	}
	if req.GPULayers != nil && *req.GPULayers >= 0 {
		layers := *req.GPULayers
		args.GPULayers = &layers
	}
	if args.ContextLimit <= 0 {
		args.ContextLimit = 4096
	}
	if args.Threads <= 0 {
		args.Threads = 4
	}

	c.mu.Lock()
	if c.loaded.ModelID == req.ModelID && c.status == StatusReady && c.engineCmd != nil {
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "model_id": req.ModelID})
		return
	}
	c.mu.Unlock()

	pid, err := c.launchEngine(r.Context(), args)
	if err != nil && args.DraftPath != "" {
		// Speculative decoding is an optimization, and support for a given
		// module is newer than the models shipping them. Losing the speed-up
		// beats refusing to serve the model at all — but say so, rather than
		// quietly running something other than what was asked for.
		notice := fmt.Sprintf("speculative decoding disabled: the engine would not start with %s (%v)",
			path.Base(args.DraftPath), err)
		args.DraftPath = ""
		if retryPID, retryErr := c.launchEngine(r.Context(), args); retryErr == nil {
			pid, err = retryPID, nil
			c.mu.Lock()
			c.notice = notice
			c.mu.Unlock()
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "ready",
		"model_id": req.ModelID,
		"pid":      pid,
	})
}

// launchEngine starts llama-server, waits for it to report healthy, and only
// then marks the model ready.
func (c *Controller) launchEngine(ctx context.Context, args EngineArgs) (int, error) {
	c.mu.Lock()
	c.stopEngineLocked()
	c.status = StatusLoading
	c.lastError = ""
	c.loaded = args
	c.mu.Unlock()

	engineCtx, cancel := context.WithCancel(context.Background())
	cmd := c.engineCommand(engineCtx, args)

	// The engine's output still goes where it went before; it is also kept so
	// a failed load can quote it.
	tail := &outputTail{}
	stdout := io.Writer(os.Stdout)
	if cmd.Stdout != nil {
		stdout = cmd.Stdout
	}
	stderr := io.Writer(os.Stderr)
	if cmd.Stderr != nil {
		stderr = cmd.Stderr
	}
	cmd.Stdout = io.MultiWriter(stdout, tail)
	cmd.Stderr = io.MultiWriter(stderr, tail)

	if err := cmd.Start(); err != nil {
		cancel()
		c.mu.Lock()
		c.status = StatusError
		c.lastError = err.Error()
		c.loaded = EngineArgs{}
		c.mu.Unlock()
		return 0, fmt.Errorf("failed to launch engine: %w", err)
	}

	done := make(chan struct{})
	c.mu.Lock()
	c.engineCmd = cmd
	c.engineCancel = cancel
	c.engineDone = done
	c.mu.Unlock()

	// Reap the child. Without this a crashed engine left a zombie process and
	// a status that still claimed "ready".
	go func() {
		waitErr := cmd.Wait()
		close(done)

		c.mu.Lock()
		defer c.mu.Unlock()
		if c.engineDone != done {
			return // superseded by a newer engine
		}
		c.engineCmd = nil
		c.engineDone = nil
		if c.status != StatusIdle {
			c.status = StatusError
			exit := errors.New("engine exited unexpectedly")
			if waitErr != nil {
				exit = waitErr
			}
			c.lastError = withEngineOutput(exit, tail).Error()
		}
	}()

	if err := c.waitForHealthy(ctx, done); err != nil {
		err = withEngineOutput(err, tail)
		c.mu.Lock()
		c.status = StatusError
		c.lastError = err.Error()
		c.loaded = EngineArgs{}
		c.stopEngineLocked()
		c.mu.Unlock()
		return 0, err
	}

	c.mu.Lock()
	c.status = StatusReady
	c.notice = ""
	c.lastRequester = ""
	pid := 0
	if c.engineCmd != nil && c.engineCmd.Process != nil {
		pid = c.engineCmd.Process.Pid
	}
	c.mu.Unlock()

	return pid, nil
}

func (c *Controller) waitForHealthy(ctx context.Context, engineDone <-chan struct{}) error {
	deadline := time.Now().Add(healthPollTimeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-engineDone:
			return errors.New("engine exited before becoming ready")
		default:
		}

		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := c.healthCheck(probeCtx, c.enginePort)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("engine did not become ready within %s: %w", healthPollTimeout, err)
		}

		timer := time.NewTimer(healthPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-engineDone:
			timer.Stop()
			return errors.New("engine exited before becoming ready")
		case <-timer.C:
		}
	}
}

func (c *Controller) handleUnloadModel(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.status = StatusIdle
	c.stopEngineLocked()
	c.loaded = EngineArgs{}
	c.lastRequester = ""
	c.lastError = ""
	c.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (c *Controller) stopEngineLocked() {
	if c.engineCancel != nil {
		c.engineCancel()
		c.engineCancel = nil
	}
	if c.engineCmd != nil && c.engineCmd.Process != nil {
		_ = c.engineCmd.Process.Kill()
	}
	c.engineCmd = nil
	c.engineDone = nil
}

func (c *Controller) handleRestartEngine(w http.ResponseWriter, r *http.Request) {
	c.mu.RLock()
	args := c.loaded
	c.mu.RUnlock()

	if args.ModelID == "" || args.Filename == "" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "restarted": false})
		return
	}

	// Reuse the exact arguments the load used. Restarting with only -m and
	// --port silently changed the context size, thread count, and GPU offload.
	if _, err := c.launchEngine(r.Context(), args); err != nil {
		http.Error(w, fmt.Sprintf("failed to restart engine: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":        true,
		"restarted": true,
		"model_id":  args.ModelID,
	})
}

// InferenceRequest is the typed body the runner accepts. Building the engine
// request from these fields rather than proxying the caller's bytes keeps a
// client from injecting arbitrary llama-server parameters.
type InferenceRequest struct {
	Model             string        `json:"model"`
	ModelID           string        `json:"model_id,omitempty"`
	Messages          []ChatMessage `json:"messages"`
	Stream            bool          `json:"stream"`
	MaxTokens         int           `json:"max_tokens,omitempty"`
	RequesterMemberID string        `json:"requester_member_id,omitempty"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (c *Controller) handleInference(w http.ResponseWriter, r *http.Request) {
	var req InferenceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid or oversized request payload", http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 {
		http.Error(w, "messages are required", http.StatusBadRequest)
		return
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "user", "assistant":
		default:
			http.Error(w, fmt.Sprintf("unsupported message role %q", m.Role), http.StatusBadRequest)
			return
		}
	}

	// One engine, one KV cache, one request at a time.
	c.inferenceMu.Lock()
	defer c.inferenceMu.Unlock()

	c.mu.RLock()
	status := c.status
	loadedID := c.loaded.ModelID
	loadedFile := c.loaded.Filename
	lastRequester := c.lastRequester
	c.mu.RUnlock()

	if status != StatusReady || loadedID == "" {
		http.Error(w, "no model is loaded in runner", http.StatusServiceUnavailable)
		return
	}

	// Enforce loaded-model identity: reject requests intended for a different model
	if req.ModelID != "" && req.ModelID != loadedID {
		http.Error(w, fmt.Sprintf("model %q is not currently loaded in runner (loaded: %q)", req.ModelID, loadedID), http.StatusConflict)
		return
	}
	if req.ModelID == "" && req.Model != "" && req.Model != loadedID && req.Model != loadedFile && req.Model != strings.TrimSuffix(loadedFile, ".gguf") {
		http.Error(w, fmt.Sprintf("model %q is not currently loaded in runner (loaded: %q)", req.Model, loadedID), http.StatusConflict)
		return
	}

	// A new requesting member gets a freshly restarted engine, so nothing of
	// the previous member's session survives into theirs.
	if req.RequesterMemberID != "" && lastRequester != "" && req.RequesterMemberID != lastRequester {
		c.mu.RLock()
		args := c.loaded
		c.mu.RUnlock()
		if _, err := c.launchEngine(r.Context(), args); err != nil {
			http.Error(w, fmt.Sprintf("engine restart between members failed: %v", err), http.StatusServiceUnavailable)
			return
		}
	}

	c.mu.Lock()
	c.lastRequester = req.RequesterMemberID
	port := c.enginePort
	c.mu.Unlock()

	reqCtx, reqCancel := context.WithCancel(r.Context())
	defer reqCancel()

	c.mu.Lock()
	c.cancelActive = reqCancel
	c.activeRequest = req.Model
	c.status = StatusBusy
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.cancelActive = nil
		c.activeRequest = ""
		if c.status == StatusBusy {
			c.status = StatusReady
		}
		c.mu.Unlock()
	}()

	enginePayload := map[string]any{
		"model":    req.Model,
		"messages": req.Messages,
		"stream":   true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}
	if req.MaxTokens > 0 {
		enginePayload["max_tokens"] = req.MaxTokens
	}
	bodyBytes, err := json.Marshal(enginePayload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	engineURL := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port)
	engineReq, err := http.NewRequestWithContext(reqCtx, "POST", engineURL, bytes.NewReader(bodyBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	engineReq.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 0}).Do(engineReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("engine request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)

	flusher, isFlusher := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		if _, err := fmt.Fprintf(w, "%s\n", scanner.Text()); err != nil {
			return
		}
		if isFlusher {
			flusher.Flush()
		}
	}
}

func (c *Controller) handleCancelInference(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	cancelled := false
	if c.cancelActive != nil {
		c.cancelActive()
		cancelled = true
	}
	c.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"cancelled": cancelled})
}

func (c *Controller) Serve(listener net.Listener) error {
	server := &http.Server{
		Handler:     c.mux,
		ReadTimeout: 30 * time.Second,
		// Inference streams for as long as the model takes.
		WriteTimeout: 0,
	}
	return server.Serve(listener)
}

// Close stops the engine and reaps it.
func (c *Controller) Close() error {
	c.mu.Lock()
	c.status = StatusIdle
	c.stopEngineLocked()
	c.mu.Unlock()
	return nil
}
