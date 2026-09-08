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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"conversation": conv,
		"messages":     msgs,
	})
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
		})
		pastMsgs, _ := s.store.ListMessages(convID)
		for _, m := range pastMsgs {
			chatMsgs = append(chatMsgs, hosting.ChatMessage{Role: m.Role, Content: m.Content})
		}
	}

	stream, err := sse.New(w)
	if err != nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	defer stream.Stop()

	reqIDBytes := make([]byte, 8)
	_, _ = rand.Read(reqIDBytes)
	requestID := "req-" + hex.EncodeToString(reqIDBytes)

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

	if body.HostMemberID == myMemberID {
		localModel, mErr := s.store.GetHostedModel(body.ModelID)
		if mErr != nil || !localModel.Enabled || !localModel.Published {
			http.Error(w, "model not available locally", http.StatusServiceUnavailable)
			return
		}

		// Local requests go through the same fair queue as remote ones so the
		// owner's own usage is visible to the limits and to the queue estimate
		// peers see in the catalog.
		err = s.infer.Execute(r.Context(), myMemberID, requestID, localModel, chatMsgs, emit)
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
		targetAddr := s.peerAddress(body.HostMemberID)
		if targetAddr == "" {
			http.Error(w, "host address unknown", http.StatusServiceUnavailable)
			return
		}

		// Learn about removals before handing a peer a conversation: a host
		// removed since the last poll must not receive this prompt.
		s.SyncMembership(r.Context())

		conn, dialErr := s.dialPeer(r.Context(), body.HostMemberID, targetAddr)
		if dialErr != nil {
			http.Error(w, "failed to connect to host: "+dialErr.Error(), http.StatusServiceUnavailable)
			return
		}
		defer conn.Close()

		inferReq := peerapi.InferenceRequest{
			RequestID: requestID,
			ModelID:   body.ModelID,
			Messages:  chatMsgs,
		}
		resp, reqErr := peerRoundTrip(r.Context(), conn, "POST", "/peer/v1/inference", inferReq)
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

		streamInterrupted = s.relayPeerStream(r.Context(), resp.Body, emit, stream, body.HostMemberID, targetAddr)
	}

	if conv.NoSave {
		// The transcript stays in memory only. Nothing about a no-save chat is
		// written to the messages table, on this node or any other.
		s.appendNoSaveTurn(convID, userTurn)
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
		asstMsgIDBytes := make([]byte, 8)
		_, _ = rand.Read(asstMsgIDBytes)

		_ = s.store.SaveMessage(store.MessageRecord{
			ID:             "msg-" + hex.EncodeToString(asstMsgIDBytes),
			ConversationID: convID,
			Role:           "assistant",
			Content:        content,
			HostMemberID:   body.HostMemberID,
			ModelID:        body.ModelID,
			CreatedAt:      time.Now().Unix(),
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
