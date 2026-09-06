package localapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/cbaack/woolwire/internal/community"
	"github.com/cbaack/woolwire/internal/identity"
	"github.com/cbaack/woolwire/internal/peerapi"
	"github.com/cbaack/woolwire/internal/room"
	"github.com/cbaack/woolwire/internal/store"
)

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]store.ChannelRecord{})
		return
	}

	channels, err := s.store.ListChannels(roomRec.RoomID)
	if err != nil {
		http.Error(w, "failed to query channels", http.StatusInternalServerError)
		return
	}

	// Auto-create default #general channel if none exists
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

	cleanName := strings.ToLower(strings.TrimSpace(body.Name))
	cleanName = strings.TrimPrefix(cleanName, "#")

	randBytes := make([]byte, 6)
	_, _ = rand.Read(randBytes)
	id := "chan-" + hex.EncodeToString(randBytes)

	ch := store.ChannelRecord{
		ID:          id,
		RoomID:      roomRec.RoomID,
		Name:        cleanName,
		Description: strings.TrimSpace(body.Description),
		CreatedAt:   time.Now().Unix(),
	}

	if err := s.store.SaveChannel(ch); err != nil {
		http.Error(w, "failed to save channel", http.StatusInternalServerError)
		return
	}

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
		events = append(events, community.Event{
			ID:             rec.ID,
			RoomID:         rec.RoomID,
			ChannelID:      rec.ChannelID,
			AuthorMemberID: rec.AuthorMemberID,
			AuthorSeq:      rec.AuthorSeq,
			EventType:      community.EventType(rec.EventType),
			TargetEventID:  rec.TargetEventID,
			Content:        rec.Content,
			Timestamp:        rec.Timestamp,
			Signature:        rec.Signature,
			ReplicatedStatus: rec.ReplicatedStatus,
		})
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
	myMemberID := "m-" + device.DevicePublic[:16]
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

	rec := store.EventRecord{
		ID:               event.ID,
		RoomID:           event.RoomID,
		ChannelID:        event.ChannelID,
		AuthorMemberID:   event.AuthorMemberID,
		AuthorSeq:        event.AuthorSeq,
		EventType:        string(event.EventType),
		Content:          event.Content,
		Timestamp:        event.Timestamp,
		Signature:        event.Signature,
		ReplicatedStatus: "local",
	}

	if err := s.store.SaveEvent(rec); err != nil {
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
	myMemberID := "m-" + device.DevicePublic[:16]
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

	_ = s.store.SaveEvent(store.EventRecord{
		ID:               event.ID,
		RoomID:           event.RoomID,
		ChannelID:        event.ChannelID,
		AuthorMemberID:   event.AuthorMemberID,
		AuthorSeq:        event.AuthorSeq,
		EventType:        string(event.EventType),
		TargetEventID:    event.TargetEventID,
		Content:          event.Content,
		Timestamp:        event.Timestamp,
		Signature:        event.Signature,
		ReplicatedStatus: "local",
	})

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
	myMemberID := "m-" + device.DevicePublic[:16]
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

	_ = s.store.SaveEvent(store.EventRecord{
		ID:               event.ID,
		RoomID:           event.RoomID,
		ChannelID:        event.ChannelID,
		AuthorMemberID:   event.AuthorMemberID,
		AuthorSeq:        event.AuthorSeq,
		EventType:        string(event.EventType),
		TargetEventID:    event.TargetEventID,
		Content:          event.Content,
		Timestamp:        event.Timestamp,
		Signature:        event.Signature,
		ReplicatedStatus: "local",
	})

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
	myMemberID := "m-" + device.DevicePublic[:16]
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

	_ = s.store.SaveEvent(store.EventRecord{
		ID:               event.ID,
		RoomID:           event.RoomID,
		ChannelID:        event.ChannelID,
		AuthorMemberID:   event.AuthorMemberID,
		AuthorSeq:        event.AuthorSeq,
		EventType:        string(event.EventType),
		TargetEventID:    event.TargetEventID,
		Content:          event.Content,
		Timestamp:        event.Timestamp,
		Signature:        event.Signature,
		ReplicatedStatus: "local",
	})

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

func (s *Server) SyncCommunityEvents(ctx context.Context) {
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		return
	}

	device, err := s.store.GetDeviceIdentity()
	if err != nil {
		return
	}
	myMemberID := "m-" + device.DevicePublic[:16]

	allEvents, _ := s.store.ListAllEvents(roomRec.RoomID)
	knownIDs := make([]string, 0, len(allEvents))
	var pushEvents []community.Event
	for _, rec := range allEvents {
		knownIDs = append(knownIDs, rec.ID)
		if rec.ReplicatedStatus == "local" {
			pushEvents = append(pushEvents, community.Event{
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
		}
	}

	peerAddrs, _ := s.store.ListPeerAddresses()
	for _, pa := range peerAddrs {
		if pa.MemberID == myMemberID {
			continue
		}

		dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		conn, dialErr := s.trans.Dial(dialCtx, pa.TailcatAddr, s.peerPort)
		if dialErr != nil {
			cancel()
			continue
		}

		reqPayload := peerapi.CommunitySyncRequest{
			RoomID:        roomRec.RoomID,
			MemberID:      myMemberID,
			KnownEventIDs: knownIDs,
			PushEvents:    pushEvents,
		}
		b, _ := json.Marshal(reqPayload)

		httpReq, _ := http.NewRequestWithContext(dialCtx, "POST", "http://woolwire-peer/peer/v1/community/sync", bytes.NewReader(b))
		httpReq.Header.Set("Content-Type", "application/json")
		_ = httpReq.Write(conn)

		resp, readErr := http.ReadResponse(bufio.NewReader(conn), httpReq)
		if readErr == nil {
			if resp.StatusCode == http.StatusOK {
				var syncResp peerapi.CommunitySyncResponse
				if json.NewDecoder(resp.Body).Decode(&syncResp) == nil {
					for _, pe := range syncResp.PullEvents {
						if pe.EventType == community.EventTombstone {
							authPubBytes, err := identity.DecodeToken(roomRec.AuthorityPublic, ed25519.PublicKeySize)
							if err != nil || pe.Verify(ed25519.PublicKey(authPubBytes)) != nil {
								continue
							}
						} else {
							author, err := s.store.GetMember(pe.AuthorMemberID)
							if err != nil || author.Status != string(room.StatusAdmitted) {
								continue
							}
							authorPubBytes, err := identity.DecodeToken(author.DevicePublic, ed25519.PublicKeySize)
							if err != nil || pe.Verify(ed25519.PublicKey(authorPubBytes)) != nil {
								continue
							}
						}

						// Save pulled event
						_ = s.store.SaveEvent(store.EventRecord{
							ID:               pe.ID,
							RoomID:           pe.RoomID,
							ChannelID:        pe.ChannelID,
							AuthorMemberID:   pe.AuthorMemberID,
							AuthorSeq:        pe.AuthorSeq,
							EventType:        string(pe.EventType),
							TargetEventID:    pe.TargetEventID,
							Content:          pe.Content,
							Timestamp:        pe.Timestamp,
							Signature:        pe.Signature,
							ReplicatedStatus: "replicated",
						})
					}
				}
			}
			resp.Body.Close()
		}
		conn.Close()
		cancel()
	}

	// Mark pushed events as replicated
	for _, pe := range pushEvents {
		rec, _ := s.store.GetEvent(pe.ID)
		if rec != nil {
			rec.ReplicatedStatus = "replicated"
			_ = s.store.SaveEvent(*rec)
		}
	}
}
