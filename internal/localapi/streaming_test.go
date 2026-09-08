package localapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Chrisbaack/woolwire/internal/hosting"
	"github.com/Chrisbaack/woolwire/internal/inference"
	"github.com/Chrisbaack/woolwire/internal/store"
	"github.com/Chrisbaack/woolwire/internal/transport"
)

// serveLocal runs a node's local API on a real loopback listener. The rest of
// the suite uses httptest recorders, which never exercise the server's write
// timeouts or its cookies; this is the one place both are real.
func serveLocal(t *testing.T, node *testNode) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = node.localSrv.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = node.localSrv.Shutdown(ctx)
		_ = l.Close()
	})
	return "http://" + l.Addr().String()
}

// TestStreamingSurvivesPastTheOldWriteTimeout covers task 11. The local server
// used to set a fixed 15 second WriteTimeout, which cut off chat SSE, OpenAI
// streaming, and artifact downloads mid-response. Recorder-based tests never
// noticed because they do not go through net/http's write deadlines.
func TestStreamingSurvivesPastTheOldWriteTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}

	// A backend that dribbles output for 20 seconds: past the old ceiling.
	const (
		chunks       = 20
		chunkSpacing = time.Second
	)
	backend := &http.Server{}
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < chunks; i++ {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"chunk%d \"}}]}\n\n", i)
			flusher.Flush()
			time.Sleep(chunkSpacing)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	go func() { _ = backend.Serve(backendListener) }()
	t.Cleanup(func() { _ = backend.Close() })

	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "stream-node", 4280)
	node.login(t)
	node.hostRoom(t, "Alice", "Streaming Room")

	base := serveLocal(t, node)

	client := &http.Client{Timeout: 90 * time.Second}
	jar := node.cookie

	post := func(path string, payload any, out any) {
		t.Helper()
		b, _ := json.Marshal(payload)
		req, err := http.NewRequest("POST", base+path, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(csrfHeader, "1")
		req.AddCookie(jar)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			t.Fatalf("%s returned %d: %s", path, resp.StatusCode, body)
		}
		if out != nil {
			_ = json.NewDecoder(resp.Body).Decode(out)
		}
	}

	var model store.HostedModelRecord
	post("/api/v1/hosted-models", map[string]any{
		"name":          "Slow-Model",
		"endpoint_url":  "http://" + backendListener.Addr().String() + "/v1",
		"context_limit": 8192,
		"max_tokens":    2048,
		"published":     true,
	}, &model)

	// Refresh the catalog so the chat route accepts the model.
	getCatalog, _ := http.NewRequest("GET", base+"/api/v1/catalog", nil)
	getCatalog.AddCookie(jar)
	if resp, err := client.Do(getCatalog); err == nil {
		resp.Body.Close()
	}

	var conv store.ConversationRecord
	post("/api/v1/chats", map[string]any{"title": "Slow chat"}, &conv)

	body, _ := json.Marshal(map[string]string{
		"content":        "take your time",
		"host_member_id": node.memberID,
		"model_id":       model.ID,
	})
	req, err := http.NewRequest("POST", base+"/api/v1/chats/"+conv.ID+"/message", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, "1")
	req.AddCookie(jar)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send message: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		t.Fatalf("send message returned %d: %s", resp.StatusCode, b)
	}

	var seen int
	var sawDone bool
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "data: [DONE]":
			sawDone = true
		case strings.HasPrefix(line, "data: "):
			var chunk struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) == nil && chunk.Delta != "" {
				seen++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("stream ended with an error after %s: %v", time.Since(start), err)
	}

	if elapsed := time.Since(start); elapsed < 15*time.Second {
		t.Fatalf("the backend was supposed to stream for ~%s, finished in %s", chunks*chunkSpacing, elapsed)
	}
	if seen != chunks {
		t.Fatalf("received %d of %d chunks; the stream was cut short", seen, chunks)
	}
	if !sawDone {
		t.Fatal("the stream never reached [DONE]")
	}

	// The whole conversation is persisted, not a truncated prefix.
	msgs, _ := node.store.ListMessages(conv.ID)
	if len(msgs) != 2 {
		t.Fatalf("expected the user and assistant messages, got %d", len(msgs))
	}
	if strings.Contains(msgs[1].Content, "[interrupted]") {
		t.Fatalf("the response was recorded as interrupted: %q", msgs[1].Content)
	}
	if !strings.Contains(msgs[1].Content, fmt.Sprintf("chunk%d", chunks-1)) {
		t.Fatalf("the final chunk is missing from the saved message: %q", msgs[1].Content)
	}
}

// TestContextLimitRejectedBeforeDispatch covers task 19: an oversized prompt
// must cost neither a queue slot nor a backend connection.
func TestContextLimitRejectedBeforeDispatch(t *testing.T) {
	var backendCalls int
	backend := httptestServer(t, func(w http.ResponseWriter, r *http.Request) {
		backendCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	})

	svc := inference.NewService(nil, hosting.NewExternalAdapter(), nil)
	model := &store.HostedModelRecord{
		ID:           "model-1",
		Name:         "tiny",
		ModelType:    "external",
		EndpointURL:  backend + "/v1",
		ContextLimit: 100,
		MaxTokens:    20,
		Enabled:      true,
	}

	oversized := []hosting.ChatMessage{{
		Role:    "user",
		Content: strings.Repeat("x", 100*4), // ~100 tokens against an 80 token budget
	}}

	err := svc.Execute(context.Background(), "m-a", "req-1", model, oversized, func(string) error { return nil })
	if err == nil {
		t.Fatal("an oversized prompt was accepted")
	}
	if backendCalls != 0 {
		t.Fatalf("the backend was contacted %d times for a prompt that should never have been dispatched", backendCalls)
	}
	if active, queued := svc.Queue().Stats(); active != 0 || queued != 0 {
		t.Fatalf("a rejected prompt occupied the queue: active=%d queued=%d", active, queued)
	}

	// A prompt that fits goes through.
	fits := []hosting.ChatMessage{{Role: "user", Content: "hello"}}
	if err := svc.Execute(context.Background(), "m-a", "req-2", model, fits, func(string) error { return nil }); err != nil {
		t.Fatalf("a prompt within the limit was refused: %v", err)
	}
	if backendCalls != 1 {
		t.Fatalf("expected exactly one backend call, got %d", backendCalls)
	}
}

// TestLocalRequestOccupiesTheSharedSlot covers the other half of task 19: the
// owner's own usage must be visible to the same fairness limits remote
// members are held to.
func TestLocalRequestOccupiesTheSharedSlot(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "queue-node", 4281)
	node.login(t)
	node.hostRoom(t, "Alice", "Queue Room")

	svc := node.localSrv.Inference()

	running := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = svc.Queue().Submit(context.Background(), node.memberID, "local-req", func(context.Context) error {
			close(running)
			<-release
			return nil
		})
	}()
	<-running

	active, _ := svc.Queue().Stats()
	if active != 1 {
		t.Fatalf("a local request did not occupy the shared active slot: active=%d", active)
	}

	// The catalog advertises the same queue depth to peers.
	w := node.doJSON("GET", "/api/v1/metrics", nil)
	var metrics MetricsResponse
	_ = json.NewDecoder(w.Body).Decode(&metrics)
	if metrics.Queue.Active != 1 {
		t.Fatalf("metrics report %d active, want 1", metrics.Queue.Active)
	}

	close(release)
}

func httptestServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + l.Addr().String()
}
