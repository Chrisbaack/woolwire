package peerapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/cbaack/woolwire/internal/catalog"
	"github.com/cbaack/woolwire/internal/community"
	"github.com/cbaack/woolwire/internal/contributions"
	"github.com/cbaack/woolwire/internal/hosting"
	"github.com/cbaack/woolwire/internal/identity"
	"github.com/cbaack/woolwire/internal/inference"
	"github.com/cbaack/woolwire/internal/room"
	"github.com/cbaack/woolwire/internal/store"
)

type Server struct {
	store     *store.Store
	authority ed25519.PrivateKey // non-nil if creator
	deviceKey ed25519.PrivateKey // local host key for signing ads
	queue     *inference.FairQueue
	adapter   *hosting.ExternalAdapter

	mux    *http.ServeMux
	server *http.Server
	mu     sync.RWMutex
}

func NewServer(s *store.Store, authority ed25519.PrivateKey) *Server {
	limits := inference.DefaultLimits()
	if s != nil {
		if l, err := s.GetHostLimits(); err == nil {
			limits = inference.Limits{
				MaxActive:          l.MaxActive,
				MaxQueuedPerMember: l.MaxQueuedPerMember,
				MaxQueuedTotal:     l.MaxQueuedTotal,
				QueueTimeout:       time.Duration(l.QueueTimeoutSeconds) * time.Second,
				ExecTimeout:        time.Duration(l.ExecutionTimeoutSeconds) * time.Second,
			}
		}
	}

	var devKey ed25519.PrivateKey
	if s != nil {
		if dev, err := s.GetDeviceIdentity(); err == nil && len(dev.DevicePrivate) == ed25519.PrivateKeySize {
			devKey = ed25519.PrivateKey(dev.DevicePrivate)
		}
	}

	srv := &Server{
		store:     s,
		authority: authority,
		deviceKey: devKey,
		queue:     inference.NewFairQueue(limits),
		adapter:   hosting.NewExternalAdapter(),
		mux:       http.NewServeMux(),
	}
	srv.routes()
	srv.server = &http.Server{
		Handler:     srv.mux,
		ReadTimeout: 30 * time.Second,
		// WriteTimeout is 0 to allow continuous SSE streaming
		WriteTimeout: 0,
	}
	return srv
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /bootstrap/v1/join", s.handleJoin)
	s.mux.HandleFunc("POST /peer/v1/membership/sync", s.handleSync)
	s.mux.HandleFunc("GET /peer/v1/catalog", s.handleCatalog)
	s.mux.HandleFunc("POST /peer/v1/inference", s.handleInference)
	s.mux.HandleFunc("POST /peer/v1/inference/cancel", s.handleCancelInference)
	s.mux.HandleFunc("POST /peer/v1/community/sync", s.handleCommunitySync)
	s.mux.HandleFunc("POST /peer/v1/contributions/ack", s.handleContributionsAck)
	s.mux.HandleFunc("POST /peer/v1/contributions/sync", s.handleContributionsSync)
}

type JoinRequest struct {
	RoomID          string `json:"room_id"`
	InvitationID    string `json:"invitation_id"`
	AdmissionSecret string `json:"admission_secret"`
	DevicePublic    string `json:"device_public"`
	DisplayName     string `json:"display_name"`
	TailcatAddr     string `json:"tailcat_addr"`
}

type JoinResponse struct {
	Status        room.MemberStatus         `json:"status"`
	Membership    *room.Membership          `json:"membership,omitempty"`
	Roster        []room.Membership         `json:"roster,omitempty"`
	PeerAddresses []store.PeerAddressRecord `json:"peer_addresses,omitempty"`
	Reason        string                    `json:"reason,omitempty"`
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req JoinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	if req.RoomID != roomRec.RoomID {
		http.Error(w, "wrong room", http.StatusBadRequest)
		return
	}

	// Verify invitation
	if roomRec.InvitationCode == "" {
		http.Error(w, "invitations disabled", http.StatusForbidden)
		return
	}
	inv, err := room.ParseInvitation(roomRec.InvitationCode)
	if err != nil || inv.InvitationID != req.InvitationID || !inv.VerifySecret(req.AdmissionSecret) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(JoinResponse{
			Status: "rejected",
			Reason: "invalid or rotated invitation code",
		})
		return
	}

	status := room.StatusAdmitted
	if roomRec.ApprovalMode {
		status = room.StatusPending
	}

	memberID := "m-" + req.DevicePublic[:16]
	newRosterVersion := roomRec.RosterVersion + 1

	membership := room.Membership{
		MemberID:      memberID,
		RoomID:        roomRec.RoomID,
		DevicePublic:  req.DevicePublic,
		DisplayName:   req.DisplayName,
		Status:        status,
		RosterVersion: newRosterVersion,
	}

	if s.authority != nil {
		if err := membership.Sign(s.authority); err != nil {
			http.Error(w, "sign membership failed", http.StatusInternalServerError)
			return
		}
	}

	_ = s.store.SaveMember(store.MemberRecord{
		MemberID:      membership.MemberID,
		RoomID:        membership.RoomID,
		DevicePublic:  membership.DevicePublic,
		DisplayName:   membership.DisplayName,
		Status:        string(membership.Status),
		RosterVersion: membership.RosterVersion,
		Signature:     membership.Signature,
	})

	if req.TailcatAddr != "" {
		_ = s.store.SavePeerAddress(membership.MemberID, req.TailcatAddr)
	}

	roomRec.RosterVersion = newRosterVersion
	_ = s.store.SaveRoomState(*roomRec)

	members, _ := s.store.ListMembers(roomRec.RoomID)
	roster := make([]room.Membership, 0, len(members))
	for _, m := range members {
		roster = append(roster, room.Membership{
			MemberID:      m.MemberID,
			RoomID:        m.RoomID,
			DevicePublic:  m.DevicePublic,
			DisplayName:   m.DisplayName,
			Status:        room.MemberStatus(m.Status),
			RosterVersion: m.RosterVersion,
			Signature:     m.Signature,
		})
	}

	peerAddrs, _ := s.store.ListPeerAddresses()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(JoinResponse{
		Status:        status,
		Membership:    &membership,
		Roster:        roster,
		PeerAddresses: peerAddrs,
	})
}

type SyncRequest struct {
	KnownVersion int64  `json:"known_version"`
	MemberID     string `json:"member_id,omitempty"`
	TailcatAddr  string `json:"tailcat_addr,omitempty"`
}

type SyncResponse struct {
	RosterVersion int64                     `json:"roster_version"`
	Members       []room.Membership         `json:"members"`
	PeerAddresses []store.PeerAddressRecord `json:"peer_addresses,omitempty"`
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	var req SyncRequest
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req)

	if req.MemberID != "" && req.TailcatAddr != "" {
		_ = s.store.SavePeerAddress(req.MemberID, req.TailcatAddr)
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil {
		http.Error(w, "no active room", http.StatusNotFound)
		return
	}

	allMembers, _ := s.store.ListMembers(roomRec.RoomID)
	updates := make([]room.Membership, 0)
	for _, m := range allMembers {
		if m.RosterVersion > req.KnownVersion {
			updates = append(updates, room.Membership{
				MemberID:      m.MemberID,
				RoomID:        m.RoomID,
				DevicePublic:  m.DevicePublic,
				DisplayName:   m.DisplayName,
				Status:        room.MemberStatus(m.Status),
				RosterVersion: m.RosterVersion,
				Signature:     m.Signature,
			})
		}
	}

	peerAddrs, _ := s.store.ListPeerAddresses()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(SyncResponse{
		RosterVersion: roomRec.RosterVersion,
		Members:       updates,
		PeerAddresses: peerAddrs,
	})
}

// Catalog Endpoint
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	roomRec, err := s.store.GetRoomState()
	if err != nil {
		http.Error(w, "no active room", http.StatusNotFound)
		return
	}

	device, err := s.store.GetDeviceIdentity()
	if err != nil {
		http.Error(w, "device identity missing", http.StatusInternalServerError)
		return
	}
	hostMemberID := "m-" + device.DevicePublic[:16]

	models, err := s.store.ListHostedModels()
	if err != nil {
		http.Error(w, "failed to query models", http.StatusInternalServerError)
		return
	}

	active, queued := s.queue.Stats()
	queueEst := active + queued

	var ads []catalog.ModelAd
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

// Inference Endpoint
type InferenceRequest struct {
	RequestID string                `json:"request_id"`
	MemberID  string                `json:"member_id"`
	ModelID   string                `json:"model_id"`
	Messages  []hosting.ChatMessage `json:"messages"`
}

func (s *Server) handleInference(w http.ResponseWriter, r *http.Request) {
	var req InferenceRequest
	// Cap HTTP body to 1 MiB per architecture specification
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "request payload too large or invalid", http.StatusBadRequest)
		return
	}

	if req.RequestID == "" || req.ModelID == "" || len(req.Messages) == 0 {
		http.Error(w, "missing required fields", http.StatusBadRequest)
		return
	}

	// Verify requester is an admitted member
	roomRec, err := s.store.GetRoomState()
	if err != nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	members, _ := s.store.ListMembers(roomRec.RoomID)
	var admitted bool
	for _, m := range members {
		if m.MemberID == req.MemberID && m.Status == string(room.StatusAdmitted) {
			admitted = true
			break
		}
	}
	if !admitted {
		http.Error(w, "unauthorized member", http.StatusForbidden)
		return
	}

	// Lookup requested model
	model, err := s.store.GetHostedModel(req.ModelID)
	if err != nil || !model.Enabled {
		http.Error(w, "model not found or unavailable", http.StatusNotFound)
		return
	}

	// Prepare SSE response
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Enqueue into fair queue
	err = s.queue.Submit(r.Context(), req.MemberID, req.RequestID, func(execCtx context.Context) error {
		return s.adapter.StreamChat(
			execCtx,
			model.EndpointURL,
			model.APIKey,
			model.ID,
			req.Messages,
			func(delta string) error {
				chunkJSON, _ := json.Marshal(map[string]string{"delta": delta})
				_, writeErr := fmt.Fprintf(w, "data: %s\n\n", string(chunkJSON))
				if writeErr != nil {
					return writeErr
				}
				flusher.Flush()
				return nil
			},
		)
	})

	if err != nil {
		if errors.Is(err, inference.ErrMemberQueueFull) {
			http.Error(w, "member queue limit exceeded", http.StatusTooManyRequests)
			return
		}
		if errors.Is(err, inference.ErrQueueOverflow) {
			http.Error(w, "host queue is full", http.StatusServiceUnavailable)
			return
		}
		// If error occurred after headers were sent, write error event
		errJSON, _ := json.Marshal(map[string]string{"error": err.Error()})
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", string(errJSON))
		flusher.Flush()
		return
	}

	// Host signs contribution receipt if neither has opted out
	optOut, _ := s.store.GetSetting("contributions_opt_out")
	device, _ := s.store.GetDeviceIdentity()
	if optOut != "true" && device != nil && len(s.deviceKey) == ed25519.PrivateKeySize {
		hostMemberID := "m-" + device.DevicePublic[:16]
		if req.MemberID != hostMemberID {
			rec := contributions.Receipt{
				RequestID:         req.RequestID,
				RoomID:            roomRec.RoomID,
				HostMemberID:      hostMemberID,
				RequesterMemberID: req.MemberID,
				Timestamp:         time.Now().Unix(),
				Completed:         true,
			}
			if err := rec.SignHost(s.deviceKey); err == nil {
				recJSON, _ := json.Marshal(rec)
				_, _ = fmt.Fprintf(w, "event: receipt\ndata: %s\n\n", string(recJSON))
			}
		}
	}

	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) handleCancelInference(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestID string `json:"request_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.RequestID != "" {
		s.queue.Cancel(body.RequestID)
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"cancelled": true})
}

type CommunitySyncRequest struct {
	RoomID        string            `json:"room_id"`
	MemberID      string            `json:"member_id"`
	KnownEventIDs []string          `json:"known_event_ids"`
	PushEvents    []community.Event `json:"push_events,omitempty"`
}

type CommunitySyncResponse struct {
	PullEvents []community.Event `json:"pull_events"`
}

func (s *Server) handleCommunitySync(w http.ResponseWriter, r *http.Request) {
	var req CommunitySyncRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512*1024)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec.RoomID != req.RoomID {
		http.Error(w, "room not found or mismatch", http.StatusBadRequest)
		return
	}

	// Verify requester is admitted
	members, _ := s.store.ListMembers(roomRec.RoomID)
	var admitted bool
	for _, m := range members {
		if m.MemberID == req.MemberID && m.Status == string(room.StatusAdmitted) {
			admitted = true
			break
		}
	}
	if !admitted {
		http.Error(w, "unauthorized: requester is not an admitted member", http.StatusForbidden)
		return
	}

	// Ingest pushed events
	for _, e := range req.PushEvents {
		if e.EventType == community.EventTombstone {
			// Tombstone requires room authority signature!
			authPubBytes, err := identity.DecodeToken(roomRec.AuthorityPublic, ed25519.PublicKeySize)
			if err != nil || e.Verify(ed25519.PublicKey(authPubBytes)) != nil {
				continue // reject forged or non-creator tombstone
			}
		} else {
			// Regular event: verify author key
			author, err := s.store.GetMember(e.AuthorMemberID)
			if err != nil || author.Status != string(room.StatusAdmitted) {
				continue
			}
			authorPubBytes, err := identity.DecodeToken(author.DevicePublic, ed25519.PublicKeySize)
			if err != nil || e.Verify(ed25519.PublicKey(authorPubBytes)) != nil {
				continue // reject invalid signature
			}
		}

		_ = s.store.SaveEvent(store.EventRecord{
			ID:               e.ID,
			RoomID:           e.RoomID,
			ChannelID:        e.ChannelID,
			AuthorMemberID:   e.AuthorMemberID,
			AuthorSeq:        e.AuthorSeq,
			EventType:        string(e.EventType),
			TargetEventID:    e.TargetEventID,
			Content:          e.Content,
			Timestamp:        e.Timestamp,
			Signature:        e.Signature,
			ReplicatedStatus: "replicated",
		})
	}

	// Collect missing events for requester
	knownMap := make(map[string]bool)
	for _, id := range req.KnownEventIDs {
		knownMap[id] = true
	}

	allEvents, _ := s.store.ListAllEvents(roomRec.RoomID)
	pullEvents := make([]community.Event, 0)
	for _, rec := range allEvents {
		if !knownMap[rec.ID] {
			pullEvents = append(pullEvents, community.Event{
				ID:             rec.ID,
				RoomID:         rec.RoomID,
				ChannelID:      rec.ChannelID,
				AuthorMemberID: rec.AuthorMemberID,
				AuthorSeq:      rec.AuthorSeq,
				EventType:      community.EventType(rec.EventType),
				TargetEventID:  rec.TargetEventID,
				Content:        rec.Content,
				Timestamp:      rec.Timestamp,
				Signature:      rec.Signature,
			})
			if len(pullEvents) >= 100 {
				break // bound page size
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(CommunitySyncResponse{
		PullEvents: pullEvents,
	})
}

func (s *Server) handleContributionsAck(w http.ResponseWriter, r *http.Request) {
	var rec contributions.Receipt
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&rec); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec.RoomID != rec.RoomID {
		http.Error(w, "room mismatch", http.StatusBadRequest)
		return
	}
	if rec.HostMemberID == rec.RequesterMemberID || !rec.Completed {
		http.Error(w, "invalid receipt", http.StatusBadRequest)
		return
	}
	hostMember, _ := s.store.GetMember(rec.HostMemberID)
	reqMember, _ := s.store.GetMember(rec.RequesterMemberID)
	if hostMember == nil || reqMember == nil {
		http.Error(w, "member not found", http.StatusBadRequest)
		return
	}
	hostPubBytes, _ := identity.DecodeToken(hostMember.DevicePublic, ed25519.PublicKeySize)
	reqPubBytes, _ := identity.DecodeToken(reqMember.DevicePublic, ed25519.PublicKeySize)
	if rec.VerifyBoth(ed25519.PublicKey(hostPubBytes), ed25519.PublicKey(reqPubBytes)) != nil {
		http.Error(w, "invalid signatures", http.StatusBadRequest)
		return
	}
	_ = s.store.SaveReceipt(store.ContributionReceiptRecord{
		RequestID:          rec.RequestID,
		RoomID:             rec.RoomID,
		HostMemberID:       rec.HostMemberID,
		RequesterMemberID:  rec.RequesterMemberID,
		Timestamp:          rec.Timestamp,
		Completed:          rec.Completed,
		HostSignature:      rec.HostSignature,
		RequesterSignature: rec.RequesterSignature,
		ReplicatedStatus:   "replicated",
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

type ContributionsSyncRequest struct {
	RoomID          string                  `json:"room_id"`
	MemberID        string                  `json:"member_id"`
	KnownRequestIDs []string                `json:"known_request_ids"`
	PushReceipts    []contributions.Receipt `json:"push_receipts,omitempty"`
}

type ContributionsSyncResponse struct {
	PullReceipts []contributions.Receipt `json:"pull_receipts"`
}

func (s *Server) handleContributionsSync(w http.ResponseWriter, r *http.Request) {
	var req ContributionsSyncRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512*1024)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec.RoomID != req.RoomID {
		http.Error(w, "room not found or mismatch", http.StatusBadRequest)
		return
	}
	members, _ := s.store.ListMembers(roomRec.RoomID)
	var admitted bool
	for _, m := range members {
		if m.MemberID == req.MemberID && m.Status == string(room.StatusAdmitted) {
			admitted = true
			break
		}
	}
	if !admitted {
		http.Error(w, "unauthorized member", http.StatusForbidden)
		return
	}

	// Ingest pushed receipts
	for _, rec := range req.PushReceipts {
		if rec.HostMemberID == rec.RequesterMemberID || !rec.Completed {
			continue
		}
		hostMember, _ := s.store.GetMember(rec.HostMemberID)
		reqMember, _ := s.store.GetMember(rec.RequesterMemberID)
		if hostMember == nil || reqMember == nil {
			continue
		}
		hostPubBytes, _ := identity.DecodeToken(hostMember.DevicePublic, ed25519.PublicKeySize)
		reqPubBytes, _ := identity.DecodeToken(reqMember.DevicePublic, ed25519.PublicKeySize)
		if rec.VerifyBoth(ed25519.PublicKey(hostPubBytes), ed25519.PublicKey(reqPubBytes)) != nil {
			continue
		}
		_ = s.store.SaveReceipt(store.ContributionReceiptRecord{
			RequestID:          rec.RequestID,
			RoomID:             rec.RoomID,
			HostMemberID:       rec.HostMemberID,
			RequesterMemberID:  rec.RequesterMemberID,
			Timestamp:          rec.Timestamp,
			Completed:          rec.Completed,
			HostSignature:      rec.HostSignature,
			RequesterSignature: rec.RequesterSignature,
			ReplicatedStatus:   "replicated",
		})
	}

	// Collect pull receipts
	knownMap := make(map[string]bool)
	for _, id := range req.KnownRequestIDs {
		knownMap[id] = true
	}
	allReceipts, _ := s.store.ListReceipts(roomRec.RoomID)
	pullReceipts := make([]contributions.Receipt, 0)
	for _, rec := range allReceipts {
		if !knownMap[rec.RequestID] {
			pullReceipts = append(pullReceipts, contributions.Receipt{
				RequestID:          rec.RequestID,
				RoomID:             rec.RoomID,
				HostMemberID:       rec.HostMemberID,
				RequesterMemberID:  rec.RequesterMemberID,
				Timestamp:          rec.Timestamp,
				Completed:          rec.Completed,
				HostSignature:      rec.HostSignature,
				RequesterSignature: rec.RequesterSignature,
			})
			if len(pullReceipts) >= 100 {
				break
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ContributionsSyncResponse{
		PullReceipts: pullReceipts,
	})
}

func (s *Server) Serve(listener net.Listener) error {
	return s.server.Serve(listener)
}

func (s *Server) Close() error {
	return s.server.Close()
}
