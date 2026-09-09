package localapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Chrisbaack/woolwire/internal/catalog"
	"github.com/Chrisbaack/woolwire/internal/hosting"
	"github.com/Chrisbaack/woolwire/internal/identity"
	"github.com/Chrisbaack/woolwire/internal/peerapi"
	"github.com/Chrisbaack/woolwire/internal/room"
	"github.com/Chrisbaack/woolwire/internal/store"
)

func (s *Server) handleListHostedModels(w http.ResponseWriter, r *http.Request) {
	s.syncManagedInventory()
	models, err := s.store.ListHostedModels()
	if err != nil {
		http.Error(w, "failed to query hosted models", http.StatusInternalServerError)
		return
	}
	if models == nil {
		models = []store.HostedModelRecord{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(models)
}

// inventorySyncInterval bounds how often an advertisement refresh rescans the
// models directory.
const inventorySyncInterval = 20 * time.Second

// syncManagedInventoryThrottled is the variant for paths that run on a timer
// or on a route the UI polls. The scan walks the models directory and writes a
// row per new file, which is far more work than an advertisement refresh
// should repeat several times a second.
func (s *Server) syncManagedInventoryThrottled() {
	s.mu.Lock()
	stale := time.Since(s.lastInventorySync) >= inventorySyncInterval
	if stale {
		s.lastInventorySync = time.Now()
	}
	s.mu.Unlock()
	if stale {
		s.syncManagedInventory()
	}
}

// syncManagedInventory creates disabled-by-default hosted rows for weights the
// artifact manager found on disk. This keeps the sharing screen complete even
// when a model was downloaded by an earlier session, while Published remains
// an explicit sharing opt-in.
func (s *Server) syncManagedInventory() {
	if s.artifactMgr == nil {
		return
	}
	// Stamp the clock here too, so that a direct call from the sharing screen
	// also satisfies the next throttled refresh.
	s.mu.Lock()
	s.lastInventorySync = time.Now()
	s.mu.Unlock()
	artifacts, err := s.artifactMgr.ListArtifacts()
	if err != nil {
		return
	}
	seen := make(map[string]hosting.ArtifactManifest, len(artifacts))
	seenPath := make(map[string]bool, len(artifacts))
	for _, a := range artifacts {
		seen[a.ID] = a
		seenPath[a.Path] = true
	}
	models, err := s.store.ListHostedModels()
	if err != nil {
		return
	}
	byID := make(map[string]store.HostedModelRecord, len(models))
	byPath := make(map[string]store.HostedModelRecord, len(models))
	for _, m := range models {
		byID[m.ID] = m
		if m.ModelType == "managed" && m.Filename != "" {
			byPath[m.Filename] = m
		}
	}
	for id, a := range seen {
		if existing, ok := byID[id]; ok {
			if existing.ModelType != "managed" {
				continue
			}
			changed := false
			if existing.Filename == "" {
				existing.Filename, existing.ContextLimit = a.Path, a.ContextLimit
				if existing.ContextLimit <= 0 {
					existing.ContextLimit = 4096
				}
				if existing.Name == "" {
					existing.Name = managedDisplayName(a.Name)
				}
				if existing.BackendModel == "" {
					existing.BackendModel = existing.Name
				}
				changed = true
			}
			// Whether a model reasons is read from its weights, which an older
			// row predates and a re-quantized file can change.
			if existing.SupportsThinking != a.SupportsThinking {
				existing.SupportsThinking = a.SupportsThinking
				changed = true
			}
			// An earlier scan named rows after the file, suffix and all.
			// Correct that in place, but only while the stored name is still
			// exactly what the scanner produced, so a name the owner chose is
			// never overwritten.
			if trimmed := managedDisplayName(a.Name); existing.Name == a.Name && trimmed != a.Name {
				if existing.BackendModel == existing.Name {
					existing.BackendModel = trimmed
				}
				existing.Name = trimmed
				existing.Revision++
				changed = true
			}
			if changed {
				_ = s.store.SaveHostedModel(existing)
			}
			continue
		}
		// A prepared row may use an owner-chosen model ID. Reuse that row when
		// the inventory scanner later derives its art-* ID for the same path.
		if _, ok := byPath[a.Path]; ok {
			continue
		}
		contextLimit := a.ContextLimit
		if contextLimit <= 0 {
			contextLimit = 4096
		}
		maxTokens := contextLimit / 2
		if maxTokens < 2048 && contextLimit >= 2048 {
			maxTokens = 2048
		}
		if maxTokens > 4096 {
			maxTokens = 4096
		}
		if maxTokens <= 0 {
			maxTokens = 1024
		}
		displayName := managedDisplayName(a.Name)
		_ = s.store.SaveHostedModel(store.HostedModelRecord{ID: id, Name: displayName,
			ModelType: "managed", EndpointURL: managedEndpointSentinel, BackendModel: displayName,
			ContextLimit: contextLimit, MaxTokens: maxTokens, Enabled: true, Published: false,
			Revision: 1, Filename: a.Path, Projector: companionPath(a, hosting.CompanionProjector),
			DraftModel: companionPath(a, hosting.CompanionDraft), SupportsThinking: a.SupportsThinking})
	}
	// Rows whose source file disappeared are no longer loadable. Disable them
	// for future requests but leave the record for the owner to inspect.
	for _, m := range models {
		if m.ModelType == "managed" && m.Filename != "" && s.artifactMgr != nil {
			if !seenPath[m.Filename] && m.Enabled {
				m.Enabled = false
				m.Revision++
				_ = s.store.SaveHostedModel(m)
			}
		}
	}
}

func companionPath(a hosting.ArtifactManifest, kind hosting.CompanionKind) string {
	for _, c := range a.Companions {
		if c.Kind == kind {
			return c.Path
		}
	}
	return ""
}

func (s *Server) handleSaveHostedModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID                  string `json:"id"`
		Name                string `json:"name"`
		ModelType           string `json:"model_type"`
		EndpointURL         string `json:"endpoint_url"`
		APIKey              string `json:"api_key"`
		BackendModel        string `json:"backend_model"`
		ContextLimit        int    `json:"context_limit"`
		MaxTokens           int    `json:"max_tokens"`
		Enabled             *bool  `json:"enabled"`
		Published           bool   `json:"published"`
		AllowPrivateNetwork bool   `json:"allow_private_network"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(body.Name) == "" {
		http.Error(w, "model name is required", http.StatusBadRequest)
		return
	}

	if body.ModelType == "" {
		body.ModelType = "external"
	}
	id := strings.TrimSpace(body.ID)
	var existing *store.HostedModelRecord
	if id != "" {
		existing, _ = s.store.GetHostedModel(id)
	}
	if body.ModelType == "managed" {
		// Managed rows can only be created through /prepare or /load, where
		// the runner arguments are validated and persisted. Generic settings
		// edits may change sharing flags, but must retain that configuration.
		if existing == nil || existing.ModelType != "managed" || existing.Filename == "" {
			http.Error(w, "managed models must be prepared before they can be edited", http.StatusBadRequest)
			return
		}
		body.EndpointURL = existing.EndpointURL
		body.APIKey = existing.APIKey
		body.AllowPrivateNetwork = existing.AllowPrivateNetwork
	}

	// Validate endpoint destination to protect against SSRF and insecure
	// off-machine HTTP. The authoritative check runs again at dial time
	// against the addresses the name actually resolves to.
	if body.ModelType != "managed" {
		policy := hosting.DestinationPolicy{AllowPrivateNetwork: body.AllowPrivateNetwork}
		if err := hosting.ValidateDestinationWithPolicy(body.EndpointURL, policy); err != nil {
			http.Error(w, fmt.Sprintf("invalid endpoint destination: %v", err), http.StatusBadRequest)
			return
		}
	}

	revision := 1
	if id == "" {
		randBytes := make([]byte, 8)
		_, _ = rand.Read(randBytes)
		id = "model-" + hex.EncodeToString(randBytes)
	} else if existing != nil {
		revision = existing.Revision + 1
	}

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

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	} else if existing != nil {
		enabled = existing.Enabled
	}

	// BackendModel defaults to the display name, which is what a discovery
	// call returns and what a multi-model backend expects to be sent.
	backendModel := strings.TrimSpace(body.BackendModel)
	if backendModel == "" {
		backendModel = strings.TrimSpace(body.Name)
	}

	rec := store.HostedModelRecord{
		ID:                  id,
		Name:                strings.TrimSpace(body.Name),
		ModelType:           body.ModelType,
		EndpointURL:         strings.TrimSpace(body.EndpointURL),
		APIKey:              body.APIKey,
		BackendModel:        backendModel,
		ContextLimit:        contextLimit,
		MaxTokens:           maxTokens,
		Enabled:             enabled,
		Published:           body.Published,
		Revision:            revision,
		AllowPrivateNetwork: body.AllowPrivateNetwork,
	}
	if existing != nil && existing.ModelType == "managed" {
		if body.ContextLimit <= 0 {
			rec.ContextLimit = existing.ContextLimit
		}
		if body.MaxTokens <= 0 {
			rec.MaxTokens = existing.MaxTokens
		}
		rec.Filename = existing.Filename
		rec.Threads = existing.Threads
		rec.GPULayers = existing.GPULayers
		rec.Projector = existing.Projector
		rec.DraftModel = existing.DraftModel
		rec.ExtraArgs = append([]string(nil), existing.ExtraArgs...)
		rec.SupportsThinking = existing.SupportsThinking
	}

	if err := s.store.SaveHostedModel(rec); err != nil {
		http.Error(w, "failed to save hosted model", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
	s.advertiseLocalModels()
}

func (s *Server) handleDeleteHostedModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "model id required", http.StatusBadRequest)
		return
	}

	if err := s.store.DeleteHostedModel(id); err != nil {
		http.Error(w, "failed to delete hosted model", http.StatusInternalServerError)
		return
	}
	s.advertiseLocalModels()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleTestHostedModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID                  string `json:"id"`
		EndpointURL         string `json:"endpoint_url"`
		APIKey              string `json:"api_key"`
		AllowPrivateNetwork bool   `json:"allow_private_network"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	endpointURL := strings.TrimSpace(body.EndpointURL)
	apiKey := strings.TrimSpace(body.APIKey)
	policy := hosting.DestinationPolicy{AllowPrivateNetwork: body.AllowPrivateNetwork}

	// If ID provided, load from store
	if body.ID != "" && endpointURL == "" {
		rec, err := s.store.GetHostedModel(body.ID)
		if err != nil {
			http.Error(w, "model not found", http.StatusNotFound)
			return
		}
		endpointURL = rec.EndpointURL
		apiKey = rec.APIKey
		policy.AllowPrivateNetwork = rec.AllowPrivateNetwork
	}

	if endpointURL == "" {
		http.Error(w, "endpoint_url is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	latency, models, err := s.adapter.TestEndpoint(ctx, endpointURL, apiKey, policy)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":         false,
			"error":      err.Error(),
			"latency_ms": latency,
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":         true,
		"latency_ms": latency,
		"models":     models,
	})
}

func (s *Server) handleDiscoverHostedModels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EndpointURL         string `json:"endpoint_url"`
		APIKey              string `json:"api_key"`
		AllowPrivateNetwork bool   `json:"allow_private_network"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	endpointURL := strings.TrimSpace(body.EndpointURL)
	if endpointURL == "" {
		http.Error(w, "endpoint_url is required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	models, err := s.adapter.QueryModels(ctx, endpointURL, strings.TrimSpace(body.APIKey),
		hosting.DestinationPolicy{AllowPrivateNetwork: body.AllowPrivateNetwork})
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"models": models,
	})
}

func (s *Server) handleGetHostLimits(w http.ResponseWriter, r *http.Request) {
	limits, err := s.store.GetHostLimits()
	if err != nil {
		http.Error(w, "failed to get host limits", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(limits)
}

func (s *Server) handleSaveHostLimits(w http.ResponseWriter, r *http.Request) {
	var body store.HostLimitsRecord
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if body.MaxActive <= 0 {
		body.MaxActive = 1
	}
	if body.MaxQueuedPerMember <= 0 {
		body.MaxQueuedPerMember = 1
	}
	if body.MaxQueuedTotal <= 0 {
		body.MaxQueuedTotal = 10
	}
	if body.QueueTimeoutSeconds <= 0 {
		body.QueueTimeoutSeconds = 300
	}
	if body.ExecutionTimeoutSeconds <= 0 {
		body.ExecutionTimeoutSeconds = 600
	}
	// Zero and negative are both meaningful here — release straight away, and
	// never release — so only a value below "never" is a mistake to correct.
	if body.IdleUnloadSeconds < -1 {
		body.IdleUnloadSeconds = store.DefaultIdleUnloadSeconds
	}

	if err := s.store.SaveHostLimits(body); err != nil {
		http.Error(w, "failed to save host limits", http.StatusInternalServerError)
		return
	}
	// The shared queue picks up the new limits immediately; otherwise an
	// edited limit would only take effect on the next restart.
	s.infer.ReloadLimits()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

type CatalogItem struct {
	catalog.ModelAd
	HostDisplayName string `json:"host_display_name"`
}

// catalogSyncInterval bounds how often a catalog read refreshes membership.
// Short enough that a member who joined moments ago is found on the next poll,
// long enough that polling does not turn into a peer sync loop.
const catalogSyncInterval = 20 * time.Second

func (s *Server) handleGetCatalog(w http.ResponseWriter, r *http.Request) {
	// Discovery needs the current roster and address hints before choosing
	// hosts to query. Otherwise a member that joined after this client is
	// invisible until the background membership timer happens to run.
	//
	// Throttled, because this is a read path the UI polls: syncing on every
	// request meant peer network I/O several times a minute per open tab, any
	// one of which could stall the response for the full timeout.
	s.mu.Lock()
	staleCatalog := time.Since(s.lastCatalogSync) >= catalogSyncInterval
	if staleCatalog {
		s.lastCatalogSync = time.Now()
	}
	s.mu.Unlock()
	if staleCatalog {
		syncCtx, cancelSync := context.WithTimeout(r.Context(), 5*time.Second)
		s.SyncMembership(syncCtx)
		cancelSync()
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]CatalogItem{})
		return
	}

	device, _ := s.store.GetDeviceIdentity()
	var myMemberID string
	if device != nil {
		myMemberID, _ = peerapi.MemberIDForDevicePublic(device.DevicePublic)
	}

	// 1. Ingest local published models
	s.advertiseLocalModels()

	// 2. Query all known peer addresses for their catalogs over authenticated
	//    connections, and accept an advertisement only from the member whose
	//    identity the handshake proved.
	for _, pa := range s.knownPeers() {
		if pa.MemberID == myMemberID {
			continue
		}

		dialCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		var ads []catalog.ModelAd
		err := s.peerJSON(dialCtx, pa.MemberID, pa.TailcatAddr, "GET", "/peer/v1/catalog", nil, &ads)
		cancel()
		if err != nil {
			continue
		}

		for _, ad := range ads {
			if ad.HostMemberID != pa.MemberID {
				continue // a peer may only advertise its own models
			}
			m, mErr := s.store.GetMember(ad.HostMemberID)
			if mErr != nil || m == nil || m.Status != string(room.StatusAdmitted) {
				continue
			}
			pubBytes, dErr := identity.DecodeToken(m.DevicePublic, ed25519.PublicKeySize)
			if dErr != nil {
				continue
			}
			_ = s.catalog.Upsert(ad, ed25519.PublicKey(pubBytes))
		}
	}

	// 3. Return all currently available ads (filtering out stale / offline)
	available := s.catalog.ListAvailable()
	items := make([]CatalogItem, 0, len(available))
	for _, ad := range available {
		displayName := "Peer"
		if ad.HostMemberID == myMemberID {
			name, _ := s.store.GetSetting("display_name")
			if name != "" {
				displayName = name
			} else {
				displayName = "You"
			}
		} else if m, err := s.store.GetMember(ad.HostMemberID); err == nil && m.DisplayName != "" {
			displayName = m.DisplayName
		}
		items = append(items, CatalogItem{
			ModelAd:         ad,
			HostDisplayName: displayName,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

// advertiseLocalModels re-signs an advertisement for every published local
// model.
//
// Advertisements carry a timestamp and expire after
// catalog.StaleAdTimeoutSeconds, which is what lets a node notice a peer that
// went away. A node's own models are different: nothing about their
// availability depends on the network, and this node is the authority on them.
// Refreshing them only inside the catalog handler tied their availability to a
// browser polling the dashboard — leave the chat page open for ninety seconds
// and sending a message answered "selected model is offline or host is
// unreachable" for a model the runner was still serving. The refresh therefore
// runs on a timer as well; see catalogLoop.
func (s *Server) advertiseLocalModels() {
	if s.catalog == nil {
		return
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		// Outside a room there is no one to advertise to.
		return
	}
	device, err := s.store.GetDeviceIdentity()
	if err != nil || device == nil {
		return
	}
	myMemberID, err := peerapi.MemberIDForDevicePublic(device.DevicePublic)
	if err != nil {
		return
	}
	pubBytes, err := identity.DecodeToken(device.DevicePublic, ed25519.PublicKeySize)
	if err != nil || len(device.DevicePrivate) != ed25519.PrivateKeySize {
		return
	}
	myPubKey := ed25519.PublicKey(pubBytes)
	myPrivKey := ed25519.PrivateKey(device.DevicePrivate)
	advertised := make(map[string]bool)

	s.syncManagedInventoryThrottled()
	localModels, err := s.store.ListHostedModels()
	if err != nil {
		return
	}

	// A managed model is advertised as unloaded while its downloaded weights are
	// available and the runner can accept a load. Only the model currently held
	// by the runner is ready; an unreachable runner is offline.
	loadedModelID := ""
	runnerAvailable := false
	if s.runnerClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.infer.SetRunner(s.runnerClient)
		if health, err := s.infer.RunnerHealth(ctx); err == nil {
			// Answering at all is what makes a runner available. Even an error
			// status is a reachable runner whose last engine attempt failed,
			// and downloaded weights remain loadable on the next request.
			runnerAvailable = true
			loadedModelID = health.LoadedModelID
		}
		cancel()
	}

	for _, m := range localModels {
		if !m.Enabled || !m.Published {
			continue
		}
		advertised[m.ID] = true
		managed := m.ModelType == "managed"
		availability := "ready"
		if managed {
			switch {
			case m.Filename == "":
				// Legacy rows created before load arguments were persisted cannot
				// be demand-loaded safely after restart.
				availability = "offline"
			case !runnerAvailable:
				availability = "offline"
			case m.ID == loadedModelID:
				availability = "ready"
			default:
				availability = "unloaded"
			}
		}
		ad := catalog.ModelAd{
			SupportsThinking: m.SupportsThinking,
			RoomID:           roomRec.RoomID,
			HostMemberID:     myMemberID,
			ModelID:          m.ID,
			Revision:         m.Revision,
			Name:             m.Name,
			ContextLimit:     m.ContextLimit,
			Availability:     availability,
			QueueEstimate:    0,
			IsManaged:        managed,
		}
		if err := ad.Sign(myPrivKey); err != nil {
			continue
		}
		_ = s.catalog.Upsert(ad, myPubKey)
	}
	for _, old := range s.catalog.List() {
		if old.HostMemberID != myMemberID || advertised[old.ModelID] || old.Availability == "offline" {
			continue
		}
		old.Availability = "offline"
		old.Revision++
		if old.Sign(myPrivKey) == nil {
			_ = s.catalog.Upsert(old, myPubKey)
		}
	}
}
