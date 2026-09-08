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

	"github.com/cbaack/woolwire/internal/catalog"
	"github.com/cbaack/woolwire/internal/hosting"
	"github.com/cbaack/woolwire/internal/identity"
	"github.com/cbaack/woolwire/internal/peerapi"
	"github.com/cbaack/woolwire/internal/room"
	"github.com/cbaack/woolwire/internal/store"
)

func (s *Server) handleListHostedModels(w http.ResponseWriter, r *http.Request) {
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

	// Validate endpoint destination to protect against SSRF and insecure
	// off-machine HTTP. The authoritative check runs again at dial time
	// against the addresses the name actually resolves to.
	policy := hosting.DestinationPolicy{AllowPrivateNetwork: body.AllowPrivateNetwork}
	if err := hosting.ValidateDestinationWithPolicy(body.EndpointURL, policy); err != nil {
		http.Error(w, fmt.Sprintf("invalid endpoint destination: %v", err), http.StatusBadRequest)
		return
	}

	id := strings.TrimSpace(body.ID)
	revision := 1
	if id == "" {
		randBytes := make([]byte, 8)
		_, _ = rand.Read(randBytes)
		id = "model-" + hex.EncodeToString(randBytes)
	} else {
		if existing, err := s.store.GetHostedModel(id); err == nil && existing != nil {
			revision = existing.Revision + 1
		}
	}

	contextLimit := body.ContextLimit
	if contextLimit <= 0 {
		contextLimit = 4096
	}
	maxTokens := body.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
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

	if err := s.store.SaveHostedModel(rec); err != nil {
		http.Error(w, "failed to save hosted model", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
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

func (s *Server) handleGetCatalog(w http.ResponseWriter, r *http.Request) {
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

	localModels, err := s.store.ListHostedModels()
	if err != nil {
		return
	}

	// A managed model is only being served while the runner actually holds its
	// weights. The hosted_models row survives a runner restart, so without
	// asking, a refresh would keep advertising a model the runner dropped —
	// and the refresh now runs on a timer, so it would say so indefinitely.
	loadedModelID := ""
	if s.runnerClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if health, err := s.runnerClient.Health(ctx); err == nil {
			loadedModelID = health.LoadedModelID
		}
		cancel()
	}

	for _, m := range localModels {
		if !m.Enabled || !m.Published {
			continue
		}
		managed := m.ModelType == "managed"
		availability := "ready"
		if managed && m.ID != loadedModelID {
			availability = "offline"
		}
		ad := catalog.ModelAd{
			RoomID:        roomRec.RoomID,
			HostMemberID:  myMemberID,
			ModelID:       m.ID,
			Revision:      m.Revision,
			Name:          m.Name,
			ContextLimit:  m.ContextLimit,
			Availability:  availability,
			QueueEstimate: 0,
			IsManaged:     managed,
		}
		if err := ad.Sign(myPrivKey); err != nil {
			continue
		}
		_ = s.catalog.Upsert(ad, myPubKey)
	}
}
