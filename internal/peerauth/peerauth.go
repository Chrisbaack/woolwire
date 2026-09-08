// Package peerauth carries the mutual-TLS identity layer that sits between
// the Tailcat transport and the peer API. Every peer connection presents the
// device certificate whose Ed25519 key is the member's device_public. The
// membership record naming that key must itself verify against the pinned
// room authority, so an attacker who reaches a node's Tailcat address without
// an authority-signed membership cannot complete a handshake.
//
// Bootstrap is deliberately a separate trust relationship: the joiner has no
// roster yet, so it pins the creator's authority-signed room certificate
// instead, and the creator accepts any well-formed device certificate.
package peerauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Chrisbaack/woolwire/internal/identity"
	"github.com/Chrisbaack/woolwire/internal/room"
	"github.com/Chrisbaack/woolwire/internal/store"
)

// RoomCertificateCommonName marks the creator's authority-signed bootstrap
// certificate. It is checked by joiners alongside the authority signature.
const RoomCertificateCommonName = "woolwire-room"

var (
	// ErrNotAdmitted is returned when a peer certificate carries a key that no
	// admitted, authority-signed membership names.
	ErrNotAdmitted = errors.New("peer device key is not an admitted member of this room")
	// ErrNoRoom is returned when the node holds no room state.
	ErrNoRoom = errors.New("no active room")
)

// Identity is the authenticated peer behind a TLS connection.
type Identity struct {
	MemberID     string
	DevicePublic string
}

// Roster answers admission questions from local persisted room state. Every
// answer re-verifies the membership signature against the pinned authority so
// a tampered members row cannot admit anyone on its own.
type Roster struct {
	store *store.Store
}

func NewRoster(s *store.Store) *Roster { return &Roster{store: s} }

// Authority returns the pinned room authority public key.
func (r *Roster) Authority() (ed25519.PublicKey, error) {
	if r == nil || r.store == nil {
		return nil, ErrNoRoom
	}
	roomRec, err := r.store.GetRoomState()
	if err != nil || roomRec == nil {
		return nil, ErrNoRoom
	}
	pub, err := identity.DecodeToken(roomRec.AuthorityPublic, ed25519.PublicKeySize)
	if err != nil {
		return nil, fmt.Errorf("decode room authority: %w", err)
	}
	return ed25519.PublicKey(pub), nil
}

// Admitted resolves an encoded device public key to its admitted membership.
func (r *Roster) Admitted(devicePublic string) (*room.Membership, error) {
	if r == nil || r.store == nil {
		return nil, ErrNoRoom
	}
	authority, err := r.Authority()
	if err != nil {
		return nil, err
	}
	rec, err := r.store.GetMemberByDevicePublic(devicePublic)
	if err != nil || rec == nil {
		return nil, ErrNotAdmitted
	}
	m := Membership(*rec)
	if m.Status != room.StatusAdmitted {
		return nil, ErrNotAdmitted
	}
	if err := m.Verify(authority); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotAdmitted, err)
	}
	return &m, nil
}

// DevicePublicFromCerts extracts the encoded Ed25519 key from a single
// presented certificate. Anything else is rejected: chains, RSA keys, and
// multi-certificate bundles have no meaning in this protocol.
func DevicePublicFromCerts(rawCerts [][]byte) (string, *x509.Certificate, error) {
	if len(rawCerts) != 1 {
		return "", nil, errors.New("peer must present exactly one certificate")
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return "", nil, fmt.Errorf("parse peer certificate: %w", err)
	}
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", nil, errors.New("peer certificate is not Ed25519")
	}
	return identity.EncodeToken(pub), cert, nil
}

// IdentityFromConnState reports the authenticated peer on an established
// connection. It re-runs the roster check rather than trusting handshake-time
// state, so a membership removed mid-connection stops being accepted.
func (r *Roster) IdentityFromConnState(cs tls.ConnectionState) (Identity, error) {
	if len(cs.PeerCertificates) != 1 {
		return Identity{}, errors.New("peer certificate is required")
	}
	pub, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return Identity{}, errors.New("peer certificate is not Ed25519")
	}
	devicePublic := identity.EncodeToken(pub)
	m, err := r.Admitted(devicePublic)
	if err != nil {
		return Identity{}, err
	}
	return Identity{MemberID: m.MemberID, DevicePublic: devicePublic}, nil
}

// BootstrapIdentityFromConnState reports the device key of a joiner that has
// not been admitted yet. It proves possession of the key through the TLS
// handshake and nothing more.
func BootstrapIdentityFromConnState(cs tls.ConnectionState) (string, error) {
	if len(cs.PeerCertificates) != 1 {
		return "", errors.New("device certificate is required")
	}
	pub, ok := cs.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return "", errors.New("device certificate is not Ed25519")
	}
	if len(pub) != ed25519.PublicKeySize {
		return "", errors.New("device certificate key is not a 32-byte Ed25519 key")
	}
	return identity.EncodeToken(pub), nil
}

// PeerServerTLSConfig serves the peer API. Only admitted members complete the
// handshake, so an unauthorized caller never reaches a route handler.
func PeerServerTLSConfig(cert tls.Certificate, r *Roster) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		// Every peer request opens a fresh connection over the transport, so
		// resumption buys nothing and only adds a ticket the server writes
		// after the handshake — which a client that immediately sends its
		// request never drains.
		SessionTicketsDisabled: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			devicePublic, _, err := DevicePublicFromCerts(rawCerts)
			if err != nil {
				return err
			}
			if _, err := r.Admitted(devicePublic); err != nil {
				return err
			}
			return nil
		},
	}
}

// PeerClientTLSConfig dials the peer API. The remote must be the admitted
// member the caller intended to reach, which is what stops a rebound address
// from collecting another member's conversation context.
func PeerClientTLSConfig(cert tls.Certificate, r *Roster, expectMemberID string) (*tls.Config, error) {
	if expectMemberID == "" {
		return nil, errors.New("expected member id is required")
	}
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{cert},
		SessionTicketsDisabled: true,
		InsecureSkipVerify:     true, // replaced by the pinned roster check below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			devicePublic, _, err := DevicePublicFromCerts(rawCerts)
			if err != nil {
				return err
			}
			m, err := r.Admitted(devicePublic)
			if err != nil {
				return err
			}
			if m.MemberID != expectMemberID {
				return fmt.Errorf("peer identifies as %q, expected %q", m.MemberID, expectMemberID)
			}
			return nil
		},
	}, nil
}

// BootstrapServerTLSConfig serves /bootstrap/v1/join. The creator presents its
// authority-signed room certificate and accepts any well-formed device
// certificate, because the joiner is by definition not on the roster yet.
func BootstrapServerTLSConfig(roomCert tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{roomCert},
		ClientAuth:             tls.RequireAnyClientCert,
		SessionTicketsDisabled: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			_, _, err := DevicePublicFromCerts(rawCerts)
			return err
		},
	}
}

// BootstrapClientTLSConfig pins the room authority carried in the invitation.
// The Tailcat address authenticates the transport endpoint; this independent
// check stops a substituted endpoint from becoming the room creator.
func BootstrapClientTLSConfig(cert tls.Certificate, authority ed25519.PublicKey) (*tls.Config, error) {
	if len(authority) != ed25519.PublicKeySize {
		return nil, errors.New("pinned room authority key is required")
	}
	pinned := append(ed25519.PublicKey(nil), authority...)
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{cert},
		SessionTicketsDisabled: true,
		InsecureSkipVerify:     true, // replaced by the pinned authority check below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			_, serverCert, err := DevicePublicFromCerts(rawCerts)
			if err != nil {
				return err
			}
			if serverCert.SignatureAlgorithm != x509.PureEd25519 ||
				!ed25519.Verify(pinned, serverCert.RawTBSCertificate, serverCert.Signature) {
				return errors.New("bootstrap certificate is not signed by the pinned room authority")
			}
			if serverCert.Subject.CommonName != RoomCertificateCommonName {
				return errors.New("bootstrap certificate has an unexpected identity")
			}
			return nil
		},
	}, nil
}

// IssueRoomCertificate signs the creator's device key with the room authority
// so joiners can pin the bootstrap endpoint from the invitation code alone.
func IssueRoomCertificate(authorityPrivate ed25519.PrivateKey, devicePublic ed25519.PublicKey) ([]byte, error) {
	if len(authorityPrivate) != ed25519.PrivateKeySize {
		return nil, errors.New("room authority private key is required")
	}
	if len(devicePublic) != ed25519.PublicKeySize {
		return nil, errors.New("device public key is required")
	}

	serialBytes := make([]byte, 16)
	if _, err := rand.Read(serialBytes); err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber:          new(big.Int).SetBytes(serialBytes),
		Subject:               pkix.Name{CommonName: RoomCertificateCommonName},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(3650 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{RoomCertificateCommonName},
	}

	// The authority is the issuer, so the parent template carries its subject
	// and the signature is made with the authority key over the device key.
	parent := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "woolwire-room-authority"},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, parent, devicePublic, authorityPrivate)
	if err != nil {
		return nil, fmt.Errorf("create room certificate: %w", err)
	}
	return der, nil
}

// MemberRecord converts a signed membership to its stored form. It exists so
// no caller has to remember the field list: an earlier hand-written copy
// dropped SigVersion, which silently made every record fail to verify on the
// receiving node.
func MemberRecord(m room.Membership) store.MemberRecord {
	return store.MemberRecord{
		MemberID:      m.MemberID,
		RoomID:        m.RoomID,
		DevicePublic:  m.DevicePublic,
		DisplayName:   m.DisplayName,
		Status:        string(m.Status),
		RosterVersion: m.RosterVersion,
		Signature:     m.Signature,
		SigVersion:    m.SigVersion,
	}
}

// Membership converts a stored roster row back to a verifiable membership.
func Membership(rec store.MemberRecord) room.Membership {
	return room.Membership{
		MemberID:      rec.MemberID,
		RoomID:        rec.RoomID,
		DevicePublic:  rec.DevicePublic,
		DisplayName:   rec.DisplayName,
		Status:        room.MemberStatus(rec.Status),
		RosterVersion: rec.RosterVersion,
		Signature:     rec.Signature,
		SigVersion:    rec.SigVersion,
	}
}
