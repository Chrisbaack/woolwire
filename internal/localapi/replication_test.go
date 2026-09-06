package localapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cbaack/woolwire/internal/community"
	"github.com/cbaack/woolwire/internal/peerapi"
	"github.com/cbaack/woolwire/internal/store"
	"github.com/cbaack/woolwire/internal/transport"
)

// TestSyncConvergesAtScale covers task 16. The previous request shape carried
// every known event ID, so a room of this size produced a body past the 512
// KiB cap and sync failed permanently with 400.
func TestSyncConvergesAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}

	vNet := transport.NewMemoryNetwork()
	const peerPort = 4270

	creator := setupTestNode(t, vNet, "scale-creator", peerPort)
	creator.login(t)
	roomID, invitation := creator.hostRoom(t, "Alice", "Scale Room")

	member := setupTestNode(t, vNet, "scale-member", peerPort)
	member.login(t)
	if w := member.joinRoom(t, "Bob", invitation); w.Code != http.StatusOK {
		t.Fatalf("member join failed: %d %s", w.Code, w.Body.String())
	}

	// Seed 20k events authored by the creator, written directly so the test
	// measures replication rather than HTTP throughput.
	const total = 20000
	creatorDev, _ := creator.store.GetDeviceIdentity()
	priv := ed25519.PrivateKey(creatorDev.DevicePrivate)
	now := time.Now().Unix()

	for i := 1; i <= total; i++ {
		e := community.Event{
			RoomID:         roomID,
			ChannelID:      "chan-general",
			AuthorMemberID: creator.memberID,
			AuthorSeq:      int64(i),
			EventType:      community.EventMessage,
			Content:        fmt.Sprintf("message %d", i),
			Timestamp:      now - int64(total-i),
		}
		if err := e.Sign(priv); err != nil {
			t.Fatal(err)
		}
		if err := creator.store.SaveEvent(peerapi.EventRecord(e, "replicated")); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}

	// Every request body must stay well under the cap no matter how large the
	// history is, because the cursors are one entry per author.
	assertRequestBodyBounded := func(cursors map[string]int64) {
		body, err := json.Marshal(peerapi.CommunitySyncRequest{RoomID: roomID, Cursors: cursors})
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > 64*1024 {
			t.Fatalf("sync request body is %d bytes; cursors are supposed to keep it small", len(body))
		}
	}

	deadline := time.Now().Add(120 * time.Second)
	for {
		cursors, _ := member.store.AuthorCursors(roomID)
		assertRequestBodyBounded(cursors)

		member.localSrv.SyncCommunityEvents(context.Background())

		got, _ := member.store.ListAllEvents(roomID)
		if len(got) >= total {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sync did not converge: member holds %d of %d events", len(got), total)
		}
	}

	memberEvents, _ := member.store.ListAllEvents(roomID)
	if len(memberEvents) != total {
		t.Fatalf("member converged on %d events, want %d", len(memberEvents), total)
	}
}

// TestFutureDatedEventIsRejected covers the inbound half of task 17: an
// author-controlled timestamp far in the future would otherwise never age out
// of the retention window.
func TestFutureDatedEventIsRejected(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4271

	creator := setupTestNode(t, vNet, "retain-creator", peerPort)
	creator.login(t)
	roomID, invitation := creator.hostRoom(t, "Alice", "Retention Room")

	member := setupTestNode(t, vNet, "retain-member", peerPort)
	member.login(t)
	if w := member.joinRoom(t, "Bob", invitation); w.Code != http.StatusOK {
		t.Fatalf("member join failed: %d %s", w.Code, w.Body.String())
	}

	memberDev, _ := member.store.GetDeviceIdentity()
	priv := ed25519.PrivateKey(memberDev.DevicePrivate)

	authority, err := creatorAuthority(creator.store)
	if err != nil {
		t.Fatal(err)
	}

	sign := func(seq int64, ts int64, content string) community.Event {
		e := community.Event{
			RoomID:         roomID,
			ChannelID:      "chan-general",
			AuthorMemberID: member.memberID,
			AuthorSeq:      seq,
			EventType:      community.EventMessage,
			Content:        content,
			Timestamp:      ts,
		}
		if err := e.Sign(priv); err != nil {
			t.Fatal(err)
		}
		return e
	}

	now := time.Now()
	cases := []struct {
		desc   string
		event  community.Event
		accept bool
	}{
		{"present", sign(1, now.Unix(), "now"), true},
		{"slightly ahead within skew", sign(2, now.Add(2*time.Minute).Unix(), "soon"), true},
		{"far future", sign(3, now.Add(48*time.Hour).Unix(), "the year 3000"), false},
		{"beyond the retention horizon", sign(4, now.Add(-40*24*time.Hour).Unix(), "ancient"), false},
	}

	// A hand-crafted event with no timestamp is refused too. Event.Sign fills
	// one in, so this shape can only arrive from a peer forging JSON, and the
	// signature check catches it first.
	undated := sign(5, now.Unix(), "undated")
	undated.Timestamp = 0
	if peerapi.AcceptInboundEvent(creator.store, undated, authority) {
		t.Error("an event with no timestamp was accepted")
	}

	for _, tc := range cases {
		got := peerapi.AcceptInboundEvent(creator.store, tc.event, authority)
		if got != tc.accept {
			t.Errorf("%s: accepted=%v, want %v", tc.desc, got, tc.accept)
		}
	}
}

// TestRetentionPurgesExpiredAndEnforcesCap covers the local half of task 17.
func TestRetentionPurgesExpiredAndEnforcesCap(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4272

	node := setupTestNode(t, vNet, "purge-node", peerPort)
	node.login(t)
	roomID, _ := node.hostRoom(t, "Alice", "Purge Room")

	dev, _ := node.store.GetDeviceIdentity()
	priv := ed25519.PrivateKey(dev.DevicePrivate)

	write := func(seq int64, age time.Duration, eventType community.EventType, content string) community.Event {
		e := community.Event{
			RoomID:         roomID,
			ChannelID:      "chan-general",
			AuthorMemberID: node.memberID,
			AuthorSeq:      seq,
			EventType:      eventType,
			Content:        content,
			Timestamp:      time.Now().Add(-age).Unix(),
		}
		if err := e.Sign(priv); err != nil {
			t.Fatal(err)
		}
		if err := node.store.SaveEvent(peerapi.EventRecord(e, "replicated")); err != nil {
			t.Fatal(err)
		}
		return e
	}

	fresh := write(1, time.Hour, community.EventMessage, "recent")
	stale := write(2, 40*24*time.Hour, community.EventMessage, "expired")
	oldTombstone := write(3, 40*24*time.Hour, community.EventTombstone, "removed by moderator")

	node.localSrv.EnforceRetention()

	if !node.store.HasEvent(fresh.ID) {
		t.Fatal("retention purged an event inside the window")
	}
	if node.store.HasEvent(stale.ID) {
		t.Fatal("retention kept an event past the 30 day window")
	}
	// A moderation decision must not be lost to a storage sweep.
	if !node.store.HasEvent(oldTombstone.ID) {
		t.Fatal("retention purged a tombstone")
	}

	// The byte cap trims oldest-first.
	for i := int64(10); i < 40; i++ {
		write(i, time.Duration(40-i)*time.Hour, community.EventMessage,
			fmt.Sprintf("filler %d", i))
	}
	before, _ := node.store.EventStorageBytes(roomID)
	if before == 0 {
		t.Fatal("no stored bytes reported")
	}
	if _, err := node.store.PurgeEventsOverCap(roomID, before/2); err != nil {
		t.Fatal(err)
	}
	after, _ := node.store.EventStorageBytes(roomID)
	if after > before/2 {
		t.Fatalf("storage cap not enforced: %d bytes remain, cap was %d", after, before/2)
	}
	if !node.store.HasEvent(oldTombstone.ID) {
		t.Fatal("the byte cap purged a tombstone")
	}
}

// TestChannelsReplicate covers task 15: a channel created on one node must be
// listable and readable on every other.
func TestChannelsReplicate(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4273

	nodeA := setupTestNode(t, vNet, "chan-a", peerPort)
	nodeA.login(t)
	_, invitation := nodeA.hostRoom(t, "Alice", "Channel Room")

	nodeB := setupTestNode(t, vNet, "chan-b", peerPort)
	nodeB.login(t)
	if w := nodeB.joinRoom(t, "Bob", invitation); w.Code != http.StatusOK {
		t.Fatalf("member join failed: %d %s", w.Code, w.Body.String())
	}

	// Node A creates #ops and posts in it.
	w := nodeA.doJSON("POST", "/api/v1/community/channels", map[string]string{
		"name":        "#ops",
		"description": "operations",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create channel: %d %s", w.Code, w.Body.String())
	}
	var created store.ChannelRecord
	_ = json.NewDecoder(w.Body).Decode(&created)
	if created.Name != "ops" {
		t.Fatalf("unexpected channel name %q", created.Name)
	}

	w = nodeA.doJSON("POST", "/api/v1/community/channels/"+created.ID+"/messages",
		map[string]string{"content": "deploy at noon"})
	if w.Code != http.StatusOK {
		t.Fatalf("post to #ops: %d %s", w.Code, w.Body.String())
	}

	// Node B syncs and must see the channel and its message.
	nodeB.localSrv.SyncCommunityEvents(context.Background())

	w = nodeB.doJSON("GET", "/api/v1/community/channels", nil)
	var channels []store.ChannelRecord
	_ = json.NewDecoder(w.Body).Decode(&channels)

	var found bool
	for _, ch := range channels {
		if ch.ID == created.ID {
			found = true
			if ch.Name != "ops" || ch.Description != "operations" {
				t.Fatalf("replicated channel has wrong metadata: %#v", ch)
			}
		}
	}
	if !found {
		t.Fatalf("node B cannot see the replicated channel: %#v", channels)
	}

	w = nodeB.doJSON("GET", "/api/v1/community/channels/"+created.ID+"/messages", nil)
	var msgs []EnrichedMaterializedMessage
	_ = json.NewDecoder(w.Body).Decode(&msgs)
	if len(msgs) != 1 || msgs[0].Content != "deploy at noon" {
		t.Fatalf("node B cannot read the replicated channel: %#v", msgs)
	}
}

func creatorAuthority(s *store.Store) (ed25519.PublicKey, error) {
	roomRec, err := s.GetRoomState()
	if err != nil {
		return nil, err
	}
	priv := ed25519.PrivateKey(roomRec.AuthorityPrivate)
	return priv.Public().(ed25519.PublicKey), nil
}
