package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

// TailcatConfig holds persistent parameters for a Tailcat-based transport.
type TailcatConfig struct {
	NodeKey      key.NodePrivate
	PresharedKey tailcat.PresharedKey
	Address      string
	Region       *tailcfg.DERPRegion
	Logf         logger.Logf
}

// TailcatTransport implements Transport using embedded Tailcat.
type TailcatTransport struct {
	mu        sync.RWMutex
	cfg       TailcatConfig
	server    *tailcat.Server
	listeners map[uint16]*tailcatBridgeListener
	clients   map[string]*tailcat.Client
	closed    bool
}

// NewTailcatTransport initializes a Tailcat transport instance.
func NewTailcatTransport(cfg TailcatConfig) (*TailcatTransport, error) {
	if cfg.NodeKey.IsZero() {
		cfg.NodeKey = key.NewNode()
	}
	if cfg.PresharedKey.IsZero() {
		cfg.PresharedKey = tailcat.NewPresharedKey()
	}
	if cfg.Logf == nil {
		cfg.Logf = logger.Discard
	}

	t := &TailcatTransport{
		cfg:       cfg,
		listeners: make(map[uint16]*tailcatBridgeListener),
		clients:   make(map[string]*tailcat.Client),
	}

	server := &tailcat.Server{
		Key:            cfg.NodeKey,
		PresharedKey:   cfg.PresharedKey,
		ServedTCPPorts: []filter.PortRange{{First: 1, Last: 65535}},
		OnTCP:          t.handleTCP,
		Region:         cfg.Region,
		Logf:           cfg.Logf,
	}
	if cfg.Address != "" {
		addr, err := tailcat.ParseAddr(tailcat.Addr(cfg.Address))
		if err == nil && len(addr.Region) == 1 {
			server.Region = addr.Region[0]
		}
	}

	if err := server.Start(); err != nil {
		return nil, fmt.Errorf("start tailcat transport: %w", err)
	}
	t.server = server
	t.cfg.Address = string(server.TailcatAddr())

	return t, nil
}

func (t *TailcatTransport) Address() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.cfg.Address
}

func (t *TailcatTransport) NodeKey() key.NodePrivate {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.cfg.NodeKey
}

func (t *TailcatTransport) PresharedKey() tailcat.PresharedKey {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.cfg.PresharedKey
}

func (t *TailcatTransport) handleTCP(port uint16) func(net.Conn) {
	t.mu.RLock()
	listener, ok := t.listeners[port]
	t.mu.RUnlock()
	if !ok || listener == nil {
		return nil
	}
	return func(c net.Conn) {
		select {
		case listener.inbound <- c:
		case <-listener.done:
			_ = c.Close()
		case <-time.After(10 * time.Second):
			_ = c.Close()
		}
	}
}

func (t *TailcatTransport) Listen(port uint16) (net.Listener, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errors.New("transport closed")
	}
	if _, exists := t.listeners[port]; exists {
		return nil, fmt.Errorf("port %d already registered", port)
	}

	l := &tailcatBridgeListener{
		addr:    bridgeAddr{addr: t.cfg.Address, port: port},
		inbound: make(chan net.Conn, 32),
		done:    make(chan struct{}),
		onClose: func() {
			t.mu.Lock()
			delete(t.listeners, port)
			t.mu.Unlock()
		},
	}
	t.listeners[port] = l
	return l, nil
}

func (t *TailcatTransport) Dial(ctx context.Context, target string, port uint16) (net.Conn, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, errors.New("transport closed")
	}
	client, ok := t.clients[target]
	if !ok {
		client = &tailcat.Client{
			Server: tailcat.Addr(target),
			Key:    t.cfg.NodeKey,
			Logf:   t.cfg.Logf,
		}
		t.clients[target] = client
	}
	t.mu.Unlock()

	return client.DialTCPPort(ctx, port)
}

func (t *TailcatTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	listeners := make([]*tailcatBridgeListener, 0, len(t.listeners))
	for _, l := range t.listeners {
		listeners = append(listeners, l)
	}
	t.listeners = make(map[uint16]*tailcatBridgeListener)

	clients := make([]*tailcat.Client, 0, len(t.clients))
	for _, c := range t.clients {
		clients = append(clients, c)
	}
	t.clients = make(map[string]*tailcat.Client)

	server := t.server
	t.server = nil
	t.mu.Unlock()

	for _, l := range listeners {
		_ = l.Close()
	}
	for _, c := range clients {
		_ = c.Close()
	}
	if server != nil {
		return server.Close()
	}
	return nil
}

type tailcatBridgeListener struct {
	addr    bridgeAddr
	inbound chan net.Conn
	done    chan struct{}
	once    sync.Once
	onClose func()
}

func (l *tailcatBridgeListener) Accept() (net.Conn, error) {
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

func (l *tailcatBridgeListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.onClose()
	})
	return nil
}

func (l *tailcatBridgeListener) Addr() net.Addr {
	return l.addr
}

type bridgeAddr struct {
	addr string
	port uint16
}

func (a bridgeAddr) Network() string { return "tailcat" }
func (a bridgeAddr) String() string  { return fmt.Sprintf("%s:%d", a.addr, a.port) }
