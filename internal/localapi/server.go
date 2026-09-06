package localapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cbaack/woolwire/internal/catalog"
	"github.com/cbaack/woolwire/internal/hosting"
	"github.com/cbaack/woolwire/internal/identity"
	"github.com/cbaack/woolwire/internal/peerapi"
	"github.com/cbaack/woolwire/internal/room"
	"github.com/cbaack/woolwire/internal/store"
	"github.com/cbaack/woolwire/internal/transport"
)

type Config struct {
	Store       *store.Store
	Transport   transport.Transport
	SetupToken  string
	PeerPort    uint16
	WebAssets   http.FileSystem
	StartPeerFn func(authority ed25519.PrivateKey) error
	ModelsDir    string
	RunnerURL    string
	RunnerToken  string
	AllowedHosts []string
}

type Server struct {
	store        *store.Store
	trans        transport.Transport
	setupHash    [32]byte
	setupToken   string
	sessionKey   string
	peerPort     uint16
	webAssets    http.FileSystem
	startPeerFn  func(authority ed25519.PrivateKey) error
	catalog      *catalog.Catalog
	adapter      *hosting.ExternalAdapter
	artifactMgr  *hosting.ArtifactManager
	runnerClient *hosting.RunnerClient
	allowedHosts []string

	mux    *http.ServeMux
	server *http.Server
	mu     sync.RWMutex
}

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
		artMgr, _ = hosting.NewArtifactManager(cfg.ModelsDir, 50*(1<<30))
	}
	var runnerCli *hosting.RunnerClient
	if cfg.RunnerURL != "" {
		runnerCli = hosting.NewRunnerClient(cfg.RunnerURL, cfg.RunnerToken)
	}

	s := &Server{
		store:        cfg.Store,
		trans:        cfg.Transport,
		setupHash:    sha256.Sum256([]byte(cfg.SetupToken)),
		setupToken:   cfg.SetupToken,
		sessionKey:   sessionKey,
		peerPort:     cfg.PeerPort,
		webAssets:    cfg.WebAssets,
		startPeerFn:  cfg.StartPeerFn,
		catalog:      catalog.NewCatalog(),
		adapter:      hosting.NewExternalAdapter(),
		artifactMgr:  artMgr,
		runnerClient: runnerCli,
		allowedHosts: cfg.AllowedHosts,
		mux:          http.NewServeMux(),
	}

	s.routes()
	s.server = &http.Server{
		Handler:      s.securityMiddleware(s.mux),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	return s, nil
}

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

func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := cleanHost(r.Host)
		if !s.isAllowedHost(host) {
			http.Error(w, "invalid host header", http.StatusForbidden)
			return
		}

		// Security headers
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:;")

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

	// Authenticated setup info & token update
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

	// Hardware & Managed Models
	s.mux.HandleFunc("GET /api/v1/hardware", s.authMiddleware(s.handleGetHardware))
	s.mux.HandleFunc("GET /api/v1/managed-models/artifacts", s.authMiddleware(s.handleListArtifacts))
	s.mux.HandleFunc("POST /api/v1/managed-models/download", s.authMiddleware(s.handleDownloadArtifact))
	s.mux.HandleFunc("DELETE /api/v1/managed-models/artifacts/{filename}", s.authMiddleware(s.handleDeleteArtifact))
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

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	providedHash := sha256.Sum256([]byte(strings.TrimSpace(body.Token)))
	s.mu.RLock()
	match := subtle.ConstantTimeCompare(s.setupHash[:], providedHash[:]) == 1
	s.mu.RUnlock()

	if !match {
		http.Error(w, "invalid setup token", http.StatusUnauthorized)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "woolwire_session",
		Value:    s.sessionKey,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   365 * 24 * 3600, // 1 year persistence
	})

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleGetSetupInfo(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	token := s.setupToken
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token": token,
	})
}

func (s *Server) handleUpdateSetupToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	newToken := strings.TrimSpace(body.Token)
	if len(newToken) < 4 {
		http.Error(w, "setup token or PIN must be at least 4 characters", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.setupToken = newToken
	s.setupHash = sha256.Sum256([]byte(newToken))
	s.mu.Unlock()

	_ = s.store.SetSetting("setup_token", newToken)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":    true,
		"token": newToken,
	})
}

func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	displayName, _ := s.store.GetSetting("display_name")
	device, _ := s.store.GetDeviceIdentity()
	devicePublic := ""
	memberID := ""
	if device != nil {
		devicePublic = device.DevicePublic
		if len(device.DevicePublic) >= 16 {
			memberID = "m-" + device.DevicePublic[:16]
		}
	}

	roomRec, _ := s.store.GetRoomState()
	var roomData any
	if roomRec != nil {
		roomData = map[string]any{
			"room_id":         roomRec.RoomID,
			"room_name":       roomRec.RoomName,
			"role":            roomRec.Role,
			"invitation_code": roomRec.InvitationCode,
			"approval_mode":   roomRec.ApprovalMode,
			"roster_version":  roomRec.RosterVersion,
		}
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
	var body struct {
		RoomName string `json:"room_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.RoomName) == "" {
		http.Error(w, "invalid room name", http.StatusBadRequest)
		return
	}

	// Generate room authority
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
	}

	if err := s.store.SaveRoomState(roomRec); err != nil {
		http.Error(w, "save room state failed", http.StatusInternalServerError)
		return
	}

	// Creator auto-enrolls itself as first admitted member
	device, _ := s.store.GetDeviceIdentity()
	displayName, _ := s.store.GetSetting("display_name")
	if device != nil {
		creatorMemberID := "m-" + device.DevicePublic[:16]
		_ = s.store.SavePeerAddress(creatorMemberID, bootstrapAddr)
		m := room.Membership{
			MemberID:      creatorMemberID,
			RoomID:        roomID,
			DevicePublic:  device.DevicePublic,
			DisplayName:   displayName,
			Status:        room.StatusAdmitted,
			RosterVersion: 1,
		}
		_ = m.Sign(priv)
		_ = s.store.SaveMember(store.MemberRecord{
			MemberID:      m.MemberID,
			RoomID:        m.RoomID,
			DevicePublic:  m.DevicePublic,
			DisplayName:   m.DisplayName,
			Status:        string(m.Status),
			RosterVersion: m.RosterVersion,
			Signature:     m.Signature,
		})
	}

	if s.startPeerFn != nil {
		_ = s.startPeerFn(priv)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"room_id":         roomID,
		"invitation_code": code,
	})
}

func (s *Server) handleJoinRoom(w http.ResponseWriter, r *http.Request) {
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

	// Connect to creator bootstrap address over transport
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	conn, err := s.trans.Dial(ctx, inv.BootstrapAddr, s.peerPort)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to reach creator: %v", err), http.StatusBadGateway)
		return
	}
	defer conn.Close()

	joinReq := peerapi.JoinRequest{
		RoomID:          inv.RoomID,
		InvitationID:    inv.InvitationID,
		AdmissionSecret: inv.AdmissionSecret,
		DevicePublic:    device.DevicePublic,
		DisplayName:     displayName,
		TailcatAddr:     s.trans.Address(),
	}

	reqBytes, _ := json.Marshal(joinReq)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", "http://woolwire-bootstrap/bootstrap/v1/join", bytes.NewReader(reqBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	if err := httpReq.Write(conn); err != nil {
		http.Error(w, fmt.Sprintf("send join request failed: %v", err), http.StatusBadGateway)
		return
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), httpReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("read join response failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
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

	// Verify membership signature against pinned authority
	authorityPubBytes, _ := identity.DecodeToken(inv.AuthorityPublic, ed25519.PublicKeySize)
	authorityPub := ed25519.PublicKey(authorityPubBytes)
	if err := joinResp.Membership.Verify(authorityPub); err != nil {
		http.Error(w, "untrusted membership signature from creator", http.StatusBadGateway)
		return
	}

	// Save room record
	_ = s.store.SaveRoomState(store.RoomRecord{
		RoomID:          inv.RoomID,
		Role:            "member",
		RoomName:        "Room",
		AuthorityPublic: inv.AuthorityPublic,
		BootstrapAddr:   inv.BootstrapAddr,
		InvitationCode:  strings.TrimSpace(body.InvitationCode),
		RosterVersion:   joinResp.Membership.RosterVersion,
	})

	// Save member record
	_ = s.store.SaveMember(store.MemberRecord{
		MemberID:      joinResp.Membership.MemberID,
		RoomID:        joinResp.Membership.RoomID,
		DevicePublic:  joinResp.Membership.DevicePublic,
		DisplayName:   joinResp.Membership.DisplayName,
		Status:        string(joinResp.Membership.Status),
		RosterVersion: joinResp.Membership.RosterVersion,
		Signature:     joinResp.Membership.Signature,
	})

	// Save other members in roster
	for _, m := range joinResp.Roster {
		if err := m.Verify(authorityPub); err == nil {
			_ = s.store.SaveMember(store.MemberRecord{
				MemberID:      m.MemberID,
				RoomID:        m.RoomID,
				DevicePublic:  m.DevicePublic,
				DisplayName:   m.DisplayName,
				Status:        string(m.Status),
				RosterVersion: m.RosterVersion,
				Signature:     m.Signature,
			})
		}
	}

	// Save creator address and other peer addresses
	creatorMemberID := "m-" + inv.AuthorityPublic[:16]
	_ = s.store.SavePeerAddress(creatorMemberID, inv.BootstrapAddr)
	for _, pa := range joinResp.PeerAddresses {
		_ = s.store.SavePeerAddress(pa.MemberID, pa.TailcatAddr)
	}

	if s.startPeerFn != nil {
		_ = s.startPeerFn(nil)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  joinResp.Status,
		"room_id": inv.RoomID,
	})
}

func (s *Server) handleLeaveRoom(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ClearRoom(); err != nil {
		http.Error(w, "failed to leave room", http.StatusInternalServerError)
		return
	}
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

	m.Status = string(room.StatusRemoved)
	m.RosterVersion = newVersion
	m.Signature = removal.Signature
	_ = s.store.SaveMember(*m)

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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":              true,
		"invitation_code": roomRec.InvitationCode,
	})
}

func (s *Server) Serve(listener net.Listener) error {
	return s.server.Serve(listener)
}

func (s *Server) Close() error {
	return s.server.Close()
}
