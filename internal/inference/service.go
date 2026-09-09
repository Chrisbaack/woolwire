package inference

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Chrisbaack/woolwire/internal/hosting"
	"github.com/Chrisbaack/woolwire/internal/store"
)

var (
	// ErrContextLimitExceeded is returned before the backend is contacted, so
	// an oversized prompt costs the host nothing.
	ErrContextLimitExceeded = errors.New("prompt exceeds the model's configured context limit")
	// ErrModelUnavailable covers a missing, disabled, or unloadable model.
	ErrModelUnavailable = errors.New("model not found or unavailable")
	// ErrRunnerUnavailable is returned for a managed model with no runner.
	ErrRunnerUnavailable  = errors.New("managed model has no runner configured")
	ErrManagedModelConfig = errors.New("managed model has no persisted load configuration")
)

// ThinkingLevel is how hard the requester asked the model to reason. The empty
// value leaves the model's own default alone, which is what every request did
// before the control existed, so an old client keeps working unchanged.
type ThinkingLevel string

const (
	ThinkingDefault ThinkingLevel = ""
	ThinkingOff     ThinkingLevel = "off"
	ThinkingLow     ThinkingLevel = "low"
	ThinkingMedium  ThinkingLevel = "medium"
	ThinkingHigh    ThinkingLevel = "high"
)

// ParseThinkingLevel accepts the levels a client may ask for and rejects
// anything else, so an unrecognized value is a bad request rather than a
// string forwarded to an inference engine.
func ParseThinkingLevel(s string) (ThinkingLevel, bool) {
	switch ThinkingLevel(s) {
	case ThinkingDefault, ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh:
		return ThinkingLevel(s), true
	}
	return ThinkingDefault, false
}

// Effort is the level as an OpenAI reasoning_effort value, or "" where there
// is nothing meaningful to send. "off" has no reasoning_effort spelling: it is
// expressed by turning the template's thinking branch off instead.
func (t ThinkingLevel) Effort() string {
	switch t {
	case ThinkingLow, ThinkingMedium, ThinkingHigh:
		return string(t)
	}
	return ""
}

// charsPerToken is the v1 prompt-size estimate. It is deliberately crude: the
// point is to reject obviously oversized prompts before dispatch, not to
// reproduce a tokenizer.
const charsPerToken = 4

const (
	defaultManagedIdleAfter     = 5 * time.Minute
	defaultManagedUnloadTimeout = 30 * time.Second
	// healthProbeTimeout bounds the "what are you actually holding?" question
	// asked after a load reports failure.
	healthProbeTimeout = 3 * time.Second
	// minIdleRecheck keeps the "release as soon as the queue drains" policy
	// from spinning: a busy queue is looked at again after this long rather
	// than immediately.
	minIdleRecheck = 2 * time.Second
)

// IdleUnloadNever, as an idleAfter value, keeps a demand-loaded engine warm
// until something explicitly unloads it. Zero releases it as soon as the
// queue drains; anything positive waits that long first.
const IdleUnloadNever = -1 * time.Second

// Service is the single admission and dispatch path for inference on this
// node. Local chats, the OpenAI-compatible routes, and peer requests all go
// through it, so the host's own usage is visible to the same fairness limits
// that remote members are held to.
type Service struct {
	store    *store.Store
	queue    *FairQueue
	adapter  *hosting.ExternalAdapter
	runnerMu sync.RWMutex
	runner   *hosting.RunnerClient
	// managedGate covers the whole runner lifecycle and inference stream. It is
	// a channel rather than a mutex so requests waiting for a model switch can
	// stop waiting when their queue context expires.
	managedGate    chan struct{}
	stateMu        sync.Mutex
	idleTimer      *time.Timer
	idleGeneration uint64
	demandLoaded   bool
	manualPinned   bool
	closed         bool
	lifecycleCtx   context.Context
	lifecycleStop  context.CancelFunc
	// idleAfter is the owner's unload policy, and a field so service tests can
	// use short intervals: negative keeps the engine warm indefinitely, zero
	// releases it as soon as the queue drains, positive waits that long.
	idleAfter     time.Duration
	unloadTimeout time.Duration
}

func NewService(s *store.Store, adapter *hosting.ExternalAdapter, runner *hosting.RunnerClient) *Service {
	limits := DefaultLimits()
	idleAfter := defaultManagedIdleAfter
	if s != nil {
		if l, err := s.GetHostLimits(); err == nil {
			limits = limitsFromRecord(l)
			idleAfter = idleUnloadFromRecord(l)
		}
	}
	if adapter == nil {
		adapter = hosting.NewExternalAdapter()
	}
	lifecycleCtx, lifecycleStop := context.WithCancel(context.Background())
	managedGate := make(chan struct{}, 1)
	managedGate <- struct{}{}
	return &Service{
		store:         s,
		queue:         NewFairQueue(limits),
		adapter:       adapter,
		runner:        runner,
		managedGate:   managedGate,
		lifecycleCtx:  lifecycleCtx,
		lifecycleStop: lifecycleStop,
		idleAfter:     idleAfter,
		unloadTimeout: defaultManagedUnloadTimeout,
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

// idleUnloadFromRecord reads the owner's unload policy. Any negative value
// means "never", so the record does not have to agree on which one.
func idleUnloadFromRecord(l store.HostLimitsRecord) time.Duration {
	if l.IdleUnloadSeconds < 0 {
		return IdleUnloadNever
	}
	return time.Duration(l.IdleUnloadSeconds) * time.Second
}

func (s *Service) SetRunner(runner *hosting.RunnerClient) {
	s.runnerMu.Lock()
	defer s.runnerMu.Unlock()
	s.runner = runner
}

func (s *Service) currentRunner() *hosting.RunnerClient {
	s.runnerMu.RLock()
	defer s.runnerMu.RUnlock()
	return s.runner
}

// RunnerHealth is an informational probe and does not take the lifecycle
// gate. In particular, catalog refresh must still be able to observe the
// runner while a long generation is in progress.
func (s *Service) RunnerHealth(ctx context.Context) (*hosting.RunnerHealth, error) {
	if s.isClosed() {
		return nil, context.Canceled
	}
	runner := s.currentRunner()
	if runner == nil {
		return nil, ErrRunnerUnavailable
	}
	return runner.Health(ctx)
}

// LoadManagedModel is the explicit, owner-driven load path. Explicit loads
// stay pinned; demand-loaded models are released by the idle timer below.
func (s *Service) LoadManagedModel(ctx context.Context, load hosting.LoadRequest) error {
	if err := s.acquireManaged(ctx); err != nil {
		return err
	}
	defer s.releaseManaged()
	runner := s.currentRunner()
	if runner == nil {
		return ErrRunnerUnavailable
	}
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return context.Canceled
	}
	previousDemand := s.demandLoaded
	s.stopIdleTimerLocked()
	s.manualPinned = true
	s.demandLoaded = false
	s.stateMu.Unlock()
	if err := runner.LoadModel(ctx, load); err != nil {
		// A failed explicit load must not leave a phantom pin that prevents a
		// later demand request from managing the runner. It must also not
		// forget an engine that is still resident: dropping demandLoaded here
		// would strand loaded weights with no idle release. The runner is the
		// authority on what survived the failure.
		loadedAfterFailure := s.probeLoaded(runner, load.ModelID, load.Filename)
		s.stateMu.Lock()
		s.manualPinned = false
		s.demandLoaded = loadedAfterFailure || previousDemand
		if s.demandLoaded {
			s.scheduleIdleUnloadLocked()
		}
		s.stateMu.Unlock()
		return err
	}
	return nil
}

func (s *Service) UnloadManagedModel(ctx context.Context) error {
	if err := s.acquireManaged(ctx); err != nil {
		return err
	}
	defer s.releaseManaged()
	runner := s.currentRunner()
	if runner == nil {
		return ErrRunnerUnavailable
	}
	s.stateMu.Lock()
	s.stopIdleTimerLocked()
	s.stateMu.Unlock()
	if err := runner.UnloadModel(ctx); err != nil {
		return err
	}
	s.stateMu.Lock()
	s.manualPinned = false
	s.demandLoaded = false
	s.stateMu.Unlock()
	return nil
}

// Close stops demand unloads, cancels queued and active requests, and makes
// any lifecycle waiter return. It is safe to call more than once.
func (s *Service) Close() {
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return
	}
	s.closed = true
	s.stopIdleTimerLocked()
	s.stateMu.Unlock()
	if s.lifecycleStop != nil {
		s.lifecycleStop()
	}
	if s.queue != nil {
		s.queue.Close()
	}
}

func (s *Service) isClosed() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.closed
}

func (s *Service) acquireManaged(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.managedGate == nil {
		return ErrRunnerUnavailable
	}
	lifecycleCtx := s.lifecycleCtx
	if lifecycleCtx == nil {
		lifecycleCtx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-lifecycleCtx.Done():
		return context.Canceled
	case <-s.managedGate:
	}
	if s.isClosed() {
		s.releaseManaged()
		return context.Canceled
	}
	return nil
}

func (s *Service) releaseManaged() {
	if s.managedGate != nil {
		s.managedGate <- struct{}{}
	}
}

func (s *Service) stopIdleTimerLocked() {
	s.idleGeneration++
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

func (s *Service) scheduleIdleUnloadLocked() {
	// Stop first, unconditionally. A policy change to "never" has to cancel
	// the timer an earlier policy armed, and every other path here wants a
	// fresh generation anyway.
	s.stopIdleTimerLocked()
	if s.closed || s.manualPinned || !s.demandLoaded || s.idleAfter < 0 || s.currentRunner() == nil {
		return
	}
	s.armIdleTimerLocked(s.idleGeneration)
}

// SetIdleUnload applies the owner's unload policy and re-evaluates any timer
// already running under the previous one.
func (s *Service) SetIdleUnload(d time.Duration) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.idleAfter = d
	s.scheduleIdleUnloadLocked()
}

// armIdleTimerLocked is called with stateMu held. The timer callback takes the
// lifecycle gate before unloading, so it can never interrupt a generation or
// race a manual model switch.
func (s *Service) armIdleTimerLocked(generation uint64) {
	if s.idleAfter < 0 {
		return
	}
	s.rearmIdleTimerLocked(generation, s.idleAfter)
}

// rearmIdleTimerLocked schedules the release check for `delay` from now under
// an existing generation. Splitting it out lets the callback postpone itself
// without re-reading a policy that may have changed underneath it.
func (s *Service) rearmIdleTimerLocked(generation uint64, delay time.Duration) {
	if delay < 0 {
		return
	}
	// A busy queue postpones the release rather than cancelling it, and at
	// zero delay it has to postpone by something or the timer spins.
	retry := delay
	if retry < minIdleRecheck {
		retry = minIdleRecheck
	}
	s.idleTimer = time.AfterFunc(delay, func() {
		unloadCtx := s.lifecycleCtx
		if unloadCtx == nil {
			unloadCtx = context.Background()
		}
		timeout := s.unloadTimeout
		if timeout <= 0 {
			timeout = defaultManagedUnloadTimeout
		}
		unloadCtx, cancel := context.WithTimeout(unloadCtx, timeout)
		defer cancel()
		if err := s.acquireManaged(unloadCtx); err != nil {
			return
		}
		defer s.releaseManaged()

		s.stateMu.Lock()
		if s.closed || generation != s.idleGeneration || s.manualPinned || !s.demandLoaded {
			s.stateMu.Unlock()
			return
		}
		active, queued := s.queue.Stats()
		if active > 0 || queued > 0 {
			// A request was admitted while this timer was waiting. Leave the
			// model warm and look again once it has had a chance to finish.
			s.idleTimer = nil
			s.rearmIdleTimerLocked(generation, retry)
			s.stateMu.Unlock()
			return
		}
		runner := s.currentRunner()
		s.stateMu.Unlock()
		if runner == nil {
			return
		}

		if err := runner.UnloadModel(unloadCtx); err != nil {
			// Preserve demandLoaded on an unload failure. A later request can
			// still use the engine, and a subsequent idle period can retry.
			s.stateMu.Lock()
			if !s.closed && generation == s.idleGeneration && s.demandLoaded && !s.manualPinned {
				s.idleTimer = nil
				s.rearmIdleTimerLocked(generation, retry)
			}
			s.stateMu.Unlock()
			return
		}
		s.stateMu.Lock()
		if generation == s.idleGeneration && !s.closed && !s.manualPinned {
			s.demandLoaded = false
			s.idleTimer = nil
		}
		s.stateMu.Unlock()
	})
}

func loadRequestForModel(model *store.HostedModelRecord) (hosting.LoadRequest, error) {
	if model == nil || model.Filename == "" {
		return hosting.LoadRequest{}, ErrManagedModelConfig
	}
	return hosting.LoadRequest{
		ModelID: model.ID, Filename: model.Filename, ContextLimit: model.ContextLimit,
		Threads: model.Threads, GPULayers: model.GPULayers, Projector: model.Projector,
		DraftModel: model.DraftModel, ExtraArgs: append([]string(nil), model.ExtraArgs...),
	}, nil
}

func runnerHealthMatchesModel(health *hosting.RunnerHealth, model *store.HostedModelRecord) bool {
	if model == nil {
		return false
	}
	return runnerHealthHolds(health, model.ID, model.Filename)
}

func runnerHealthHolds(health *hosting.RunnerHealth, modelID, filename string) bool {
	if health == nil || modelID == "" || health.LoadedModelID != modelID {
		return false
	}
	// Older runner companions omitted status. Accept that legacy response, but
	// never treat an explicit error/loading state as a usable loaded engine.
	if health.Status != "" && health.Status != "ready" && health.Status != "busy" {
		return false
	}
	return health.LoadedFile == "" || health.LoadedFile == filename
}

// probeLoaded asks the runner what it is actually holding. A load request can
// be canceled or time out after the runner has already accepted it, so the
// engine's own answer is the only reliable one.
func (s *Service) probeLoaded(runner *hosting.RunnerClient, modelID, filename string) bool {
	probeCtx := s.lifecycleCtx
	if probeCtx == nil {
		probeCtx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(probeCtx, healthProbeTimeout)
	defer cancel()
	health, err := runner.Health(probeCtx)
	return err == nil && runnerHealthHolds(health, modelID, filename)
}

// ensureManagedLoadedGated brings the runner to the requested model. The
// caller must hold the lifecycle gate; stateMu is taken here as needed.
func (s *Service) ensureManagedLoadedGated(ctx context.Context, model *store.HostedModelRecord) error {
	runner := s.currentRunner()
	if runner == nil {
		return ErrRunnerUnavailable
	}
	load, err := loadRequestForModel(model)
	if err != nil {
		return err
	}
	health, healthErr := runner.Health(ctx)
	loaded := healthErr == nil && runnerHealthMatchesModel(health, model)
	if loaded {
		// A previous load request can time out after the runner accepted it.
		// Reconcile that state here so a successfully loaded demand model is
		// never left without an eventual idle release.
		s.stateMu.Lock()
		s.stopIdleTimerLocked()
		if !s.manualPinned {
			s.demandLoaded = true
		}
		s.stateMu.Unlock()
		return nil
	}
	s.stateMu.Lock()
	previousDemand := s.demandLoaded
	previousPinned := s.manualPinned
	// A switch invalidates the previous timer. If the runner reports an
	// ambiguous load result below, the old state is restored or reconciled.
	s.stopIdleTimerLocked()
	s.demandLoaded = false
	s.manualPinned = false
	s.stateMu.Unlock()
	if err := runner.LoadModel(ctx, load); err != nil {
		// The HTTP request can be canceled after the runner has completed the
		// load. Probe before deciding whether the engine needs an idle release.
		loadedAfterFailure := s.probeLoaded(runner, model.ID, model.Filename)
		s.stateMu.Lock()
		if previousPinned {
			s.manualPinned = true
		} else if loadedAfterFailure || previousDemand {
			s.demandLoaded = true
			// Re-arm the release for either a confirmed partial load or the
			// previous demand-loaded engine that remained active after failure.
			s.scheduleIdleUnloadLocked()
		}
		s.stateMu.Unlock()
		return err
	}
	s.stateMu.Lock()
	s.demandLoaded = true
	s.manualPinned = false
	s.stateMu.Unlock()
	return nil
}

// ReloadLimits re-reads the owner's host limits after they are edited.
func (s *Service) ReloadLimits() {
	if s.store == nil {
		return
	}
	if l, err := s.store.GetHostLimits(); err == nil {
		s.queue.SetLimits(limitsFromRecord(l))
		s.SetIdleUnload(idleUnloadFromRecord(l))
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
	thinking ThinkingLevel,
	onDelta func(delta string) error,
) error {
	if model == nil || !model.Enabled {
		return ErrModelUnavailable
	}
	// A model that cannot reason is sent no reasoning instruction at all,
	// whatever the requester asked for. Forwarding one would at best be
	// ignored and at worst confuse a backend that validates its inputs.
	if !model.SupportsThinking {
		thinking = ThinkingDefault
	}
	if err := CheckContextLimit(model, messages); err != nil {
		return err
	}
	return s.queue.Submit(ctx, memberID, requestID, func(execCtx context.Context) error {
		if model.ModelType == "managed" {
			if err := s.acquireManaged(execCtx); err != nil {
				return err
			}
			defer s.releaseManaged()
			if err := s.ensureManagedLoadedGated(execCtx, model); err != nil {
				return err
			}
			runner := s.currentRunner()
			if runner == nil {
				return ErrRunnerUnavailable
			}
			err := runner.StreamChat(execCtx, memberID, model.ID, BackendModelName(model), model.MaxTokens, messages, string(thinking), onDelta)
			s.stateMu.Lock()
			s.scheduleIdleUnloadLocked()
			s.stateMu.Unlock()
			return err
		}
		return s.adapter.StreamChat(execCtx, hosting.ChatRequest{
			ReasoningEffort:     thinking.Effort(),
			EndpointURL:         model.EndpointURL,
			APIKey:              model.APIKey,
			Model:               BackendModelName(model),
			MaxTokens:           model.MaxTokens,
			Messages:            messages,
			AllowPrivateNetwork: model.AllowPrivateNetwork,
		}, onDelta)
	})
}
