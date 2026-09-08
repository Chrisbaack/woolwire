package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Chrisbaack/woolwire/internal/transport"
)

func TestPeerRoundTripContextDeadlinesAndCancellation(t *testing.T) {
	// 1. Peer withholds response headers: client context timeout must release promptly
	t.Run("Peer withholds headers", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()

		go func() {
			conn, acceptErr := ln.Accept()
			if acceptErr == nil {
				// Withhold headers and hold connection open until client closes
				buf := make([]byte, 1024)
				_, _ = conn.Read(buf)
				time.Sleep(2 * time.Second)
				_ = conn.Close()
			}
		}()

		clientConn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer clientConn.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		start := time.Now()
		_, err = peerRoundTrip(ctx, clientConn, "POST", "/peer/v1/inference", map[string]string{"foo": "bar"})
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("expected error on withheld headers, got nil")
		}
		if elapsed > 1*time.Second {
			t.Fatalf("call took %s; expected prompt unblock on context deadline", elapsed)
		}
	})

	// 2. Peer stalls mid-body: context cancellation must unblock reading response body
	t.Run("Peer stalls mid-body read", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()

		serverStall := make(chan struct{})
		go func() {
			conn, acceptErr := ln.Accept()
			if acceptErr == nil {
				// Read request
				buf := make([]byte, 1024)
				_, _ = conn.Read(buf)
				// Send HTTP headers and initial partial body
				_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nstart\r\n"))
				<-serverStall
				_ = conn.Close()
			}
		}()
		defer close(serverStall)

		clientConn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer clientConn.Close()

		ctx, cancel := context.WithCancel(context.Background())
		resp, err := peerRoundTrip(ctx, clientConn, "POST", "/peer/v1/inference", map[string]string{"test": "body"})
		if err != nil {
			t.Fatalf("roundtrip failed: %v", err)
		}
		defer resp.Body.Close()

		// Read initial chunk
		buf := make([]byte, 5)
		n, err := io.ReadFull(resp.Body, buf)
		if err != nil || string(buf[:n]) != "start" {
			t.Fatalf("failed reading initial chunk: n=%d, err=%v", n, err)
		}

		// Cancel context and assert next read unblocks promptly
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()

		readStart := time.Now()
		readBuf := make([]byte, 10)
		_, readErr := resp.Body.Read(readBuf)
		readElapsed := time.Since(readStart)

		if readErr == nil {
			t.Fatal("expected error reading mid-body after context cancellation")
		}
		if readElapsed > 1*time.Second {
			t.Fatalf("mid-body read took %s after cancellation; connection was not closed promptly", readElapsed)
		}
	})
}

func TestReadPeerDeltasErrorHandlingAndPrematureEOF(t *testing.T) {
	// Case 1: Explicit error event
	t.Run("Explicit error frame", func(t *testing.T) {
		stream := "event: error\ndata: {\"error\":\"queue overloaded\"}\n\n"
		r := io.NopCloser(strings.NewReader(stream))
		var deltas []string
		err := readPeerDeltas(r, func(d string) error {
			deltas = append(deltas, d)
			return nil
		})
		if err == nil {
			t.Fatal("expected error on event: error, got nil")
		}
		if !strings.Contains(err.Error(), "queue overloaded") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	// Case 2: Partial deltas followed by explicit error
	t.Run("Partial deltas followed by error", func(t *testing.T) {
		stream := "data: {\"delta\":\"partial text\"}\n\n" +
			"event: error\ndata: {\"error\":\"peer execution timeout\"}\n\n"
		r := io.NopCloser(strings.NewReader(stream))
		var deltas []string
		err := readPeerDeltas(r, func(d string) error {
			deltas = append(deltas, d)
			return nil
		})
		if err == nil {
			t.Fatal("expected error when stream ends with error event, got nil")
		}
		if !strings.Contains(err.Error(), "peer execution timeout") {
			t.Fatalf("unexpected error message: %v", err)
		}
		if len(deltas) != 1 || deltas[0] != "partial text" {
			t.Fatalf("expected 1 partial delta before error, got %v", deltas)
		}
	})

	// Case 3: Premature EOF without [DONE]
	t.Run("Premature EOF without DONE", func(t *testing.T) {
		stream := "data: {\"delta\":\"incomplete\"}\n\n"
		r := io.NopCloser(strings.NewReader(stream))
		var deltas []string
		err := readPeerDeltas(r, func(d string) error {
			deltas = append(deltas, d)
			return nil
		})
		if err == nil {
			t.Fatal("expected ErrUnexpectedEOF on missing [DONE], got nil")
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("expected io.ErrUnexpectedEOF, got: %v", err)
		}
	})

	// Case 4: Normal completed stream
	t.Run("Normal completed stream with DONE", func(t *testing.T) {
		stream := "data: {\"delta\":\"hello \"}\n\n" +
			"data: {\"delta\":\"world\"}\n\n" +
			"data: [DONE]\n\n"
		r := io.NopCloser(strings.NewReader(stream))
		var deltas []string
		err := readPeerDeltas(r, func(d string) error {
			deltas = append(deltas, d)
			return nil
		})
		if err != nil {
			t.Fatalf("expected successful stream completion, got: %v", err)
		}
		if strings.Join(deltas, "") != "hello world" {
			t.Fatalf("unexpected content: %v", deltas)
		}
	})
}

func TestOpenAICompatibilityStreamErrorPropagation(t *testing.T) {
	// Verify that when inference fails mid-stream or hits premature EOF,
	// OpenAI streaming route does not emit [DONE] and non-streaming returns 502/500
	// rather than a 200 OK with finish_reason: "stop".

	// Setup a fake backend that emits partial output and then abruptly closes
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"early abort\"}}]}\n\n")
		flusher.Flush()
		// Abruptly close without [DONE]
	}))
	defer backend.Close()

	// Test via testNode
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "openai-err-node", 4290)
	node.login(t)
	tokenW := node.doJSON("GET", "/api/v1/local-api-token", nil)
	var tokenResp struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(tokenW.Body).Decode(&tokenResp)

	// Save hosted model pointing at faulty backend
	_ = node.doJSON("POST", "/api/v1/hosted-models", map[string]any{
		"id":                    "model-faulty",
		"name":                  "FaultyModel",
		"endpoint_url":          backend.URL,
		"allow_private_network": true,
		"enabled":               true,
		"published":             true,
	})

	// 1. Non-streaming OpenAI request: must NOT return 200 with finish_reason "stop"
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model": "FaultyModel",
		"messages": [{"role": "user", "content": "hi"}],
		"stream": false
	}`))
	req.Header.Set("Authorization", "Bearer "+tokenResp.Token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	node.localSrv.mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("expected error status for aborted stream in non-streaming OpenAI route, got 200: %s", w.Body.String())
	}

	// 2. Streaming OpenAI request: must NOT emit [DONE] when interrupted
	streamReq := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model": "FaultyModel",
		"messages": [{"role": "user", "content": "hi"}],
		"stream": true
	}`))
	streamReq.Header.Set("Authorization", "Bearer "+tokenResp.Token)
	streamReq.Header.Set("Content-Type", "application/json")
	streamW := httptest.NewRecorder()
	node.localSrv.mux.ServeHTTP(streamW, streamReq)

	streamBody := streamW.Body.String()
	if strings.Contains(streamBody, "[DONE]") {
		t.Fatalf("interrupted streaming request must not emit [DONE], got: %s", streamBody)
	}
}
