package peerapi

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"time"

	"github.com/cbaack/woolwire/internal/community"
	"github.com/cbaack/woolwire/internal/contributions"
	"github.com/cbaack/woolwire/internal/identity"
	"github.com/cbaack/woolwire/internal/peerauth"
	"github.com/cbaack/woolwire/internal/room"
	"github.com/cbaack/woolwire/internal/store"
)

const (
	// syncBodyLimit bounds one sync request. With per-author cursors the
	// request no longer grows with the event count, so this is a guard against
	// a hostile peer rather than a ceiling the honest path approaches.
	syncBodyLimit = 512 * 1024
	// syncPageSize bounds one response page. The caller loops on the cursors it
	// gets back until a page comes up empty.
	syncPageSize = 500
	// eventFutureSkew is how far ahead of local time an inbound event's
	// author-controlled timestamp may sit before it is refused. Without it a
	// future-dated event never ages out of the retention window.
	eventFutureSkew = 5 * time.Minute
	// eventRetentionWindow matches the documented 30-day community retention.
	eventRetentionWindow = 30 * 24 * time.Hour
)

// CommunitySyncRequest replaces the full known-ID list with one cursor per
// author. The old shape grew without bound and made sync fail permanently
// once the room passed roughly fifteen thousand events.
type CommunitySyncRequest struct {
	RoomID     string            `json:"room_id"`
	Cursors    map[string]int64  `json:"cursors"`
	PushEvents []community.Event `json:"push_events,omitempty"`
}

type CommunitySyncResponse struct {
	PullEvents []community.Event `json:"pull_events"`
	// NextCursors is the high-water mark per author in this page. An empty
	// PullEvents means the caller is caught up.
	NextCursors map[string]int64 `json:"next_cursors"`
}

func (s *Server) handleCommunitySync(w http.ResponseWriter, r *http.Request, caller peerauth.Identity) {
	var req CommunitySyncRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, syncBodyLimit)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil || roomRec.RoomID != req.RoomID {
		http.Error(w, "room not found or mismatch", http.StatusBadRequest)
		return
	}

	authorityPub, authErr := identity.DecodeToken(roomRec.AuthorityPublic, ed25519.PublicKeySize)

	for _, e := range req.PushEvents {
		if authErr != nil {
			break
		}
		if !s.acceptInboundEvent(e, ed25519.PublicKey(authorityPub)) {
			continue
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

	records, _ := s.store.ListEventsAfterCursor(roomRec.RoomID, req.Cursors, syncPageSize)
	pullEvents := make([]community.Event, 0, len(records))
	nextCursors := make(map[string]int64, len(records))
	for _, rec := range records {
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
		if rec.AuthorSeq > nextCursors[rec.AuthorMemberID] {
			nextCursors[rec.AuthorMemberID] = rec.AuthorSeq
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(CommunitySyncResponse{
		PullEvents:  pullEvents,
		NextCursors: nextCursors,
	})
}

// AcceptInboundEvent decides whether a replicated event may be stored. It is
// exported so the local API applies exactly the same rule to events it pulls.
func AcceptInboundEvent(s *store.Store, e community.Event, authority ed25519.PublicKey) bool {
	if !withinRetentionWindow(e.Timestamp, time.Now()) {
		return false
	}

	if e.EventType == community.EventTombstone {
		// Only the room authority may tombstone; a member-signed one is forged.
		return e.Verify(authority) == nil
	}

	author, err := s.GetMember(e.AuthorMemberID)
	if err != nil || author == nil || author.Status != string(room.StatusAdmitted) {
		return false
	}
	authorPub, err := identity.DecodeToken(author.DevicePublic, ed25519.PublicKeySize)
	if err != nil {
		return false
	}
	if e.Verify(ed25519.PublicKey(authorPub)) != nil {
		return false
	}
	// A channel event carries room-wide structure, so it is held to the same
	// author check as a message but is additionally required to name itself.
	if e.EventType == community.EventChannel && e.ChannelID == "" {
		return false
	}
	return true
}

func (s *Server) acceptInboundEvent(e community.Event, authority ed25519.PublicKey) bool {
	return AcceptInboundEvent(s.store, e, authority)
}

// withinRetentionWindow refuses events dated implausibly far in the future or
// already past the retention horizon. Timestamps are author-controlled, so an
// unchecked one would either never expire or arrive already expired.
func withinRetentionWindow(timestamp int64, now time.Time) bool {
	if timestamp <= 0 {
		return false
	}
	ts := time.Unix(timestamp, 0)
	if ts.After(now.Add(eventFutureSkew)) {
		return false
	}
	if ts.Before(now.Add(-eventRetentionWindow)) {
		return false
	}
	return true
}

type ContributionsSyncRequest struct {
	RoomID       string                  `json:"room_id"`
	Cursors      map[string]int64        `json:"cursors"`
	PushReceipts []contributions.Receipt `json:"push_receipts,omitempty"`
}

type ContributionsSyncResponse struct {
	PullReceipts []contributions.Receipt `json:"pull_receipts"`
	NextCursors  map[string]int64        `json:"next_cursors"`
}

func (s *Server) handleContributionsAck(w http.ResponseWriter, r *http.Request, caller peerauth.Identity) {
	var rec contributions.Receipt
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&rec); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil || roomRec.RoomID != rec.RoomID {
		http.Error(w, "room mismatch", http.StatusBadRequest)
		return
	}
	// The acknowledging peer must be the receipt's requester. Otherwise any
	// member could inject counter-signed receipts naming other members.
	if rec.RequesterMemberID != caller.MemberID {
		http.Error(w, "receipt does not belong to the calling member", http.StatusForbidden)
		return
	}
	if !VerifyReceipt(s.store, rec) {
		http.Error(w, "invalid receipt", http.StatusBadRequest)
		return
	}

	_ = s.store.SaveReceipt(receiptRecord(rec, "replicated"))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleContributionsSync(w http.ResponseWriter, r *http.Request, caller peerauth.Identity) {
	var req ContributionsSyncRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, syncBodyLimit)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil || roomRec.RoomID != req.RoomID {
		http.Error(w, "room not found or mismatch", http.StatusBadRequest)
		return
	}

	for _, rec := range req.PushReceipts {
		if !VerifyReceipt(s.store, rec) {
			continue
		}
		_ = s.store.SaveReceipt(receiptRecord(rec, "replicated"))
	}

	records, _ := s.store.ListReceiptsAfterCursor(roomRec.RoomID, req.Cursors, syncPageSize)
	pullReceipts := make([]contributions.Receipt, 0, len(records))
	nextCursors := make(map[string]int64, len(records))
	for _, rec := range records {
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
		key := rec.HostMemberID + "/" + rec.RequesterMemberID
		if rec.Timestamp > nextCursors[key] {
			nextCursors[key] = rec.Timestamp
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ContributionsSyncResponse{
		PullReceipts: pullReceipts,
		NextCursors:  nextCursors,
	})
}

// VerifyReceipt checks both signatures against the roster. It is exported so
// the local API's pull path applies the identical rule.
func VerifyReceipt(s *store.Store, rec contributions.Receipt) bool {
	if rec.HostMemberID == rec.RequesterMemberID || !rec.Completed {
		return false
	}
	hostMember, err := s.GetMember(rec.HostMemberID)
	if err != nil || hostMember == nil {
		return false
	}
	reqMember, err := s.GetMember(rec.RequesterMemberID)
	if err != nil || reqMember == nil {
		return false
	}
	hostPub, err := identity.DecodeToken(hostMember.DevicePublic, ed25519.PublicKeySize)
	if err != nil {
		return false
	}
	reqPub, err := identity.DecodeToken(reqMember.DevicePublic, ed25519.PublicKeySize)
	if err != nil {
		return false
	}
	return rec.VerifyBoth(ed25519.PublicKey(hostPub), ed25519.PublicKey(reqPub)) == nil
}

func receiptRecord(rec contributions.Receipt, status string) store.ContributionReceiptRecord {
	return store.ContributionReceiptRecord{
		RequestID:          rec.RequestID,
		RoomID:             rec.RoomID,
		HostMemberID:       rec.HostMemberID,
		RequesterMemberID:  rec.RequesterMemberID,
		Timestamp:          rec.Timestamp,
		Completed:          rec.Completed,
		HostSignature:      rec.HostSignature,
		RequesterSignature: rec.RequesterSignature,
		ReplicatedStatus:   status,
	}
}
