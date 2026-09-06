package hosting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestValidateDestinationSecurityRules(t *testing.T) {
	// Prohibited destinations
	badURLs := []string{
		"http://169.254.169.254/latest/meta-data",
		"http://metadata.google.internal/computeMetadata/v1",
		"http://0.0.0.0:8000",
		"ftp://example.com/model",
		"http://external-api.openai.com/v1", // plain HTTP remote prohibited
	}
	for _, u := range badURLs {
		if err := ValidateDestination(u); err == nil {
			t.Fatalf("expected %q to fail destination validation", u)
		}
	}

	// Permitted destinations
	goodURLs := []string{
		"http://127.0.0.1:8000",
		"http://localhost:11434",
		"https://api.openai.com/v1",
	}
	for _, u := range goodURLs {
		if err := ValidateDestination(u); err != nil {
			t.Fatalf("expected %q to pass destination validation: %v", u, err)
		}
	}
}

// TestDestinationPolicyBypassForms covers each shape of the metadata and
// private-network bypasses the earlier two-literal check missed.
func TestDestinationPolicyBypassForms(t *testing.T) {
	tests := []struct {
		desc         string
		ip           string
		allowPrivate bool
		wantBlocked  bool
	}{
		{"IPv4 cloud metadata", "169.254.169.254", false, true},
		{"IPv4 cloud metadata with private override", "169.254.169.254", true, true},
		{"ECS task metadata", "169.254.170.2", false, true},
		{"IPv6 cloud metadata", "fd00:ec2::254", false, true},
		{"IPv6 cloud metadata with private override", "fd00:ec2::254", true, true},
		{"Alibaba metadata", "100.100.100.200", false, true},
		{"hex-encoded metadata address", "169.254.169.254", false, true},
		{"link-local unicast", "169.254.1.5", false, true},
		{"multicast", "224.0.0.1", false, true},
		{"IPv6 link-local", "fe80::1", false, true},
		{"IPv6 multicast", "ff02::1", false, true},
		{"unspecified", "0.0.0.0", false, true},
		{"RFC1918 class A", "10.1.2.3", false, true},
		{"RFC1918 class B", "172.16.0.1", false, true},
		{"RFC1918 class C", "192.168.1.50", false, true},
		{"IPv6 unique local", "fc00::1", false, true},
		{"carrier-grade NAT", "100.64.0.1", false, true},
		{"IPv4-mapped IPv6 private", "::ffff:192.168.1.50", false, true},
		{"RFC1918 with owner override", "192.168.1.50", true, false},
		{"IPv6 ULA with owner override", "fc00::1", true, false},
		{"loopback", "127.0.0.1", false, false},
		{"IPv6 loopback", "::1", false, false},
		{"public address", "93.184.216.34", false, false},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			policy := DestinationPolicy{AllowPrivateNetwork: tc.allowPrivate}
			err := policy.CheckIP(net.ParseIP(tc.ip))
			if tc.wantBlocked && err == nil {
				t.Fatalf("expected %s (%s) to be blocked", tc.desc, tc.ip)
			}
			if !tc.wantBlocked && err != nil {
				t.Fatalf("expected %s (%s) to be allowed, got %v", tc.desc, tc.ip, err)
			}
		})
	}
}

// TestAlternateEncodingsAreNotDialed checks the decimal and hexadecimal forms
// of 169.254.169.254. They are not valid IP literals, so the dialer must
// resolve them and judge whatever comes back rather than passing them through.
func TestAlternateEncodingsAreNotDialed(t *testing.T) {
	dial := GuardedDialer(DestinationPolicy{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for _, host := range []string{"2852039166", "0xa9fea9fe", "0251.0376.0251.0376"} {
		conn, err := dial(ctx, "tcp", net.JoinHostPort(host, "80"))
		if err == nil {
			_ = conn.Close()
			t.Fatalf("dialer connected to alternate-encoding host %q", host)
		}
	}
}

func TestGuardedDialerRefusesNonAllowlistedPortOffMachine(t *testing.T) {
	dial := GuardedDialer(DestinationPolicy{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := dial(ctx, "tcp", "93.184.216.34:2375")
	if !errors.Is(err, ErrBlockedPort) {
		t.Fatalf("expected ErrBlockedPort for an off-machine docker port, got %v", err)
	}

	// Loopback is exempt: local inference servers bind on arbitrary ports.
	if err := (DestinationPolicy{}).checkPort(net.ParseIP("127.0.0.1"), 39217); err != nil {
		t.Fatalf("loopback port should be allowed: %v", err)
	}
}

func TestValidateDestinationRejectsNonAllowlistedPort(t *testing.T) {
	if err := ValidateDestination("https://example.com:2375/v1"); !errors.Is(err, ErrBlockedPort) {
		t.Fatalf("expected ErrBlockedPort, got %v", err)
	}
	if err := ValidateDestinationWithPolicy("https://example.com:2375/v1",
		DestinationPolicy{AllowPrivateNetwork: true}); err != nil {
		t.Fatalf("owner override should permit a non-standard port: %v", err)
	}
}

func TestExternalAdapterStreamChat(t *testing.T) {
	var gotModel string
	var gotMaxTokens float64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		gotMaxTokens, _ = body["max_tokens"].(float64)

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		chunks := []string{"Hello", " from", " external", " model!"}
		for _, c := range chunks {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%s\"}}]}\n\n", c)
			flusher.Flush()
		}
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	adapter := NewExternalAdapter()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var collected string
	err := adapter.StreamChat(ctx, ChatRequest{
		EndpointURL: server.URL,
		APIKey:      "test-key",
		Model:       "llama3:latest",
		MaxTokens:   256,
		Messages:    []ChatMessage{{Role: "user", Content: "hi"}},
	}, func(delta string) error {
		collected += delta
		return nil
	})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}

	if collected != "Hello from external model!" {
		t.Fatalf("got %q, want 'Hello from external model!'", collected)
	}
	// The backend must receive the model name it knows, never the opaque
	// Woolwire model ID.
	if gotModel != "llama3:latest" {
		t.Fatalf("backend received model %q, want %q", gotModel, "llama3:latest")
	}
	if gotMaxTokens != 256 {
		t.Fatalf("backend received max_tokens %v, want 256", gotMaxTokens)
	}
}

func TestPlainHTTPRestrictedToLoopback(t *testing.T) {
	if err := requirePlainHTTPIsLocal("http://llama-runner:8080/v1", DestinationPolicy{}); err == nil {
		t.Fatal("plain HTTP to a single-label name should be refused without the private-network opt-in")
	}
	if err := requirePlainHTTPIsLocal("http://llama-runner:8080/v1",
		DestinationPolicy{AllowPrivateNetwork: true}); err != nil {
		t.Fatalf("owner override should permit a container name: %v", err)
	}
	if err := requirePlainHTTPIsLocal("http://127.0.0.1:8000/v1", DestinationPolicy{}); err != nil {
		t.Fatalf("loopback plain HTTP should be permitted: %v", err)
	}
}

// FuzzValidateDestination checks that no input panics and that every accepted
// URL is one the policy would also accept at dial time.
func FuzzValidateDestination(f *testing.F) {
	seeds := []string{
		"https://api.openai.com/v1",
		"http://127.0.0.1:8000",
		"http://169.254.169.254/",
		"http://[::1]:8080",
		"",
		"://",
		"http://%zz",
		strings.Repeat("h", 5000),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		if err := ValidateDestination(raw); err != nil {
			return
		}

		// Anything that validated must parse to an http(s) URL with a host,
		// and any literal address in it must satisfy the policy. Names are
		// deliberately deferred to the dialer, which resolves them and checks
		// every address before connecting.
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("accepted %q, which does not parse as a URL", raw)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			t.Fatalf("accepted %q with scheme %q", raw, u.Scheme)
		}
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if host == "" {
			t.Fatalf("accepted %q with an empty host", raw)
		}
		if ip := net.ParseIP(host); ip != nil {
			if err := (DestinationPolicy{}).CheckIP(ip); err != nil {
				t.Fatalf("accepted %q whose literal address is blocked: %v", raw, err)
			}
			if u.Scheme == "http" && !ip.IsLoopback() {
				t.Fatalf("accepted plain HTTP to the non-loopback literal %q", raw)
			}
		}
	})
}
