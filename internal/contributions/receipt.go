package contributions

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Chrisbaack/woolwire/internal/identity"
)

type Receipt struct {
	RequestID          string `json:"request_id"`
	RoomID             string `json:"room_id"`
	HostMemberID       string `json:"host_member_id"`
	RequesterMemberID  string `json:"requester_member_id"`
	Timestamp          int64  `json:"timestamp"`
	Completed          bool   `json:"completed"`
	HostSignature      string `json:"host_signature"`
	RequesterSignature string `json:"requester_signature"`
	ReplicatedStatus   string `json:"replicated_status,omitempty"`
	// SigVersion selects the signing payload format; see identity.SigningPayload.
	SigVersion uint8 `json:"sig_version,omitempty"`
}

func (r *Receipt) Payload() []byte {
	if r.SigVersion == 0 {
		// Legacy colon-joined payload, kept only to verify existing receipts.
		return []byte(fmt.Sprintf("%s:%s:%s:%s:%d:%t",
			r.RoomID, r.RequestID, r.HostMemberID, r.RequesterMemberID, r.Timestamp, r.Completed))
	}
	return identity.SigningPayload(r.SigVersion, "woolwire/contribution-receipt",
		r.RoomID, r.RequestID, r.HostMemberID, r.RequesterMemberID, r.Timestamp, r.Completed)
}

func (r *Receipt) SignHost(priv ed25519.PrivateKey) error {
	if len(priv) != ed25519.PrivateKeySize {
		return errors.New("invalid host private key")
	}
	if r.Timestamp == 0 {
		r.Timestamp = time.Now().Unix()
	}
	r.SigVersion = identity.SigVersionCanonical
	sig := ed25519.Sign(priv, r.Payload())
	r.HostSignature = identity.EncodeToken(sig)
	return nil
}

func (r *Receipt) SignRequester(priv ed25519.PrivateKey) error {
	if len(priv) != ed25519.PrivateKeySize {
		return errors.New("invalid requester private key")
	}
	sig := ed25519.Sign(priv, r.Payload())
	r.RequesterSignature = identity.EncodeToken(sig)
	return nil
}

func (r *Receipt) VerifyHost(pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("invalid host public key")
	}
	sig, err := identity.DecodeToken(r.HostSignature, ed25519.SignatureSize)
	if err != nil {
		return errors.New("invalid host signature encoding")
	}
	if !ed25519.Verify(pub, r.Payload(), sig) {
		return errors.New("host signature verification failed")
	}
	return nil
}

func (r *Receipt) VerifyRequester(pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("invalid requester public key")
	}
	sig, err := identity.DecodeToken(r.RequesterSignature, ed25519.SignatureSize)
	if err != nil {
		return errors.New("invalid requester signature encoding")
	}
	if !ed25519.Verify(pub, r.Payload(), sig) {
		return errors.New("requester signature verification failed")
	}
	return nil
}

func (r *Receipt) VerifyBoth(hostPub, requesterPub ed25519.PublicKey) error {
	if err := r.VerifyHost(hostPub); err != nil {
		return err
	}
	return r.VerifyRequester(requesterPub)
}

type LeaderboardEntry struct {
	HostMemberID          string `json:"host_member_id"`
	DisplayName           string `json:"display_name"`
	Score                 int    `json:"score"`
	DistinctMembersHelped int    `json:"distinct_members_helped"`
}

// CalculateLeaderboard computes 30-day rankings:
// - Excludes self-service (requester == host)
// - Excludes incomplete requests
// - Caps at 20 points per (host, requester) pair per UTC day
// - Window: rolling 30 days
// - Suppresses members who have opted out
func CalculateLeaderboard(receipts []Receipt, memberNames map[string]string, optOutMap map[string]bool, now int64) []LeaderboardEntry {
	const (
		windowSeconds = 30 * 24 * 3600
		maxPerPairDay = 20
	)

	cutoff := now - windowSeconds

	// Map: host -> requester -> set of helped requests
	// Track per-day counts: host -> requester -> utcDay -> count
	type pairDayKey struct {
		host      string
		requester string
		utcDay    string
	}
	pairDayCounts := make(map[pairDayKey]int)

	hostScores := make(map[string]int)
	hostHelpedRequesters := make(map[string]map[string]bool)

	// Deduplicate by request ID
	seenRequests := make(map[string]bool)

	for _, r := range receipts {
		if seenRequests[r.RequestID] {
			continue
		}
		seenRequests[r.RequestID] = true

		if !r.Completed {
			continue
		}
		// Self-service is excluded
		if r.HostMemberID == r.RequesterMemberID {
			continue
		}
		// 30-day window
		if r.Timestamp < cutoff || r.Timestamp > now+300 { // allow 5m clock skew
			continue
		}
		// Check opt-out
		if optOutMap[r.HostMemberID] {
			continue
		}

		utcDay := time.Unix(r.Timestamp, 0).UTC().Format("2006-01-02")
		pdk := pairDayKey{
			host:      r.HostMemberID,
			requester: r.RequesterMemberID,
			utcDay:    utcDay,
		}

		currentDayCount := pairDayCounts[pdk]
		if currentDayCount >= maxPerPairDay {
			continue // hit 20/pair/day ceiling
		}
		pairDayCounts[pdk]++

		hostScores[r.HostMemberID]++
		if hostHelpedRequesters[r.HostMemberID] == nil {
			hostHelpedRequesters[r.HostMemberID] = make(map[string]bool)
		}
		hostHelpedRequesters[r.HostMemberID][r.RequesterMemberID] = true
	}

	var entries []LeaderboardEntry
	for hostID, score := range hostScores {
		name := memberNames[hostID]
		if name == "" {
			name = "Member " + hostID
		}
		entries = append(entries, LeaderboardEntry{
			HostMemberID:          hostID,
			DisplayName:           name,
			Score:                 score,
			DistinctMembersHelped: len(hostHelpedRequesters[hostID]),
		})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Score != entries[j].Score {
			return entries[i].Score > entries[j].Score
		}
		if entries[i].DistinctMembersHelped != entries[j].DistinctMembersHelped {
			return entries[i].DistinctMembersHelped > entries[j].DistinctMembersHelped
		}
		return entries[i].HostMemberID < entries[j].HostMemberID
	})

	if entries == nil {
		return []LeaderboardEntry{}
	}
	return entries
}
