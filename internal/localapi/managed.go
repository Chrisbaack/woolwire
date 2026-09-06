package localapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/cbaack/woolwire/internal/hosting"
	"github.com/cbaack/woolwire/internal/store"
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

// handleDownloadArtifact starts a background download and returns a job id.
// Running a multi-gigabyte transfer synchronously inside the handler blocked
// the request for the whole download and held the artifact lock with it.
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

	id, err := s.artifactMgr.StartDownload(body.SourceURL, body.Filename, body.ExpectedSHA256, body.MaxSizeBytes)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to start download: %v", err), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"download_id": id, "status": "downloading"})
}

func (s *Server) handleDownloadStatus(w http.ResponseWriter, r *http.Request) {
	if s.artifactMgr == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]hosting.DownloadState{})
		return
	}

	states, err := s.artifactMgr.DownloadStatus(r.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(states)
}

func (s *Server) handleCancelDownload(w http.ResponseWriter, r *http.Request) {
	if s.artifactMgr == nil {
		http.Error(w, "artifact manager not configured", http.StatusServiceUnavailable)
		return
	}
	if err := s.artifactMgr.CancelDownload(r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
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
		Name         string `json:"name"`
		Filename     string `json:"filename"`
		ContextLimit int    `json:"context_limit"`
		MaxTokens    int    `json:"max_tokens"`
		Threads      int    `json:"threads"`
		GPULayers    int    `json:"gpu_layers"`
		Published    *bool  `json:"published"`
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

	if err := s.runnerClient.LoadModel(r.Context(), body.ModelID, body.Filename, body.ContextLimit, body.Threads, body.GPULayers); err != nil {
		http.Error(w, fmt.Sprintf("failed to load model in runner: %v", err), http.StatusInternalServerError)
		return
	}

	// Loading weights into the runner is only half the job. Without a
	// hosted_models row the model was never advertised in the catalog and no
	// peer could ever request it, so managed models were unreachable.
	contextLimit := body.ContextLimit
	if contextLimit <= 0 {
		contextLimit = 4096
	}
	maxTokens := body.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = strings.TrimSuffix(body.Filename, ".gguf")
	}

	revision := 1
	if existing, err := s.store.GetHostedModel(body.ModelID); err == nil && existing != nil {
		revision = existing.Revision + 1
	}

	rec := store.HostedModelRecord{
		ID:        body.ModelID,
		Name:      name,
		ModelType: "managed",
		// Managed models are served through the runner client, not dialed, so
		// the endpoint column carries a sentinel rather than a URL.
		EndpointURL:  managedEndpointSentinel,
		BackendModel: name,
		ContextLimit: contextLimit,
		MaxTokens:    maxTokens,
		Enabled:      true,
		Published:    body.Published == nil || *body.Published,
		Revision:     revision,
	}
	if err := s.store.SaveHostedModel(rec); err != nil {
		http.Error(w, "failed to record managed model", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

// managedEndpointSentinel marks a hosted_models row that is served by the
// runner companion rather than dialed over HTTP.
const managedEndpointSentinel = "runner://managed"

func (s *Server) handleUnloadManagedModel(w http.ResponseWriter, r *http.Request) {
	if s.runnerClient == nil {
		http.Error(w, "runner companion container not configured", http.StatusServiceUnavailable)
		return
	}

	// Find which model the runner currently holds before unloading, so its
	// advertisement can be withdrawn rather than left pointing at nothing.
	var loadedID string
	if health, err := s.runnerClient.Health(r.Context()); err == nil && health != nil {
		loadedID = health.LoadedModelID
	}

	if err := s.runnerClient.UnloadModel(r.Context()); err != nil {
		http.Error(w, fmt.Sprintf("failed to unload model: %v", err), http.StatusInternalServerError)
		return
	}

	if loadedID != "" {
		if rec, err := s.store.GetHostedModel(loadedID); err == nil && rec != nil {
			rec.Enabled = false
			rec.Revision++
			_ = s.store.SaveHostedModel(*rec)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
