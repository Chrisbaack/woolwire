package transport

import (
	"context"
	"net"
)

// Transport defines the underlying network transport interface.
// Business logic operates behind this seam and has no dependency on Tailcat internals.
type Transport interface {
	// Address returns this transport endpoint's routable address string.
	Address() string

	// Listen returns a net.Listener on the transport tunnel.
	Listen(port uint16) (net.Listener, error)

	// Dial establishes an encrypted connection to target at port.
	Dial(ctx context.Context, target string, port uint16) (net.Conn, error)

	// Close shuts down the transport listener and all active connections.
	Close() error
}
