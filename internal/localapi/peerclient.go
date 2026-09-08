package localapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cbaack/woolwire/internal/peerauth"
	"github.com/cbaack/woolwire/internal/store"
)

// peerAddress resolves a member to its last known Tailcat address.
func (s *Server) peerAddress(memberID string) string {
	addrs, _ := s.store.ListPeerAddresses()
	for _, pa := range addrs {
		if pa.MemberID == memberID {
			return pa.TailcatAddr
		}
	}
	return ""
}

func (s *Server) knownPeers() []store.PeerAddressRecord {
	addrs, _ := s.store.ListPeerAddresses()
	return addrs
}

// dialPeer opens a mutually authenticated connection to one member. The
// handshake proves the remote holds the device key of the member we intended
// to reach, so a rebound address cannot silently collect a conversation.
func (s *Server) dialPeer(ctx context.Context, memberID, tailcatAddr string) (net.Conn, error) {
	if memberID == "" {
		return nil, fmt.Errorf("peer member id is required")
	}
	cfg, err := peerauth.PeerClientTLSConfig(s.deviceCert, s.roster, memberID)
	if err != nil {
		return nil, err
	}

	raw, err := s.trans.Dial(ctx, tailcatAddr, s.peerPort)
	if err != nil {
		return nil, err
	}

	tlsConn := tls.Client(raw, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("peer handshake with %s failed: %w", memberID, err)
	}
	return tlsConn, nil
}

// dialBootstrap opens a connection to a room creator's bootstrap endpoint. The
// joiner has no roster yet, so it pins the room authority from the invitation
// instead of checking membership.
func (s *Server) dialBootstrap(ctx context.Context, bootstrapAddr string, authority []byte) (net.Conn, error) {
	cfg, err := peerauth.BootstrapClientTLSConfig(s.deviceCert, authority)
	if err != nil {
		return nil, err
	}

	raw, err := s.trans.Dial(ctx, bootstrapAddr, s.peerPort+bootstrapPortOffset())
	if err != nil {
		return nil, err
	}

	tlsConn := tls.Client(raw, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("bootstrap handshake failed: %w", err)
	}
	return tlsConn, nil
}

type contextConnBody struct {
	io.ReadCloser
	ctx  context.Context
	stop func() bool
}

func (b *contextConnBody) Read(p []byte) (n int, err error) {
	n, err = b.ReadCloser.Read(p)
	if err != nil && b.ctx.Err() != nil {
		return n, b.ctx.Err()
	}
	return n, err
}

func (b *contextConnBody) Close() error {
	if b.stop != nil {
		b.stop()
	}
	return b.ReadCloser.Close()
}

// peerRoundTrip writes one request over an already-open peer connection and
// returns the response. The caller owns both the connection and the response
// body; streaming callers read the body incrementally.
func peerRoundTrip(ctx context.Context, conn net.Conn, method, path string, body any) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var reader io.Reader = bytes.NewReader(nil)
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://woolwire-peer"+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
	})

	if err := req.Write(conn); err != nil {
		stop()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("send request to peer: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		stop()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("read response from peer: %w", err)
	}

	resp.Body = &contextConnBody{
		ReadCloser: resp.Body,
		ctx:        ctx,
		stop:       stop,
	}
	return resp, nil
}

// peerJSON performs one authenticated JSON request against a member and
// decodes the response into out.
func (s *Server) peerJSON(ctx context.Context, memberID, tailcatAddr, method, path string, in, out any) error {
	conn, err := s.dialPeer(ctx, memberID, tailcatAddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	resp, err := peerRoundTrip(ctx, conn, method, path, in)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("peer %s returned %d: %s", memberID, resp.StatusCode, string(b))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// readPeerDeltas consumes a peer's SSE inference response, handing each delta
// to emit. Keepalive comments are skipped rather than surfaced as content.
func readPeerDeltas(body io.Reader, emit func(string) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var currentEvent string
	var sawDone bool

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, ": ") || line == ":" {
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				sawDone = true
				return nil
			}
			if currentEvent == "error" {
				var errObj struct {
					Error string `json:"error"`
				}
				errMsg := payload
				if json.Unmarshal([]byte(payload), &errObj) == nil && errObj.Error != "" {
					errMsg = errObj.Error
				}
				return fmt.Errorf("peer error: %s", errMsg)
			}
			var chunk struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal([]byte(payload), &chunk) == nil && chunk.Delta != "" {
				if err := emit(chunk.Delta); err != nil {
					return err
				}
			}
			currentEvent = ""
		} else if line == "" && currentEvent == "error" {
			return errors.New("peer stream error")
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !sawDone {
		return io.ErrUnexpectedEOF
	}
	return nil
}
