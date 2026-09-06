package localapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/cbaack/woolwire/internal/hosting"
)

func (s *Server) handleGetHardware(w http.ResponseWriter, r *http.Request) {
	profile := hosting.DetectHardware()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(profile)
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	if s.artifactMgr == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]hosting.ArtifactManifest{})
		return
	}

	manifests, err := s.artifactMgr.ListArtifacts()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to list artifacts: %v", err), http.StatusInternalServerError)
		return
	}
	if manifests == nil {
		manifests = []hosting.ArtifactManifest{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(manifests)
}

func (s *Server) handleDownloadArtifact(w http.ResponseWriter, r *http.Request) {
	if s.artifactMgr == nil {
		http.Error(w, "artifact manager not configured", http.StatusServiceUnavailable)
		return
	}

	var body struct {
		SourceURL      string `json:"source_url"`
		Filename       string `json:"filename"`
		ExpectedSHA256 string `json:"expected_sha256"`
		MaxSizeBytes   int64  `json:"max_size_bytes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if body.SourceURL == "" || body.Filename == "" {
		http.Error(w, "source_url and filename are required", http.StatusBadRequest)
		return
	}

	manifest, err := s.artifactMgr.DownloadArtifact(
		r.Context(),
		body.SourceURL,
		body.Filename,
		body.ExpectedSHA256,
		body.MaxSizeBytes,
		nil,
	)
	if err != nil {
		http.Error(w, fmt.Sprintf("artifact download failed: %v", err), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(manifest)
}

func (s *Server) handleDeleteArtifact(w http.ResponseWriter, r *http.Request) {
	if s.artifactMgr == nil {
		http.Error(w, "artifact manager not configured", http.StatusServiceUnavailable)
		return
	}

	filename := r.PathValue("filename")
	if filename == "" {
		http.Error(w, "filename required", http.StatusBadRequest)
		return
	}

	if err := s.artifactMgr.DeleteArtifact(filename); err != nil {
		http.Error(w, fmt.Sprintf("delete artifact failed: %v", err), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleRunnerHealth(w http.ResponseWriter, r *http.Request) {
	if s.runnerClient == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": false,
			"status":     "offline",
		})
		return
	}

	health, err := s.runnerClient.Health(r.Context())
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": true,
			"status":     "error",
			"error":      err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"configured":      true,
		"status":          health.Status,
		"loaded_model_id": health.LoadedModelID,
		"loaded_file":     health.LoadedFile,
		"engine_pid":      health.EnginePID,
	})
}

func (s *Server) handleLoadManagedModel(w http.ResponseWriter, r *http.Request) {
	if s.runnerClient == nil {
		http.Error(w, "runner companion container not configured", http.StatusServiceUnavailable)
		return
	}

	var body struct {
		ModelID      string `json:"model_id"`
		Filename     string `json:"filename"`
		ContextLimit int    `json:"context_limit"`
		Threads      int    `json:"threads"`
		GPULayers    int    `json:"gpu_layers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ModelID == "" || body.Filename == "" {
		http.Error(w, "model_id and filename required", http.StatusBadRequest)
		return
	}

	// Filename traversal check
	if strings.Contains(body.Filename, "..") || strings.Contains(body.Filename, "/") {
		http.Error(w, "path traversal prohibited", http.StatusBadRequest)
		return
	}

	err := s.runnerClient.LoadModel(r.Context(), body.ModelID, body.Filename, body.ContextLimit, body.Threads, body.GPULayers)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to load model in runner: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "model_id": body.ModelID})
}

func (s *Server) handleUnloadManagedModel(w http.ResponseWriter, r *http.Request) {
	if s.runnerClient == nil {
		http.Error(w, "runner companion container not configured", http.StatusServiceUnavailable)
		return
	}

	if err := s.runnerClient.UnloadModel(r.Context()); err != nil {
		http.Error(w, fmt.Sprintf("failed to unload model: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
