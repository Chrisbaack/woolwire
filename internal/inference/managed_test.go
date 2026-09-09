package inference

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Chrisbaack/woolwire/internal/hosting"
	"github.com/Chrisbaack/woolwire/internal/store"
)

// runnerHarness is deliberately HTTP based: Service must serialize the same
// calls that the real runner exposes, rather than a test double bypassing the
// RunnerClient's request and cancellation behavior.
type runnerHarness struct {
	mu          sync.Mutex
	loadedID    string
	loadedFile  string
	unloads     int
	blockLoads  bool
	thinking    string
	loadStarted chan struct{}
	loadRelease chan struct{}
	server      *httptest.Server
}

func newRunnerHarness(t *testing.T) *runnerHarness {
	t.Helper()
	h := &runnerHarness{
		loadStarted: make(chan struct{}, 1),
		loadRelease: make(chan struct{}),
	}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/runner/v1/health":
			h.mu.Lock()
			body := map[string]any{
				"status":          "ready",
				"loaded_model_id": h.loadedID,
				"loaded_file":     h.loadedFile,
			}
			h.mu.Unlock()
			writeRunnerJSON(w, body)
		case "/runner/v1/models/load":
			var body struct {
				ModelID  string `json:"model_id"`
				Filename string `json:"filename"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			h.mu.Lock()
			block := h.blockLoads
			h.mu.Unlock()
			if block {
				h.loadStarted <- struct{}{}
				select {
				case <-h.loadRelease:
				case <-r.Context().Done():
					return
				}
			}
			h.mu.Lock()
			h.loadedID = body.ModelID
			h.loadedFile = body.Filename
			h.mu.Unlock()
			writeRunnerJSON(w, map[string]any{"status": "ready"})
		case "/runner/v1/models/unload":
			h.mu.Lock()
			h.unloads++
			h.loadedID = ""
			h.loadedFile = ""
			h.mu.Unlock()
			writeRunnerJSON(w, map[string]any{"ok": true})
		case "/runner/v1/inference":
			var body struct {
				Thinking string `json:"thinking"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			h.mu.Lock()
			h.thinking = body.Thinking
			h.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() { h.server.Close() })
	return h
}

func writeRunnerJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func (h *runnerHarness) client() *hosting.RunnerClient {
	return hosting.NewRunnerClient(h.server.URL, "")
}

// lastThinking is the reasoning level the most recent request carried.
func (h *runnerHarness) lastThinking() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.thinking
}

func (h *runnerHarness) unloadCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.unloads
}

func managedTestModel(id string) *store.HostedModelRecord {
	return &store.HostedModelRecord{
		ID:           id,
		Name:         id,
		ModelType:    "managed",
		Filename:     id + ".gguf",
		Enabled:      true,
		ContextLimit: 4096,
	}
}

func waitForManaged(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}

func TestManagedDemandLoadUnloadsAfterIdle(t *testing.T) {
	h := newRunnerHarness(t)
	svc := NewService(nil, nil, h.client())
	svc.idleAfter = 15 * time.Millisecond
	svc.unloadTimeout = time.Second
	defer svc.Close()

	err := svc.Execute(context.Background(), "member", "request", managedTestModel("demand"), nil, ThinkingDefault, func(string) error { return nil })
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	waitForManaged(t, time.Second, func() bool { return h.unloadCount() == 1 })
}

func TestManagedExplicitLoadStaysPinned(t *testing.T) {
	h := newRunnerHarness(t)
	svc := NewService(nil, nil, h.client())
	svc.idleAfter = 10 * time.Millisecond
	defer svc.Close()

	if err := svc.LoadManagedModel(context.Background(), hosting.LoadRequest{
		ModelID:  "pinned",
		Filename: "pinned.gguf",
	}); err != nil {
		t.Fatalf("LoadManagedModel: %v", err)
	}
	time.Sleep(4 * svc.idleAfter)
	if got := h.unloadCount(); got != 0 {
		t.Fatalf("explicit load was unloaded after idle: %d unloads", got)
	}
}

func TestManagedGateHonorsContextWhileSwitchIsInFlight(t *testing.T) {
	h := newRunnerHarness(t)
	h.mu.Lock()
	h.blockLoads = true
	h.mu.Unlock()
	svc := NewService(nil, nil, h.client())
	defer svc.Close()

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- svc.LoadManagedModel(context.Background(), hosting.LoadRequest{
			ModelID:  "first",
			Filename: "first.gguf",
		})
	}()
	select {
	case <-h.loadStarted:
	case <-time.After(time.Second):
		t.Fatal("first load did not reach runner")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := svc.LoadManagedModel(ctx, hosting.LoadRequest{ModelID: "second", Filename: "second.gguf"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second load error = %v, want context deadline", err)
	}
	close(h.loadRelease)
	if err := <-firstDone; err != nil {
		t.Fatalf("first load: %v", err)
	}
}

func TestManagedCloseCancelsIdleUnload(t *testing.T) {
	h := newRunnerHarness(t)
	svc := NewService(nil, nil, h.client())
	svc.idleAfter = 15 * time.Millisecond
	if err := svc.Execute(context.Background(), "member", "request", managedTestModel("close"), nil, ThinkingDefault, func(string) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	svc.Close()
	time.Sleep(4 * svc.idleAfter)
	if got := h.unloadCount(); got != 0 {
		t.Fatalf("Close allowed an idle unload: %d unloads", got)
	}
}

// TestManagedUnloadPolicyNeverKeepsTheEngineWarm covers the owner's choice to
// keep a demand-loaded model resident: no timer is armed, so nothing releases
// the weights until an explicit unload.
func TestManagedUnloadPolicyNeverKeepsTheEngineWarm(t *testing.T) {
	h := newRunnerHarness(t)
	svc := NewService(nil, nil, h.client())
	svc.SetIdleUnload(IdleUnloadNever)
	defer svc.Close()

	if err := svc.Execute(context.Background(), "member", "request", managedTestModel("warm"), nil, ThinkingDefault, func(string) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := h.unloadCount(); got != 0 {
		t.Fatalf("a model kept warm was unloaded %d times", got)
	}
	if err := svc.UnloadManagedModel(context.Background()); err != nil {
		t.Fatalf("UnloadManagedModel: %v", err)
	}
	if got := h.unloadCount(); got != 1 {
		t.Fatalf("an explicit unload did not reach the runner: %d", got)
	}
}

// TestManagedUnloadPolicyImmediateReleasesOnceTheQueueDrains is the other end
// of the range: a zero idle period releases the engine as soon as the request
// that loaded it is done, without waiting out a timer.
func TestManagedUnloadPolicyImmediateReleasesOnceTheQueueDrains(t *testing.T) {
	h := newRunnerHarness(t)
	svc := NewService(nil, nil, h.client())
	svc.SetIdleUnload(0)
	svc.unloadTimeout = time.Second
	defer svc.Close()

	if err := svc.Execute(context.Background(), "member", "request", managedTestModel("brief"), nil, ThinkingDefault, func(string) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	waitForManaged(t, time.Second, func() bool { return h.unloadCount() == 1 })
}

// TestManagedUnloadPolicyChangeCancelsAnArmedRelease is what makes the setting
// usable while the app is running: switching to "never" has to call off a
// release the previous policy already scheduled.
func TestManagedUnloadPolicyChangeCancelsAnArmedRelease(t *testing.T) {
	h := newRunnerHarness(t)
	svc := NewService(nil, nil, h.client())
	svc.idleAfter = 80 * time.Millisecond
	svc.unloadTimeout = time.Second
	defer svc.Close()

	if err := svc.Execute(context.Background(), "member", "request", managedTestModel("switch"), nil, ThinkingDefault, func(string) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	svc.SetIdleUnload(IdleUnloadNever)
	time.Sleep(4 * 80 * time.Millisecond)
	if got := h.unloadCount(); got != 0 {
		t.Fatalf("switching to never left a release armed: %d unloads", got)
	}
}

// TestThinkingLevelIsDroppedForAModelThatCannotReason keeps a requester's
// choice from reaching a backend that has no use for it: the level is offered
// per request, but whether a model reasons is a property of the model.
func TestThinkingLevelIsDroppedForAModelThatCannotReason(t *testing.T) {
	h := newRunnerHarness(t)
	svc := NewService(nil, nil, h.client())
	svc.SetIdleUnload(IdleUnloadNever)
	defer svc.Close()

	plain := managedTestModel("plain")
	if err := svc.Execute(context.Background(), "member", "plain-req", plain, nil, ThinkingHigh, func(string) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := h.lastThinking(); got != "" {
		t.Fatalf("a model that cannot reason was asked to think %q", got)
	}

	reasoner := managedTestModel("reasoner")
	reasoner.SupportsThinking = true
	if err := svc.Execute(context.Background(), "member", "reasoning-req", reasoner, nil, ThinkingHigh, func(string) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := h.lastThinking(); got != "high" {
		t.Fatalf("the requester's level did not reach the runner: %q", got)
	}
}
