package peerapi

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/cbaack/woolwire/internal/catalog"
	"github.com/cbaack/woolwire/internal/hosting"
	"github.com/cbaack/woolwire/internal/inference"
	"github.com/cbaack/woolwire/internal/peerauth"
	"github.com/cbaack/woolwire/internal/room"
	"github.com/cbaack/woolwire/internal/store"
	"github.com/cbaack/woolwire/internal/transport"
)

// BootstrapPortOffset places /bootstrap/v1/join on its own Tailcat port. The
// two endpoints have different trust rules — bootstrap accepts a stranger's
// device certificate, the peer API does not — so they cannot share a listener
// without weakening the peer API to whatever bootstrap must allow.
const BootstrapPortOffset = 1

type contextKey string

const tlsConnKey contextKey = "woolwire.tlsconn"

type Config struct {
	Store *store.Store
	// Authority is non-nil only on the creator. Admission is refused outright
	// without it, so a member's bootstrap endpoint cannot admit anyone.
	Authority ed25519.PrivateKey
	// DeviceCert is this node's device certificate, presented on the peer API.
	DeviceCert tls.Certificate
	// RoomCert is the creator's authority-signed bootstrap certificate.
	RoomCert *tls.Certificate
	// Inference is shared with the local API so the host's own usage counts
	// against the same limits remote members are held to.
	Inference *inference.Service
}

type Server struct {
	store     *store.Store
	authority ed25519.PrivateKey
	deviceKey ed25519.PrivateKey
	roster    *peerauth.Roster
	infer     *inference.Service
	adapter   *hosting.ExternalAdapter

	deviceCert tls.Certificate
	roomCert   *tls.Certificate

	listeners    []net.Listener
	peerMux      *http.ServeMux
	bootstrapMux *http.ServeMux
	peerHTTP     *http.Server
	bootstrapSrv *http.Server
	mu           sync.RWMutex
}

func NewServer(cfg Config) *Server {
	var devKey ed25519.PrivateKey
	if cfg.Store != nil {
		if dev, err := cfg.Store.GetDeviceIdentity(); err == nil && len(dev.DevicePrivate) == ed25519.PrivateKeySize {
			devKey = ed25519.PrivateKey(dev.DevicePrivate)
		}
	}

	infer := cfg.Inference
	if infer == nil {
		infer = inference.NewService(cfg.Store, hosting.NewExternalAdapter(), nil)
	}

	srv := &Server{
		store:        cfg.Store,
		authority:    cfg.Authority,
		deviceKey:    devKey,
		roster:       peerauth.NewRoster(cfg.Store),
		infer:        infer,
		adapter:      hosting.NewExternalAdapter(),
		deviceCert:   cfg.DeviceCert,
		roomCert:     cfg.RoomCert,
		peerMux:      http.NewServeMux(),
		bootstrapMux: http.NewServeMux(),
	}
	srv.routes()

	connContext := func(ctx context.Context, c net.Conn) context.Context {
		if tc, ok := c.(*tls.Conn); ok {
			return context.WithValue(ctx, tlsConnKey, tc)
		}
		return ctx
	}

	srv.peerHTTP = &http.Server{
		Handler:     srv.peerMux,
		ReadTimeout: 30 * time.Second,
		// WriteTimeout stays 0: inference responses stream for as long as the
		// model takes, and per-write deadlines are set inside the handlers.
		WriteTimeout: 0,
		ConnContext:  connContext,
	}
	srv.bootstrapSrv = &http.Server{
		Handler:      srv.bootstrapMux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		ConnContext:  connContext,
	}
	return srv
}

func (s *Server) routes() {
	s.bootstrapMux.HandleFunc("POST /bootstrap/v1/join", s.handleJoin)

	s.peerMux.HandleFunc("POST /peer/v1/membership/sync", s.authenticated(s.handleSync))
	s.peerMux.HandleFunc("GET /peer/v1/catalog", s.authenticated(s.handleCatalog))
	s.peerMux.HandleFunc("POST /peer/v1/inference", s.authenticated(s.handleInference))
	s.peerMux.HandleFunc("POST /peer/v1/inference/cancel", s.authenticated(s.handleCancelInference))
	s.peerMux.HandleFunc("POST /peer/v1/community/sync", s.authenticated(s.handleCommunitySync))
	s.peerMux.HandleFunc("POST /peer/v1/contributions/ack", s.authenticated(s.handleContributionsAck))
	s.peerMux.HandleFunc("POST /peer/v1/contributions/sync", s.authenticated(s.handleContributionsSync))
}

type peerHandler func(w http.ResponseWriter, r *http.Request, caller peerauth.Identity)

// authenticated resolves the caller from the TLS client certificate. The
// handshake already refused unadmitted peers; re-checking here means a
// membership removed while a connection is open stops being honored on the
// very next request instead of when the connection happens to close.
func (s *Server) authenticated(next peerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, err := s.CallerIdentity(r)
		if err != nil {
			http.Error(w, "unauthorized peer", http.StatusForbidden)
			return
		}
		next(w, r, caller)
	}
}

// CallerIdentity reports the authenticated member behind a peer request.
func (s *Server) CallerIdentity(r *http.Request) (peerauth.Identity, error) {
	tc, ok := r.Context().Value(tlsConnKey).(*tls.Conn)
	if !ok || tc == nil {
		return peerauth.Identity{}, errors.New("peer connection is not mutually authenticated")
	}
	return s.roster.IdentityFromConnState(tc.ConnectionState())
}

func bootstrapDevicePublic(r *http.Request) (string, error) {
	tc, ok := r.Context().Value(tlsConnKey).(*tls.Conn)
	if !ok || tc == nil {
		return "", errors.New("bootstrap connection is not TLS")
	}
	return peerauth.BootstrapIdentityFromConnState(tc.ConnectionState())
}

// MemberIDForDevicePublic derives the room-wide member identifier from a
// device key. It is the one place the derivation lives.
func MemberIDForDevicePublic(devicePublic string) (string, error) {
	if len(devicePublic) < 16 {
		return "", errors.New("device public key is too short to derive a member id")
	}
	return "m-" + devicePublic[:16], nil
}

type JoinRequest struct {
	RoomID          string `json:"room_id"`
	InvitationID    string `json:"invitation_id"`
	AdmissionSecret string `json:"admission_secret"`
	DisplayName     string `json:"display_name"`
	TailcatAddr     string `json:"tailcat_addr"`
}

type JoinResponse struct {
	Status          room.MemberStatus         `json:"status"`
	RoomName        string                    `json:"room_name,omitempty"`
	CreatorMemberID string                    `json:"creator_member_id,omitempty"`
	Membership      *room.Membership          `json:"membership,omitempty"`
	Roster          []room.Membership         `json:"roster,omitempty"`
	PeerAddresses   []store.PeerAddressRecord `json:"peer_addresses,omitempty"`
	Reason          string                    `json:"reason,omitempty"`
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	// Only the creator holds the room authority, and an unsigned membership is
	// worthless to every other node. Admitting from a member node would let a
	// leaked invitation code bypass the creator entirely.
	if len(s.authority) != ed25519.PrivateKeySize {
		http.Error(w, "only the room creator can admit members", http.StatusForbidden)
		return
	}

	devicePublic, err := bootstrapDevicePublic(r)
	if err != nil {
		http.Error(w, "device certificate required", http.StatusBadRequest)
		return
	}

	var req JoinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}
	if req.RoomID != roomRec.RoomID {
		http.Error(w, "wrong room", http.StatusBadRequest)
		return
	}

	if roomRec.InvitationCode == "" {
		http.Error(w, "invitations disabled", http.StatusForbidden)
		return
	}
	inv, err := room.ParseInvitation(roomRec.InvitationCode)
	if err != nil || inv.InvitationID != req.InvitationID || !inv.VerifySecret(req.AdmissionSecret) {
		writeJoinRejection(w, "invalid or rotated invitation code")
		return
	}

	displayName := req.DisplayName
	if displayName == "" {
		http.Error(w, "display name is required", http.StatusBadRequest)
		return
	}

	memberID, err := MemberIDForDevicePublic(devicePublic)
	if err != nil {
		http.Error(w, "invalid device public key", http.StatusBadRequest)
		return
	}

	// Re-admission is not automatic. An already-admitted row would otherwise
	// be overwritten by whoever presents the code next, and a removed member
	// whose key never changed could walk back in on an unrotated code. The
	// creator removes the row deliberately before such a device may rejoin.
	if existing, err := s.store.GetMember(memberID); err == nil && existing != nil {
		switch room.MemberStatus(existing.Status) {
		case room.StatusAdmitted:
			writeJoinRejection(w, "device is already an admitted member")
			return
		case room.StatusRemoved:
			writeJoinRejection(w, "device was removed from this room and cannot rejoin")
			return
		}
	}

	status := room.StatusAdmitted
	if roomRec.ApprovalMode {
		status = room.StatusPending
	}

	newRosterVersion := roomRec.RosterVersion + 1
	membership := room.Membership{
		MemberID:      memberID,
		RoomID:        roomRec.RoomID,
		DevicePublic:  devicePublic,
		DisplayName:   displayName,
		Status:        status,
		RosterVersion: newRosterVersion,
	}
	if err := membership.Sign(s.authority); err != nil {
		http.Error(w, "sign membership failed", http.StatusInternalServerError)
		return
	}

	if err := s.store.SaveMember(peerauth.MemberRecord(membership)); err != nil {
		http.Error(w, "save membership failed", http.StatusInternalServerError)
		return
	}

	if req.TailcatAddr != "" {
		_ = s.store.SavePeerAddress(membership.MemberID, req.TailcatAddr)
	}

	roomRec.RosterVersion = newRosterVersion
	_ = s.store.SaveRoomState(*roomRec)

	creatorMemberID := ""
	if dev, err := s.store.GetDeviceIdentity(); err == nil && dev != nil {
		creatorMemberID, _ = MemberIDForDevicePublic(dev.DevicePublic)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(JoinResponse{
		Status:          status,
		RoomName:        roomRec.RoomName,
		CreatorMemberID: creatorMemberID,
		Membership:      &membership,
		Roster:          s.rosterSnapshot(roomRec.RoomID, 0),
		PeerAddresses:   s.peerAddresses(),
	})
}

func writeJoinRejection(w http.ResponseWriter, reason string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(JoinResponse{Status: "rejected", Reason: reason})
}

func (s *Server) rosterSnapshot(roomID string, minVersion int64) []room.Membership {
	members, _ := s.store.ListMembers(roomID)
	roster := make([]room.Membership, 0, len(members))
	for _, m := range members {
		if m.RosterVersion <= minVersion {
			continue
		}
		// SigVersion travels with the signature; without it the recipient
		// rebuilds the legacy payload and every record fails to verify.
		roster = append(roster, peerauth.Membership(m))
	}
	return roster
}

func (s *Server) peerAddresses() []store.PeerAddressRecord {
	addrs, _ := s.store.ListPeerAddresses()
	if addrs == nil {
		return []store.PeerAddressRecord{}
	}
	return addrs
}

type SyncRequest struct {
	KnownVersion int64 `json:"known_version"`
	// TailcatAddr updates the caller's own address only. There is no member_id
	// field: the address is bound to the authenticated TLS identity, so a
	// member cannot rebind anyone else's address to a node it controls.
	TailcatAddr string `json:"tailcat_addr,omitempty"`
}

type SyncResponse struct {
	RosterVersion int64                     `json:"roster_version"`
	Members       []room.Membership         `json:"members"`
	PeerAddresses []store.PeerAddressRecord `json:"peer_addresses,omitempty"`
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request, caller peerauth.Identity) {
	var req SyncRequest
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req)

	if req.TailcatAddr != "" {
		_ = s.store.SavePeerAddress(caller.MemberID, req.TailcatAddr)
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		http.Error(w, "no active room", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(SyncResponse{
		RosterVersion: roomRec.RosterVersion,
		Members:       s.rosterSnapshot(roomRec.RoomID, req.KnownVersion),
		PeerAddresses: s.peerAddresses(),
	})
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request, caller peerauth.Identity) {
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		http.Error(w, "no active room", http.StatusNotFound)
		return
	}

	device, err := s.store.GetDeviceIdentity()
	if err != nil {
		http.Error(w, "device identity missing", http.StatusInternalServerError)
		return
	}
	hostMemberID, err := MemberIDForDevicePublic(device.DevicePublic)
	if err != nil {
		http.Error(w, "device identity is malformed", http.StatusInternalServerError)
		return
	}

	models, err := s.store.ListHostedModels()
	if err != nil {
		http.Error(w, "failed to query models", http.StatusInternalServerError)
		return
	}

	active, queued := s.infer.Queue().Stats()
	queueEst := active + queued

	ads := make([]catalog.ModelAd, 0, len(models))
	for _, m := range models {
		if !m.Enabled || !m.Published {
			continue
		}

		ad := catalog.ModelAd{
			RoomID:        roomRec.RoomID,
			HostMemberID:  hostMemberID,
			ModelID:       m.ID,
			Revision:      m.Revision,
			Name:          m.Name,
			ContextLimit:  m.ContextLimit,
			Availability:  "ready",
			QueueEstimate: queueEst,
			IsManaged:     m.ModelType == "managed",
		}
		if len(s.deviceKey) == ed25519.PrivateKeySize {
			_ = ad.Sign(s.deviceKey)
		}
		ads = append(ads, ad)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ads)
}

func (s *Server) Serve(listener net.Listener) error {
	return s.peerHTTP.Serve(listener)
}

// ServeBootstrap answers /bootstrap/v1/join. Only the creator ever starts it.
func (s *Server) ServeBootstrap(listener net.Listener) error {
	return s.bootstrapSrv.Serve(listener)
}

// PeerTLSConfig is the server-side configuration for the peer listener.
func (s *Server) PeerTLSConfig() *tls.Config {
	return peerauth.PeerServerTLSConfig(s.deviceCert, s.roster)
}

// BootstrapTLSConfig is the server-side configuration for the bootstrap
// listener. It returns nil when this node holds no authority-signed room
// certificate, which is the case for every non-creator.
func (s *Server) BootstrapTLSConfig() *tls.Config {
	if s.roomCert == nil {
		return nil
	}
	return peerauth.BootstrapServerTLSConfig(*s.roomCert)
}

func (s *Server) Shutdown(ctx context.Context) error {
	bootErr := s.bootstrapSrv.Shutdown(ctx)
	peerErr := s.peerHTTP.Shutdown(ctx)
	if peerErr != nil {
		return peerErr
	}
	return bootErr
}

func (s *Server) Close() error {
	_ = s.bootstrapSrv.Close()
	return s.peerHTTP.Close()
}

// Start listens on the transport for peer connections and, when this node is
// the creator, for bootstrap connections on the adjacent port. Both listeners
// are wrapped in TLS here so no caller can accidentally serve the peer API in
// the clear.
func (s *Server) Start(t transport.Transport, peerPort uint16) error {
	peerListener, err := t.Listen(peerPort)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.listeners = append(s.listeners, peerListener)
	s.mu.Unlock()
	go func() { _ = s.Serve(tls.NewListener(peerListener, s.PeerTLSConfig())) }()

	if bootstrapCfg := s.BootstrapTLSConfig(); bootstrapCfg != nil {
		bootstrapListener, err := t.Listen(peerPort + BootstrapPortOffset)
		if err != nil {
			_ = s.Stop(context.Background())
			return err
		}
		s.mu.Lock()
		s.listeners = append(s.listeners, bootstrapListener)
		s.mu.Unlock()
		go func() { _ = s.ServeBootstrap(tls.NewListener(bootstrapListener, bootstrapCfg)) }()
	}

	return nil
}

// Stop drains both servers and closes the listeners this server opened.
func (s *Server) Stop(ctx context.Context) error {
	err := s.Shutdown(ctx)

	s.mu.Lock()
	listeners := s.listeners
	s.listeners = nil
	s.mu.Unlock()

	for _, l := range listeners {
		_ = l.Close()
	}
	return err
}
