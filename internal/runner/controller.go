package runner

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
}

// EngineArgs is the resolved launch configuration for one model.
type EngineArgs struct {
	ModelID      string
	Filename     string
	ModelPath    string
	ContextLimit int
	Threads      int
	GPULayers    int
	Port         uint16
}

type Controller struct {
	modelDir    string
	runnerToken string
	enginePath  string
	enginePort  uint16
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
	if cfg.EngineCommand == nil {
		enginePath := cfg.EnginePath
		if enginePath == "" {
			enginePath = "llama-server"
		}
		cfg.EngineCommand = func(ctx context.Context, args EngineArgs) *exec.Cmd {
			return exec.CommandContext(ctx, enginePath,
				"-m", args.ModelPath,
				"-c", fmt.Sprintf("%d", args.ContextLimit),
				"-t", fmt.Sprintf("%d", args.Threads),
				"-ngl", fmt.Sprintf("%d", args.GPULayers),
				"--port", fmt.Sprintf("%d", args.Port),
				"--host", "127.0.0.1",
			)
		}
	}

	c := &Controller{
		modelDir:    cfg.ModelDir,
		runnerToken: cfg.RunnerToken,
		enginePath:  cfg.EnginePath,
		enginePort:  cfg.EnginePort,
		healthCheck:   cfg.HealthCheck,
		engineCommand: cfg.EngineCommand,
		status:      StatusIdle,
		mux:         http.NewServeMux(),
	}

	c.routes()
	return c, nil
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
	})
}

type LoadModelRequest struct {
	ModelID      string `json:"model_id"`
	Filename     string `json:"filename"`
	ContextLimit int    `json:"context_limit"`
	Threads      int    `json:"threads"`
	GPULayers    int    `json:"gpu_layers"`
}

func (c *Controller) handleLoadModel(w http.ResponseWriter, r *http.Request) {
	var req LoadModelRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Filename sanitization: prohibit path traversal, directories, or absolute paths
	cleanName := filepath.Base(req.Filename)
	if cleanName != req.Filename || strings.Contains(req.Filename, "..") || strings.Contains(req.Filename, "/") || strings.Contains(req.Filename, "\\") {
		http.Error(w, "invalid filename: path traversal prohibited", http.StatusBadRequest)
		return
	}

	modelPath := filepath.Join(c.modelDir, cleanName)
	info, err := os.Stat(modelPath)
	if err != nil || info.IsDir() {
		http.Error(w, fmt.Sprintf("model weight file %q not found in model volume", cleanName), http.StatusNotFound)
		return
	}

	args := EngineArgs{
		ModelID:      req.ModelID,
		Filename:     cleanName,
		ModelPath:    modelPath,
		ContextLimit: req.ContextLimit,
		Threads:      req.Threads,
		GPULayers:    req.GPULayers,
		Port:         c.enginePort,
	}
	if args.ContextLimit <= 0 {
		args.ContextLimit = 4096
	}
	if args.Threads <= 0 {
		args.Threads = 4
	}
	if args.GPULayers < 0 {
		args.GPULayers = 0
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
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}

	if err := cmd.Start(); err != nil {
		cancel()
		c.mu.Lock()
		c.status = StatusError
		c.lastError = err.Error()
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
			if waitErr != nil {
				c.lastError = waitErr.Error()
			} else {
				c.lastError = "engine exited unexpectedly"
			}
		}
	}()

	if err := c.waitForHealthy(ctx, done); err != nil {
		c.mu.Lock()
		c.status = StatusError
		c.lastError = err.Error()
		c.stopEngineLocked()
		c.mu.Unlock()
		return 0, err
	}

	c.mu.Lock()
	c.status = StatusReady
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
	lastRequester := c.lastRequester
	c.mu.RUnlock()

	if status != StatusReady || loadedID == "" {
		http.Error(w, "no model is loaded in runner", http.StatusServiceUnavailable)
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
