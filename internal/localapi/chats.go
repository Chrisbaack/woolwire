package localapi

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Chrisbaack/woolwire/internal/contributions"
	"github.com/Chrisbaack/woolwire/internal/hosting"
	"github.com/Chrisbaack/woolwire/internal/inference"
	"github.com/Chrisbaack/woolwire/internal/peerapi"
	"github.com/Chrisbaack/woolwire/internal/sse"
	"github.com/Chrisbaack/woolwire/internal/store"
)

// maxNoSaveTurns bounds the in-memory transcript of a privacy-mode chat so a
// long session cannot grow without limit. Older turns fall off the front.
const maxNoSaveTurns = 100

func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	convs, err := s.store.ListConversations()
	if err != nil {
		http.Error(w, "failed to query conversations", http.StatusInternalServerError)
		return
	}
	if convs == nil {
		convs = []store.ConversationRecord{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(convs)
}

func (s *Server) handleCreateChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title  string `json:"title"`
		NoSave bool   `json:"no_save"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = "New Chat"
	}

	randBytes := make([]byte, 8)
	_, _ = rand.Read(randBytes)
	id := "chat-" + hex.EncodeToString(randBytes)

	rec := store.ConversationRecord{
		ID:        id,
		Title:     title,
		NoSave:    body.NoSave,
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
	}

	if err := s.store.SaveConversation(rec); err != nil {
		http.Error(w, "failed to create conversation", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

func (s *Server) handleGetChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "conversation id required", http.StatusBadRequest)
		return
	}

	conv, err := s.store.GetConversation(id)
	if err != nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	msgs, err := s.store.ListMessages(id)
	if err != nil {
		http.Error(w, "failed to query messages", http.StatusInternalServerError)
		return
	}

	// Only the visible branch is returned. Superseded takes stay in the store
	// and are reachable by switching variants, but they are not part of the
	// transcript.
	branch := activeBranch(msgs)
	out := make([]map[string]any, 0, len(branch))
	for _, b := range branch {
		out = append(out, map[string]any{
			"ID":             b.ID,
			"ConversationID": b.ConversationID,
			"Role":           b.Role,
			"Content":        b.Content,
			"HostMemberID":   b.HostMemberID,
			"ModelID":        b.ModelID,
			"CreatedAt":      b.CreatedAt,
			"ParentID":       b.ParentID,
			"VariantIndex":   b.VariantIndex,
			"VariantCount":   b.VariantCount,
			"SiblingIDs":     b.SiblingIDs,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"conversation": conv,
		"messages":     out,
	})
}

// handleSelectVariant switches which alternative of a turn is on the visible
// branch. Everything below it in the tree comes back with it, so paging
// between takes restores each one's follow-up conversation.
func (s *Server) handleSelectVariant(w http.ResponseWriter, r *http.Request) {
	convID := r.PathValue("id")
	msgID := r.PathValue("msgID")
	if convID == "" || msgID == "" {
		http.Error(w, "conversation id and message id required", http.StatusBadRequest)
		return
	}
	if _, err := s.store.GetConversation(convID); err != nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}
	if err := s.store.ActivateMessage(convID, msgID); err != nil {
		http.Error(w, "message not found in this conversation", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "conversation id required", http.StatusBadRequest)
		return
	}

	if err := s.store.DeleteConversation(id); err != nil {
		http.Error(w, "failed to delete conversation", http.StatusInternalServerError)
		return
	}
	s.forgetNoSaveTurns(id)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// noSaveHistory returns the in-memory transcript for a privacy-mode chat.
// Keeping it here is what lets no-save conversations stay multi-turn: sending
// only the current turn made privacy mode also mean "the model forgets
// everything you just said".
func (s *Server) noSaveHistory(convID string) []hosting.ChatMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]hosting.ChatMessage(nil), s.noSaveTurns[convID]...)
}

func (s *Server) appendNoSaveTurn(convID string, msgs ...hosting.ChatMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	turns := append(s.noSaveTurns[convID], msgs...)
	if len(turns) > maxNoSaveTurns {
		turns = append([]hosting.ChatMessage(nil), turns[len(turns)-maxNoSaveTurns:]...)
	}
	s.noSaveTurns[convID] = turns
}

func (s *Server) forgetNoSaveTurns(convID string) {
	s.mu.Lock()
	delete(s.noSaveTurns, convID)
	s.mu.Unlock()
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	convID := r.PathValue("id")
	if convID == "" {
		http.Error(w, "conversation id required", http.StatusBadRequest)
		return
	}

	conv, err := s.store.GetConversation(convID)
	if err != nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	var body struct {
		Content      string `json:"content"`
		HostMemberID string `json:"host_member_id"`
		ModelID      string `json:"model_id"`
		// ParentID grafts this turn onto a specific point in the tree instead
		// of the tip of the visible branch.
		ParentID string `json:"parent_id"`
		Thinking string `json:"thinking"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	body.Content = strings.TrimSpace(body.Content)
	if body.Content == "" || body.ModelID == "" || body.HostMemberID == "" {
		http.Error(w, "content, host_member_id, and model_id are required", http.StatusBadRequest)
		return
	}

	// Offline / stale check: verify model is currently offered and not offline
	ad, ok := s.catalog.Get(body.HostMemberID, body.ModelID)
	if !ok || ad.Availability == "offline" {
		http.Error(w, "selected model is offline or host is unreachable", http.StatusServiceUnavailable)
		return
	}

	device, err := s.store.GetDeviceIdentity()
	if err != nil {
		http.Error(w, "device identity missing", http.StatusInternalServerError)
		return
	}
	myMemberID, err := peerapi.MemberIDForDevicePublic(device.DevicePublic)
	if err != nil {
		http.Error(w, "device identity is malformed", http.StatusInternalServerError)
		return
	}

	// A turn hangs off the tip of the visible branch unless the caller names a
	// parent, which is how a regenerate or an edit grafts an alternative onto
	// an earlier point without disturbing what is already there.
	parentID := strings.TrimSpace(body.ParentID)
	if !conv.NoSave && parentID == "" {
		if existing, lErr := s.store.ListMessages(convID); lErr == nil {
			if branch := activeBranch(existing); len(branch) > 0 {
				parentID = branch[len(branch)-1].ID
			}
		}
	}

	now := time.Now().Unix()
	userMsgIDBytes := make([]byte, 8)
	_, _ = rand.Read(userMsgIDBytes)
	userMsgID := "msg-" + hex.EncodeToString(userMsgIDBytes)

	userTurn := hosting.ChatMessage{Role: "user", Content: body.Content}

	var chatMsgs []hosting.ChatMessage
	if conv.NoSave {
		chatMsgs = append(s.noSaveHistory(convID), userTurn)
	} else {
		_ = s.store.SaveMessage(store.MessageRecord{
			ID:             userMsgID,
			ConversationID: convID,
			Role:           "user",
			Content:        body.Content,
			HostMemberID:   body.HostMemberID,
			ModelID:        body.ModelID,
			CreatedAt:      now,
			ParentID:       parentID,
		})
		chatMsgs = s.branchContext(convID, userMsgID)
	}

	thinking, thinkingOK := s.resolveThinking(w, body.Thinking)
	if !thinkingOK {
		return
	}

	s.streamGeneration(w, r, generation{
		conv:         conv,
		hostMemberID: body.HostMemberID,
		thinking:     thinking,
		modelID:      body.ModelID,
		myMemberID:   myMemberID,
		history:      chatMsgs,
		parentID:     userMsgID,
		noSaveTurns:  []hosting.ChatMessage{userTurn},
	})
}

// handleRegenerateMessage produces another answer for a turn already taken.
// The replaced answer is kept as a sibling, so both remain readable.
func (s *Server) handleRegenerateMessage(w http.ResponseWriter, r *http.Request) {
	convID := r.PathValue("id")
	msgID := r.PathValue("msgID")
	if convID == "" || msgID == "" {
		http.Error(w, "conversation id and message id required", http.StatusBadRequest)
		return
	}

	conv, err := s.store.GetConversation(convID)
	if err != nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}
	if conv.NoSave {
		http.Error(w, "regenerate is not available in a no-save chat: nothing is stored to branch from", http.StatusBadRequest)
		return
	}

	var body struct {
		HostMemberID string `json:"host_member_id"`
		ModelID      string `json:"model_id"`
		Thinking     string `json:"thinking"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	msgs, err := s.store.ListMessages(convID)
	if err != nil {
		http.Error(w, "failed to query messages", http.StatusInternalServerError)
		return
	}
	var target *store.MessageRecord
	for i := range msgs {
		if msgs[i].ID == msgID {
			target = &msgs[i]
			break
		}
	}
	if target == nil {
		http.Error(w, "message not found in this conversation", http.StatusNotFound)
		return
	}
	if target.Role != "assistant" {
		http.Error(w, "only an assistant message can be regenerated", http.StatusBadRequest)
		return
	}

	// Fall back to the model that produced the original answer.
	hostMemberID := strings.TrimSpace(body.HostMemberID)
	modelID := strings.TrimSpace(body.ModelID)
	if hostMemberID == "" {
		hostMemberID = target.HostMemberID
	}
	if modelID == "" {
		modelID = target.ModelID
	}

	myMemberID, ok := s.resolveSelf(w)
	if !ok {
		return
	}
	if !s.modelIsServable(w, hostMemberID, modelID) {
		return
	}

	// Context is the branch the replaced answer sits on, up to its parent --
	// the answer itself is not shown to the model.
	history := s.branchContext(convID, target.ParentID)

	thinking, thinkingOK := s.resolveThinking(w, body.Thinking)
	if !thinkingOK {
		return
	}

	s.streamGeneration(w, r, generation{
		conv:         conv,
		hostMemberID: hostMemberID,
		thinking:     thinking,
		modelID:      modelID,
		myMemberID:   myMemberID,
		history:      history,
		parentID:     target.ParentID,
	})
}

// handleEditMessage re-asks a question with new wording. The edit becomes a
// sibling of the original, and its answer is generated underneath it, so the
// original question and everything it led to stay intact on the other branch.
func (s *Server) handleEditChatMessage(w http.ResponseWriter, r *http.Request) {
	convID := r.PathValue("id")
	msgID := r.PathValue("msgID")
	if convID == "" || msgID == "" {
		http.Error(w, "conversation id and message id required", http.StatusBadRequest)
		return
	}

	conv, err := s.store.GetConversation(convID)
	if err != nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}
	if conv.NoSave {
		http.Error(w, "editing is not available in a no-save chat: nothing is stored to branch from", http.StatusBadRequest)
		return
	}

	var body struct {
		Content      string `json:"content"`
		HostMemberID string `json:"host_member_id"`
		ModelID      string `json:"model_id"`
		Thinking     string `json:"thinking"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	body.Content = strings.TrimSpace(body.Content)
	if body.Content == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}

	msgs, err := s.store.ListMessages(convID)
	if err != nil {
		http.Error(w, "failed to query messages", http.StatusInternalServerError)
		return
	}
	var target *store.MessageRecord
	for i := range msgs {
		if msgs[i].ID == msgID {
			target = &msgs[i]
			break
		}
	}
	if target == nil {
		http.Error(w, "message not found in this conversation", http.StatusNotFound)
		return
	}
	if target.Role != "user" {
		http.Error(w, "only your own message can be edited", http.StatusBadRequest)
		return
	}

	hostMemberID := strings.TrimSpace(body.HostMemberID)
	modelID := strings.TrimSpace(body.ModelID)
	if hostMemberID == "" {
		hostMemberID = target.HostMemberID
	}
	if modelID == "" {
		modelID = target.ModelID
	}

	myMemberID, ok := s.resolveSelf(w)
	if !ok {
		return
	}
	if !s.modelIsServable(w, hostMemberID, modelID) {
		return
	}

	editIDBytes := make([]byte, 8)
	_, _ = rand.Read(editIDBytes)
	editedID := "msg-" + hex.EncodeToString(editIDBytes)
	edited := store.MessageRecord{
		ID:             editedID,
		ConversationID: convID,
		Role:           "user",
		Content:        body.Content,
		HostMemberID:   hostMemberID,
		ModelID:        modelID,
		CreatedAt:      time.Now().Unix(),
		ParentID:       target.ParentID,
	}

	// Context is the branch up to the question being replaced, with the new
	// wording in its place. The edit is not stored yet, so it is appended to
	// the history rather than read back out of the tree.
	history := append(
		s.branchContext(convID, target.ParentID),
		hosting.ChatMessage{Role: "user", Content: body.Content},
	)

	thinking, thinkingOK := s.resolveThinking(w, body.Thinking)
	if !thinkingOK {
		return
	}

	s.streamGeneration(w, r, generation{
		conv:         conv,
		hostMemberID: hostMemberID,
		thinking:     thinking,
		modelID:      modelID,
		myMemberID:   myMemberID,
		history:      history,
		pendingUser:  &edited,
	})
}

// resolveSelf reports this node's member id, writing the error response
// itself when the device identity is unusable.
func (s *Server) resolveSelf(w http.ResponseWriter) (string, bool) {
	device, err := s.store.GetDeviceIdentity()
	if err != nil {
		http.Error(w, "device identity missing", http.StatusInternalServerError)
		return "", false
	}
	myMemberID, err := peerapi.MemberIDForDevicePublic(device.DevicePublic)
	if err != nil {
		http.Error(w, "device identity is malformed", http.StatusInternalServerError)
		return "", false
	}
	return myMemberID, true
}

// modelIsServable repeats the staleness check the send path makes, so a
// regenerate against a host that has since gone away fails the same way.
func (s *Server) modelIsServable(w http.ResponseWriter, hostMemberID, modelID string) bool {
	if hostMemberID == "" || modelID == "" {
		http.Error(w, "host_member_id and model_id are required", http.StatusBadRequest)
		return false
	}
	ad, ok := s.catalog.Get(hostMemberID, modelID)
	if !ok || ad.Availability == "offline" {
		http.Error(w, "selected model is offline or host is unreachable", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// branchContext returns the transcript to send to the model: the chain of
// messages ending at messageID, following the branch that message is on.
func (s *Server) branchContext(convID, messageID string) []hosting.ChatMessage {
	if messageID == "" {
		return nil
	}
	msgs, err := s.store.ListMessages(convID)
	if err != nil {
		return nil
	}
	var out []hosting.ChatMessage
	for _, b := range branchThrough(msgs, messageID) {
		out = append(out, hosting.ChatMessage{Role: b.Role, Content: b.Content})
	}
	return out
}

// generation carries everything the streaming path needs, so sending,
// regenerating, and editing differ only in the context they assemble and the
// parent the answer is filed under.
type generation struct {
	conv         *store.ConversationRecord
	hostMemberID string
	modelID      string
	myMemberID   string
	history      []hosting.ChatMessage
	// parentID is the message the generated answer hangs from.
	parentID string
	// pendingUser is a question that is only worth storing if an answer comes
	// back. An edit writes one: storing it up front moved the conversation
	// onto the edited branch, so a generation that then failed left the new
	// question with no answer and hid the original exchange behind it.
	pendingUser *store.MessageRecord
	// noSaveTurns are appended to the in-memory transcript of a privacy-mode
	// chat once the answer completes.
	noSaveTurns []hosting.ChatMessage
	// thinking is how hard the requester asked the model to reason. It is the
	// requester's choice rather than the host's, so it travels with the turn.
	thinking inference.ThinkingLevel
}

// resolveThinking validates the reasoning level a client asked for. An
// unrecognized value is the client's mistake and is refused here rather than
// forwarded to an inference engine that would have to guess what it meant.
func (s *Server) resolveThinking(w http.ResponseWriter, raw string) (inference.ThinkingLevel, bool) {
	level, ok := inference.ParseThinkingLevel(raw)
	if !ok {
		http.Error(w, "thinking must be one of off, low, medium, high", http.StatusBadRequest)
		return inference.ThinkingDefault, false
	}
	return level, true
}

// streamGeneration runs one inference and streams it to the browser, then
// files the answer under its parent. It is the single place that talks to a
// local queue or a peer, so every entry point behaves identically.
func (s *Server) streamGeneration(w http.ResponseWriter, r *http.Request, g generation) {
	convID := g.conv.ID

	reqIDBytes := make([]byte, 8)
	_, _ = rand.Read(reqIDBytes)
	requestID := "req-" + hex.EncodeToString(reqIDBytes)

	// Everything that can still fail with a status code is resolved before the
	// stream opens. sse.New flushes, which commits a 200 text/event-stream
	// response: after that http.Error can only write plain text into the event
	// stream, and the browser reads it as a generation that produced nothing
	// rather than as the error it is.
	var localModel *store.HostedModelRecord
	var peerResp *http.Response
	var peerAddr string
	if g.hostMemberID == g.myMemberID {
		model, mErr := s.store.GetHostedModel(g.modelID)
		if mErr != nil || !model.Enabled || !model.Published {
			http.Error(w, "model not available locally", http.StatusServiceUnavailable)
			return
		}
		localModel = model
	} else {
		peerAddr = s.peerAddress(g.hostMemberID)
		if peerAddr == "" {
			http.Error(w, "host address unknown", http.StatusServiceUnavailable)
			return
		}

		// Learn about removals before handing a peer a conversation: a host
		// removed since the last poll must not receive this prompt.
		s.SyncMembership(r.Context())

		conn, dialErr := s.dialPeer(r.Context(), g.hostMemberID, peerAddr)
		if dialErr != nil {
			http.Error(w, "failed to connect to host: "+dialErr.Error(), http.StatusServiceUnavailable)
			return
		}
		defer conn.Close()

		resp, reqErr := peerRoundTrip(r.Context(), conn, "POST", "/peer/v1/inference", peerapi.InferenceRequest{
			RequestID: requestID,
			ModelID:   g.modelID,
			Messages:  g.history,
			Thinking:  string(g.thinking),
		})
		if reqErr != nil {
			http.Error(w, reqErr.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			http.Error(w, string(b), resp.StatusCode)
			return
		}
		peerResp = resp
	}

	stream, err := sse.New(w)
	if err != nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	defer stream.Stop()

	var assistantContent strings.Builder
	var streamInterrupted bool
	start := time.Now()
	var firstTokenTime time.Time
	var completionTokens int

	emit := func(delta string) error {
		if firstTokenTime.IsZero() {
			firstTokenTime = time.Now()
		}
		completionTokens++
		assistantContent.WriteString(delta)
		return stream.SendJSON("", map[string]string{
			"delta":      delta,
			"request_id": requestID,
		})
	}

	if localModel != nil {
		// Local requests go through the same fair queue as remote ones so the
		// owner's own usage is visible to the limits and to the queue estimate
		// peers see in the catalog.
		err = s.infer.Execute(r.Context(), g.myMemberID, requestID, localModel, g.history, g.thinking, emit)
		if err != nil {
			streamInterrupted = true
			_ = stream.SendJSON("error", map[string]string{"error": err.Error()})
		} else {
			var ttftMs int64
			if !firstTokenTime.IsZero() {
				ttftMs = firstTokenTime.Sub(start).Milliseconds()
			}
			totalMs := time.Since(start).Milliseconds()
			var tps float64
			if !firstTokenTime.IsZero() {
				genDur := time.Since(firstTokenTime).Seconds()
				if genDur > 0 && completionTokens > 0 {
					tps = float64(completionTokens) / genDur
				}
			}
			_ = stream.SendJSON("stats", map[string]any{
				"request_id":        requestID,
				"ttft_ms":           ttftMs,
				"total_ms":          totalMs,
				"completion_tokens": completionTokens,
				"tokens_per_second": tps,
			})
			_ = stream.SendRaw("data: [DONE]\n\n")
		}
	} else {
		streamInterrupted = s.relayPeerStream(r.Context(), peerResp.Body, emit, stream, g.hostMemberID, peerAddr)
	}

	if g.conv.NoSave {
		// The transcript stays in memory only. Nothing about a no-save chat is
		// written to the messages table, on this node or any other.
		s.appendNoSaveTurn(convID, g.noSaveTurns...)
		if assistantContent.Len() > 0 {
			s.appendNoSaveTurn(convID, hosting.ChatMessage{
				Role:    "assistant",
				Content: assistantContent.String(),
			})
		}
		return
	}

	if assistantContent.Len() > 0 {
		content := assistantContent.String()
		if streamInterrupted {
			content += " [interrupted]"
		}
		// The question and the answer land together, so a branch is never
		// left half-written.
		parentID := g.parentID
		if g.pendingUser != nil {
			_ = s.store.SaveMessage(*g.pendingUser)
			parentID = g.pendingUser.ID
		}
		asstMsgIDBytes := make([]byte, 8)
		_, _ = rand.Read(asstMsgIDBytes)

		_ = s.store.SaveMessage(store.MessageRecord{
			ID:             "msg-" + hex.EncodeToString(asstMsgIDBytes),
			ConversationID: convID,
			Role:           "assistant",
			Content:        content,
			HostMemberID:   g.hostMemberID,
			ModelID:        g.modelID,
			CreatedAt:      time.Now().Unix(),
			ParentID:       parentID,
		})
	}
}

// relayPeerStream forwards a host's SSE response to the local client and
// reports whether the stream ended abnormally.
func (s *Server) relayPeerStream(
	ctx context.Context,
	body io.Reader,
	emit func(string) error,
	stream *sse.Stream,
	hostMemberID string,
	hostAddr string,
) bool {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, ": "):
			// keepalive comment from the host; nothing to forward

		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				_ = stream.SendRaw("data: [DONE]\n\n")
				return false
			}
			var chunk struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal([]byte(payload), &chunk) == nil {
				_ = emit(chunk.Delta)
			}

		case strings.HasPrefix(line, "event: receipt"):
			if scanner.Scan() {
				receiptLine := scanner.Text()
				if strings.HasPrefix(receiptLine, "data: ") {
					var rec contributions.Receipt
					if json.Unmarshal([]byte(strings.TrimPrefix(receiptLine, "data: ")), &rec) == nil {
						s.handleInboundReceipt(ctx, rec, hostMemberID, hostAddr)
					}
				}
			}

		case strings.HasPrefix(line, "event: stats"):
			if scanner.Scan() {
				statsLine := scanner.Text()
				_ = stream.SendRaw("event: stats\n" + statsLine + "\n\n")
			}

		case strings.HasPrefix(line, "event: error"):
			if scanner.Scan() {
				errLine := scanner.Text()
				_ = stream.SendRaw("event: error\n" + errLine + "\n\n")
			}
			return true
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		_ = stream.SendJSON("error", map[string]string{"error": "host disconnected unexpectedly"})
		return true
	}
	return false
}

func (s *Server) handleCancelMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestID    string `json:"request_id"`
		HostMemberID string `json:"host_member_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RequestID == "" {
		http.Error(w, "request_id required", http.StatusBadRequest)
		return
	}

	device, _ := s.store.GetDeviceIdentity()
	myMemberID := ""
	if device != nil {
		myMemberID, _ = peerapi.MemberIDForDevicePublic(device.DevicePublic)
	}

	if body.HostMemberID == "" || body.HostMemberID == myMemberID {
		s.infer.Queue().Cancel(body.RequestID, myMemberID)
	} else if addr := s.peerAddress(body.HostMemberID); addr != "" {
		dialCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_ = s.peerJSON(dialCtx, body.HostMemberID, addr, "POST", "/peer/v1/inference/cancel",
			map[string]string{"request_id": body.RequestID}, nil)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
