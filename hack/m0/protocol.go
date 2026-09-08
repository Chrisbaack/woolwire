package m0

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/tailscale/tailcat"
)

const (
	protocolPort    = 4242
	maxFrameSize    = 16 << 10
	protocolVersion = 1
)

// ProtocolPort is the only port exposed by the feasibility probe.
const ProtocolPort = protocolPort

type Hello struct {
	Version         uint8  `json:"v"`
	RoomID          string `json:"room"`
	InvitationID    string `json:"invite_id"`
	AdmissionSecret string `json:"secret"`
	DevicePublic    string `json:"device_public"`
}

type HelloAck struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason,omitempty"`
}

type ProbeMessage struct {
	Type string `json:"type"`
	Body string `json:"body"`
}

type ProbeReply struct {
	Type string `json:"type"`
	Body string `json:"body"`
}

func WriteFrame(w io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	if len(payload) == 0 || len(payload) > maxFrameSize {
		return errors.New("frame exceeds size limit")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeAll(w, header[:]); err != nil {
		return fmt.Errorf("write frame header: %w", err)
	}
	if err := writeAll(w, payload); err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	return nil
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(p) {
			return errors.New("writer returned an invalid byte count")
		}
		p = p[n:]
	}
	return nil
}

func ReadFrame(r io.Reader, value any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return fmt.Errorf("read frame header: %w", err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxFrameSize {
		return errors.New("frame exceeds size limit")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return fmt.Errorf("read frame: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode frame: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("frame contains trailing data")
		}
		return fmt.Errorf("decode trailing frame data: %w", err)
	}
	return nil
}

func DeviceTLSCertificate(state DeviceState) (tls.Certificate, error) {
	if len(state.DevicePrivate) != ed25519.PrivateKeySize || len(state.DeviceCertDER) == 0 {
		return tls.Certificate{}, errors.New("device certificate state is incomplete")
	}
	return tls.Certificate{Certificate: [][]byte{state.DeviceCertDER}, PrivateKey: state.DevicePrivate}, nil
}

func ServerTLSCertificate(state RoomState) (tls.Certificate, error) {
	if err := EnsureServerCertificate(&state); err != nil {
		return tls.Certificate{}, err
	}
	private, err := x509.ParsePKCS8PrivateKey(state.ServerPrivatePKCS8)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse server certificate key: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{state.ServerCertDER}, PrivateKey: private}, nil
}

func ServerTLSConfig(state RoomState) (*tls.Config, error) {
	certificate, err := ServerTLSCertificate(state)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAnyClientCert,
	}, nil
}

// ClientTLSConfig pins the room-authority signature on the server
// certificate. The Tailcat address authenticates the transport endpoint, but
// this independent application check prevents a substituted Tailcat peer
// from becoming the room server.
func ClientTLSConfig(invitation Invitation, device DeviceState) (*tls.Config, error) {
	if err := invitation.Validate(); err != nil {
		return nil, err
	}
	certificate, err := DeviceTLSCertificate(device)
	if err != nil {
		return nil, err
	}
	authorityBytes, err := decodeToken(invitation.AuthorityPublic, ed25519.PublicKeySize)
	if err != nil {
		return nil, fmt.Errorf("decode room authority: %w", err)
	}
	authority := ed25519.PublicKey(authorityBytes)
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{certificate},
		InsecureSkipVerify: true, // verification is the pinned callback below.
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) != 1 {
				return errors.New("room server must present exactly one certificate")
			}
			serverCertificate, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("parse room server certificate: %w", err)
			}
			if serverCertificate.SignatureAlgorithm != x509.PureEd25519 || !ed25519.Verify(authority, serverCertificate.RawTBSCertificate, serverCertificate.Signature) {
				return errors.New("room server certificate is not signed by the pinned room authority")
			}
			if serverCertificate.Subject.CommonName != "woolwire-room" {
				return errors.New("room server certificate has an unexpected identity")
			}
			return nil
		},
	}, nil
}

func VerifyHelloPeer(conn *tls.Conn, hello Hello) error {
	if hello.Version != protocolVersion || hello.RoomID == "" || hello.DevicePublic == "" {
		return errors.New("invalid hello")
	}
	state := conn.ConnectionState()
	if len(state.PeerCertificates) != 1 {
		return errors.New("device certificate is required")
	}
	deviceCertificate := state.PeerCertificates[0]
	publicKey, ok := deviceCertificate.PublicKey.(ed25519.PublicKey)
	if !ok {
		return errors.New("device certificate is not Ed25519")
	}
	encodedPublicKey := encodeToken(publicKey)
	if encodedPublicKey != hello.DevicePublic {
		return errors.New("hello device identity does not match its TLS certificate")
	}
	return nil
}

func DialTLS(ctx context.Context, client *tailcat.Client, invitation Invitation, device DeviceState) (*tls.Conn, error) {
	conn, err := client.DialTCPPort(ctx, ProtocolPort)
	if err != nil {
		return nil, err
	}
	config, err := ClientTLSConfig(invitation, device)
	if err != nil {
		conn.Close()
		return nil, err
	}
	tlsConn := tls.Client(conn, config)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		tlsConn.Close()
		return nil, err
	}
	return tlsConn, nil
}

// BindConnContext carries a request context through all subsequent frame
// reads and writes on conn. A context deadline is installed on the socket,
// and cancellation closes the socket so a blocked read or write wakes up
// immediately. The returned cleanup function must be called when the
// exchange finishes; it removes the cancellation callback when the context
// remains live.
func BindConnContext(ctx context.Context, conn net.Conn) (func(), error) {
	if ctx == nil {
		return nil, errors.New("connection context is required")
	}
	if conn == nil {
		return nil, errors.New("connection is required")
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set connection context deadline: %w", err)
		}
	}
	stop := context.AfterFunc(ctx, func() {
		// Close interrupts both directions even when the peer is not reading
		// or writing. SetDeadline also wakes implementations that report a
		// delayed close to a blocked operation.
		_ = conn.SetDeadline(time.Now())
		_ = conn.Close()
	})
	return func() { stop() }, nil
}

func AcceptTLS(conn net.Conn, config *tls.Config) (*tls.Conn, error) {
	tlsConn := tls.Server(conn, config)
	if err := tlsConn.Handshake(); err != nil {
		tlsConn.Close()
		return nil, err
	}
	return tlsConn, nil
}

type PeerCredential struct {
	Version      uint8  `json:"v"`
	RoomID       string `json:"room_id"`
	DevicePublic string `json:"device_public"`
	TailcatAddr  string `json:"tailcat_addr"`
	Signature    string `json:"sig"`
}

func (c PeerCredential) Validate() error {
	if c.Version != protocolVersion {
		return fmt.Errorf("unsupported peer credential version %d", c.Version)
	}
	if c.RoomID == "" || len(c.RoomID) > 128 {
		return errors.New("room id is invalid")
	}
	if _, err := decodeToken(c.DevicePublic, ed25519.PublicKeySize); err != nil {
		return fmt.Errorf("device public key: %w", err)
	}
	if c.TailcatAddr == "" || len(c.TailcatAddr) > 8192 {
		return errors.New("peer address is invalid")
	}
	if _, err := tailcat.ParseAddr(tailcat.Addr(c.TailcatAddr)); err != nil {
		return fmt.Errorf("peer address: %w", err)
	}
	if _, err := decodeToken(c.Signature, ed25519.SignatureSize); err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	return nil
}

func (c PeerCredential) payload() []byte {
	return []byte(fmt.Sprintf("%d:%s:%s:%s", c.Version, c.RoomID, c.DevicePublic, c.TailcatAddr))
}

func SignPeerCredential(authorityPrivate ed25519.PrivateKey, roomID, devicePublic string, addr tailcat.Addr) (PeerCredential, error) {
	if len(authorityPrivate) != ed25519.PrivateKeySize {
		return PeerCredential{}, errors.New("invalid room authority private key")
	}
	cred := PeerCredential{
		Version:      protocolVersion,
		RoomID:       roomID,
		DevicePublic: devicePublic,
		TailcatAddr:  string(addr),
	}
	sig := ed25519.Sign(authorityPrivate, cred.payload())
	cred.Signature = encodeToken(sig)
	if err := cred.Validate(); err != nil {
		return PeerCredential{}, err
	}
	return cred, nil
}

func VerifyPeerCredential(cred PeerCredential, authority ed25519.PublicKey) error {
	if err := cred.Validate(); err != nil {
		return err
	}
	if len(authority) != ed25519.PublicKeySize {
		return errors.New("invalid authority key")
	}
	sig, err := decodeToken(cred.Signature, ed25519.SignatureSize)
	if err != nil {
		return err
	}
	if !ed25519.Verify(authority, cred.payload(), sig) {
		return errors.New("peer credential signature is invalid")
	}
	return nil
}

type PeerHello struct {
	Version    uint8          `json:"v"`
	Credential PeerCredential `json:"credential"`
}

func VerifyPeerHello(conn *tls.Conn, hello PeerHello, authority ed25519.PublicKey) error {
	if hello.Version != protocolVersion {
		return errors.New("invalid peer hello version")
	}
	if err := VerifyPeerCredential(hello.Credential, authority); err != nil {
		return fmt.Errorf("invalid peer credential: %w", err)
	}
	state := conn.ConnectionState()
	if len(state.PeerCertificates) != 1 {
		return errors.New("device certificate is required")
	}
	deviceCertificate := state.PeerCertificates[0]
	publicKey, ok := deviceCertificate.PublicKey.(ed25519.PublicKey)
	if !ok {
		return errors.New("device certificate is not Ed25519")
	}
	encodedPublicKey := encodeToken(publicKey)
	if encodedPublicKey != hello.Credential.DevicePublic {
		return errors.New("peer hello identity does not match client TLS certificate")
	}
	return nil
}

func PeerServerTLSConfig(state PeerState, authority ed25519.PublicKey) (*tls.Config, error) {
	if len(state.PeerCertDER) == 0 || len(state.Device.DevicePrivate) != ed25519.PrivateKeySize {
		return nil, errors.New("peer certificate or private key is missing")
	}
	cert := tls.Certificate{
		Certificate: [][]byte{state.PeerCertDER},
		PrivateKey:  state.Device.DevicePrivate,
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
	}, nil
}

func PeerClientTLSConfig(credential PeerCredential, device DeviceState, authority ed25519.PublicKey) (*tls.Config, error) {
	if err := VerifyPeerCredential(credential, authority); err != nil {
		return nil, err
	}
	certificate, err := DeviceTLSCertificate(device)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{certificate},
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) != 1 {
				return errors.New("peer server must present exactly one certificate")
			}
			peerCertificate, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("parse peer server certificate: %w", err)
			}
			if peerCertificate.SignatureAlgorithm != x509.PureEd25519 || !ed25519.Verify(authority, peerCertificate.RawTBSCertificate, peerCertificate.Signature) {
				return errors.New("peer server certificate is not signed by the pinned room authority")
			}
			if peerCertificate.Subject.CommonName != "woolwire-peer" {
				return errors.New("peer server certificate has an unexpected identity")
			}
			pub, ok := peerCertificate.PublicKey.(ed25519.PublicKey)
			if !ok {
				return errors.New("peer server certificate is not Ed25519")
			}
			if encodeToken(pub) != credential.DevicePublic {
				return errors.New("peer certificate does not match credential device public key")
			}
			return nil
		},
	}, nil
}
