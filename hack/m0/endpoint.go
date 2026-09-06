package m0

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/wgengine/filter"
)

// RoomEndpoint is the smallest authenticated application endpoint needed by
// the M0 probe. It deliberately exposes one fixed Tailcat port and no proxy
// or arbitrary forwarding behavior.
type RoomEndpoint struct {
	mu        sync.RWMutex
	statePath string
	state     RoomState
	server    *tailcat.Server
	registry  *InviteRegistry
	tlsConfig *tls.Config
	onAccept  func(*tls.Conn, Hello)
	closeOnce sync.Once
}

func NewRoomEndpoint(statePath string, state RoomState, registry *InviteRegistry, onAccept func(*tls.Conn, Hello)) (*RoomEndpoint, error) {
	if onAccept == nil {
		return nil, errors.New("accept handler is required")
	}
	if err := EnsureServerCertificate(&state); err != nil {
		return nil, err
	}
	tlsConfig, err := ServerTLSConfig(state)
	if err != nil {
		return nil, err
	}
	state.AdmittedDevices = append([]string(nil), state.AdmittedDevices...)
	return &RoomEndpoint{
		statePath: statePath,
		state:     state,
		registry:  registry,
		tlsConfig: tlsConfig,
		onAccept:  onAccept,
	}, nil
}

func (e *RoomEndpoint) Start() error {
	e.mu.RLock()
	state := e.state
	e.mu.RUnlock()
	server := &tailcat.Server{
		Key:            state.TailcatKey,
		PresharedKey:   state.TailcatPresharedKey,
		ServedTCPPorts: []filter.PortRange{{First: ProtocolPort, Last: ProtocolPort}},
		OnTCP:          e.handleTCP,
	}
	if state.TailcatAddr != "" {
		address, err := tailcat.ParseAddr(state.TailcatAddr)
		if err != nil {
			return fmt.Errorf("parse persisted Tailcat address: %w", err)
		}
		if len(address.Region) != 1 {
			return errors.New("persisted Tailcat address does not pin one DERP region")
		}
		server.Region = address.Region[0]
	}
	if err := server.Start(); err != nil {
		return fmt.Errorf("start Tailcat server: %w", err)
	}
	e.mu.Lock()
	e.server = server
	if e.state.TailcatAddr == "" {
		updated := e.state
		updated.TailcatAddr = server.TailcatAddr()
		if err := SaveRoom(e.statePath, updated); err != nil {
			e.mu.Unlock()
			server.Close()
			e.mu.Lock()
			e.server = nil
			e.mu.Unlock()
			return fmt.Errorf("persist Tailcat address: %w", err)
		}
		e.state = updated
	}
	e.mu.Unlock()
	return nil
}

func (e *RoomEndpoint) Address() tailcat.Addr {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.server == nil {
		return ""
	}
	return e.state.TailcatAddr
}

func (e *RoomEndpoint) State() RoomState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	state := e.state
	state.AdmittedDevices = append([]string(nil), state.AdmittedDevices...)
	return state
}

// SetRegistry completes the small first-start bootstrap cycle: the room's
// Tailcat address is needed to create the invitation, while the listener must
// exist before the address can be known.
func (e *RoomEndpoint) SetRegistry(registry *InviteRegistry) error {
	if registry == nil {
		return errors.New("invitation registry is required")
	}
	e.mu.Lock()
	e.registry = registry
	e.mu.Unlock()
	return nil
}

func (e *RoomEndpoint) Close() error {
	var err error
	e.closeOnce.Do(func() {
		e.mu.RLock()
		server := e.server
		e.mu.RUnlock()
		if server != nil {
			err = server.Close()
		}
	})
	return err
}

// admitDevice authenticates a hello either with the current invitation or
// with a device identity that was admitted earlier. New identities are
// written to room state before the caller acknowledges admission. Holding
// the endpoint lock through the read-modify-write prevents concurrent joins
// from losing one another in the state file.
func (e *RoomEndpoint) admitDevice(hello Hello) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if hello.RoomID != e.state.RoomID {
		return false
	}
	for _, admitted := range e.state.AdmittedDevices {
		if admitted == hello.DevicePublic {
			return true
		}
	}
	if e.registry == nil || !e.registry.Accept(hello.InvitationID, hello.AdmissionSecret) {
		return false
	}
	updated := e.state
	updated.AdmittedDevices = append(append([]string(nil), e.state.AdmittedDevices...), hello.DevicePublic)
	if err := SaveRoom(e.statePath, updated); err != nil {
		return false
	}
	e.state = updated
	return true
}

func (e *RoomEndpoint) handleTCP(port uint16) func(net.Conn) {
	if port != ProtocolPort {
		return nil
	}
	return func(conn net.Conn) {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		tlsConn, err := AcceptTLS(conn, e.tlsConfig)
		if err != nil {
			return
		}
		defer tlsConn.Close()
		_ = tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
		var hello Hello
		if err := ReadFrame(tlsConn, &hello); err != nil || VerifyHelloPeer(tlsConn, hello) != nil {
			_ = WriteFrame(tlsConn, HelloAck{Accepted: false, Reason: "invalid hello"})
			return
		}
		if !e.admitDevice(hello) {
			_ = WriteFrame(tlsConn, HelloAck{Accepted: false, Reason: "admission rejected"})
			return
		}
		if err := WriteFrame(tlsConn, HelloAck{Accepted: true}); err != nil {
			return
		}
		_ = tlsConn.SetDeadline(time.Time{})
		e.onAccept(tlsConn, hello)
	}
}

// NewClient returns a Tailcat client using the persistent client key. It is
// separate from the TLS identity so the transport and application trust
// boundaries remain independently testable.
func NewClient(invitation Invitation, device DeviceState) (*tailcat.Client, error) {
	if err := invitation.Validate(); err != nil {
		return nil, err
	}
	return &tailcat.Client{Server: tailcat.Addr(invitation.BootstrapAddr), Key: device.TailcatKey}, nil
}

func DevicePublic(state DeviceState) (string, error) {
	certificate, err := x509.ParseCertificate(state.DeviceCertDER)
	if err != nil {
		return "", fmt.Errorf("parse device certificate: %w", err)
	}
	public, ok := certificate.PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", errors.New("device certificate is not Ed25519")
	}
	return encodeToken(public), nil
}

func (e *RoomEndpoint) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
