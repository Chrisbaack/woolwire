package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Chrisbaack/woolwire/internal/hosting"
	"github.com/Chrisbaack/woolwire/internal/modelpath"
	"github.com/Chrisbaack/woolwire/internal/runner"
	"github.com/Chrisbaack/woolwire/internal/store"
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

func (s *Server) handleArtifactStorage(w http.ResponseWriter, r *http.Request) {
	if s.artifactMgr == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": false,
		})
		return
	}

	used, err := s.artifactMgr.GetUsedDiskSpace()
	if err != nil {
		used = 0
	}
	budget := s.artifactMgr.MaxBudget()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"configured":   true,
		"used_bytes":   used,
		"budget_bytes": budget,
		"read_only":    s.artifactMgr.ReadOnly(),
	})
}

// handleRunnerFlags lists the llama.cpp tuning switches the runner accepts, so
// the settings page can offer them instead of expecting them to be memorized.
// It comes from the validator's own tables, which is what keeps what the UI
// offers and what the server accepts from drifting apart.
func (s *Server) handleRunnerFlags(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(runner.SupportedExtraFlags())
}

// handleSetArtifactStorage changes the ceiling on downloaded weights. The new
// value is persisted, so it survives a restart, and applied to the running
// manager, so the next download is measured against it immediately.
func (s *Server) handleSetArtifactStorage(w http.ResponseWriter, r *http.Request) {
	if s.artifactMgr == nil {
		http.Error(w, "artifact manager not configured", http.StatusServiceUnavailable)
		return
	}

	var body struct {
		BudgetBytes int64 `json:"budget_bytes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.BudgetBytes <= 0 {
		http.Error(w, "budget_bytes must be greater than zero", http.StatusBadRequest)
		return
	}

	if err := s.artifactMgr.SetMaxBudget(body.BudgetBytes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.SetSetting(modelBudgetSetting, strconv.FormatInt(body.BudgetBytes, 10)); err != nil {
		http.Error(w, "failed to save the storage budget", http.StatusInternalServerError)
		return
	}

	used, err := s.artifactMgr.GetUsedDiskSpace()
	if err != nil {
		used = 0
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"configured":   true,
		"used_bytes":   used,
		"budget_bytes": s.artifactMgr.MaxBudget(),
		"read_only":    s.artifactMgr.ReadOnly(),
	})
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
		// Companions travel with the model: a projector, a draft module.
		Companions []struct {
			Kind         string `json:"kind"`
			SourceURL    string `json:"source_url"`
			Filename     string `json:"filename"`
			MaxSizeBytes int64  `json:"max_size_bytes"`
		} `json:"companions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if body.SourceURL == "" || body.Filename == "" {
		http.Error(w, "source_url and filename are required", http.StatusBadRequest)
		return
	}

	req := hosting.DownloadRequest{
		SourceURL:      body.SourceURL,
		Filename:       body.Filename,
		ExpectedSHA256: body.ExpectedSHA256,
		MaxSizeBytes:   body.MaxSizeBytes,
	}
	for _, c := range body.Companions {
		if c.SourceURL == "" || c.Filename == "" {
			http.Error(w, "each companion needs a source_url and filename", http.StatusBadRequest)
			return
		}
		kind := hosting.CompanionKind(c.Kind)
		if kind != hosting.CompanionProjector && kind != hosting.CompanionDraft {
			http.Error(w, fmt.Sprintf("unknown companion kind %q", c.Kind), http.StatusBadRequest)
			return
		}
		req.Companions = append(req.Companions, hosting.CompanionRequest{
			Kind:         kind,
			SourceURL:    c.SourceURL,
			Filename:     c.Filename,
			MaxSizeBytes: c.MaxSizeBytes,
		})
	}

	id, err := s.artifactMgr.StartDownloadSet(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to start download: %v", err), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"download_id": id, "status": "downloading"})
}

// handleResolveHuggingFaceRepo lists the weight files a public Hugging Face
// repository publishes, so the download form is filled from what is there
// rather than from a guess at a filename.
func (s *Server) handleResolveHuggingFaceRepo(w http.ResponseWriter, r *http.Request) {
	if s.artifactMgr == nil {
		http.Error(w, "artifact manager not configured", http.StatusServiceUnavailable)
		return
	}

	repo, err := hosting.ParseRepoRef(r.URL.Query().Get("repo"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	weights, err := s.artifactMgr.ResolveHuggingFaceRepo(ctx, repo)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, hosting.ErrRepoNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	if len(weights) == 0 {
		http.Error(w, hosting.ErrNoWeightsInRepo.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"repo":  repo,
		"files": weights,
	})
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
		// Refusing to delete weights Woolwire did not install is a rule, not a
		// malformed request.
		status := http.StatusBadRequest
		if errors.Is(err, hosting.ErrNotManaged) {
			status = http.StatusForbidden
		}
		http.Error(w, fmt.Sprintf("delete artifact failed: %v", err), status)
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

	s.infer.SetRunner(s.runnerClient)
	health, err := s.infer.RunnerHealth(r.Context())
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"configured": true,
			"status":     "error",
			"error":      err.Error(),
		})
		return
	}
	active, queued := 0, 0
	if s.infer != nil {
		active, queued = s.infer.Queue().Stats()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"configured":             true,
		"status":                 health.Status,
		"loaded_model_id":        health.LoadedModelID,
		"loaded_file":            health.LoadedFile,
		"engine_pid":             health.EnginePID,
		"has_gpu":                health.HasGPU,
		"gpu_name":               health.GPUName,
		"engine_error":           health.Error,
		"engine_notice":          health.Notice,
		"context_limit":          health.ContextLimit,
		"threads":                health.Threads,
		"gpu_layers":             health.GPULayers,
		"extra_args":             health.ExtraArgs,
		"processing":             health.Processing,
		"queued_requests":        health.QueuedRequests,
		"requests_completed":     health.RequestsCompleted,
		"requests_failed":        health.RequestsFailed,
		"requests_cancelled":     health.RequestsCancelled,
		"prompt_tokens":          health.PromptTokens,
		"completion_tokens":      health.CompletionTokens,
		"last_prompt_tokens":     health.LastPromptTokens,
		"last_completion_tokens": health.LastCompletionTokens,
		"last_tokens_per_second": health.LastTokensPerSecond,
		"current_context_tokens": health.CurrentContextTokens,
		"started_at":             health.StartedAt,
		"host_active_requests":   active,
		"host_queued_requests":   queued,
	})
}

type managedModelRequest struct {
	ModelID      string   `json:"model_id"`
	Name         string   `json:"name"`
	Filename     string   `json:"filename"`
	ContextLimit int      `json:"context_limit"`
	MaxTokens    int      `json:"max_tokens"`
	Threads      int      `json:"threads"`
	GPULayers    *int     `json:"gpu_layers"`
	Projector    string   `json:"projector,omitempty"`
	DraftModel   string   `json:"draft_model,omitempty"`
	ExtraArgs    []string `json:"extra_args"`
	Published    *bool    `json:"published"`
}

func (s *Server) decodeManagedModelRequest(w http.ResponseWriter, r *http.Request) (managedModelRequest, hosting.LoadRequest, error) {
	var body managedModelRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || body.ModelID == "" || body.Filename == "" {
		return body, hosting.LoadRequest{}, errors.New("model_id and filename required")
	}
	if err := runner.ValidateExtraArgs(body.ExtraArgs); err != nil {
		return body, hosting.LoadRequest{}, err
	}
	ref, err := modelpath.Clean(body.Filename)
	if err != nil {
		return body, hosting.LoadRequest{}, err
	}
	body.Filename = ref
	load := hosting.LoadRequest{ModelID: body.ModelID, Filename: body.Filename, ContextLimit: body.ContextLimit,
		Threads: body.Threads, GPULayers: body.GPULayers, Projector: body.Projector,
		DraftModel: body.DraftModel, ExtraArgs: body.ExtraArgs}
	if s.artifactMgr != nil {
		for _, c := range s.artifactMgr.CompanionsFor(body.Filename) {
			switch c.Kind {
			case hosting.CompanionProjector:
				load.Projector = c.Path
			case hosting.CompanionDraft:
				load.DraftModel = c.Path
			}
		}
	}
	return body, load, nil
}

// modelSupportsThinking asks the weights, not the caller: whether a model
// reasons is a property of its chat template, and a client has no business
// claiming otherwise.
func (s *Server) modelSupportsThinking(filename string) bool {
	if s.artifactMgr == nil {
		return false
	}
	artifacts, err := s.artifactMgr.ListArtifacts()
	if err != nil {
		return false
	}
	for _, a := range artifacts {
		if a.Path == filename {
			return a.SupportsThinking
		}
	}
	return false
}

// managedDisplayName turns a weights path into a name to show a person. The
// .gguf suffix is noise in a list where every entry carries it, and a cached
// model's path is a long nested one nobody wants to read.
func managedDisplayName(weightsPath string) string {
	return strings.TrimSuffix(path.Base(strings.TrimSpace(weightsPath)), ".gguf")
}

func (s *Server) managedRecord(body managedModelRequest, load hosting.LoadRequest) store.HostedModelRecord {
	contextLimit := body.ContextLimit
	if contextLimit <= 0 {
		contextLimit = 4096
	}
	maxTokens := body.MaxTokens
	if maxTokens <= 0 {
		maxTokens = contextLimit / 2
		if maxTokens < 2048 && contextLimit >= 2048 {
			maxTokens = 2048
		}
		if maxTokens > 4096 {
			maxTokens = 4096
		}
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = managedDisplayName(body.Filename)
	}
	return store.HostedModelRecord{ID: body.ModelID, Name: name, ModelType: "managed",
		SupportsThinking: s.modelSupportsThinking(body.Filename),
		EndpointURL:      managedEndpointSentinel, BackendModel: name, ContextLimit: contextLimit,
		MaxTokens: maxTokens, Enabled: true, Published: body.Published != nil && *body.Published,
		Filename: body.Filename, Threads: load.Threads, GPULayers: load.GPULayers,
		Projector: load.Projector, DraftModel: load.DraftModel, ExtraArgs: append([]string(nil), load.ExtraArgs...)}
}

// managedRecordFor builds the row to persist for a prepare or a load. It
// merges onto any existing row, so a field the caller omitted keeps its stored
// value instead of reverting to a default and silently discarding an earlier
// choice. preserveEnabled applies to prepare: re-describing a model nobody is
// sharing must not quietly re-enable a row that was disabled, whereas loading
// one is an explicit request to serve it.
func (s *Server) managedRecordFor(body managedModelRequest, load hosting.LoadRequest, preserveEnabled bool) store.HostedModelRecord {
	rec := s.managedRecord(body, load)
	rec.Revision = 1
	existing, err := s.store.GetHostedModel(body.ModelID)
	if err != nil || existing == nil {
		return rec
	}
	rec.Revision = existing.Revision + 1
	if body.ContextLimit <= 0 {
		rec.ContextLimit = existing.ContextLimit
	}
	if body.MaxTokens <= 0 {
		rec.MaxTokens = existing.MaxTokens
	}
	if strings.TrimSpace(body.Name) == "" {
		rec.Name, rec.BackendModel = existing.Name, existing.BackendModel
	}
	if body.Published == nil {
		rec.Published = existing.Published
	}
	if preserveEnabled && !rec.Published {
		rec.Enabled = existing.Enabled
	}
	return rec
}

// weightsAreDownloaded reports whether the artifact manager can see the file a
// managed row names. A model can only be prepared for on-demand loading once
// its weights are actually on this host.
func (s *Server) weightsAreDownloaded(filename string) (bool, error) {
	if s.artifactMgr == nil {
		return true, nil
	}
	artifacts, err := s.artifactMgr.ListArtifacts()
	if err != nil {
		return false, err
	}
	for _, a := range artifacts {
		if a.Path == filename {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) handlePrepareManagedModel(w http.ResponseWriter, r *http.Request) {
	body, load, err := s.decodeManagedModelRequest(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	downloaded, listErr := s.weightsAreDownloaded(body.Filename)
	if listErr != nil {
		http.Error(w, "failed to read the downloaded model inventory", http.StatusInternalServerError)
		return
	}
	if !downloaded {
		http.Error(w, "model weights are not downloaded on this host", http.StatusConflict)
		return
	}

	rec := s.managedRecordFor(body, load, true)
	if err := s.store.SaveHostedModel(rec); err != nil {
		http.Error(w, "failed to record managed model", http.StatusInternalServerError)
		return
	}
	// Advertise immediately rather than waiting for the refresh tick, so a
	// prepared model is offered to peers as soon as the owner shares it.
	s.advertiseLocalModels()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

func (s *Server) handleLoadManagedModel(w http.ResponseWriter, r *http.Request) {
	if s.runnerClient == nil {
		http.Error(w, "runner companion container not configured", http.StatusServiceUnavailable)
		return
	}

	body, load, err := s.decodeManagedModelRequest(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.infer.SetRunner(s.runnerClient)
	if err := s.infer.LoadManagedModel(r.Context(), load); err != nil {
		// Keep prepared rows and their sharing choices intact; refresh the ad so
		// peers see the runner's actual state after a failed replacement.
		s.advertiseLocalModels()
		http.Error(w, fmt.Sprintf("failed to load model in runner: %v", err), http.StatusInternalServerError)
		return
	}

	// Loading is also a convenient way to prepare a model. It publishes only
	// when the caller explicitly supplied published=true; downloaded inventory
	// is otherwise opt-in through /prepare.
	rec := s.managedRecordFor(body, load, false)
	if err := s.store.SaveHostedModel(rec); err != nil {
		http.Error(w, "failed to record managed model", http.StatusInternalServerError)
		return
	}

	s.advertiseLocalModels()

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

	s.infer.SetRunner(s.runnerClient)
	if err := s.infer.UnloadManagedModel(r.Context()); err != nil {
		http.Error(w, fmt.Sprintf("failed to unload model: %v", err), http.StatusInternalServerError)
		return
	}

	// Explicit unload leaves the model published and therefore visible as
	// unloaded in the room; only the runner's weights have been released.
	s.advertiseLocalModels()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
