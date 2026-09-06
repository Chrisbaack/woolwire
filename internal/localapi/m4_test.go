package localapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cbaack/woolwire/internal/community"
	"github.com/cbaack/woolwire/internal/store"
	"github.com/cbaack/woolwire/internal/transport"
)

func TestM4CommunityPartitionRejoinAndMaterialization(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4243

	// Setup 2 nodes: Node 1 (Alice, creator) and Node 2 (Bob, member)
	node1 := setupTestNode(t, vNet, "m4-node-1", peerPort)
	defer node1.trans.Close()
	defer node1.store.Close()
	node1.login(t)

	node2 := setupTestNode(t, vNet, "m4-node-2", peerPort)
	defer node2.trans.Close()
	defer node2.store.Close()
	node2.login(t)

	_ = node1.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Alice"})
	_ = node2.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Bob"})

	// Alice creates room
	w := node1.doJSON("POST", "/api/v1/room/host", map[string]string{"room_name": "M4 Community Room"})
	if w.Code != http.StatusOK {
		t.Fatalf("host room failed: %d, %s", w.Code, w.Body.String())
	}
	var hostResp struct {
		RoomID         string `json:"room_id"`
		InvitationCode string `json:"invitation_code"`
	}
	_ = json.NewDecoder(w.Body).Decode(&hostResp)

	// Bob joins room
	w = node2.doJSON("POST", "/api/v1/room/join", map[string]string{"invitation_code": hostResp.InvitationCode})
	if w.Code != http.StatusOK {
		t.Fatalf("node2 join room failed: %d, %s", w.Code, w.Body.String())
	}

	aliceDev, _ := node1.store.GetDeviceIdentity()
	aliceMemberID := "m-" + aliceDev.DevicePublic[:16]
	bobDev, _ := node2.store.GetDeviceIdentity()
	bobMemberID := "m-" + bobDev.DevicePublic[:16]

	// Ensure general channel exists
	w = node1.doJSON("GET", "/api/v1/community/channels", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list channels node1 failed: %d", w.Code)
	}
	w = node2.doJSON("GET", "/api/v1/community/channels", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list channels node2 failed: %d", w.Code)
	}

	// 1. Partition scenario: disconnect nodes by removing peer addresses
	_ = node1.store.DeletePeerAddress(bobMemberID)
	_ = node2.store.DeletePeerAddress(aliceMemberID)

	w = node1.doJSON("POST", "/api/v1/community/channels/chan-general/messages", map[string]string{
		"content": "Hello from Alice during partition",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("node1 post message failed: %d, %s", w.Code, w.Body.String())
	}

	// Node 2 posts a message while partitioned
	w = node2.doJSON("POST", "/api/v1/community/channels/chan-general/messages", map[string]string{
		"content": "Hello from Bob during partition",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("node2 post message failed: %d, %s", w.Code, w.Body.String())
	}

	// Before sync, each has 1 message
	w = node1.doJSON("GET", "/api/v1/community/channels/chan-general/messages", nil)
	var n1MsgsBefore []EnrichedMaterializedMessage
	_ = json.NewDecoder(w.Body).Decode(&n1MsgsBefore)
	if len(n1MsgsBefore) != 1 {
		t.Fatalf("expected 1 message on node1 before sync, got %d", len(n1MsgsBefore))
	}

	w = node2.doJSON("GET", "/api/v1/community/channels/chan-general/messages", nil)
	var n2MsgsBefore []EnrichedMaterializedMessage
	_ = json.NewDecoder(w.Body).Decode(&n2MsgsBefore)
	if len(n2MsgsBefore) != 1 {
		t.Fatalf("expected 1 message on node2 before sync, got %d", len(n2MsgsBefore))
	}

	// 2. Rejoin: Exchange peer addresses and sync both ways
	_ = node1.store.SavePeerAddress(bobMemberID, "m4-node-2")
	_ = node2.store.SavePeerAddress(aliceMemberID, "m4-node-1")

	node1.localSrv.SyncCommunityEvents(context.Background())
	node2.localSrv.SyncCommunityEvents(context.Background())

	// Both should now have exactly 2 messages in identical order
	w = node1.doJSON("GET", "/api/v1/community/channels/chan-general/messages", nil)
	var n1MsgsAfter []EnrichedMaterializedMessage
	_ = json.NewDecoder(w.Body).Decode(&n1MsgsAfter)
	if len(n1MsgsAfter) != 2 {
		t.Fatalf("expected 2 messages on node1 after sync, got %d", len(n1MsgsAfter))
	}

	w = node2.doJSON("GET", "/api/v1/community/channels/chan-general/messages", nil)
	var n2MsgsAfter []EnrichedMaterializedMessage
	_ = json.NewDecoder(w.Body).Decode(&n2MsgsAfter)
	if len(n2MsgsAfter) != 2 {
		t.Fatalf("expected 2 messages on node2 after sync, got %d", len(n2MsgsAfter))
	}

	// Verify exact ID and content matching between peers
	for i := 0; i < 2; i++ {
		if n1MsgsAfter[i].ID != n2MsgsAfter[i].ID {
			t.Fatalf("message order mismatch at index %d: node1=%s, node2=%s", i, n1MsgsAfter[i].ID, n2MsgsAfter[i].ID)
		}
		if n1MsgsAfter[i].Content != n2MsgsAfter[i].Content {
			t.Fatalf("content mismatch at index %d", i)
		}
		if n1MsgsAfter[i].ReplicatedStatus != "replicated" {
			t.Fatalf("expected replicated status on node1 message %d, got %s", i, n1MsgsAfter[i].ReplicatedStatus)
		}
	}

	// 3. Repeat sync to test deduplication
	node1.localSrv.SyncCommunityEvents(context.Background())
	node2.localSrv.SyncCommunityEvents(context.Background())

	w = node1.doJSON("GET", "/api/v1/community/channels/chan-general/messages", nil)
	var n1MsgsDedupe []EnrichedMaterializedMessage
	_ = json.NewDecoder(w.Body).Decode(&n1MsgsDedupe)
	if len(n1MsgsDedupe) != 2 {
		t.Fatalf("deduplication failed, expected 2 messages, got %d", len(n1MsgsDedupe))
	}
}

func TestM4AuthorEditsDeletionsAndTombstones(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4244

	node1 := setupTestNode(t, vNet, "m4-edit-1", peerPort)
	defer node1.trans.Close()
	defer node1.store.Close()
	node1.login(t)

	node2 := setupTestNode(t, vNet, "m4-edit-2", peerPort)
	defer node2.trans.Close()
	defer node2.store.Close()
	node2.login(t)

	_ = node1.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Alice"})
	_ = node2.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Bob"})

	// Alice creates room
	w := node1.doJSON("POST", "/api/v1/room/host", map[string]string{"room_name": "M4 Moderation Room"})
	var hostResp struct {
		RoomID         string `json:"room_id"`
		InvitationCode string `json:"invitation_code"`
	}
	_ = json.NewDecoder(w.Body).Decode(&hostResp)

	// Bob joins room
	_ = node2.doJSON("POST", "/api/v1/room/join", map[string]string{"invitation_code": hostResp.InvitationCode})

	aliceDev, _ := node1.store.GetDeviceIdentity()
	aliceMemberID := "m-" + aliceDev.DevicePublic[:16]
	bobDev, _ := node2.store.GetDeviceIdentity()
	bobMemberID := "m-" + bobDev.DevicePublic[:16]

	_ = node1.store.SavePeerAddress(bobMemberID, "m4-edit-2")
	_ = node2.store.SavePeerAddress(aliceMemberID, "m4-edit-1")

	_ = node1.doJSON("GET", "/api/v1/community/channels", nil)
	_ = node2.doJSON("GET", "/api/v1/community/channels", nil)

	// Alice posts a message
	w = node1.doJSON("POST", "/api/v1/community/channels/chan-general/messages", map[string]string{
		"content": "Original message by Alice",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("post message failed: %d", w.Code)
	}
	var postResp struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(w.Body).Decode(&postResp)
	msgID := postResp.ID

	// Sync Alice's message to Bob
	node1.localSrv.SyncCommunityEvents(context.Background())
	node2.localSrv.SyncCommunityEvents(context.Background())

	// 1. Bob attempts unauthorized edit on Alice's message -> 403 Forbidden
	w = node2.doJSON("PUT", "/api/v1/community/messages/"+msgID, map[string]string{
		"content": "Tampered by Bob",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unauthorized edit, got %d: %s", w.Code, w.Body.String())
	}

	// 2. Alice edits her message
	w = node1.doJSON("PUT", "/api/v1/community/messages/"+msgID, map[string]string{
		"content": "Updated message by Alice",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("authorized edit failed: %d, %s", w.Code, w.Body.String())
	}

	// Sync to Bob
	node1.localSrv.SyncCommunityEvents(context.Background())
	node2.localSrv.SyncCommunityEvents(context.Background())

	w = node2.doJSON("GET", "/api/v1/community/channels/chan-general/messages", nil)
	var msgs []EnrichedMaterializedMessage
	_ = json.NewDecoder(w.Body).Decode(&msgs)
	if len(msgs) != 1 || msgs[0].Content != "Updated message by Alice" || !msgs[0].Edited {
		t.Fatalf("expected edited message on node2, got: %+v", msgs)
	}

	// 3. Bob attempts unauthorized deletion on Alice's message -> 403 Forbidden
	w = node2.doJSON("DELETE", "/api/v1/community/messages/"+msgID, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for unauthorized delete, got %d", w.Code)
	}

	// 4. Bob posts his own message
	w = node2.doJSON("POST", "/api/v1/community/channels/chan-general/messages", map[string]string{
		"content": "Inappropriate content by Bob",
	})
	var bobPostResp struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(w.Body).Decode(&bobPostResp)
	bobMsgID := bobPostResp.ID

	// Sync to Alice
	node2.localSrv.SyncCommunityEvents(context.Background())
	node1.localSrv.SyncCommunityEvents(context.Background())

	// 5. Non-creator Bob attempts to tombstone -> 403 Forbidden
	w = node2.doJSON("POST", "/api/v1/community/messages/"+bobMsgID+"/moderate", map[string]string{
		"reason": "self moderation",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when non-creator moderates, got %d", w.Code)
	}

	// 6. Creator Alice moderates Bob's message with a tombstone
	w = node1.doJSON("POST", "/api/v1/community/messages/"+bobMsgID+"/moderate", map[string]string{
		"reason": "violates community code",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("creator moderation failed: %d, %s", w.Code, w.Body.String())
	}

	// Sync tombstone to Bob
	node1.localSrv.SyncCommunityEvents(context.Background())
	node2.localSrv.SyncCommunityEvents(context.Background())

	w = node2.doJSON("GET", "/api/v1/community/channels/chan-general/messages", nil)
	_ = json.NewDecoder(w.Body).Decode(&msgs)
	var bobMsgFound *EnrichedMaterializedMessage
	for i := range msgs {
		if msgs[i].ID == bobMsgID {
			bobMsgFound = &msgs[i]
			break
		}
	}
	if bobMsgFound == nil {
		t.Fatalf("bob message not found")
	}
	if !bobMsgFound.Tombstoned || bobMsgFound.TombstoneReason != "violates community code" {
		t.Fatalf("expected tombstone on bob's message, got: %+v", bobMsgFound)
	}

	// 7. Alice deletes her own message -> verify deletion persistence across sync
	w = node1.doJSON("DELETE", "/api/v1/community/messages/"+msgID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete own message failed: %d", w.Code)
	}

	node1.localSrv.SyncCommunityEvents(context.Background())
	node2.localSrv.SyncCommunityEvents(context.Background())

	w = node2.doJSON("GET", "/api/v1/community/channels/chan-general/messages", nil)
	_ = json.NewDecoder(w.Body).Decode(&msgs)
	var aliceMsgFound *EnrichedMaterializedMessage
	for i := range msgs {
		if msgs[i].ID == msgID {
			aliceMsgFound = &msgs[i]
			break
		}
	}
	if aliceMsgFound == nil || !aliceMsgFound.Deleted || aliceMsgFound.Content != "[Message deleted by author]" {
		t.Fatalf("expected deleted message state, got: %+v", aliceMsgFound)
	}
}

func TestM4NewMemberRetainsHistory(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4245

	node1 := setupTestNode(t, vNet, "m4-hist-1", peerPort)
	defer node1.trans.Close()
	defer node1.store.Close()
	node1.login(t)

	_ = node1.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Alice"})
	w := node1.doJSON("POST", "/api/v1/room/host", map[string]string{"room_name": "History Room"})
	var hostResp struct {
		RoomID         string `json:"room_id"`
		InvitationCode string `json:"invitation_code"`
	}
	_ = json.NewDecoder(w.Body).Decode(&hostResp)

	_ = node1.doJSON("GET", "/api/v1/community/channels", nil)

	// Post 3 messages before Node 2 joins
	for i := 1; i <= 3; i++ {
		_ = node1.doJSON("POST", "/api/v1/community/channels/chan-general/messages", map[string]string{
			"content": "Early message " + string(rune('0'+i)),
		})
	}

	// Now Node 2 joins freshly
	node2 := setupTestNode(t, vNet, "m4-hist-2", peerPort)
	defer node2.trans.Close()
	defer node2.store.Close()
	node2.login(t)
	_ = node2.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Charlie"})

	w = node2.doJSON("POST", "/api/v1/room/join", map[string]string{"invitation_code": hostResp.InvitationCode})
	if w.Code != http.StatusOK {
		t.Fatalf("node2 join failed: %d", w.Code)
	}

	aliceDev, _ := node1.store.GetDeviceIdentity()
	aliceMemberID := "m-" + aliceDev.DevicePublic[:16]
	charlieDev, _ := node2.store.GetDeviceIdentity()
	charlieMemberID := "m-" + charlieDev.DevicePublic[:16]

	_ = node2.store.SavePeerAddress(aliceMemberID, "m4-hist-1")
	_ = node1.store.SavePeerAddress(charlieMemberID, "m4-hist-2")

	_ = node2.doJSON("GET", "/api/v1/community/channels", nil)

	// Node 2 syncs with Node 1
	node2.localSrv.SyncCommunityEvents(context.Background())

	w = node2.doJSON("GET", "/api/v1/community/channels/chan-general/messages", nil)
	var charlieMsgs []EnrichedMaterializedMessage
	_ = json.NewDecoder(w.Body).Decode(&charlieMsgs)

	if len(charlieMsgs) != 3 {
		t.Fatalf("expected new member to receive all 3 retained messages, got %d", len(charlieMsgs))
	}
}

func TestM4PrivacyBoundariesAndReadStateIsolation(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4246

	node1 := setupTestNode(t, vNet, "m4-priv-1", peerPort)
	defer node1.trans.Close()
	defer node1.store.Close()
	node1.login(t)

	node2 := setupTestNode(t, vNet, "m4-priv-2", peerPort)
	defer node2.trans.Close()
	defer node2.store.Close()
	node2.login(t)

	_ = node1.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Alice"})
	_ = node2.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Bob"})

	w := node1.doJSON("POST", "/api/v1/room/host", map[string]string{"room_name": "Privacy Room"})
	var hostResp struct {
		RoomID         string `json:"room_id"`
		InvitationCode string `json:"invitation_code"`
	}
	_ = json.NewDecoder(w.Body).Decode(&hostResp)

	_ = node2.doJSON("POST", "/api/v1/room/join", map[string]string{"invitation_code": hostResp.InvitationCode})

	aliceDev, _ := node1.store.GetDeviceIdentity()
	aliceMemberID := "m-" + aliceDev.DevicePublic[:16]
	bobDev, _ := node2.store.GetDeviceIdentity()
	bobMemberID := "m-" + bobDev.DevicePublic[:16]

	_ = node1.store.SavePeerAddress(bobMemberID, "m4-priv-2")
	_ = node2.store.SavePeerAddress(aliceMemberID, "m4-priv-1")

	_ = node1.doJSON("GET", "/api/v1/community/channels", nil)
	_ = node2.doJSON("GET", "/api/v1/community/channels", nil)

	// 1. Bob creates a local private chat conversation and message
	conv := store.ConversationRecord{
		ID:        "conv-bob-private-1",
		Title:     "Bob's confidential query",
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
	}
	if err := node2.store.SaveConversation(conv); err != nil {
		t.Fatalf("save conversation on node2 failed: %v", err)
	}
	if err := node2.store.SaveMessage(store.MessageRecord{
		ID:             "msg-bob-private-1",
		ConversationID: conv.ID,
		Role:           "user",
		Content:        "Confidential financial inquiry",
		CreatedAt:      time.Now().Unix(),
	}); err != nil {
		t.Fatalf("save chat message on node2 failed: %v", err)
	}

	// 2. Bob updates read and mute state on chan-general
	w = node2.doJSON("POST", "/api/v1/community/channels/chan-general/read", map[string]any{
		"muted": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("update read state failed: %d", w.Code)
	}

	// 3. Perform community sync between Node 1 and Node 2
	node2.localSrv.SyncCommunityEvents(context.Background())
	node1.localSrv.SyncCommunityEvents(context.Background())

	// 4. Verify Node 1's store contains ZERO private chats from Node 2
	aliceConvs, err := node1.store.ListConversations()
	if err != nil {
		t.Fatalf("list convs failed: %v", err)
	}
	if len(aliceConvs) != 0 {
		t.Fatalf("privacy violation: private conversations replicated to node1! found %d", len(aliceConvs))
	}

	// Verify Node 1's store has NO read state from Bob
	_, aliceMuted, err := node1.store.GetReadState("chan-general")
	if err != nil {
		t.Fatalf("get read state on node1 failed: %v", err)
	}
	if aliceMuted {
		t.Fatalf("privacy violation: local read/mute state replicated to node1!")
	}
}

// TestM4SanitizationLeavesTextIntact covers task 26. Sanitization is for
// structure only: remote tracking images are neutralized, and everything else
// reaches the client verbatim so React escapes it exactly once on render.
// Escaping here as well turned "&" into "&amp;" in stored text, and the
// tag-stripping regex deleted ordinary prose like "<3" and "a < b > c".
func TestM4SanitizationLeavesTextIntact(t *testing.T) {
	t.Run("remote images are neutralized", func(t *testing.T) {
		got := community.SanitizeContent(`![tracker](https://evil.com/logger.png)`)
		if stringContains(got, "https://evil.com/logger.png") {
			t.Fatalf("tracking image URL survived: %s", got)
		}
		if got != "[Image: tracker]" {
			t.Fatalf("expected the placeholder, got: %s", got)
		}
	})

	t.Run("ordinary text passes through unchanged", func(t *testing.T) {
		for _, input := range []string{
			"a < b & c",
			"I <3 this",
			"a < b > c",
			"x && y || z",
			`he said "hi" & left`,
			"5 > 3 && 2 < 4",
		} {
			if got := community.SanitizeContent(input); got != input {
				t.Errorf("SanitizeContent(%q) = %q, want it unchanged", input, got)
			}
		}
	})

	t.Run("markup is left for the renderer to escape", func(t *testing.T) {
		// The stored form keeps the author's literal text. React escapes it on
		// output, so the tags are displayed rather than executed; double
		// escaping here would corrupt the stored message permanently.
		input := `<script>alert('xss')</script><b>Safe text</b>`
		if got := community.SanitizeContent(input); got != input {
			t.Fatalf("SanitizeContent altered stored text: %q", got)
		}
	})
}

func stringContains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || (len(s) > 0 && len(sub) > 0 && containsSub(s, sub)))
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
