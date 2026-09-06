// Package sse writes Server-Sent Events responses that survive long model
// generations. A fixed server-wide WriteTimeout cuts every stream off
// mid-answer, so streaming handlers clear the deadline per response and set
// short per-write deadlines instead; a keepalive comment every 15 seconds
// keeps idle proxies and clients from treating a slow model as a dead
// connection.
package sse

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// KeepaliveInterval is how often a comment frame is emitted on an idle stream.
const KeepaliveInterval = 15 * time.Second

// writeDeadline bounds one write without bounding the whole response.
const writeDeadline = 30 * time.Second

// Stream serializes writes from the handler and the keepalive goroutine onto
// one response.
type Stream struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	rc      *http.ResponseController
	flusher http.Flusher
	done    chan struct{}
	closed  bool
}

// New writes the SSE headers and starts the keepalive.
func New(w http.ResponseWriter) (*Stream, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming unsupported")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	s := &Stream{
		w:       w,
		rc:      http.NewResponseController(w),
		flusher: flusher,
		done:    make(chan struct{}),
	}
	_ = s.rc.SetWriteDeadline(time.Time{})

	s.mu.Lock()
	s.flusher.Flush()
	s.mu.Unlock()

	go s.keepalive()
	return s, nil
}

func (s *Stream) keepalive() {
	ticker := time.NewTicker(KeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if err := s.SendRaw(": ping\n\n"); err != nil {
				return
			}
		}
	}
}

// SendJSON writes one frame, optionally under a named event.
func (s *Stream) SendJSON(event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if event != "" {
		return s.SendRaw(fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(b)))
	}
	return s.SendRaw(fmt.Sprintf("data: %s\n\n", string(b)))
}

// SendRaw writes a pre-formatted frame.
func (s *Stream) SendRaw(frame string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("stream closed")
	}

	_ = s.rc.SetWriteDeadline(time.Now().Add(writeDeadline))
	_, err := fmt.Fprint(s.w, frame)
	_ = s.rc.SetWriteDeadline(time.Time{})
	if err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// Stop ends the keepalive. It must be called when the handler returns.
func (s *Stream) Stop() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	s.mu.Unlock()
}
