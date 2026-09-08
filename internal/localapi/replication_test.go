package localapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Chrisbaack/woolwire/internal/community"
	"github.com/Chrisbaack/woolwire/internal/contributions"
	"github.com/Chrisbaack/woolwire/internal/peerapi"
	"github.com/Chrisbaack/woolwire/internal/store"
	"github.com/Chrisbaack/woolwire/internal/transport"
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

// TestCommunityReplicationPreservesGapsAndConflicts covers task 35:
// Sync nodes holding different subsets of an author's log (gaps) and different events
// at the same sequence (conflicts). Both must converge and quarantine consistently.
func TestCommunityReplicationPreservesGapsAndConflicts(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4275

	nodeA := setupTestNode(t, vNet, "rep-gap-a", peerPort)
	nodeA.login(t)
	roomID, invitation := nodeA.hostRoom(t, "Alice", "Gap Room")

	nodeB := setupTestNode(t, vNet, "rep-gap-b", peerPort)
	nodeB.login(t)
	if w := nodeB.joinRoom(t, "Bob", invitation); w.Code != http.StatusOK {
		t.Fatalf("nodeB join failed: %d %s", w.Code, w.Body.String())
	}

	devA, _ := nodeA.store.GetDeviceIdentity()
	privA := ed25519.PrivateKey(devA.DevicePrivate)
	now := time.Now().Unix()

	makeEvent := func(seq int64, content string) community.Event {
		e := community.Event{
			RoomID:         roomID,
			ChannelID:      "chan-general",
			AuthorMemberID: nodeA.memberID,
			AuthorSeq:      seq,
			EventType:      community.EventMessage,
			Content:        content,
			Timestamp:      now + seq,
		}
		if err := e.Sign(privA); err != nil {
			t.Fatal(err)
		}
		return e
	}

	// Create events:
	// seq 1
	e1 := makeEvent(1, "message 1")
	// seq 2 (missing on nodeA initially)
	e2 := makeEvent(2, "message 2")
	// seq 3 (missing on nodeA initially)
	e3 := makeEvent(3, "message 3")
	// seq 4 conflict: two distinct events signed by the author at seq 4
	e4a := makeEvent(4, "conflict branch A")
	e4b := makeEvent(4, "conflict branch B")
	// seq 5
	e5 := makeEvent(5, "message 5")

	// Node A holds seq 1, 4a, 5 (missing 2 and 3; holds 4a)
	for _, e := range []community.Event{e1, e4a, e5} {
		_ = nodeA.store.SaveEvent(peerapi.EventRecord(e, "local"))
	}

	// Node B holds seq 1, 2, 3, 4b, 5 (missing 4a; holds 4b)
	for _, e := range []community.Event{e1, e2, e3, e4b, e5} {
		_ = nodeB.store.SaveEvent(peerapi.EventRecord(e, "local"))
	}

	// Exchange sync between Node A and Node B
	nodeA.localSrv.SyncCommunityEvents(context.Background())
	nodeB.localSrv.SyncCommunityEvents(context.Background())
	nodeA.localSrv.SyncCommunityEvents(context.Background())

	// Both nodes must now have all 6 events: e1, e2, e3, e4a, e4b, e5
	eventsA, _ := nodeA.store.ListAllEvents(roomID)
	eventsB, _ := nodeB.store.ListAllEvents(roomID)

	if len(eventsA) != 6 {
		t.Fatalf("expected Node A to converge on 6 events (recovering gaps and conflict), got %d", len(eventsA))
	}
	if len(eventsB) != 6 {
		t.Fatalf("expected Node B to converge on 6 events, got %d", len(eventsB))
	}

	// Assert specific missing events exist on Node A
	if !nodeA.store.HasEvent(e2.ID) || !nodeA.store.HasEvent(e3.ID) {
		t.Fatal("Node A failed to recover missing gap events e2 or e3")
	}
	if !nodeA.store.HasEvent(e4b.ID) {
		t.Fatal("Node A failed to discover conflicting event e4b at sequence 4")
	}
	if !nodeB.store.HasEvent(e4a.ID) {
		t.Fatal("Node B failed to discover conflicting event e4a at sequence 4")
	}

	// Verify consistent conflict resolution across both nodes
	evListA := make([]community.Event, 0, len(eventsA))
	for _, r := range eventsA {
		evListA = append(evListA, peerapi.EventFromRecord(r))
	}
	evListB := make([]community.Event, 0, len(eventsB))
	for _, r := range eventsB {
		evListB = append(evListB, peerapi.EventFromRecord(r))
	}
	resA := community.Materialize(evListA)
	resB := community.Materialize(evListB)

	if len(resA.ConflictedAuthors) == 0 || len(resB.ConflictedAuthors) == 0 {
		t.Fatal("expected conflicting events at sequence 4 to surface conflicted author")
	}
	if len(resA.Messages) != len(resB.Messages) || len(resA.ConflictedAuthors) != len(resB.ConflictedAuthors) {
		t.Fatalf("conflict resolution mismatch between nodes: msgsA=%d msgsB=%d confA=%d confB=%d", len(resA.Messages), len(resB.Messages), len(resA.ConflictedAuthors), len(resB.ConflictedAuthors))
	}
}

// TestContributionsSyncSameTimestampAcrossPageAndOutOfOrder covers task 35:
// Sync distinct same-second receipts across a page boundary and receipts arriving out of order;
// every valid receipt must reach both nodes.
func TestContributionsSyncSameTimestampAcrossPageAndOutOfOrder(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4276

	nodeA := setupTestNode(t, vNet, "rep-rcpt-a", peerPort)
	nodeA.login(t)
	roomID, invitation := nodeA.hostRoom(t, "Alice", "Receipt Room")

	nodeB := setupTestNode(t, vNet, "rep-rcpt-b", peerPort)
	nodeB.login(t)
	if w := nodeB.joinRoom(t, "Bob", invitation); w.Code != http.StatusOK {
		t.Fatalf("nodeB join failed: %d %s", w.Code, w.Body.String())
	}

	devA, _ := nodeA.store.GetDeviceIdentity()
	privA := ed25519.PrivateKey(devA.DevicePrivate)
	devB, _ := nodeB.store.GetDeviceIdentity()
	privB := ed25519.PrivateKey(devB.DevicePrivate)

	now := time.Now().Unix()

	makeReceipt := func(reqID string, ts int64) contributions.Receipt {
		r := contributions.Receipt{
			RoomID:            roomID,
			RequestID:         reqID,
			HostMemberID:      nodeA.memberID,
			RequesterMemberID: nodeB.memberID,
			Timestamp:         ts,
			Completed:         true,
		}
		_ = r.SignHost(privA)
		_ = r.SignRequester(privB)
		return r
	}

	// 1. Generate 15 distinct receipts sharing the EXACT same second on Node A
	const sameTsCount = 15
	for i := 0; i < sameTsCount; i++ {
		r := makeReceipt(fmt.Sprintf("req-same-%02d", i), now)
		_ = nodeA.store.SaveReceipt(peerapi.ReceiptRecord(r, "local"))
	}

	// 2. Generate 3 receipts with EARLIER timestamps on Node B arriving out of order
	for i := 0; i < 3; i++ {
		r := makeReceipt(fmt.Sprintf("req-earlier-%02d", i), now-int64(100+i))
		_ = nodeB.store.SaveReceipt(peerapi.ReceiptRecord(r, "local"))
	}

	// Sync in both directions
	nodeA.localSrv.SyncContributions(context.Background())
	nodeB.localSrv.SyncContributions(context.Background())
	nodeA.localSrv.SyncContributions(context.Background())

	// Total receipts created: 15 + 3 = 18. Both nodes must have all 18 receipts!
	rcptsA, _ := nodeA.store.ListReceipts(roomID)
	rcptsB, _ := nodeB.store.ListReceipts(roomID)

	const expectedTotal = 18
	if len(rcptsA) != expectedTotal {
		t.Fatalf("Node A holds %d receipts, want %d", len(rcptsA), expectedTotal)
	}
	if len(rcptsB) != expectedTotal {
		t.Fatalf("Node B holds %d receipts, want %d", len(rcptsB), expectedTotal)
	}

	// Verify all same-timestamp receipts reached Node B
	for i := 0; i < sameTsCount; i++ {
		id := fmt.Sprintf("req-same-%02d", i)
		if r, err := nodeB.store.GetReceipt(id); err != nil || r == nil {
			t.Fatalf("Node B missing same-timestamp receipt %s", id)
		}
	}

	// Verify all earlier out-of-order receipts reached Node A
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("req-earlier-%02d", i)
		if r, err := nodeA.store.GetReceipt(id); err != nil || r == nil {
			t.Fatalf("Node A missing out-of-order earlier receipt %s", id)
		}
	}
}
