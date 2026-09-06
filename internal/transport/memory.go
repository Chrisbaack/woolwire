package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
)

// MemoryNetwork routes connections between in-memory transport nodes.
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
	l := newMemoryListener(m.addr, port, func() {
		m.mu.Lock()
		delete(m.listeners, port)
		m.mu.Unlock()
	})
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
	defer m.mu.RUnlock()
	if m.closed {
		return nil, errors.New("target transport closed")
	}
	l, ok := m.listeners[port]
	if !ok {
		return nil, fmt.Errorf("port %d not listening on %s", port, m.addr)
	}
	clientConn, serverConn := net.Pipe()
	select {
	case <-ctx.Done():
		clientConn.Close()
		serverConn.Close()
		return nil, ctx.Err()
	case l.inbound <- serverConn:
		return clientConn, nil
	case <-l.done:
		clientConn.Close()
		serverConn.Close()
		return nil, errors.New("listener closed")
	}
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

type memoryListener struct {
	addr    memoryAddr
	inbound chan net.Conn
	done    chan struct{}
	once    sync.Once
	onClose func()
}

func newMemoryListener(nodeAddr string, port uint16, onClose func()) *memoryListener {
	return &memoryListener{
		addr:    memoryAddr{node: nodeAddr, port: port},
		inbound: make(chan net.Conn, 32),
		done:    make(chan struct{}),
		onClose: onClose,
	}
}

func (l *memoryListener) Accept() (net.Conn, error) {
	select {
	case conn, ok := <-l.inbound:
		if !ok {
			return nil, errors.New("listener closed")
		}
		return conn, nil
	case <-l.done:
		return nil, errors.New("listener closed")
	}
}

func (l *memoryListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.onClose()
	})
	return nil
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
