package hosting

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultAllowedPorts is the set of destination ports a model endpoint may use
// without an explicit owner override: HTTP, HTTPS, and the ports the common
// local inference servers listen on (llama.cpp, vLLM, Ollama, LM Studio).
var DefaultAllowedPorts = map[int]bool{
	80:    true,
	443:   true,
	8000:  true,
	8080:  true,
	1234:  true,
	11434: true,
}

// knownMetadataIPs are refused even when the owner has opted in to private
// networks. There is no legitimate reason for a model endpoint to be a cloud
// instance-metadata service, and reaching one leaks instance credentials.
var knownMetadataIPs = []net.IP{
	net.ParseIP("169.254.169.254"),
	net.ParseIP("169.254.170.2"),
	net.ParseIP("fd00:ec2::254"),
	net.ParseIP("100.100.100.200"),
}

var metadataHostnames = []string{
	"metadata.google.internal",
	"metadata.goog",
	"instance-data",
}

var (
	ErrBlockedDestination = errors.New("destination is blocked by the endpoint policy")
	ErrBlockedPort        = errors.New("destination port is not in the allowed set")
)

// DestinationPolicy is the owner's decision about one endpoint. Private ranges
// and non-standard ports are refused unless the owner explicitly opted this
// endpoint in.
type DestinationPolicy struct {
	AllowPrivateNetwork bool
}

// CheckIP applies the policy to one resolved address. It is called at dial
// time on every address the resolver returns, so a name that resolves
// differently after validation gains nothing.
func (p DestinationPolicy) CheckIP(ip net.IP) error {
	if ip == nil {
		return fmt.Errorf("%w: unresolvable address", ErrBlockedDestination)
	}
	for _, meta := range knownMetadataIPs {
		if meta != nil && ip.Equal(meta) {
			return fmt.Errorf("%w: cloud metadata services are never reachable", ErrBlockedDestination)
		}
	}
	if ip.IsLoopback() {
		return nil
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("%w: unspecified address", ErrBlockedDestination)
	}
	if ip.IsMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("%w: multicast address", ErrBlockedDestination)
	}
	if ip.IsLinkLocalUnicast() {
		return fmt.Errorf("%w: link-local address", ErrBlockedDestination)
	}
	if ip.IsPrivate() {
		if p.AllowPrivateNetwork {
			return nil
		}
		return fmt.Errorf("%w: private network address (enable the per-model private network option to allow it)", ErrBlockedDestination)
	}
	// Carrier-grade NAT and IPv4-mapped IPv6 forms of the above.
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			if p.AllowPrivateNetwork {
				return nil
			}
			return fmt.Errorf("%w: carrier-grade NAT address", ErrBlockedDestination)
		}
	}
	return nil
}

// checkPort restricts which ports may be reached off-machine. Loopback is
// exempt: local inference servers bind wherever they like, and the port
// allowlist exists to limit what a mistyped or hostile URL can reach on the
// network, not on the owner's own machine.
func (p DestinationPolicy) checkPort(ip net.IP, port int) error {
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	if p.AllowPrivateNetwork {
		return nil // the owner override covers non-standard ports too
	}
	if !DefaultAllowedPorts[port] {
		return fmt.Errorf("%w: %d", ErrBlockedPort, port)
	}
	return nil
}

func defaultPortForScheme(scheme string) int {
	if scheme == "https" {
		return 443
	}
	return 80
}

// ValidateDestination is the syntactic check applied when an endpoint is saved
// or a download is requested. The authoritative check happens at dial time in
// GuardedDialer, which sees the addresses the name actually resolves to.
func ValidateDestination(endpointURL string) error {
	return ValidateDestinationWithPolicy(endpointURL, DestinationPolicy{})
}

func ValidateDestinationWithPolicy(endpointURL string, policy DestinationPolicy) error {
	u, err := url.Parse(endpointURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q; must be http or https", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return errors.New("invalid or empty host")
	}

	port := defaultPortForScheme(u.Scheme)
	if raw := u.Port(); raw != "" {
		parsed, convErr := strconv.Atoi(raw)
		if convErr != nil || parsed <= 0 || parsed > 65535 {
			return fmt.Errorf("invalid port %q", raw)
		}
		port = parsed
	}

	lowered := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, name := range metadataHostnames {
		if lowered == name || strings.HasSuffix(lowered, "."+name) {
			return fmt.Errorf("%w: cloud metadata services are never reachable", ErrBlockedDestination)
		}
	}

	// A literal address can be judged without the resolver. Names cannot, so
	// they are deferred to the dialer rather than guessed at here.
	if ip := net.ParseIP(lowered); ip != nil {
		if err := policy.CheckIP(ip); err != nil {
			return err
		}
		if err := policy.checkPort(ip, port); err != nil {
			return err
		}
		if u.Scheme == "http" && !ip.IsLoopback() && !policy.AllowPrivateNetwork {
			return errors.New("plain HTTP is restricted to loopback; off-machine endpoints require HTTPS")
		}
	} else if lowered != "localhost" {
		// The port allowlist still applies to names, which resolve off-machine
		// unless they are literally localhost.
		if err := policy.checkPort(nil, port); err != nil {
			return err
		}
	}

	return requirePlainHTTPIsLocal(endpointURL, policy)
}

// GuardedDialer resolves the destination itself and refuses any address the
// policy rejects, then connects to the address it checked. Doing both in one
// step is what closes the DNS-rebinding window a separate validate-then-dial
// sequence leaves open.
func GuardedDialer(policy DestinationPolicy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	base := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("%w: unparsable address %q", ErrBlockedDestination, addr)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("%w: unparsable port %q", ErrBlockedDestination, portStr)
		}

		var candidates []net.IP
		if ip := net.ParseIP(host); ip != nil {
			candidates = []net.IP{ip}
		} else {
			resolved, resErr := net.DefaultResolver.LookupIPAddr(ctx, host)
			if resErr != nil {
				return nil, resErr
			}
			for _, r := range resolved {
				candidates = append(candidates, r.IP)
			}
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("%w: %q resolved to no addresses", ErrBlockedDestination, host)
		}

		// Every address must pass. A name that resolves to both a public and a
		// private address is a rebinding attempt, not a fallback.
		for _, ip := range candidates {
			if err := policy.CheckIP(ip); err != nil {
				return nil, err
			}
			if err := policy.checkPort(ip, port); err != nil {
				return nil, err
			}
		}

		var lastErr error
		for _, ip := range candidates {
			conn, dialErr := base.DialContext(ctx, network, net.JoinHostPort(ip.String(), portStr))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}

// guardedClient builds an HTTP client that refuses redirects and dials only
// through the policy guard.
func guardedClient(policy DestinationPolicy, responseHeaderTimeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: 0, // streaming and large downloads are bounded by context
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errors.New("redirects are prohibited")
		},
		Transport: &http.Transport{
			DialContext:           GuardedDialer(policy),
			ResponseHeaderTimeout: responseHeaderTimeout,
			ForceAttemptHTTP2:     true,
		},
	}
}

// requirePlainHTTPIsLocal keeps cleartext traffic on the machine. It runs
// after the URL is parsed and before any request is issued.
func requirePlainHTTPIsLocal(endpointURL string, policy DestinationPolicy) error {
	u, err := url.Parse(endpointURL)
	if err != nil || u.Scheme != "http" {
		return nil
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || host == "host.docker.internal" || host == "host.containers.internal" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	if policy.AllowPrivateNetwork {
		return nil
	}
	return errors.New("plain HTTP is restricted to loopback; off-machine endpoints require HTTPS")
}
