package localapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Chrisbaack/woolwire/internal/catalog"
	"github.com/Chrisbaack/woolwire/internal/hosting"
	"github.com/Chrisbaack/woolwire/internal/identity"
	"github.com/Chrisbaack/woolwire/internal/inference"
	"github.com/Chrisbaack/woolwire/internal/peerapi"
	"github.com/Chrisbaack/woolwire/internal/peerauth"
	"github.com/Chrisbaack/woolwire/internal/room"
	"github.com/Chrisbaack/woolwire/internal/store"
	"github.com/Chrisbaack/woolwire/internal/transport"
)

// csrfHeader is the header the SPA sets on every state-changing request. A
// cross-origin page cannot set it without a preflight, and the preflight is
// refused, so it closes the hole a SameSite=Lax cookie leaves open between
// two ports on 127.0.0.1 — which the browser considers same-site.
const csrfHeader = "X-Woolwire-Request"

func bootstrapPortOffset() uint16 { return peerapi.BootstrapPortOffset }

type Config struct {
	Store        *store.Store
	Transport    transport.Transport
	SetupToken   string
	PeerPort     uint16
	WebAssets    http.FileSystem
	StartPeerFn  func(authority ed25519.PrivateKey) error
	StopPeerFn   func() error
	ModelsDir    string
	StateDir     string
	RunnerURL    string
	RunnerToken  string
	AllowedHosts []string
}

type Server struct {
	store        *store.Store
	trans        transport.Transport
	sessionKey   string
	peerPort     uint16
	webAssets    http.FileSystem
	startPeerFn  func(authority ed25519.PrivateKey) error
	stopPeerFn   func() error
	catalog      *catalog.Catalog
	adapter      *hosting.ExternalAdapter
	artifactMgr  *hosting.ArtifactManager
	runnerClient *hosting.RunnerClient
	infer        *inference.Service
	roster       *peerauth.Roster
	deviceCert   tls.Certificate
	allowedHosts []string
	stateDir     string
	modelsDir    string

	// setupHash is the hash of the current one-time setup secret. setupUsed
	// records that it has been redeemed; a redeemed secret never works again.
	setupHash [32]byte
	setupUsed bool
	// setupAttempts drives per-IP exponential backoff on the setup route,
	// which is otherwise a brute-force target on a LAN-exposed bind address.
	setupAttempts map[string]*attemptRecord

	// noSaveTurns holds the running transcript of privacy-mode conversations.
	// The architecture already keeps queued prompt bodies in memory; keeping
	// the conversation there too is what lets no-save chats stay multi-turn
	// without ever touching the messages table.
	noSaveTurns map[string][]hosting.ChatMessage

	mux    *http.ServeMux
	server *http.Server
	mu     sync.RWMutex

	bgCancel context.CancelFunc
	bgWG     sync.WaitGroup
}

type attemptRecord struct {
	failures  int
	nextAllow time.Time
}

// The models directory holds multi-gigabyte weights, so the ceiling on what
// Woolwire will download into it is a setting rather than a constant.
const (
	modelBudgetSetting        = "models_storage_budget_bytes"
	defaultModelStorageBudget = 50 * (1 << 30) // 50 GB
)

func NewServer(cfg Config) (*Server, error) {
	if cfg.Store == nil || cfg.Transport == nil {
		return nil, errors.New("store and transport are required")
	}

	sessionKey, err := cfg.Store.GetSetting("session_token")
	if errors.Is(err, store.ErrNotFound) || sessionKey == "" {
		sessionBytes := make([]byte, 32)
		if _, err := rand.Read(sessionBytes); err != nil {
			return nil, err
		}
		sessionKey = hex.EncodeToString(sessionBytes)
		_ = cfg.Store.SetSetting("session_token", sessionKey)
	}

	var artMgr *hosting.ArtifactManager
	if cfg.ModelsDir != "" {
		// The storage budget is a setting rather than a constant: how much
		// disk a node is willing to give models is the node operator's call,
		// and it is changed from the Settings page.
		budget := int64(defaultModelStorageBudget)
		if raw, err := cfg.Store.GetSetting(modelBudgetSetting); err == nil {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
				budget = parsed
			}
		}
		artMgr, _ = hosting.NewArtifactManager(cfg.ModelsDir, budget)
	}
	var runnerCli *hosting.RunnerClient
	if cfg.RunnerURL != "" {
		runnerCli = hosting.NewRunnerClient(cfg.RunnerURL, cfg.RunnerToken)
	}

	adapter := hosting.NewExternalAdapter()

	var deviceCert tls.Certificate
	if dev, err := cfg.Store.GetDeviceIdentity(); err == nil && dev != nil {
		deviceCert = tls.Certificate{
			Certificate: [][]byte{dev.DeviceCertDER},
			PrivateKey:  ed25519.PrivateKey(dev.DevicePrivate),
		}
	}

	used, _ := cfg.Store.GetSetting("setup_token_used")

	s := &Server{
		store:         cfg.Store,
		trans:         cfg.Transport,
		setupHash:     sha256.Sum256([]byte(cfg.SetupToken)),
		setupUsed:     used == "true",
		setupAttempts: make(map[string]*attemptRecord),
		noSaveTurns:   make(map[string][]hosting.ChatMessage),
		sessionKey:    sessionKey,
		peerPort:      cfg.PeerPort,
		webAssets:     cfg.WebAssets,
		startPeerFn:   cfg.StartPeerFn,
		stopPeerFn:    cfg.StopPeerFn,
		catalog:       catalog.NewCatalog(),
		adapter:       adapter,
		artifactMgr:   artMgr,
		runnerClient:  runnerCli,
		infer:         inference.NewService(cfg.Store, adapter, runnerCli),
		roster:        peerauth.NewRoster(cfg.Store),
		deviceCert:    deviceCert,
		allowedHosts:  cfg.AllowedHosts,
		stateDir:      cfg.StateDir,
		modelsDir:     cfg.ModelsDir,
		mux:           http.NewServeMux(),
	}

	// The OpenAI-compatible routes are always authenticated. The token is
	// generated on first boot rather than left unset, because "unset means
	// open" made a browser-reachable endpoint that drives peers' hardware.
	if _, err := s.localAPIToken(); err != nil {
		return nil, err
	}

	s.routes()
	s.server = &http.Server{
		Handler:     s.securityMiddleware(s.mux),
		ReadTimeout: 15 * time.Second,
		// WriteTimeout stays 0: chat SSE, OpenAI streaming, and artifact
		// downloads all run past any fixed ceiling. Streaming handlers set
		// their own per-write deadlines through http.ResponseController.
		WriteTimeout:      0,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	return s, nil
}

// Inference exposes the shared queue so the peer server can be constructed
// against the same admission limits the local routes use.
func (s *Server) Inference() *inference.Service { return s.infer }

// DeviceCertificate is this node's device certificate.
func (s *Server) DeviceCertificate() tls.Certificate { return s.deviceCert }

func cleanHost(host string) string {
	h, _, err := net.SplitHostPort(host)
	if err == nil {
		return h
	}
	return strings.Trim(host, "[]")
}

func (s *Server) isAllowedHost(host string) bool {
	// Loopback / localhost
	if host == "127.0.0.1" || host == "localhost" || host == "::1" {
		return true
	}

	// Validate against IP literals (loopback, RFC 1918 private IPv4, RFC 4193 private IPv6, link-local)
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return true
		}
	}

	// Explicitly allowed hostnames (e.g. mDNS woolwire.local, home domain, or wildcard)
	for _, allowed := range s.allowedHosts {
		if allowed == "*" || strings.EqualFold(allowed, host) {
			return true
		}
	}

	return false
}

// isJSONRoute reports whether a path expects a JSON body. Enforcing the
// content type on these routes blocks the simple cross-origin POST — a
// text/plain form submission needs no preflight and would otherwise reach a
// handler that never looked at Content-Type.
func isJSONRoute(path string) bool {
	return strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/v1/")
}

func (s *Server) checkCSRF(r *http.Request) error {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return nil
	}

	// Either an Origin matching this very host, or the header the SPA always
	// sets. Both are unforgeable from a cross-origin page without a preflight.
	if r.Header.Get(csrfHeader) == "" {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return errors.New("missing Origin header on state-changing request")
		}
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return errors.New("unparsable Origin header")
		}
		if !strings.EqualFold(u.Host, r.Host) {
			return fmt.Errorf("origin %q does not match host %q", u.Host, r.Host)
		}
	}

	if isJSONRoute(r.URL.Path) && r.ContentLength != 0 {
		mediaType := r.Header.Get("Content-Type")
		if idx := strings.IndexByte(mediaType, ';'); idx >= 0 {
			mediaType = mediaType[:idx]
		}
		if !strings.EqualFold(strings.TrimSpace(mediaType), "application/json") {
			return fmt.Errorf("unsupported content type %q", mediaType)
		}
	}

	return nil
}

func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := cleanHost(r.Host)
		if !s.isAllowedHost(host) {
			http.Error(w, "invalid host header", http.StatusForbidden)
			return
		}

		if err := s.checkCSRF(r); err != nil {
			http.Error(w, "cross-site request refused: "+err.Error(), http.StatusForbidden)
			return
		}

		// Security headers
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; form-action 'none'; frame-ancestors 'none'")

		next.ServeHTTP(w, r)
	})
}

func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("woolwire_session")
		if err != nil || cookie.Value == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		s.mu.RLock()
		valid := subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(s.sessionKey)) == 1
		s.mu.RUnlock()

		if !valid {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}

func (s *Server) creatorMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		roomRec, err := s.store.GetRoomState()
		if err != nil || roomRec.Role != "creator" || len(roomRec.AuthorityPrivate) == 0 {
			http.Error(w, "forbidden: creator authority required", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

func (s *Server) routes() {
	// Public setup
	s.mux.HandleFunc("POST /api/v1/setup", s.handleSetup)

	// Authenticated setup info & secret reset
	s.mux.HandleFunc("GET /api/v1/setup/info", s.authMiddleware(s.handleGetSetupInfo))
	s.mux.HandleFunc("POST /api/v1/setup/token", s.authMiddleware(s.handleUpdateSetupToken))

	// Authenticated local routes
	s.mux.HandleFunc("GET /api/v1/state", s.authMiddleware(s.handleGetState))
	s.mux.HandleFunc("POST /api/v1/profile", s.authMiddleware(s.handleUpdateProfile))
	s.mux.HandleFunc("POST /api/v1/room/host", s.authMiddleware(s.handleHostRoom))
	s.mux.HandleFunc("POST /api/v1/room/join", s.authMiddleware(s.handleJoinRoom))
	s.mux.HandleFunc("POST /api/v1/room/leave", s.authMiddleware(s.handleLeaveRoom))

	// Hosted Models & Limits
	s.mux.HandleFunc("GET /api/v1/hosted-models", s.authMiddleware(s.handleListHostedModels))
	s.mux.HandleFunc("POST /api/v1/hosted-models", s.authMiddleware(s.handleSaveHostedModel))
	s.mux.HandleFunc("POST /api/v1/hosted-models/test", s.authMiddleware(s.handleTestHostedModel))
	s.mux.HandleFunc("POST /api/v1/hosted-models/discover", s.authMiddleware(s.handleDiscoverHostedModels))
	s.mux.HandleFunc("DELETE /api/v1/hosted-models/{id}", s.authMiddleware(s.handleDeleteHostedModel))
	s.mux.HandleFunc("GET /api/v1/host-limits", s.authMiddleware(s.handleGetHostLimits))
	s.mux.HandleFunc("POST /api/v1/host-limits", s.authMiddleware(s.handleSaveHostLimits))

	// Local API token for the OpenAI-compatible routes
	s.mux.HandleFunc("GET /api/v1/local-api-token", s.authMiddleware(s.handleGetLocalAPIToken))
	s.mux.HandleFunc("POST /api/v1/local-api-token/regenerate", s.authMiddleware(s.handleRegenerateLocalAPIToken))

	// Hardware & Managed Models
	s.mux.HandleFunc("GET /api/v1/hardware", s.authMiddleware(s.handleGetHardware))
	s.mux.HandleFunc("GET /api/v1/managed-models/artifacts", s.authMiddleware(s.handleListArtifacts))
	s.mux.HandleFunc("GET /api/v1/managed-models/storage", s.authMiddleware(s.handleArtifactStorage))
	s.mux.HandleFunc("POST /api/v1/managed-models/storage", s.authMiddleware(s.handleSetArtifactStorage))
	s.mux.HandleFunc("POST /api/v1/managed-models/download", s.authMiddleware(s.handleDownloadArtifact))
	s.mux.HandleFunc("GET /api/v1/managed-models/downloads", s.authMiddleware(s.handleDownloadStatus))
	s.mux.HandleFunc("GET /api/v1/managed-models/huggingface", s.authMiddleware(s.handleResolveHuggingFaceRepo))
	s.mux.HandleFunc("POST /api/v1/managed-models/downloads/{id}/cancel", s.authMiddleware(s.handleCancelDownload))
	s.mux.HandleFunc("DELETE /api/v1/managed-models/artifacts/{filename...}", s.authMiddleware(s.handleDeleteArtifact))
	s.mux.HandleFunc("GET /api/v1/managed-models/runner-health", s.authMiddleware(s.handleRunnerHealth))
	s.mux.HandleFunc("POST /api/v1/managed-models/load", s.authMiddleware(s.handleLoadManagedModel))
	s.mux.HandleFunc("POST /api/v1/managed-models/unload", s.authMiddleware(s.handleUnloadManagedModel))

	// Catalog
	s.mux.HandleFunc("GET /api/v1/catalog", s.authMiddleware(s.handleGetCatalog))

	// Chats ("My chats")
	s.mux.HandleFunc("GET /api/v1/chats", s.authMiddleware(s.handleListChats))
	s.mux.HandleFunc("POST /api/v1/chats", s.authMiddleware(s.handleCreateChat))
	s.mux.HandleFunc("GET /api/v1/chats/{id}", s.authMiddleware(s.handleGetChat))
	s.mux.HandleFunc("DELETE /api/v1/chats/{id}", s.authMiddleware(s.handleDeleteChat))
	s.mux.HandleFunc("POST /api/v1/chats/{id}/message", s.authMiddleware(s.handleSendMessage))
	s.mux.HandleFunc("POST /api/v1/chats/{id}/cancel", s.authMiddleware(s.handleCancelMessage))
	s.mux.HandleFunc("POST /api/v1/chats/{id}/messages/{msgID}/regenerate", s.authMiddleware(s.handleRegenerateMessage))
	s.mux.HandleFunc("POST /api/v1/chats/{id}/messages/{msgID}/edit", s.authMiddleware(s.handleEditChatMessage))
	s.mux.HandleFunc("POST /api/v1/chats/{id}/messages/{msgID}/select", s.authMiddleware(s.handleSelectVariant))

	// Community
	s.mux.HandleFunc("GET /api/v1/community/channels", s.authMiddleware(s.handleListChannels))
	s.mux.HandleFunc("POST /api/v1/community/channels", s.authMiddleware(s.handleCreateChannel))
	s.mux.HandleFunc("GET /api/v1/community/channels/{id}/messages", s.authMiddleware(s.handleGetChannelMessages))
	s.mux.HandleFunc("POST /api/v1/community/channels/{id}/messages", s.authMiddleware(s.handlePostMessage))
	s.mux.HandleFunc("PUT /api/v1/community/messages/{id}", s.authMiddleware(s.handleEditMessage))
	s.mux.HandleFunc("DELETE /api/v1/community/messages/{id}", s.authMiddleware(s.handleDeleteMessage))
	s.mux.HandleFunc("POST /api/v1/community/messages/{id}/moderate", s.creatorMiddleware(s.handleModerateMessage))
	s.mux.HandleFunc("POST /api/v1/community/channels/{id}/read", s.authMiddleware(s.handleUpdateReadState))
	s.mux.HandleFunc("POST /api/v1/community/sync", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		s.SyncCommunityEvents(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))

	// Contributions & Social Recognition
	s.mux.HandleFunc("GET /api/v1/contributions/leaderboard", s.authMiddleware(s.handleGetLeaderboard))
	s.mux.HandleFunc("GET /api/v1/contributions/settings", s.authMiddleware(s.handleGetContributionsSettings))
	s.mux.HandleFunc("POST /api/v1/contributions/settings", s.authMiddleware(s.handleSaveContributionsSettings))
	s.mux.HandleFunc("POST /api/v1/contributions/sync", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		s.SyncContributions(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))

	// Local Metrics & Backup
	s.mux.HandleFunc("GET /api/v1/metrics", s.authMiddleware(s.handleGetMetrics))
	s.mux.HandleFunc("POST /api/v1/backup/export", s.authMiddleware(s.handleExportBackup))

	// Local OpenAI Compatibility
	s.mux.HandleFunc("GET /v1/models", s.handleOpenAIModels)
	s.mux.HandleFunc("POST /v1/chat/completions", s.handleOpenAIChatCompletions)

	// Creator-only room admin routes
	s.mux.HandleFunc("POST /api/v1/room-admin/invitation/rotate", s.creatorMiddleware(s.handleRotateInvitation))
	s.mux.HandleFunc("GET /api/v1/room-admin/members", s.creatorMiddleware(s.handleListMembers))
	s.mux.HandleFunc("POST /api/v1/room-admin/members/{id}/remove", s.creatorMiddleware(s.handleRemoveMember))
	s.mux.HandleFunc("GET /api/v1/room-admin/pending", s.creatorMiddleware(s.handleListPending))
	s.mux.HandleFunc("POST /api/v1/room-admin/members/{id}/approve", s.creatorMiddleware(s.handleApproveMember))
	s.mux.HandleFunc("POST /api/v1/room-admin/approval-mode", s.creatorMiddleware(s.handleSetApprovalMode))

	// Web UI assets fallback
	if s.webAssets != nil {
		fileServer := http.FileServer(s.webAssets)
		s.mux.Handle("/", fileServer)
	} else {
		s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>Woolwire</title></head><body><h1>Woolwire API Ready</h1></body></html>`))
		})
	}
}

// setupBackoff enforces exponential delay per client address. Ten failures
// push the next allowed attempt minutes out, which is what makes even a short
// custom PIN impractical to guess over the network.
func (s *Server) setupBackoff(remoteAddr string) time.Duration {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.setupAttempts[host]
	if !ok {
		return 0
	}
	if wait := time.Until(rec.nextAllow); wait > 0 {
		return wait
	}
	return 0
}

func (s *Server) recordSetupFailure(remoteAddr string) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.setupAttempts[host]
	if !ok {
		rec = &attemptRecord{}
		s.setupAttempts[host] = rec
	}
	rec.failures++

	// 0s, 0s, 1s, 2s, 4s ... capped at 5 minutes.
	if rec.failures <= 2 {
		return
	}
	backoff := time.Duration(1<<uint(min(rec.failures-3, 12))) * time.Second
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	rec.nextAllow = time.Now().Add(backoff)
}

func (s *Server) clearSetupFailures(remoteAddr string) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	s.mu.Lock()
	delete(s.setupAttempts, host)
	s.mu.Unlock()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if wait := s.setupBackoff(r.RemoteAddr); wait > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(wait.Seconds())+1))
		http.Error(w, "too many failed setup attempts; try again later", http.StatusTooManyRequests)
		return
	}

	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	alreadyUsed := s.setupUsed
	expected := s.setupHash
	s.mu.RUnlock()

	if alreadyUsed {
		http.Error(w, "setup secret has already been redeemed; reset it from a signed-in session", http.StatusGone)
		return
	}

	providedHash := sha256.Sum256([]byte(strings.TrimSpace(body.Token)))
	if subtle.ConstantTimeCompare(expected[:], providedHash[:]) != 1 {
		s.recordSetupFailure(r.RemoteAddr)
		http.Error(w, "invalid setup token", http.StatusUnauthorized)
		return
	}

	// A one-time secret is what the docs promise. Redeeming it here is what
	// stops it from being reusable forever by anyone who saw a boot log.
	s.mu.Lock()
	s.setupUsed = true
	s.mu.Unlock()
	_ = s.store.SetSetting("setup_token_used", "true")
	_ = s.store.SetSetting("setup_token", "")
	s.clearSetupFailures(r.RemoteAddr)

	http.SetCookie(w, &http.Cookie{
		Name:     "woolwire_session",
		Value:    s.sessionKey,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   365 * 24 * 3600, // 1 year persistence
	})

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleGetSetupInfo(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	used := s.setupUsed
	s.mu.RUnlock()

	// The secret itself is never returned. It is single-use, so echoing it to
	// an authenticated page would only create another copy to leak.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"redeemed": used,
		"active":   !used,
	})
}

// handleUpdateSetupToken mints a new one-time secret for pairing another
// device. With no body it generates a 128-bit secret and returns it once;
// with a custom secret it enforces a 12-character minimum.
func (s *Server) handleUpdateSetupToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&body)

	newToken := strings.TrimSpace(body.Token)
	if newToken == "" {
		generated, err := generateSecret()
		if err != nil {
			http.Error(w, "failed to generate setup secret", http.StatusInternalServerError)
			return
		}
		newToken = generated
	} else if len(newToken) < 12 {
		http.Error(w, "setup secret must be at least 12 characters", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.setupHash = sha256.Sum256([]byte(newToken))
	s.setupUsed = false
	s.setupAttempts = make(map[string]*attemptRecord)
	s.mu.Unlock()

	_ = s.store.SetSetting("setup_token", newToken)
	_ = s.store.SetSetting("setup_token_used", "false")

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":    true,
		"token": newToken,
	})
}

func generateSecret() (string, error) {
	b := make([]byte, 16) // 128 bits
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	displayName, _ := s.store.GetSetting("display_name")
	device, _ := s.store.GetDeviceIdentity()
	devicePublic := ""
	memberID := ""
	if device != nil {
		devicePublic = device.DevicePublic
		memberID, _ = peerapi.MemberIDForDevicePublic(device.DevicePublic)
	}

	roomRec, _ := s.store.GetRoomState()
	var roomData any
	if roomRec != nil {
		data := map[string]any{
			"room_id":        roomRec.RoomID,
			"room_name":      roomRec.RoomName,
			"role":           roomRec.Role,
			"approval_mode":  roomRec.ApprovalMode,
			"roster_version": roomRec.RosterVersion,
		}
		// Only the creator holds an invitation code. Members never store one:
		// possessing it would let a leaked member database admit new devices.
		if roomRec.Role == "creator" {
			data["invitation_code"] = roomRec.InvitationCode
		}
		roomData = data
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"configured":    displayName != "",
		"display_name":  displayName,
		"device_public": devicePublic,
		"member_id":     memberID,
		"room":          roomData,
	})
}

func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.DisplayName) == "" {
		http.Error(w, "invalid display name", http.StatusBadRequest)
		return
	}

	if err := s.store.SetSetting("display_name", strings.TrimSpace(body.DisplayName)); err != nil {
		http.Error(w, "failed to save profile", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleHostRoom(w http.ResponseWriter, r *http.Request) {
	// A second room row would leave GetRoomState choosing arbitrarily between
	// two identities, so hosting while already in a room is refused outright.
	if n, err := s.store.CountRoomStates(); err == nil && n > 0 {
		http.Error(w, "already in a room; leave it before hosting a new one", http.StatusConflict)
		return
	}

	var body struct {
		RoomName string `json:"room_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.RoomName) == "" {
		http.Error(w, "invalid room name", http.StatusBadRequest)
		return
	}

	// Membership.Verify refuses an empty display name, so a creator without
	// one would publish a roster record no peer could verify.
	displayName, _ := s.store.GetSetting("display_name")
	if strings.TrimSpace(displayName) == "" {
		http.Error(w, "set a display name before hosting a room", http.StatusBadRequest)
		return
	}

	device, err := s.store.GetDeviceIdentity()
	if err != nil || device == nil {
		http.Error(w, "device identity missing", http.StatusInternalServerError)
		return
	}
	creatorMemberID, err := peerapi.MemberIDForDevicePublic(device.DevicePublic)
	if err != nil {
		http.Error(w, "device identity is malformed", http.StatusInternalServerError)
		return
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		http.Error(w, "generate room authority failed", http.StatusInternalServerError)
		return
	}

	roomIDBytes := make([]byte, 16)
	_, _ = rand.Read(roomIDBytes)
	roomID := "room-" + identity.EncodeToken(roomIDBytes)

	bootstrapAddr := s.trans.Address()
	inv, err := room.NewInvitation(roomID, pub, bootstrapAddr)
	if err != nil {
		http.Error(w, "create invitation failed", http.StatusInternalServerError)
		return
	}
	code, err := inv.Encode()
	if err != nil {
		http.Error(w, "encode invitation failed", http.StatusInternalServerError)
		return
	}

	// Joiners pin this certificate from the invitation's authority key, which
	// is the only thing they can verify before they hold a roster.
	devicePubBytes, err := identity.DecodeToken(device.DevicePublic, ed25519.PublicKeySize)
	if err != nil {
		http.Error(w, "device public key is malformed", http.StatusInternalServerError)
		return
	}
	roomCertDER, err := peerauth.IssueRoomCertificate(priv, ed25519.PublicKey(devicePubBytes))
	if err != nil {
		http.Error(w, "issue room certificate failed", http.StatusInternalServerError)
		return
	}

	roomRec := store.RoomRecord{
		RoomID:           roomID,
		Role:             "creator",
		RoomName:         strings.TrimSpace(body.RoomName),
		AuthorityPublic:  identity.EncodeToken(pub),
		AuthorityPrivate: priv,
		BootstrapAddr:    bootstrapAddr,
		InvitationCode:   code,
		InvitationID:     inv.InvitationID,
		RosterVersion:    1,
		RoomCertDER:      roomCertDER,
	}

	if err := s.store.SaveRoomState(roomRec); err != nil {
		http.Error(w, "save room state failed", http.StatusInternalServerError)
		return
	}

	// Creator auto-enrolls itself as the first admitted member
	_ = s.store.SavePeerAddress(creatorMemberID, bootstrapAddr)
	m := room.Membership{
		MemberID:      creatorMemberID,
		RoomID:        roomID,
		DevicePublic:  device.DevicePublic,
		DisplayName:   strings.TrimSpace(displayName),
		Status:        room.StatusAdmitted,
		RosterVersion: 1,
	}
	_ = m.Sign(priv)
	saveMembership(s.store, m)

	if s.startPeerFn != nil {
		if err := s.startPeerFn(priv); err != nil {
			http.Error(w, fmt.Sprintf("start peer listener failed: %v", err), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"room_id":         roomID,
		"invitation_code": code,
	})
}

func (s *Server) handleJoinRoom(w http.ResponseWriter, r *http.Request) {
	if n, err := s.store.CountRoomStates(); err == nil && n > 0 {
		http.Error(w, "already in a room; leave it before joining another", http.StatusConflict)
		return
	}

	var body struct {
		InvitationCode string `json:"invitation_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.InvitationCode) == "" {
		http.Error(w, "invalid invitation code", http.StatusBadRequest)
		return
	}

	inv, err := room.ParseInvitation(strings.TrimSpace(body.InvitationCode))
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid invitation: %v", err), http.StatusBadRequest)
		return
	}

	device, err := s.store.GetDeviceIdentity()
	if err != nil {
		http.Error(w, "device identity missing", http.StatusInternalServerError)
		return
	}
	displayName, _ := s.store.GetSetting("display_name")
	if strings.TrimSpace(displayName) == "" {
		http.Error(w, "set a display name before joining a room", http.StatusBadRequest)
		return
	}

	authorityPubBytes, err := identity.DecodeToken(inv.AuthorityPublic, ed25519.PublicKeySize)
	if err != nil {
		http.Error(w, "invitation carries a malformed authority key", http.StatusBadRequest)
		return
	}
	authorityPub := ed25519.PublicKey(authorityPubBytes)

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	conn, err := s.dialBootstrap(ctx, inv.BootstrapAddr, authorityPub)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to reach creator: %v", err), http.StatusBadGateway)
		return
	}
	defer conn.Close()

	// device_public is not sent: the creator derives it from the certificate
	// this connection just proved possession of.
	joinReq := peerapi.JoinRequest{
		RoomID:          inv.RoomID,
		InvitationID:    inv.InvitationID,
		AdmissionSecret: inv.AdmissionSecret,
		DisplayName:     strings.TrimSpace(displayName),
		TailcatAddr:     s.trans.Address(),
	}

	resp, err := peerRoundTrip(ctx, conn, "POST", "/bootstrap/v1/join", joinReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		http.Error(w, fmt.Sprintf("join rejected: %s", string(b)), http.StatusForbidden)
		return
	}

	var joinResp peerapi.JoinResponse
	if err := json.NewDecoder(resp.Body).Decode(&joinResp); err != nil {
		http.Error(w, "invalid join response", http.StatusBadGateway)
		return
	}

	if joinResp.Status != room.StatusAdmitted {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": joinResp.Status,
			"reason": joinResp.Reason,
		})
		return
	}

	if joinResp.Membership == nil || joinResp.Membership.Verify(authorityPub) != nil {
		http.Error(w, "untrusted membership signature from creator", http.StatusBadGateway)
		return
	}
	if joinResp.Membership.DevicePublic != device.DevicePublic {
		http.Error(w, "creator issued a membership for a different device", http.StatusBadGateway)
		return
	}

	roomName := joinResp.RoomName
	if roomName == "" {
		roomName = "Room"
	}

	// The invitation code is deliberately not stored on a member node. Doing
	// so put the admission secret in every member's database, which is how a
	// leaked member backup could admit new devices behind the creator's back.
	//
	// This write is checked: reporting a successful join while the room row
	// failed to persist would leave the node believing it is in no room.
	if err := s.store.SaveRoomState(store.RoomRecord{
		RoomID:          inv.RoomID,
		Role:            "member",
		RoomName:        roomName,
		AuthorityPublic: inv.AuthorityPublic,
		BootstrapAddr:   inv.BootstrapAddr,
		RosterVersion:   joinResp.Membership.RosterVersion,
	}); err != nil {
		http.Error(w, "failed to save room state", http.StatusInternalServerError)
		return
	}

	saveMembership(s.store, *joinResp.Membership)
	for _, m := range joinResp.Roster {
		if m.Verify(authorityPub) == nil {
			saveMembership(s.store, m)
		}
	}

	// The creator's member ID comes from the join response. Deriving it from
	// the authority key produced a phantom ID that matched no real member, so
	// the creator's address was stored under an entry nothing ever looked up.
	if joinResp.CreatorMemberID != "" {
		_ = s.store.SavePeerAddress(joinResp.CreatorMemberID, inv.BootstrapAddr)
	}
	for _, pa := range joinResp.PeerAddresses {
		_ = s.store.SavePeerAddress(pa.MemberID, pa.TailcatAddr)
	}

	if s.startPeerFn != nil {
		if err := s.startPeerFn(nil); err != nil {
			http.Error(w, fmt.Sprintf("start peer listener failed: %v", err), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  joinResp.Status,
		"room_id": inv.RoomID,
	})
}

func saveMembership(s *store.Store, m room.Membership) {
	_ = s.SaveMember(peerauth.MemberRecord(m))
}

func (s *Server) handleLeaveRoom(w http.ResponseWriter, r *http.Request) {
	// Stop serving peers first. Leaving with the listener still running left a
	// node answering peer requests for a room it had erased.
	if s.stopPeerFn != nil {
		_ = s.stopPeerFn()
	}

	if err := s.store.ClearRoom(); err != nil {
		http.Error(w, "failed to leave room", http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.noSaveTurns = make(map[string][]hosting.ChatMessage)
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleRotateInvitation(w http.ResponseWriter, r *http.Request) {
	roomRec, err := s.store.GetRoomState()
	if err != nil {
		http.Error(w, "no active room", http.StatusBadRequest)
		return
	}

	authority := ed25519.PrivateKey(roomRec.AuthorityPrivate)
	authorityPub := authority.Public().(ed25519.PublicKey)

	newInv, err := room.NewInvitation(roomRec.RoomID, authorityPub, roomRec.BootstrapAddr)
	if err != nil {
		http.Error(w, "generate invitation failed", http.StatusInternalServerError)
		return
	}
	code, err := newInv.Encode()
	if err != nil {
		http.Error(w, "encode invitation failed", http.StatusInternalServerError)
		return
	}

	roomRec.InvitationCode = code
	roomRec.InvitationID = newInv.InvitationID
	_ = s.store.SaveRoomState(*roomRec)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"invitation_code": code})
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	roomRec, err := s.store.GetRoomState()
	if err != nil {
		http.Error(w, "no active room", http.StatusBadRequest)
		return
	}

	members, err := s.store.ListMembers(roomRec.RoomID)
	if err != nil {
		http.Error(w, "failed to list members", http.StatusInternalServerError)
		return
	}
	if members == nil {
		members = []store.MemberRecord{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(members)
}

func (s *Server) handleListPending(w http.ResponseWriter, r *http.Request) {
	roomRec, err := s.store.GetRoomState()
	if err != nil {
		http.Error(w, "no active room", http.StatusBadRequest)
		return
	}

	members, _ := s.store.ListMembers(roomRec.RoomID)
	pending := make([]store.MemberRecord, 0)
	for _, m := range members {
		if m.Status == string(room.StatusPending) {
			pending = append(pending, m)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pending)
}

func (s *Server) handleApproveMember(w http.ResponseWriter, r *http.Request) {
	memberID := r.PathValue("id")
	if memberID == "" {
		http.Error(w, "member id required", http.StatusBadRequest)
		return
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil {
		http.Error(w, "no active room", http.StatusBadRequest)
		return
	}
	m, err := s.store.GetMember(memberID)
	if err != nil || m == nil {
		http.Error(w, "member not found", http.StatusNotFound)
		return
	}
	if m.Status != string(room.StatusPending) {
		http.Error(w, "member is not pending approval", http.StatusConflict)
		return
	}

	newVersion := roomRec.RosterVersion + 1
	approval := room.Membership{
		MemberID:      m.MemberID,
		RoomID:        m.RoomID,
		DevicePublic:  m.DevicePublic,
		DisplayName:   m.DisplayName,
		Status:        room.StatusAdmitted,
		RosterVersion: newVersion,
	}
	if err := approval.Sign(ed25519.PrivateKey(roomRec.AuthorityPrivate)); err != nil {
		http.Error(w, "sign approval failed", http.StatusInternalServerError)
		return
	}
	saveMembership(s.store, approval)

	roomRec.RosterVersion = newVersion
	_ = s.store.SaveRoomState(*roomRec)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "admitted"})
}

func (s *Server) handleSetApprovalMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ApprovalMode bool `json:"approval_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil {
		http.Error(w, "no active room", http.StatusBadRequest)
		return
	}
	roomRec.ApprovalMode = body.ApprovalMode
	if err := s.store.SaveRoomState(*roomRec); err != nil {
		http.Error(w, "failed to save approval mode", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "approval_mode": roomRec.ApprovalMode})
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	memberID := r.PathValue("id")
	if memberID == "" {
		http.Error(w, "member id required", http.StatusBadRequest)
		return
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil {
		http.Error(w, "no active room", http.StatusBadRequest)
		return
	}

	m, err := s.store.GetMember(memberID)
	if err != nil {
		http.Error(w, "member not found", http.StatusNotFound)
		return
	}

	authority := ed25519.PrivateKey(roomRec.AuthorityPrivate)
	newVersion := roomRec.RosterVersion + 1

	removal := room.Membership{
		MemberID:      m.MemberID,
		RoomID:        m.RoomID,
		DevicePublic:  m.DevicePublic,
		DisplayName:   m.DisplayName,
		Status:        room.StatusRemoved,
		RosterVersion: newVersion,
	}
	_ = removal.Sign(authority)
	saveMembership(s.store, removal)

	// A removed member's queued and running work stops immediately rather than
	// finishing on hardware they no longer have access to.
	s.infer.Queue().CancelMember(memberID)
	_ = s.store.DeletePeerAddress(memberID)

	roomRec.RosterVersion = newVersion

	// By default, removing a member rotates the invitation code to prevent reentry
	var reqBody struct {
		RotateInvitation *bool `json:"rotate_invitation"`
	}
	_ = json.NewDecoder(r.Body).Decode(&reqBody)
	shouldRotate := true
	if reqBody.RotateInvitation != nil {
		shouldRotate = *reqBody.RotateInvitation
	}

	if shouldRotate {
		authorityPub := authority.Public().(ed25519.PublicKey)
		if newInv, err := room.NewInvitation(roomRec.RoomID, authorityPub, roomRec.BootstrapAddr); err == nil {
			if code, err := newInv.Encode(); err == nil {
				roomRec.InvitationCode = code
				roomRec.InvitationID = newInv.InvitationID
			}
		}
	}

	_ = s.store.SaveRoomState(*roomRec)

	// Push the removal out immediately so peers stop serving the removed
	// member without waiting for the next scheduled poll.
	go s.SyncMembership(context.Background())

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":              true,
		"invitation_code": roomRec.InvitationCode,
	})
}

func (s *Server) Serve(listener net.Listener) error {
	return s.server.Serve(listener)
}

// StartBackground launches the membership, retention, and model-advertisement
// jobs.
func (s *Server) StartBackground() {
	ctx, cancel := context.WithCancel(context.Background())

	s.mu.Lock()
	if s.bgCancel != nil {
		s.mu.Unlock()
		cancel()
		return
	}
	s.bgCancel = cancel
	s.mu.Unlock()

	s.bgWG.Add(4)
	go func() {
		defer s.bgWG.Done()
		s.membershipLoop(ctx)
	}()
	go func() {
		defer s.bgWG.Done()
		s.migrateArtifacts(ctx)
	}()
	go func() {
		defer s.bgWG.Done()
		s.retentionLoop(ctx)
	}()
	go func() {
		defer s.bgWG.Done()
		s.catalogLoop(ctx)
	}()
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.bgCancel
	s.bgCancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.bgWG.Wait()
	return s.server.Shutdown(ctx)
}

func (s *Server) Close() error {
	s.mu.Lock()
	cancel := s.bgCancel
	s.bgCancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.bgWG.Wait()
	return s.server.Close()
}
