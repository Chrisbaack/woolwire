package localapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Chrisbaack/woolwire/internal/hosting"
	"github.com/Chrisbaack/woolwire/internal/inference"
	"github.com/Chrisbaack/woolwire/internal/peerapi"
	"github.com/Chrisbaack/woolwire/internal/sse"
)

type openAIModelItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// localAPIToken returns the bearer token for the OpenAI-compatible routes,
// generating it on first use. It is never optional: an unset token used to
// mean "no authentication", which left /v1/chat/completions reachable by a
// simple cross-origin POST from any page the owner happened to visit, driving
// inference on other members' hardware.
func (s *Server) localAPIToken() (string, error) {
	token, err := s.store.GetSetting("local_api_token")
	if err == nil && token != "" {
		return token, nil
	}

	generated, err := generateSecret()
	if err != nil {
		return "", err
	}
	if err := s.store.SetSetting("local_api_token", generated); err != nil {
		return "", err
	}
	return generated, nil
}

func (s *Server) checkOpenAIAuth(r *http.Request) bool {
	token, err := s.localAPIToken()
	if err != nil || token == "" {
		return false
	}
	expected := "Bearer " + token
	provided := r.Header.Get("Authorization")
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func (s *Server) handleGetLocalAPIToken(w http.ResponseWriter, r *http.Request) {
	token, err := s.localAPIToken()
	if err != nil {
		http.Error(w, "failed to read local API token", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"token": token})
}

func (s *Server) handleRegenerateLocalAPIToken(w http.ResponseWriter, r *http.Request) {
	token, err := generateSecret()
	if err != nil {
		http.Error(w, "failed to generate token", http.StatusInternalServerError)
		return
	}
	if err := s.store.SetSetting("local_api_token", token); err != nil {
		http.Error(w, "failed to save token", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"token": token})
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
		// ReasoningEffort is the OpenAI spelling of a thinking level, so a
		// client already written against that API asks for one the usual way.
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 {
		http.Error(w, "messages are required", http.StatusBadRequest)
		return
	}
	thinking, thinkingOK := s.resolveThinking(w, req.ReasoningEffort)
	if !thinkingOK {
		return
	}

	var matchedHostID, matchedModelID string
	for _, m := range s.catalog.ListAvailable() {
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
	myMemberID, err := peerapi.MemberIDForDevicePublic(device.DevicePublic)
	if err != nil {
		http.Error(w, "device identity is malformed", http.StatusInternalServerError)
		return
	}

	reqIDBytes := make([]byte, 8)
	_, _ = rand.Read(reqIDBytes)
	requestID := "chatcmpl-" + hex.EncodeToString(reqIDBytes)

	var collected strings.Builder
	collect := func(delta string) error {
		collected.WriteString(delta)
		return nil
	}

	var stream *sse.Stream
	emit := collect
	if req.Stream {
		stream, err = sse.New(w)
		if err != nil {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		defer stream.Stop()
		emit = func(delta string) error {
			collected.WriteString(delta)
			return stream.SendJSON("", openAIChunk(requestID, req.Model, delta))
		}
	}

	runErr := s.runOpenAIRequest(r, myMemberID, matchedHostID, matchedModelID, requestID, req.Messages, thinking, emit)

	if req.Stream {
		if runErr != nil {
			_ = stream.SendJSON("error", map[string]string{"error": runErr.Error()})
			return
		}
		_ = stream.SendRaw("data: [DONE]\n\n")
		return
	}

	if runErr != nil {
		http.Error(w, runErr.Error(), openAIStatusFor(runErr))
		return
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
					"content": collected.String(),
				},
				"finish_reason": "stop",
			},
		},
	})
}

func openAIStatusFor(err error) int {
	if errors.Is(err, errPeerUnreachable) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}

var errPeerUnreachable = errors.New("host is unreachable")

// runOpenAIRequest dispatches to the local queue for a self-hosted model, or
// to the owning peer over authenticated TLS.
func (s *Server) runOpenAIRequest(
	r *http.Request,
	myMemberID, hostMemberID, modelID, requestID string,
	messages []hosting.ChatMessage,
	thinking inference.ThinkingLevel,
	emit func(string) error,
) error {
	if hostMemberID == myMemberID {
		localModel, err := s.store.GetHostedModel(modelID)
		if err != nil || !localModel.Enabled || !localModel.Published {
			return errors.New("model unavailable locally")
		}
		// The owner's own OpenAI-compatible traffic occupies the same slots as
		// remote members', so local usage is visible to the fairness limits.
		return s.infer.Execute(r.Context(), myMemberID, requestID, localModel, messages, thinking, emit)
	}

	targetAddr := s.peerAddress(hostMemberID)
	if targetAddr == "" {
		return errPeerUnreachable
	}

	// Learn about removals before handing a peer a conversation, the same way
	// the chat route does: a host removed since the last poll must not receive
	// this prompt.
	s.SyncMembership(r.Context())

	conn, err := s.dialPeer(r.Context(), hostMemberID, targetAddr)
	if err != nil {
		return errPeerUnreachable
	}
	defer conn.Close()

	resp, err := peerRoundTrip(r.Context(), conn, "POST", "/peer/v1/inference", peerapi.InferenceRequest{
		RequestID: requestID,
		ModelID:   modelID,
		Messages:  messages,
		Thinking:  string(thinking),
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return errors.New(strings.TrimSpace(string(b)))
	}

	return readPeerDeltas(resp.Body, emit)
}

func openAIChunk(requestID, model, delta string) map[string]any {
	return map[string]any{
		"id":      requestID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
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
}
