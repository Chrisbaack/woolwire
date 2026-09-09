package peerapi

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Chrisbaack/woolwire/internal/contributions"
	"github.com/Chrisbaack/woolwire/internal/hosting"
	"github.com/Chrisbaack/woolwire/internal/inference"
	"github.com/Chrisbaack/woolwire/internal/peerauth"
	"github.com/Chrisbaack/woolwire/internal/sse"
)

// InferenceRequest carries no member_id: the requester is the authenticated
// TLS identity, so a member cannot submit work billed to someone else.
type InferenceRequest struct {
	RequestID string                `json:"request_id"`
	ModelID   string                `json:"model_id"`
	Messages  []hosting.ChatMessage `json:"messages"`
	// Thinking is the reasoning level the requesting member chose. A peer
	// that predates the control sends nothing, which leaves the model's own
	// default alone.
	Thinking string `json:"thinking,omitempty"`
}

func (s *Server) handleInference(w http.ResponseWriter, r *http.Request, caller peerauth.Identity) {
	var req InferenceRequest
	// Cap HTTP body to 1 MiB per architecture specification.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "request payload too large or invalid", http.StatusBadRequest)
		return
	}

	if req.RequestID == "" || req.ModelID == "" || len(req.Messages) == 0 {
		http.Error(w, "missing required fields", http.StatusBadRequest)
		return
	}

	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	model, err := s.store.GetHostedModel(req.ModelID)
	if err != nil || !model.Enabled || !model.Published {
		http.Error(w, "model not found or unavailable", http.StatusNotFound)
		return
	}

	// Reject an oversized prompt before it costs a queue slot or a backend
	// connection, and report it as a client error rather than a stream error.
	if err := inference.CheckContextLimit(model, req.Messages); err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}

	thinking, ok := inference.ParseThinkingLevel(req.Thinking)
	if !ok {
		http.Error(w, "unknown thinking level", http.StatusBadRequest)
		return
	}

	stream, err := sse.New(w)
	if err != nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	defer stream.Stop()

	start := time.Now()
	var firstTokenTime time.Time
	var completionTokens int

	err = s.infer.Execute(r.Context(), caller.MemberID, req.RequestID, model, req.Messages, thinking, func(delta string) error {
		if firstTokenTime.IsZero() {
			firstTokenTime = time.Now()
		}
		completionTokens++
		return stream.SendJSON("", map[string]string{"delta": delta})
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
		if errors.Is(err, inference.ErrDuplicateRequest) {
			http.Error(w, "duplicate request id", http.StatusConflict)
			return
		}
		_ = stream.SendJSON("error", map[string]string{"error": err.Error()})
		return
	}

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
		"request_id":        req.RequestID,
		"ttft_ms":           ttftMs,
		"total_ms":          totalMs,
		"completion_tokens": completionTokens,
		"prompt_tokens_est": inference.EstimateTokens(req.Messages),
		"tokens_per_second": tps,
	})

	// The host signs a contribution receipt unless it has opted out. The
	// requester counter-signs and acknowledges it out of band.
	optOut, _ := s.store.GetSetting("contributions_opt_out")
	device, _ := s.store.GetDeviceIdentity()
	if optOut != "true" && device != nil && len(s.deviceKey) == ed25519.PrivateKeySize {
		hostMemberID, idErr := MemberIDForDevicePublic(device.DevicePublic)
		if idErr == nil && caller.MemberID != hostMemberID {
			rec := contributions.Receipt{
				RequestID:         req.RequestID,
				RoomID:            roomRec.RoomID,
				HostMemberID:      hostMemberID,
				RequesterMemberID: caller.MemberID,
				Timestamp:         time.Now().Unix(),
				Completed:         true,
			}
			if err := rec.SignHost(s.deviceKey); err == nil {
				_ = stream.SendJSON("receipt", rec)
			}
		}
	}

	_ = stream.SendRaw("data: [DONE]\n\n")
}

func (s *Server) handleCancelInference(w http.ResponseWriter, r *http.Request, caller peerauth.Identity) {
	var body struct {
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&body); err != nil || body.RequestID == "" {
		http.Error(w, "request_id required", http.StatusBadRequest)
		return
	}

	// Cancellation is scoped to the submitter. Without the member argument any
	// caller could stop any other member's in-flight request by guessing or
	// observing a request id.
	cancelled := s.infer.Queue().Cancel(body.RequestID, caller.MemberID)

	w.Header().Set("Content-Type", "application/json")
	if !cancelled {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"cancelled": false})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"cancelled": true})
}
