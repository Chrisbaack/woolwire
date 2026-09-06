package localapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cbaack/woolwire/internal/contributions"
	"github.com/cbaack/woolwire/internal/hosting"
	"github.com/cbaack/woolwire/internal/peerapi"
	"github.com/cbaack/woolwire/internal/store"
)

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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
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
	myMemberID := "m-" + device.DevicePublic[:16]

	// Save user message locally (if not no_save mode)
	now := time.Now().Unix()
	userMsgIDBytes := make([]byte, 8)
	_, _ = rand.Read(userMsgIDBytes)
	userMsgID := "msg-" + hex.EncodeToString(userMsgIDBytes)

	userMsg := store.MessageRecord{
		ID:             userMsgID,
		ConversationID: convID,
		Role:           "user",
		Content:        body.Content,
		HostMemberID:   body.HostMemberID,
		ModelID:        body.ModelID,
		CreatedAt:      now,
	}

	if !conv.NoSave {
		_ = s.store.SaveMessage(userMsg)
	}

	// Build context from past messages
	var chatMsgs []hosting.ChatMessage
	if !conv.NoSave {
		pastMsgs, _ := s.store.ListMessages(convID)
		for _, m := range pastMsgs {
			chatMsgs = append(chatMsgs, hosting.ChatMessage{
				Role:    m.Role,
				Content: m.Content,
			})
		}
	} else {
		chatMsgs = append(chatMsgs, hosting.ChatMessage{
			Role:    "user",
			Content: body.Content,
		})
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	reqIDBytes := make([]byte, 8)
	_, _ = rand.Read(reqIDBytes)
	requestID := "req-" + hex.EncodeToString(reqIDBytes)

	var assistantContent strings.Builder
	var streamInterrupted bool

	if body.HostMemberID == myMemberID {
		// Host is local self
		localModel, mErr := s.store.GetHostedModel(body.ModelID)
		if mErr != nil || !localModel.Enabled || !localModel.Published {
			http.Error(w, "model not available locally", http.StatusServiceUnavailable)
			return
		}

		err = s.adapter.StreamChat(
			r.Context(),
			localModel.EndpointURL,
			localModel.APIKey,
			localModel.ID,
			chatMsgs,
			func(delta string) error {
				assistantContent.WriteString(delta)
				chunkJSON, _ := json.Marshal(map[string]string{
					"delta":      delta,
					"request_id": requestID,
				})
				_, writeErr := fmt.Fprintf(w, "data: %s\n\n", string(chunkJSON))
				if writeErr != nil {
					return writeErr
				}
				flusher.Flush()
				return nil
			},
		)
		if err != nil {
			streamInterrupted = true
			errJSON, _ := json.Marshal(map[string]string{"error": err.Error()})
			_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", string(errJSON))
			flusher.Flush()
		} else {
			_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
		}
	} else {
		// Host is remote peer
		peerAddrs, _ := s.store.ListPeerAddresses()
		var targetAddr string
		for _, pa := range peerAddrs {
			if pa.MemberID == body.HostMemberID {
				targetAddr = pa.TailcatAddr
				break
			}
		}

		if targetAddr == "" {
			http.Error(w, "host address unknown", http.StatusServiceUnavailable)
			return
		}

		conn, dialErr := s.trans.Dial(r.Context(), targetAddr, s.peerPort)
		if dialErr != nil {
			http.Error(w, fmt.Sprintf("failed to connect to host: %v", dialErr), http.StatusServiceUnavailable)
			return
		}
		defer conn.Close()

		inferReq := peerapi.InferenceRequest{
			RequestID: requestID,
			MemberID:  myMemberID,
			ModelID:   body.ModelID,
			Messages:  chatMsgs,
		}
		reqBytes, _ := json.Marshal(inferReq)

		httpReq, reqErr := http.NewRequestWithContext(r.Context(), "POST", "http://woolwire-peer/peer/v1/inference", bytes.NewReader(reqBytes))
		if reqErr != nil {
			http.Error(w, reqErr.Error(), http.StatusInternalServerError)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")

		if writeErr := httpReq.Write(conn); writeErr != nil {
			http.Error(w, fmt.Sprintf("failed to send request to host: %v", writeErr), http.StatusBadGateway)
			return
		}

		resp, respErr := http.ReadResponse(bufio.NewReader(conn), httpReq)
		if respErr != nil {
			http.Error(w, fmt.Sprintf("failed to read response from host: %v", respErr), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			http.Error(w, string(b), resp.StatusCode)
			return
		}

		// Stream SSE from remote peer to client
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				payload := strings.TrimPrefix(line, "data: ")
				if payload == "[DONE]" {
					_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
					flusher.Flush()
					break
				}
				var chunk struct {
					Delta string `json:"delta"`
				}
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					assistantContent.WriteString(chunk.Delta)
					chunkWithReqID, _ := json.Marshal(map[string]string{
						"delta":      chunk.Delta,
						"request_id": requestID,
					})
					_, _ = fmt.Fprintf(w, "data: %s\n\n", string(chunkWithReqID))
					flusher.Flush()
				}
			} else if strings.HasPrefix(line, "event: receipt") {
				if scanner.Scan() {
					receiptLine := scanner.Text()
					if strings.HasPrefix(receiptLine, "data: ") {
						receiptData := strings.TrimPrefix(receiptLine, "data: ")
						var rec contributions.Receipt
						if json.Unmarshal([]byte(receiptData), &rec) == nil {
							s.handleInboundReceipt(r.Context(), rec, body.HostMemberID, targetAddr)
						}
					}
				}
			} else if strings.HasPrefix(line, "event: error") {
				streamInterrupted = true
				_, _ = fmt.Fprintf(w, "%s\n", line)
				if scanner.Scan() {
					_, _ = fmt.Fprintf(w, "%s\n\n", scanner.Text())
				}
				flusher.Flush()
				break
			}
		}

		if scanErr := scanner.Err(); scanErr != nil && scanErr != io.EOF {
			streamInterrupted = true
			errJSON, _ := json.Marshal(map[string]string{"error": "host disconnected unexpectedly"})
			_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", string(errJSON))
			flusher.Flush()
		}
	}

	// Local-only persistence: Save assistant message if not no_save
	if !conv.NoSave && assistantContent.Len() > 0 {
		content := assistantContent.String()
		if streamInterrupted {
			content += " [interrupted]"
		}
		asstMsgIDBytes := make([]byte, 8)
		_, _ = rand.Read(asstMsgIDBytes)
		asstMsgID := "msg-" + hex.EncodeToString(asstMsgIDBytes)

		_ = s.store.SaveMessage(store.MessageRecord{
			ID:             asstMsgID,
			ConversationID: convID,
			Role:           "assistant",
			Content:        content,
			HostMemberID:   body.HostMemberID,
			ModelID:        body.ModelID,
			CreatedAt:      time.Now().Unix(),
		})
	}
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
		myMemberID = "m-" + device.DevicePublic[:16]
	}

	if body.HostMemberID != "" && body.HostMemberID != myMemberID {
		peerAddrs, _ := s.store.ListPeerAddresses()
		for _, pa := range peerAddrs {
			if pa.MemberID == body.HostMemberID {
				dialCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				defer cancel()
				if conn, err := s.trans.Dial(dialCtx, pa.TailcatAddr, s.peerPort); err == nil {
					defer conn.Close()
					reqBytes, _ := json.Marshal(map[string]string{"request_id": body.RequestID})
					httpReq, reqErr := http.NewRequestWithContext(dialCtx, "POST", "http://woolwire-peer/peer/v1/inference/cancel", bytes.NewReader(reqBytes))
					if reqErr == nil {
						httpReq.Header.Set("Content-Type", "application/json")
						_ = httpReq.Write(conn)
					}
				}
				break
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
