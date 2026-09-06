package room

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
)

type Roster struct {
	mu        sync.RWMutex
	roomID    string
	authority ed25519.PublicKey
	members   map[string]Membership
}

func NewRoster(roomID string, authority ed25519.PublicKey) (*Roster, error) {
	if roomID == "" {
		return nil, errors.New("room id required")
	}
	if len(authority) != ed25519.PublicKeySize {
		return nil, errors.New("valid authority public key required")
	}
	return &Roster{
		roomID:    roomID,
		authority: authority,
		members:   make(map[string]Membership),
	}, nil
}

func (r *Roster) Apply(m Membership) error {
	if m.RoomID != r.roomID {
		return fmt.Errorf("membership belongs to room %q, not %q", m.RoomID, r.roomID)
	}
	if err := m.Verify(r.authority); err != nil {
		return fmt.Errorf("verify membership: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	existing, ok := r.members[m.MemberID]
	if ok && m.RosterVersion <= existing.RosterVersion {
		// Ignore stale or duplicate updates
		return nil
	}

	r.members[m.MemberID] = m
	return nil
}

func (r *Roster) Get(memberID string) (Membership, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.members[memberID]
	return m, ok
}

func (r *Roster) List() []Membership {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res := make([]Membership, 0, len(r.members))
	for _, m := range r.members {
		res = append(res, m)
	}
	return res
}

func (r *Roster) IsAdmitted(memberID, devicePublic string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.members[memberID]
	if !ok {
		return false
	}
	return m.Status == StatusAdmitted && m.DevicePublic == devicePublic
}

func (r *Roster) IsRemoved(memberID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.members[memberID]
	if !ok {
		return false
	}
	return m.Status == StatusRemoved
}
