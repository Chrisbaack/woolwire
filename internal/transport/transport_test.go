package transport

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestMemoryTransportListenDialAndClose(t *testing.T) {
	net := NewMemoryNetwork()
	nodeA := net.NewTransport("node-a")
	defer nodeA.Close()

	nodeB := net.NewTransport("node-b")
	defer nodeB.Close()

	l, err := nodeA.Listen(8080)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	received := make(chan string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		received <- string(buf[:n])
		_, _ = conn.Write([]byte("pong"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := nodeB.Dial(ctx, "node-a", 8080)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}

	reply := make([]byte, 1024)
	n, err := conn.Read(reply)
	if err != nil && err != io.EOF {
		t.Fatalf("read: %v", err)
	}

	select {
	case msg := <-received:
		if msg != "ping" {
			t.Fatalf("got %q, want ping", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}

	if string(reply[:n]) != "pong" {
		t.Fatalf("got reply %q, want pong", string(reply[:n]))
	}
}
