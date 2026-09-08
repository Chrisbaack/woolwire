package localapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Chrisbaack/woolwire/internal/catalog"
	"github.com/Chrisbaack/woolwire/internal/hosting"
	"github.com/Chrisbaack/woolwire/internal/identity"
	"github.com/Chrisbaack/woolwire/internal/modelpath"
	"github.com/Chrisbaack/woolwire/internal/peerapi"
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
		"has_gpu":         health.HasGPU,
		"gpu_name":        health.GPUName,
		"engine_error":    health.Error,
		"engine_notice":   health.Notice,
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
		// GPULayers is optional: omitted, the runner decides, since it is the
		// container holding the GPU.
		GPULayers *int  `json:"gpu_layers"`
		Published *bool `json:"published"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ModelID == "" || body.Filename == "" {
		http.Error(w, "model_id and filename required", http.StatusBadRequest)
		return
	}

	// The reference may name a file inside a subdirectory of the models
	// directory — that is where a Hugging Face cache keeps them — but it must
	// not point outside it.
	ref, err := modelpath.Clean(body.Filename)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body.Filename = ref

	// The model's companions are looked up rather than asked for. Which
	// projector or draft module goes with a model follows from the model, and
	// making someone choose one is how they end up with a vision model that
	// cannot see.
	load := hosting.LoadRequest{
		ModelID:      body.ModelID,
		Filename:     body.Filename,
		ContextLimit: body.ContextLimit,
		Threads:      body.Threads,
		GPULayers:    body.GPULayers,
	}
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

	if err := s.runnerClient.LoadModel(r.Context(), load); err != nil {
		s.withdrawManagedModels("")
		http.Error(w, fmt.Sprintf("failed to load model in runner: %v", err), http.StatusInternalServerError)
		return
	}

	// Withdraw any other managed models that were replaced by the newly loaded model.
	s.withdrawManagedModels(body.ModelID)

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
		name = strings.TrimSuffix(path.Base(body.Filename), ".gguf")
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

	// Advertise it immediately rather than waiting for the refresh tick, so
	// the model is selectable the moment the UI reloads the catalog.
	s.advertiseLocalModels()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

func (s *Server) withdrawManagedModels(exceptModelID string) {
	models, err := s.store.ListHostedModels()
	if err != nil {
		return
	}

	roomRec, _ := s.store.GetRoomState()
	device, _ := s.store.GetDeviceIdentity()
	var myMemberID string
	var myPubKey ed25519.PublicKey
	var myPrivKey ed25519.PrivateKey
	if device != nil {
		myMemberID, _ = peerapi.MemberIDForDevicePublic(device.DevicePublic)
		if pubBytes, err := identity.DecodeToken(device.DevicePublic, ed25519.PublicKeySize); err == nil {
			myPubKey = ed25519.PublicKey(pubBytes)
		}
		if len(device.DevicePrivate) == ed25519.PrivateKeySize {
			myPrivKey = ed25519.PrivateKey(device.DevicePrivate)
		}
	}

	for _, m := range models {
		if m.ModelType != "managed" {
			continue
		}
		if exceptModelID != "" && m.ID == exceptModelID {
			continue
		}
		if !m.Enabled {
			continue
		}
		m.Enabled = false
		m.Revision++
		_ = s.store.SaveHostedModel(m)

		if s.catalog != nil && roomRec != nil && myPubKey != nil && myPrivKey != nil {
			ad := catalog.ModelAd{
				RoomID:        roomRec.RoomID,
				HostMemberID:  myMemberID,
				ModelID:       m.ID,
				Revision:      m.Revision,
				Name:          m.Name,
				ContextLimit:  m.ContextLimit,
				Availability:  "offline",
				QueueEstimate: 0,
				IsManaged:     true,
			}
			_ = ad.Sign(myPrivKey)
			_ = s.catalog.Upsert(ad, myPubKey)
		}
	}
}

// managedEndpointSentinel marks a hosted_models row that is served by the
// runner companion rather than dialed over HTTP.
const managedEndpointSentinel = "runner://managed"

func (s *Server) handleUnloadManagedModel(w http.ResponseWriter, r *http.Request) {
	if s.runnerClient == nil {
		http.Error(w, "runner companion container not configured", http.StatusServiceUnavailable)
		return
	}

	if err := s.runnerClient.UnloadModel(r.Context()); err != nil {
		http.Error(w, fmt.Sprintf("failed to unload model: %v", err), http.StatusInternalServerError)
		return
	}

	s.withdrawManagedModels("")

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
