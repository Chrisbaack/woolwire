package hosting

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidateDestinationSecurityRules(t *testing.T) {
	// Prohibited destinations
	badURLs := []string{
		"http://169.254.169.254/latest/meta-data",
		"http://metadata.google.internal/computeMetadata/v1",
		"http://0.0.0.0:8000",
		"ftp://example.com/model",
		"http://external-api.openai.com/v1", // plain HTTP remote prohibited
	}
	for _, u := range badURLs {
		if err := ValidateDestination(u); err == nil {
			t.Fatalf("expected %q to fail destination validation", u)
		}
	}

	// Permitted destinations
	goodURLs := []string{
		"http://127.0.0.1:8000",
		"http://localhost:11434",
		"http://llama-runner:8080",
		"https://api.openai.com/v1",
	}
	for _, u := range goodURLs {
		if err := ValidateDestination(u); err != nil {
			t.Fatalf("expected %q to pass destination validation: %v", u, err)
		}
	}
}

func TestExternalAdapterStreamChat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		chunks := []string{"Hello", " from", " external", " model!"}
		for _, c := range chunks {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n\n", c)
			flusher.Flush()
		}
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	adapter := NewExternalAdapter()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var collected string
	err := adapter.StreamChat(
		ctx,
		server.URL,
		"test-key",
		"test-model",
		[]ChatMessage{{Role: "user", Content: "hi"}},
		func(delta string) error {
			collected += delta
			return nil
		},
	)
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}

	if collected != "Hello from external model!" {
		t.Fatalf("got %q, want 'Hello from external model!'", collected)
	}
}
