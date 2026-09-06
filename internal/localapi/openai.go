package localapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cbaack/woolwire/internal/hosting"
	"github.com/cbaack/woolwire/internal/peerapi"
)

type openAIModelItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (s *Server) checkOpenAIAuth(r *http.Request) bool {
	token, _ := s.store.GetSetting("local_api_token")
	if token == "" {
		return true // unauthenticated if no token configured
	}
	auth := r.Header.Get("Authorization")
	expected := "Bearer " + token
	return auth == expected
}

func (s *Server) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	if !s.checkOpenAIAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	available := s.catalog.ListAvailable()
	list := make([]openAIModelItem, 0, len(available))
	for _, m := range available {
		list = append(list, openAIModelItem{
			ID:      m.ModelID,
			Object:  "model",
			Created: m.Timestamp,
			OwnedBy: m.HostMemberID,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   list,
	})
}

func (s *Server) handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.checkOpenAIAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		Model    string                `json:"model"`
		Messages []hosting.ChatMessage `json:"messages"`
		Stream   bool                  `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Match model from catalog
	available := s.catalog.ListAvailable()
	var matchedHostID string
	var matchedModelID string
	for _, m := range available {
		if m.ModelID == req.Model || m.Name == req.Model {
			matchedHostID = m.HostMemberID
			matchedModelID = m.ModelID
			break
		}
	}

	if matchedHostID == "" {
		http.Error(w, "model not found or offline", http.StatusNotFound)
		return
	}

	device, err := s.store.GetDeviceIdentity()
	if err != nil {
		http.Error(w, "device identity missing", http.StatusInternalServerError)
		return
	}
	myMemberID := "m-" + device.DevicePublic[:16]

	reqIDBytes := make([]byte, 8)
	_, _ = rand.Read(reqIDBytes)
	requestID := "chatcmpl-" + hex.EncodeToString(reqIDBytes)

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		sendChunk := func(delta string) error {
			chunk := map[string]any{
				"id":      requestID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   req.Model,
				"choices": []map[string]any{
					{
						"index": 0,
						"delta": map[string]string{
							"content": delta,
						},
						"finish_reason": nil,
					},
				},
			}
			chunkJSON, _ := json.Marshal(chunk)
			_, err := fmt.Fprintf(w, "data: %s\n\n", string(chunkJSON))
			if err != nil {
				return err
			}
			flusher.Flush()
			return nil
		}

		if matchedHostID == myMemberID {
			localModel, mErr := s.store.GetHostedModel(matchedModelID)
			if mErr != nil || !localModel.Enabled || !localModel.Published {
				http.Error(w, "model unavailable locally", http.StatusServiceUnavailable)
				return
			}
			_ = s.adapter.StreamChat(r.Context(), localModel.EndpointURL, localModel.APIKey, localModel.ID, req.Messages, sendChunk)
		} else {
			peerAddrs, _ := s.store.ListPeerAddresses()
			var targetAddr string
			for _, pa := range peerAddrs {
				if pa.MemberID == matchedHostID {
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
				http.Error(w, "host dial error", http.StatusServiceUnavailable)
				return
			}
			defer conn.Close()

			inferReq := peerapi.InferenceRequest{
				RequestID: requestID,
				MemberID:  myMemberID,
				ModelID:   matchedModelID,
				Messages:  req.Messages,
			}
			reqBytes, _ := json.Marshal(inferReq)
			httpReq, _ := http.NewRequestWithContext(r.Context(), "POST", "http://woolwire-peer/peer/v1/inference", bytes.NewReader(reqBytes))
			httpReq.Header.Set("Content-Type", "application/json")
			_ = httpReq.Write(conn)

			resp, readErr := http.ReadResponse(bufio.NewReader(conn), httpReq)
			if readErr != nil {
				http.Error(w, "host read error", http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()

			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "data: ") {
					payload := strings.TrimPrefix(line, "data: ")
					if payload == "[DONE]" {
						break
					}
					var c struct {
						Delta string `json:"delta"`
					}
					if json.Unmarshal([]byte(payload), &c) == nil {
						_ = sendChunk(c.Delta)
					}
				}
			}
		}

		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	} else {
		// Non-streaming: accumulate and return complete object
		var fullContent strings.Builder
		collectChunk := func(delta string) error {
			fullContent.WriteString(delta)
			return nil
		}

		if matchedHostID == myMemberID {
			localModel, _ := s.store.GetHostedModel(matchedModelID)
			_ = s.adapter.StreamChat(r.Context(), localModel.EndpointURL, localModel.APIKey, localModel.ID, req.Messages, collectChunk)
		} else {
			peerAddrs, _ := s.store.ListPeerAddresses()
			var targetAddr string
			for _, pa := range peerAddrs {
				if pa.MemberID == matchedHostID {
					targetAddr = pa.TailcatAddr
					break
				}
			}
			if targetAddr != "" {
				dialCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
				defer cancel()
				if conn, err := s.trans.Dial(dialCtx, targetAddr, s.peerPort); err == nil {
					defer conn.Close()
					inferReq := peerapi.InferenceRequest{
						RequestID: requestID,
						MemberID:  myMemberID,
						ModelID:   matchedModelID,
						Messages:  req.Messages,
					}
					reqBytes, _ := json.Marshal(inferReq)
					httpReq, _ := http.NewRequestWithContext(dialCtx, "POST", "http://woolwire-peer/peer/v1/inference", bytes.NewReader(reqBytes))
					httpReq.Header.Set("Content-Type", "application/json")
					_ = httpReq.Write(conn)

					if resp, err := http.ReadResponse(bufio.NewReader(conn), httpReq); err == nil {
						defer resp.Body.Close()
						scanner := bufio.NewScanner(resp.Body)
						for scanner.Scan() {
							line := scanner.Text()
							if strings.HasPrefix(line, "data: ") {
								payload := strings.TrimPrefix(line, "data: ")
								if payload == "[DONE]" {
									break
								}
								var c struct {
									Delta string `json:"delta"`
								}
								if json.Unmarshal([]byte(payload), &c) == nil {
									fullContent.WriteString(c.Delta)
								}
							}
						}
					}
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      requestID,
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   req.Model,
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]string{
						"role":    "assistant",
						"content": fullContent.String(),
					},
					"finish_reason": "stop",
				},
			},
		})
	}
}
