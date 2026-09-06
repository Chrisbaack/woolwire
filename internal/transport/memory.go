package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
)

// MemoryNetwork routes connections between in-memory transport nodes. Each
// listening port is backed by a real loopback socket rather than net.Pipe:
// TLS writes its session tickets and alerts after the handshake, and an
// unbuffered pipe deadlocks the moment both ends write at once. A kernel
// socket buffers like the real transport does.
type MemoryNetwork struct {
	mu    sync.RWMutex
	nodes map[string]*MemoryTransport
}

// NewMemoryNetwork creates a virtual in-memory network.
func NewMemoryNetwork() *MemoryNetwork {
	return &MemoryNetwork{
		nodes: make(map[string]*MemoryTransport),
	}
}

// NewTransport allocates a new virtual transport node on this network.
func (n *MemoryNetwork) NewTransport(addr string) *MemoryTransport {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := &MemoryTransport{
		network:   n,
		addr:      addr,
		listeners: make(map[uint16]*memoryListener),
	}
	n.nodes[addr] = t
	return t
}

func (n *MemoryNetwork) unregister(addr string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.nodes, addr)
}

func (n *MemoryNetwork) dial(ctx context.Context, target string, port uint16) (net.Conn, error) {
	n.mu.RLock()
	node, ok := n.nodes[target]
	n.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("target node %q not found on virtual network", target)
	}
	return node.acceptInbound(ctx, port)
}

// MemoryTransport is an in-memory implementation of Transport.
type MemoryTransport struct {
	network   *MemoryNetwork
	addr      string
	mu        sync.RWMutex
	listeners map[uint16]*memoryListener
	closed    bool
}

func (m *MemoryTransport) Address() string {
	return m.addr
}

func (m *MemoryTransport) Listen(port uint16) (net.Listener, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("transport closed")
	}
	if _, exists := m.listeners[port]; exists {
		return nil, fmt.Errorf("port %d already in use", port)
	}

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	l := &memoryListener{
		Listener: inner,
		addr:     memoryAddr{node: m.addr, port: port},
		onClose: func() {
			m.mu.Lock()
			delete(m.listeners, port)
			m.mu.Unlock()
		},
	}
	m.listeners[port] = l
	return l, nil
}

func (m *MemoryTransport) Dial(ctx context.Context, target string, port uint16) (net.Conn, error) {
	m.mu.RLock()
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return nil, errors.New("transport closed")
	}
	return m.network.dial(ctx, target, port)
}

func (m *MemoryTransport) acceptInbound(ctx context.Context, port uint16) (net.Conn, error) {
	m.mu.RLock()
	closed := m.closed
	l, ok := m.listeners[port]
	m.mu.RUnlock()

	if closed {
		return nil, errors.New("target transport closed")
	}
	if !ok {
		return nil, fmt.Errorf("port %d not listening on %s", port, m.addr)
	}

	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", l.Listener.Addr().String())
}

func (m *MemoryTransport) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	listeners := make([]*memoryListener, 0, len(m.listeners))
	for _, l := range m.listeners {
		listeners = append(listeners, l)
	}
	m.listeners = make(map[uint16]*memoryListener)
	m.mu.Unlock()

	for _, l := range listeners {
		_ = l.Close()
	}
	m.network.unregister(m.addr)
	return nil
}

// memoryListener presents a loopback listener under the node's virtual
// address, so callers see transport addresses rather than 127.0.0.1 ports.
type memoryListener struct {
	net.Listener
	addr    memoryAddr
	once    sync.Once
	onClose func()
}

func (l *memoryListener) Close() error {
	var err error
	l.once.Do(func() {
		err = l.Listener.Close()
		l.onClose()
	})
	return err
}

func (l *memoryListener) Addr() net.Addr {
	return l.addr
}

type memoryAddr struct {
	node string
	port uint16
}

func (a memoryAddr) Network() string { return "memory" }
func (a memoryAddr) String() string  { return fmt.Sprintf("%s:%d", a.node, a.port) }
