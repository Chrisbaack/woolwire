package localapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Chrisbaack/woolwire/internal/community"
	"github.com/Chrisbaack/woolwire/internal/identity"
	"github.com/Chrisbaack/woolwire/internal/peerapi"
	"github.com/Chrisbaack/woolwire/internal/store"
)

// handleListChannels materializes the channel list from the signed event log
// and merges it with locally created rows. Listing only local rows meant a
// channel created on one node was invisible on every other, so events for it
// arrived and were never displayed.
func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]store.ChannelRecord{})
		return
	}

	s.materializeChannels(roomRec.RoomID)

	channels, err := s.store.ListChannels(roomRec.RoomID)
	if err != nil {
		http.Error(w, "failed to query channels", http.StatusInternalServerError)
		return
	}

	// Every room has #general. It needs no channel event: the ID is fixed, so
	// all nodes agree on it without replication.
	if len(channels) == 0 {
		general := store.ChannelRecord{
			ID:          "chan-general",
			RoomID:      roomRec.RoomID,
			Name:        "general",
			Description: "General discussion for room members",
			CreatedAt:   time.Now().Unix(),
		}
		_ = s.store.SaveChannel(general)
		channels = append(channels, general)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(channels)
}

// materializeChannels folds replicated channel events into local rows.
func (s *Server) materializeChannels(roomID string) {
	records, err := s.store.ListAllEvents(roomID)
	if err != nil {
		return
	}

	events := make([]community.Event, 0, len(records))
	for _, rec := range records {
		switch rec.EventType {
		case string(community.EventChannel), string(community.EventChannelDelete):
			events = append(events, peerapi.EventFromRecord(rec))
		}
	}

	result := community.Materialize(events)
	for _, ch := range result.Channels {
		_ = s.store.SaveChannel(store.ChannelRecord{
			ID:          ch.ID,
			RoomID:      ch.RoomID,
			Name:        ch.Name,
			Description: ch.Description,
			CreatedAt:   ch.CreatedAt,
			CreatedBy:   ch.AuthorMemberID,
		})
	}
	// A withdrawal has to remove the row, not just stop adding it: the channel
	// was already stored on every node that saw it announced.
	for _, id := range result.DeletedChannels {
		_ = s.store.DeleteChannel(id)
	}
}

// generalChannelID is the room's implicit channel. It is created without an
// event — every node derives the same fixed ID — so there is no announcement
// for a deletion to undo, and the list would simply recreate it.
const generalChannelID = "chan-general"

// handleDeleteChannel withdraws a channel from the room. A member may delete a
// channel they announced; the room's creator may delete any.
func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	channelID := r.PathValue("id")
	if channelID == "" {
		http.Error(w, "channel id required", http.StatusBadRequest)
		return
	}
	if channelID == generalChannelID {
		http.Error(w, "the general channel cannot be deleted", http.StatusBadRequest)
		return
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		http.Error(w, "no active room", http.StatusBadRequest)
		return
	}

	device, err := s.store.GetDeviceIdentity()
	if err != nil || device == nil {
		http.Error(w, "device identity missing", http.StatusInternalServerError)
		return
	}
	myMemberID, err := peerapi.MemberIDForDevicePublic(device.DevicePublic)
	if err != nil {
		http.Error(w, "device identity is malformed", http.StatusInternalServerError)
		return
	}

	// Fold in anything replicated since the last read, so the author this
	// decision rests on is the one the log records rather than a stale row.
	s.materializeChannels(roomRec.RoomID)
	channels, err := s.store.ListChannels(roomRec.RoomID)
	if err != nil {
		http.Error(w, "failed to query channels", http.StatusInternalServerError)
		return
	}
	var target *store.ChannelRecord
	for i := range channels {
		if channels[i].ID == channelID {
			target = &channels[i]
			break
		}
	}
	if target == nil {
		http.Error(w, "channel not found", http.StatusNotFound)
		return
	}

	isCreator := roomRec.Role == "creator" && len(roomRec.AuthorityPrivate) > 0
	if target.CreatedBy != myMemberID && !isCreator {
		http.Error(w, "only the channel's author or the room's creator can delete it", http.StatusForbidden)
		return
	}

	seq, _ := s.store.GetLatestAuthorSeq(roomRec.RoomID, myMemberID)
	event := community.Event{
		RoomID:         roomRec.RoomID,
		ChannelID:      channelID,
		AuthorMemberID: myMemberID,
		AuthorSeq:      seq + 1,
		EventType:      community.EventChannelDelete,
		Content:        "",
		Timestamp:      time.Now().Unix(),
	}
	if err := event.Sign(ed25519.PrivateKey(device.DevicePrivate)); err != nil {
		http.Error(w, "sign channel deletion failed", http.StatusInternalServerError)
		return
	}
	if err := s.store.SaveEvent(peerapi.EventRecord(event, "local")); err != nil {
		http.Error(w, "failed to save channel deletion", http.StatusInternalServerError)
		return
	}

	s.materializeChannels(roomRec.RoomID)
	go s.SyncCommunityEvents(context.Background())

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleCreateChannel publishes a signed channel event so the channel exists
// on every node, then materializes it locally.
func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		http.Error(w, "no active room", http.StatusBadRequest)
		return
	}

	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		http.Error(w, "channel name required", http.StatusBadRequest)
		return
	}

	device, err := s.store.GetDeviceIdentity()
	if err != nil || device == nil {
		http.Error(w, "device identity missing", http.StatusInternalServerError)
		return
	}
	myMemberID, err := peerapi.MemberIDForDevicePublic(device.DevicePublic)
	if err != nil {
		http.Error(w, "device identity is malformed", http.StatusInternalServerError)
		return
	}

	cleanName := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(body.Name)), "#")

	randBytes := make([]byte, 6)
	_, _ = rand.Read(randBytes)
	channelID := "chan-" + hex.EncodeToString(randBytes)

	payload, err := json.Marshal(community.ChannelPayload{
		Name:        cleanName,
		Description: strings.TrimSpace(body.Description),
	})
	if err != nil {
		http.Error(w, "encode channel payload failed", http.StatusInternalServerError)
		return
	}

	seq, _ := s.store.GetLatestAuthorSeq(roomRec.RoomID, myMemberID)
	event := community.Event{
		RoomID:         roomRec.RoomID,
		ChannelID:      channelID,
		AuthorMemberID: myMemberID,
		AuthorSeq:      seq + 1,
		EventType:      community.EventChannel,
		Content:        string(payload),
		Timestamp:      time.Now().Unix(),
	}
	if err := event.Sign(ed25519.PrivateKey(device.DevicePrivate)); err != nil {
		http.Error(w, "sign channel event failed", http.StatusInternalServerError)
		return
	}
	if err := s.store.SaveEvent(peerapi.EventRecord(event, "local")); err != nil {
		http.Error(w, "failed to save channel event", http.StatusInternalServerError)
		return
	}

	ch := store.ChannelRecord{
		ID:          channelID,
		RoomID:      roomRec.RoomID,
		Name:        cleanName,
		Description: strings.TrimSpace(body.Description),
		CreatedAt:   event.Timestamp,
		CreatedBy:   myMemberID,
	}
	if err := s.store.SaveChannel(ch); err != nil {
		http.Error(w, "failed to save channel", http.StatusInternalServerError)
		return
	}

	go s.SyncCommunityEvents(context.Background())

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ch)
}

type EnrichedMaterializedMessage struct {
	community.MaterializedMessage
	AuthorDisplayName string `json:"author_display_name"`
}

func (s *Server) handleGetChannelMessages(w http.ResponseWriter, r *http.Request) {
	channelID := r.PathValue("id")
	if channelID == "" {
		http.Error(w, "channel id required", http.StatusBadRequest)
		return
	}

	records, err := s.store.ListEvents(channelID)
	if err != nil {
		http.Error(w, "failed to query events", http.StatusInternalServerError)
		return
	}

	events := make([]community.Event, 0, len(records))
	for _, rec := range records {
		events = append(events, peerapi.EventFromRecord(rec))
	}

	materialized := community.MaterializeEvents(events)
	enriched := make([]EnrichedMaterializedMessage, 0, len(materialized))

	// Map member display names
	roomRec, _ := s.store.GetRoomState()
	memberMap := make(map[string]string)
	if roomRec != nil {
		if members, err := s.store.ListMembers(roomRec.RoomID); err == nil {
			for _, m := range members {
				memberMap[m.MemberID] = m.DisplayName
			}
		}
	}

	for _, msg := range materialized {
		displayName := memberMap[msg.AuthorMemberID]
		if displayName == "" {
			displayName = "Member"
		}
		enriched = append(enriched, EnrichedMaterializedMessage{
			MaterializedMessage: msg,
			AuthorDisplayName:   displayName,
		})
	}
	if enriched == nil {
		enriched = []EnrichedMaterializedMessage{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(enriched)
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	channelID := r.PathValue("id")
	if channelID == "" {
		http.Error(w, "channel id required", http.StatusBadRequest)
		return
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		http.Error(w, "no active room", http.StatusBadRequest)
		return
	}

	device, err := s.store.GetDeviceIdentity()
	if err != nil {
		http.Error(w, "device identity missing", http.StatusInternalServerError)
		return
	}
	myMemberID, idErr := peerapi.MemberIDForDevicePublic(device.DevicePublic)
	if idErr != nil {
		http.Error(w, "device identity is malformed", http.StatusInternalServerError)
		return
	}
	myPrivKey := ed25519.PrivateKey(device.DevicePrivate)

	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Content) == "" {
		http.Error(w, "message content required", http.StatusBadRequest)
		return
	}

	seq, _ := s.store.GetLatestAuthorSeq(roomRec.RoomID, myMemberID)
	newSeq := seq + 1

	event := community.Event{
		RoomID:         roomRec.RoomID,
		ChannelID:      channelID,
		AuthorMemberID: myMemberID,
		AuthorSeq:      newSeq,
		EventType:      community.EventMessage,
		Content:        body.Content,
		Timestamp:      time.Now().Unix(),
	}

	if err := event.Sign(myPrivKey); err != nil {
		http.Error(w, "sign event failed", http.StatusInternalServerError)
		return
	}

	if err := s.store.SaveEvent(peerapi.EventRecord(event, "local")); err != nil {
		http.Error(w, "failed to save event", http.StatusInternalServerError)
		return
	}

	// Trigger replication to peers in background
	go s.SyncCommunityEvents(context.Background())

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(event)
}

func (s *Server) handleEditMessage(w http.ResponseWriter, r *http.Request) {
	targetID := r.PathValue("id")
	if targetID == "" {
		http.Error(w, "event id required", http.StatusBadRequest)
		return
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		http.Error(w, "no active room", http.StatusBadRequest)
		return
	}

	device, _ := s.store.GetDeviceIdentity()
	myMemberID, idErr := peerapi.MemberIDForDevicePublic(device.DevicePublic)
	if idErr != nil {
		http.Error(w, "device identity is malformed", http.StatusInternalServerError)
		return
	}
	myPrivKey := ed25519.PrivateKey(device.DevicePrivate)

	targetRec, err := s.store.GetEvent(targetID)
	if err != nil || targetRec == nil {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	if targetRec.AuthorMemberID != myMemberID {
		http.Error(w, "forbidden: cannot edit message by another author", http.StatusForbidden)
		return
	}

	var body struct {
		Content   string `json:"content"`
		ChannelID string `json:"channel_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Content) == "" {
		http.Error(w, "content required", http.StatusBadRequest)
		return
	}
	if body.ChannelID == "" {
		body.ChannelID = targetRec.ChannelID
	}

	seq, _ := s.store.GetLatestAuthorSeq(roomRec.RoomID, myMemberID)
	newSeq := seq + 1

	event := community.Event{
		RoomID:         roomRec.RoomID,
		ChannelID:      body.ChannelID,
		AuthorMemberID: myMemberID,
		AuthorSeq:      newSeq,
		EventType:      community.EventEdit,
		TargetEventID:  targetID,
		Content:        body.Content,
		Timestamp:      time.Now().Unix(),
	}

	if err := event.Sign(myPrivKey); err != nil {
		http.Error(w, "sign edit event failed", http.StatusInternalServerError)
		return
	}

	_ = s.store.SaveEvent(peerapi.EventRecord(event, "local"))

	go s.SyncCommunityEvents(context.Background())

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(event)
}

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	targetID := r.PathValue("id")
	if targetID == "" {
		http.Error(w, "event id required", http.StatusBadRequest)
		return
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		http.Error(w, "no active room", http.StatusBadRequest)
		return
	}

	device, _ := s.store.GetDeviceIdentity()
	myMemberID, idErr := peerapi.MemberIDForDevicePublic(device.DevicePublic)
	if idErr != nil {
		http.Error(w, "device identity is malformed", http.StatusInternalServerError)
		return
	}
	myPrivKey := ed25519.PrivateKey(device.DevicePrivate)

	targetRec, err := s.store.GetEvent(targetID)
	if err != nil || targetRec == nil {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}
	if targetRec.AuthorMemberID != myMemberID {
		http.Error(w, "forbidden: cannot delete message by another author", http.StatusForbidden)
		return
	}

	var body struct {
		ChannelID string `json:"channel_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.ChannelID == "" {
		body.ChannelID = targetRec.ChannelID
	}

	seq, _ := s.store.GetLatestAuthorSeq(roomRec.RoomID, myMemberID)
	newSeq := seq + 1

	event := community.Event{
		RoomID:         roomRec.RoomID,
		ChannelID:      body.ChannelID,
		AuthorMemberID: myMemberID,
		AuthorSeq:      newSeq,
		EventType:      community.EventDelete,
		TargetEventID:  targetID,
		Content:        "deleted",
		Timestamp:      time.Now().Unix(),
	}

	if err := event.Sign(myPrivKey); err != nil {
		http.Error(w, "sign delete event failed", http.StatusInternalServerError)
		return
	}

	_ = s.store.SaveEvent(peerapi.EventRecord(event, "local"))

	go s.SyncCommunityEvents(context.Background())

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleModerateMessage(w http.ResponseWriter, r *http.Request) {
	targetID := r.PathValue("id")
	if targetID == "" {
		http.Error(w, "event id required", http.StatusBadRequest)
		return
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil || roomRec.Role != "creator" || len(roomRec.AuthorityPrivate) == 0 {
		http.Error(w, "only creator can moderate messages", http.StatusForbidden)
		return
	}

	var body struct {
		ChannelID string `json:"channel_id"`
		Reason    string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Reason == "" {
		body.Reason = "inappropriate content"
	}
	targetRec, _ := s.store.GetEvent(targetID)
	if targetRec != nil && body.ChannelID == "" {
		body.ChannelID = targetRec.ChannelID
	}

	device, _ := s.store.GetDeviceIdentity()
	myMemberID, idErr := peerapi.MemberIDForDevicePublic(device.DevicePublic)
	if idErr != nil {
		http.Error(w, "device identity is malformed", http.StatusInternalServerError)
		return
	}
	authorityPriv := ed25519.PrivateKey(roomRec.AuthorityPrivate)

	seq, _ := s.store.GetLatestAuthorSeq(roomRec.RoomID, myMemberID)
	newSeq := seq + 1

	event := community.Event{
		RoomID:         roomRec.RoomID,
		ChannelID:      body.ChannelID,
		AuthorMemberID: myMemberID,
		AuthorSeq:      newSeq,
		EventType:      community.EventTombstone,
		TargetEventID:  targetID,
		Content:        body.Reason,
		Timestamp:      time.Now().Unix(),
	}

	// Signed with room authority key!
	if err := event.Sign(authorityPriv); err != nil {
		http.Error(w, "sign tombstone failed", http.StatusInternalServerError)
		return
	}

	_ = s.store.SaveEvent(peerapi.EventRecord(event, "local"))

	go s.SyncCommunityEvents(context.Background())

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "tombstone_id": event.ID})
}

func (s *Server) handleUpdateReadState(w http.ResponseWriter, r *http.Request) {
	channelID := r.PathValue("id")
	if channelID == "" {
		http.Error(w, "channel id required", http.StatusBadRequest)
		return
	}

	var body struct {
		Muted bool `json:"muted"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	if err := s.store.SaveReadState(channelID, time.Now().Unix(), body.Muted); err != nil {
		http.Error(w, "failed to save read state", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// SyncCommunityEvents exchanges events with every known peer using per-author
// cursors. The previous shape sent every known event ID in each request, which
// grew without bound and made sync fail permanently once a room passed roughly
// fifteen thousand events.
func (s *Server) SyncCommunityEvents(ctx context.Context) {
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
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

	authorityBytes, err := identity.DecodeToken(roomRec.AuthorityPublic, ed25519.PublicKeySize)
	if err != nil {
		return
	}
	authority := ed25519.PublicKey(authorityBytes)

	pushEvents := s.pendingLocalEvents(roomRec.RoomID)

	for _, pa := range s.knownPeers() {
		if pa.MemberID == myMemberID {
			continue
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		s.syncEventsWithPeer(ctx, roomRec.RoomID, authority, pa, pushEvents)
	}

	// Only mark pushed events replicated once a peer has been offered them.
	for _, pe := range pushEvents {
		if rec, err := s.store.GetEvent(pe.ID); err == nil && rec != nil {
			rec.ReplicatedStatus = "replicated"
			_ = s.store.SaveEvent(*rec)
		}
	}

	// Materialize channels after syncing so any newly pulled channel events
	// are immediately available in the local channels table.
	s.materializeChannels(roomRec.RoomID)
}

func (s *Server) pendingLocalEvents(roomID string) []community.Event {
	all, _ := s.store.ListAllEvents(roomID)
	var pending []community.Event
	for _, rec := range all {
		if rec.ReplicatedStatus != "local" {
			continue
		}
		pending = append(pending, peerapi.EventFromRecord(rec))
	}
	return pending
}

// syncEventsWithPeer pages until the peer reports nothing further. Each round
// trip carries only the cursors, so the request body stays bounded no matter
// how large the room's history grows.
func (s *Server) syncEventsWithPeer(
	ctx context.Context,
	roomID string,
	authority ed25519.PublicKey,
	peer store.PeerAddressRecord,
	pushEvents []community.Event,
) {
	const maxPages = 200

	for page := 0; page < maxPages; page++ {
		dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)

		cursors, _ := s.store.AuthorCursors(roomID)
		gaps, _ := s.store.AuthorGaps(roomID)
		knownIDs, _ := s.store.ListEventIDs(roomID)
		if len(knownIDs) > 500 {
			knownIDs = nil
		}
		req := peerapi.CommunitySyncRequest{
			RoomID:        roomID,
			Cursors:       cursors,
			MissingRanges: gaps,
			KnownIDs:      knownIDs,
		}
		if page == 0 {
			req.PushEvents = pushEvents
		}

		var resp peerapi.CommunitySyncResponse
		err := s.peerJSON(dialCtx, peer.MemberID, peer.TailcatAddr, "POST", "/peer/v1/community/sync", req, &resp)
		cancel()
		if err != nil {
			return
		}

		applied := 0
		for _, pe := range resp.PullEvents {
			if !peerapi.AcceptInboundEvent(s.store, pe, authority) {
				continue
			}
			if err := s.store.SaveEvent(peerapi.EventRecord(pe, "replicated")); err == nil {
				applied++
			}
		}

		// An empty page, or a page that advanced nothing, means either caught
		// up or stuck; either way there is no progress left to make here.
		if len(resp.PullEvents) == 0 || applied == 0 {
			return
		}
	}
}
