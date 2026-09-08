package hosting

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// collect returns an onChunk that appends every delta, plus the joined result.
func collect(sb *strings.Builder) func(string) error {
	return func(delta string) error {
		sb.WriteString(delta)
		return nil
	}
}

func TestReasoningWrapperWrapsThinkingPhase(t *testing.T) {
	var sb strings.Builder
	rw := newReasoningWrapper(collect(&sb))

	for _, r := range []string{"Let", " me", " think"} {
		if err := rw.emit("", r); err != nil {
			t.Fatalf("emit reasoning: %v", err)
		}
	}
	for _, c := range []string{"The", " answer"} {
		if err := rw.emit(c, ""); err != nil {
			t.Fatalf("emit content: %v", err)
		}
	}
	if err := rw.closeThink(); err != nil {
		t.Fatalf("closeThink: %v", err)
	}

	want := "<think>Let me think</think>The answer"
	if got := sb.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A response whose token budget runs out mid-thought used to reach the caller
// as nothing at all: no content deltas, so an empty turn that was never saved.
func TestReasoningWrapperClosesUnfinishedThinking(t *testing.T) {
	var sb strings.Builder
	rw := newReasoningWrapper(collect(&sb))

	if err := rw.emit("", "still working on it"); err != nil {
		t.Fatalf("emit reasoning: %v", err)
	}
	if err := rw.closeThink(); err != nil {
		t.Fatalf("closeThink: %v", err)
	}

	want := "<think>still working on it</think>"
	if got := sb.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if sb.Len() == 0 {
		t.Error("reasoning-only response produced no text to save")
	}
}

func TestReasoningWrapperPlainContentIsUntouched(t *testing.T) {
	var sb strings.Builder
	rw := newReasoningWrapper(collect(&sb))

	if err := rw.emit("hello", ""); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := rw.closeThink(); err != nil {
		t.Fatalf("closeThink: %v", err)
	}

	if got := sb.String(); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

// StreamChat must carry reasoning_content through, not just content. The
// frames here match what llama.cpp emits for a thinking model: content is
// null for the whole reasoning phase.
func TestExternalAdapterStreamsReasoningContent(t *testing.T) {
	frames := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":null}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"17 x 23"}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":" = 391"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"391"}}]}`,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var sb strings.Builder
	err := NewExternalAdapter().StreamChat(context.Background(), ChatRequest{
		EndpointURL:         srv.URL,
		Model:               "test-model",
		Messages:            []ChatMessage{{Role: "user", Content: "17*23?"}},
		AllowPrivateNetwork: true,
	}, collect(&sb))
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}

	want := "<think>17 x 23 = 391</think>391"
	if got := sb.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The reasoning-only case, end to end: the stream must still produce a
// saveable turn rather than silently nothing.
func TestExternalAdapterReasoningOnlyResponseIsNotEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"reasoning_content":"thinking"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var sb strings.Builder
	err := NewExternalAdapter().StreamChat(context.Background(), ChatRequest{
		EndpointURL:         srv.URL,
		Model:               "test-model",
		Messages:            []ChatMessage{{Role: "user", Content: "hi"}},
		AllowPrivateNetwork: true,
	}, collect(&sb))
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}

	if got := sb.String(); got != "<think>thinking</think>" {
		t.Errorf("got %q, want %q", got, "<think>thinking</think>")
	}
}

// StreamChat must also handle backends that use the "reasoning" key instead of "reasoning_content".
func TestExternalAdapterStreamsReasoningAlternativeField(t *testing.T) {
	frames := []string{
		`{"choices":[{"index":0,"delta":{"reasoning":"pondering"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"done"}}]}`,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var sb strings.Builder
	err := NewExternalAdapter().StreamChat(context.Background(), ChatRequest{
		EndpointURL:         srv.URL,
		Model:               "test-model",
		Messages:            []ChatMessage{{Role: "user", Content: "think"}},
		AllowPrivateNetwork: true,
	}, collect(&sb))
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}

	want := "<think>pondering</think>done"
	if got := sb.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
