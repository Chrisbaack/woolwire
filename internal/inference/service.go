package inference

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cbaack/woolwire/internal/hosting"
	"github.com/cbaack/woolwire/internal/store"
)

var (
	// ErrContextLimitExceeded is returned before the backend is contacted, so
	// an oversized prompt costs the host nothing.
	ErrContextLimitExceeded = errors.New("prompt exceeds the model's configured context limit")
	// ErrModelUnavailable covers a missing, disabled, or unloadable model.
	ErrModelUnavailable = errors.New("model not found or unavailable")
	// ErrRunnerUnavailable is returned for a managed model with no runner.
	ErrRunnerUnavailable = errors.New("managed model has no runner configured")
)

// charsPerToken is the v1 prompt-size estimate. It is deliberately crude: the
// point is to reject obviously oversized prompts before dispatch, not to
// reproduce a tokenizer.
const charsPerToken = 4

// Service is the single admission and dispatch path for inference on this
// node. Local chats, the OpenAI-compatible routes, and peer requests all go
// through it, so the host's own usage is visible to the same fairness limits
// that remote members are held to.
type Service struct {
	store   *store.Store
	queue   *FairQueue
	adapter *hosting.ExternalAdapter
	runner  *hosting.RunnerClient
}

func NewService(s *store.Store, adapter *hosting.ExternalAdapter, runner *hosting.RunnerClient) *Service {
	limits := DefaultLimits()
	if s != nil {
		if l, err := s.GetHostLimits(); err == nil {
			limits = limitsFromRecord(l)
		}
	}
	if adapter == nil {
		adapter = hosting.NewExternalAdapter()
	}
	return &Service{
		store:   s,
		queue:   NewFairQueue(limits),
		adapter: adapter,
		runner:  runner,
	}
}

func limitsFromRecord(l store.HostLimitsRecord) Limits {
	return Limits{
		MaxActive:          l.MaxActive,
		MaxQueuedPerMember: l.MaxQueuedPerMember,
		MaxQueuedTotal:     l.MaxQueuedTotal,
		QueueTimeout:       time.Duration(l.QueueTimeoutSeconds) * time.Second,
		ExecTimeout:        time.Duration(l.ExecutionTimeoutSeconds) * time.Second,
	}
}

func (s *Service) Queue() *FairQueue { return s.queue }

func (s *Service) SetRunner(runner *hosting.RunnerClient) { s.runner = runner }

// ReloadLimits re-reads the owner's host limits after they are edited.
func (s *Service) ReloadLimits() {
	if s.store == nil {
		return
	}
	if l, err := s.store.GetHostLimits(); err == nil {
		s.queue.SetLimits(limitsFromRecord(l))
	}
}

// EstimateTokens approximates prompt size for the context-limit check.
func EstimateTokens(messages []hosting.ChatMessage) int {
	var chars int
	for _, m := range messages {
		chars += len(m.Content) + len(m.Role) + 4 // per-message framing overhead
	}
	return chars / charsPerToken
}

// CheckContextLimit rejects prompts that cannot fit, before any queue slot or
// backend connection is spent on them.
func CheckContextLimit(model *store.HostedModelRecord, messages []hosting.ChatMessage) error {
	if model == nil || model.ContextLimit <= 0 {
		return nil
	}
	// Reserve room for the completion the caller asked for.
	budget := model.ContextLimit - model.MaxTokens
	if budget <= 0 {
		budget = model.ContextLimit
	}
	if estimate := EstimateTokens(messages); estimate > budget {
		return fmt.Errorf("%w: estimated %d tokens, limit %d", ErrContextLimitExceeded, estimate, budget)
	}
	return nil
}

// BackendModelName is the identifier sent to the backend server. Falling back
// to Name rather than ID matters: the opaque Woolwire ID means nothing to a
// multi-model server and gets rejected or silently ignored.
func BackendModelName(model *store.HostedModelRecord) string {
	if model == nil {
		return ""
	}
	if model.BackendModel != "" {
		return model.BackendModel
	}
	return model.Name
}

// Execute admits a request to the fair queue and streams the backend response
// through onDelta. Managed models are served by the runner companion; external
// models go out through the SSRF-checked adapter.
func (s *Service) Execute(
	ctx context.Context,
	memberID string,
	requestID string,
	model *store.HostedModelRecord,
	messages []hosting.ChatMessage,
	onDelta func(delta string) error,
) error {
	if model == nil || !model.Enabled {
		return ErrModelUnavailable
	}
	if err := CheckContextLimit(model, messages); err != nil {
		return err
	}
	if model.ModelType == "managed" && s.runner == nil {
		return ErrRunnerUnavailable
	}

	return s.queue.Submit(ctx, memberID, requestID, func(execCtx context.Context) error {
		if model.ModelType == "managed" {
			return s.runner.StreamChat(execCtx, memberID, BackendModelName(model), model.MaxTokens, messages, onDelta)
		}
		return s.adapter.StreamChat(execCtx, hosting.ChatRequest{
			EndpointURL:         model.EndpointURL,
			APIKey:              model.APIKey,
			Model:               BackendModelName(model),
			MaxTokens:           model.MaxTokens,
			Messages:            messages,
			AllowPrivateNetwork: model.AllowPrivateNetwork,
		}, onDelta)
	})
}
