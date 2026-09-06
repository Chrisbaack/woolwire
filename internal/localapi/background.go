package localapi

import (
	"context"
	"crypto/ed25519"
	"math/rand"
	"time"

	"github.com/cbaack/woolwire/internal/identity"
	"github.com/cbaack/woolwire/internal/peerapi"
	"github.com/cbaack/woolwire/internal/room"
	"github.com/cbaack/woolwire/internal/store"
)

const (
	// membershipSyncInterval is the base poll period. Removals propagate on
	// this schedule; without a poller they never reached anyone but the
	// creator, so a removed member kept full access on every other node.
	membershipSyncInterval = 30 * time.Second
	// membershipSyncJitter spreads polls so a room does not synchronize into
	// one burst every thirty seconds.
	membershipSyncJitter = 10 * time.Second

	retentionInterval = time.Hour
	// retentionWindow and retentionByteCap are the documented community
	// retention policy: thirty days or 250 MiB, whichever binds first.
	retentionWindow  = 30 * 24 * time.Hour
	retentionByteCap = 250 * (1 << 20)
)

func (s *Server) membershipLoop(ctx context.Context) {
	s.SyncMembership(ctx)

	for {
		delay := membershipSyncInterval + time.Duration(rand.Int63n(int64(membershipSyncJitter)))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.SyncMembership(ctx)
		}
	}
}

func (s *Server) retentionLoop(ctx context.Context) {
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()

	s.EnforceRetention()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.EnforceRetention()
		}
	}
}

// SyncMembership pulls roster updates from every known peer, applies only
// authority-signed records that advance the roster version, and acts on any
// removal it learns about.
func (s *Server) SyncMembership(ctx context.Context) {
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		return
	}

	authorityBytes, err := identity.DecodeToken(roomRec.AuthorityPublic, ed25519.PublicKeySize)
	if err != nil {
		return
	}
	authority := ed25519.PublicKey(authorityBytes)

	device, err := s.store.GetDeviceIdentity()
	if err != nil || device == nil {
		return
	}
	myMemberID, err := peerapi.MemberIDForDevicePublic(device.DevicePublic)
	if err != nil {
		return
	}

	for _, pa := range s.knownPeers() {
		if pa.MemberID == myMemberID {
			continue
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		s.syncMembershipWithPeer(ctx, roomRec, authority, pa)
	}
}

func (s *Server) syncMembershipWithPeer(
	ctx context.Context,
	roomRec *store.RoomRecord,
	authority ed25519.PublicKey,
	peer store.PeerAddressRecord,
) {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var resp peerapi.SyncResponse
	req := peerapi.SyncRequest{
		KnownVersion: roomRec.RosterVersion,
		TailcatAddr:  s.trans.Address(),
	}
	if err := s.peerJSON(dialCtx, peer.MemberID, peer.TailcatAddr, "POST", "/peer/v1/membership/sync", req, &resp); err != nil {
		return
	}

	s.applyMembershipUpdates(roomRec, authority, resp.Members)

	// Peer addresses are advisory hints; a member's own address is authoritative
	// only for itself, which the responder enforces on its side.
	for _, pa := range resp.PeerAddresses {
		if existing, err := s.store.GetMember(pa.MemberID); err == nil && existing != nil &&
			existing.Status == string(room.StatusAdmitted) {
			_ = s.store.SavePeerAddress(pa.MemberID, pa.TailcatAddr)
		}
	}
}

// ApplyMembershipUpdates verifies and applies roster records. Only records
// that verify against the pinned authority and carry a higher roster version
// than the stored row take effect.
func (s *Server) applyMembershipUpdates(roomRec *store.RoomRecord, authority ed25519.PublicKey, updates []room.Membership) {
	highest := roomRec.RosterVersion

	for _, m := range updates {
		if m.RoomID != roomRec.RoomID {
			continue
		}
		if m.Verify(authority) != nil {
			continue
		}

		existing, err := s.store.GetMember(m.MemberID)
		if err == nil && existing != nil && m.RosterVersion <= existing.RosterVersion {
			continue
		}

		saveMembership(s.store, m)
		if m.RosterVersion > highest {
			highest = m.RosterVersion
		}

		if m.Status == room.StatusRemoved {
			// Stop serving the member now: cancel anything of theirs in the
			// queue and forget the address so nothing new is dialed to them.
			s.infer.Queue().CancelMember(m.MemberID)
			_ = s.store.DeletePeerAddress(m.MemberID)
		}
	}

	if highest > roomRec.RosterVersion {
		roomRec.RosterVersion = highest
		_ = s.store.SaveRoomState(*roomRec)
	}
}

// EnforceRetention applies the documented community retention policy: purge
// events past the thirty-day window, then trim oldest-first until the stored
// events fit the byte cap.
func (s *Server) EnforceRetention() {
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		return
	}

	horizon := time.Now().Add(-retentionWindow).Unix()
	_, _ = s.store.PurgeExpiredEvents(roomRec.RoomID, horizon)
	_, _ = s.store.PurgeEventsOverCap(roomRec.RoomID, retentionByteCap)
}
