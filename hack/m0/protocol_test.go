package m0

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestFramesAreBoundedAndRoundTrip(t *testing.T) {
	var wire bytes.Buffer
	want := ProbeMessage{Type: "echo", Body: "hello"}
	if err := WriteFrame(&wire, want); err != nil {
		t.Fatal(err)
	}
	var got ProbeMessage
	if err := ReadFrame(&wire, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestFramesRejectOversizePayload(t *testing.T) {
	var wire bytes.Buffer
	if err := WriteFrame(&wire, ProbeMessage{Body: string(bytes.Repeat([]byte{'x'}, maxFrameSize))}); err == nil {
		t.Fatal("expected oversize frame to fail")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], maxFrameSize+1)
	wire.Write(header[:])
	var got ProbeMessage
	if err := ReadFrame(&wire, &got); err == nil {
		t.Fatal("expected oversize input frame to fail")
	}
}

func TestBindConnContextDeadlineInterruptsStalledRead(t *testing.T) {
	conn, peer := net.Pipe()
	defer peer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	stop, err := BindConnContext(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	done := make(chan error, 1)
	go func() {
		var message ProbeMessage
		done <- ReadFrame(conn, &message)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled frame read unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("context deadline did not interrupt stalled frame read")
	}
}

func TestBindConnContextCancellationInterruptsStalledWrite(t *testing.T) {
	conn, peer := net.Pipe()
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	stop, err := BindConnContext(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	done := make(chan error, 1)
	go func() {
		done <- WriteFrame(conn, ProbeMessage{Type: "stalled", Body: "write"})
	}()
	// net.Pipe has no write buffer, so the frame remains blocked until the
	// context callback closes conn.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled frame write unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not interrupt stalled frame write")
	}
}

func TestBindConnContextCleanupLeavesConnectionOpen(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	stop, err := BindConnContext(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	cancel()

	// stop removes the callback, so cancellation after a completed exchange
	// must not close a still-live connection.
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := peer.Write([]byte("ok"))
		writeDone <- writeErr
	}()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var got [2]byte
	if _, err := io.ReadFull(conn, got[:]); err != nil {
		t.Fatalf("connection was closed after watcher cleanup: %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("peer write failed after watcher cleanup: %v", err)
	}
}
