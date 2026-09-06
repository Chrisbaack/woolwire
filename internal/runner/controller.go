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
	"path/filepath"
	"strings"
	"sync"
)

type EngineStatus string

const (
	StatusIdle    EngineStatus = "idle"
	StatusLoading EngineStatus = "loading"
	StatusReady   EngineStatus = "ready"
	StatusBusy    EngineStatus = "busy"
	StatusError   EngineStatus = "error"
)

type Config struct {
	ModelDir    string
	RunnerToken string
	EnginePath  string
	EnginePort  uint16
}

type Controller struct {
	modelDir    string
	runnerToken string
	enginePath  string
	enginePort  uint16

	mu            sync.RWMutex
	loadedModelID string
	loadedFile    string
	status        EngineStatus
	engineCmd     *exec.Cmd
	engineCancel  context.CancelFunc
	activeReqCtx  context.Context
	cancelActive  context.CancelFunc

	mux *http.ServeMux
}

func NewController(cfg Config) (*Controller, error) {
	if cfg.ModelDir == "" {
		return nil, errors.New("model directory is required")
	}
	if cfg.EnginePort == 0 {
		cfg.EnginePort = 8081
	}

	c := &Controller{
		modelDir:    cfg.ModelDir,
		runnerToken: cfg.RunnerToken,
		enginePath:  cfg.EnginePath,
		enginePort:  cfg.EnginePort,
		status:      StatusIdle,
		mux:         http.NewServeMux(),
	}

	c.routes()
	return c, nil
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
		"loaded_model_id": c.loadedModelID,
		"loaded_file":     c.loadedFile,
		"engine_pid":      pid,
		"model_dir":       c.modelDir,
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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

	c.mu.Lock()
	defer c.mu.Unlock()

	// If already loaded the same model and engine is running, noop
	if c.loadedModelID == req.ModelID && c.status == StatusReady && c.engineCmd != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "model_id": req.ModelID})
		return
	}

	// Unload current engine if running
	c.stopEngineLocked()

	c.status = StatusLoading
	c.loadedModelID = req.ModelID
	c.loadedFile = cleanName

	// Approved argument synthesis - strictly validated
	ctxLimit := req.ContextLimit
	if ctxLimit <= 0 {
		ctxLimit = 4096
	}
	threads := req.Threads
	if threads <= 0 {
		threads = 4
	}
	gpuLayers := req.GPULayers
	if gpuLayers < 0 {
		gpuLayers = 0
	}

	engineExe := c.enginePath
	if engineExe == "" {
		engineExe = "llama-server"
	}

	args := []string{
		"-m", modelPath,
		"-c", fmt.Sprintf("%d", ctxLimit),
		"-t", fmt.Sprintf("%d", threads),
		"-ngl", fmt.Sprintf("%d", gpuLayers),
		"--port", fmt.Sprintf("%d", c.enginePort),
		"--host", "127.0.0.1",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, engineExe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		cancel()
		c.status = StatusError
		http.Error(w, fmt.Sprintf("failed to launch engine: %v", err), http.StatusInternalServerError)
		return
	}

	c.engineCmd = cmd
	c.engineCancel = cancel
	c.status = StatusReady

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "ready",
		"model_id": req.ModelID,
		"pid":      cmd.Process.Pid,
	})
}

func (c *Controller) handleUnloadModel(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.stopEngineLocked()
	c.status = StatusIdle
	c.loadedModelID = ""
	c.loadedFile = ""

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (c *Controller) stopEngineLocked() {
	if c.engineCancel != nil {
		c.engineCancel()
	}
	if c.engineCmd != nil && c.engineCmd.Process != nil {
		_ = c.engineCmd.Process.Kill()
		_ = c.engineCmd.Wait()
	}
	c.engineCmd = nil
	c.engineCancel = nil
}

func (c *Controller) handleRestartEngine(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.loadedModelID == "" || c.loadedFile == "" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "restarted": false})
		return
	}

	modelID := c.loadedModelID
	cleanName := c.loadedFile
	c.stopEngineLocked()

	modelPath := filepath.Join(c.modelDir, cleanName)
	engineExe := c.enginePath
	if engineExe == "" {
		engineExe = "llama-server"
	}

	args := []string{
		"-m", modelPath,
		"--port", fmt.Sprintf("%d", c.enginePort),
		"--host", "127.0.0.1",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, engineExe, args...)
	if err := cmd.Start(); err != nil {
		cancel()
		c.status = StatusError
		http.Error(w, fmt.Sprintf("failed to restart engine: %v", err), http.StatusInternalServerError)
		return
	}

	c.engineCmd = cmd
	c.engineCancel = cancel
	c.status = StatusReady

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":        true,
		"restarted": true,
		"model_id":  modelID,
	})
}

func (c *Controller) handleInference(w http.ResponseWriter, r *http.Request) {
	c.mu.RLock()
	status := c.status
	loadedID := c.loadedModelID
	port := c.enginePort
	c.mu.RUnlock()

	if status != StatusReady || loadedID == "" {
		http.Error(w, "no model is loaded in runner", http.StatusServiceUnavailable)
		return
	}

	bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "request payload too large", http.StatusBadRequest)
		return
	}

	engineURL := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port)
	reqCtx, reqCancel := context.WithCancel(r.Context())
	defer reqCancel()

	c.mu.Lock()
	c.activeReqCtx = reqCtx
	c.cancelActive = reqCancel
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.activeReqCtx = nil
		c.cancelActive = nil
		c.mu.Unlock()
	}()

	engineReq, err := http.NewRequestWithContext(reqCtx, "POST", engineURL, bytes.NewReader(bodyBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	engineReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(engineReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("engine request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, isFlusher := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		_, _ = fmt.Fprintf(w, "%s\n", scanner.Text())
		if isFlusher {
			flusher.Flush()
		}
	}
}

func (c *Controller) handleCancelInference(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	if c.cancelActive != nil {
		c.cancelActive()
	}
	c.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"cancelled": true})
}

func (c *Controller) Serve(listener net.Listener) error {
	server := &http.Server{
		Handler: c.mux,
	}
	return server.Serve(listener)
}
