package localapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cbaack/woolwire/internal/contributions"
	"github.com/cbaack/woolwire/internal/peerapi"
	"github.com/cbaack/woolwire/internal/store"
	"github.com/cbaack/woolwire/internal/transport"
)

func TestM5ContributionsReceiptFlowAndConvergence(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4250

	// 3 Nodes: Alice (Creator), Bob (Host), Charlie (Requester)
	node1 := setupTestNode(t, vNet, "m5-node-1", peerPort)
	defer node1.trans.Close()
	defer node1.store.Close()
	node1.login(t)

	node2 := setupTestNode(t, vNet, "m5-node-2", peerPort)
	defer node2.trans.Close()
	defer node2.store.Close()
	node2.login(t)

	node3 := setupTestNode(t, vNet, "m5-node-3", peerPort)
	defer node3.trans.Close()
	defer node3.store.Close()
	node3.login(t)

	_ = node1.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Alice"})
	_ = node2.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Bob"})
	_ = node3.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Charlie"})

	// Alice creates room
	w := node1.doJSON("POST", "/api/v1/room/host", map[string]string{"room_name": "Recognition Room"})
	var hostResp struct {
		RoomID         string `json:"room_id"`
		InvitationCode string `json:"invitation_code"`
	}
	_ = json.NewDecoder(w.Body).Decode(&hostResp)

	// Bob and Charlie join
	_ = node2.doJSON("POST", "/api/v1/room/join", map[string]string{"invitation_code": hostResp.InvitationCode})
	_ = node3.doJSON("POST", "/api/v1/room/join", map[string]string{"invitation_code": hostResp.InvitationCode})

	aliceDev, _ := node1.store.GetDeviceIdentity()
	aliceMemberID := "m-" + aliceDev.DevicePublic[:16]
	bobDev, _ := node2.store.GetDeviceIdentity()
	bobMemberID := "m-" + bobDev.DevicePublic[:16]
	charlieDev, _ := node3.store.GetDeviceIdentity()
	charlieMemberID := "m-" + charlieDev.DevicePublic[:16]

	// Save peer addresses for all nodes
	_ = node1.store.SavePeerAddress(bobMemberID, "m5-node-2")
	_ = node1.store.SavePeerAddress(charlieMemberID, "m5-node-3")
	_ = node2.store.SavePeerAddress(aliceMemberID, "m5-node-1")
	_ = node2.store.SavePeerAddress(charlieMemberID, "m5-node-3")
	_ = node3.store.SavePeerAddress(aliceMemberID, "m5-node-1")
	_ = node3.store.SavePeerAddress(bobMemberID, "m5-node-2")

	// Sync roster across nodes from creator
	charlieMem, _ := node1.store.GetMember(charlieMemberID)
	if charlieMem != nil {
		_ = node2.store.SaveMember(*charlieMem)
	}
	bobMem, _ := node1.store.GetMember(bobMemberID)
	if bobMem != nil {
		_ = node3.store.SaveMember(*bobMem)
	}

	// 1. Generate a valid jointly-signed receipt between Bob (host) and Charlie (requester)
	bobPriv := ed25519.PrivateKey(bobDev.DevicePrivate)
	charliePriv := ed25519.PrivateKey(charlieDev.DevicePrivate)

	receipt := contributions.Receipt{
		RequestID:         "req-joint-001",
		RoomID:            hostResp.RoomID,
		HostMemberID:      bobMemberID,
		RequesterMemberID: charlieMemberID,
		Timestamp:         time.Now().Unix(),
		Completed:         true,
	}

	if err := receipt.SignHost(bobPriv); err != nil {
		t.Fatalf("sign host failed: %v", err)
	}
	if err := receipt.SignRequester(charliePriv); err != nil {
		t.Fatalf("sign requester failed: %v", err)
	}

	// Verify privacy: verify receipt has NO prompt text, output, or content hashes
	receiptJSON, _ := json.Marshal(receipt)
	var rawFields map[string]any
	_ = json.Unmarshal(receiptJSON, &rawFields)
	for _, forbidden := range []string{"prompt", "output", "content", "hash", "messages"} {
		if _, exists := rawFields[forbidden]; exists {
			t.Fatalf("privacy violation: receipt contains forbidden field %q", forbidden)
		}
	}

	// Save receipt on Charlie's node (local)
	_ = node3.store.SaveReceipt(peerapi.ReceiptRecord(receipt, "local"))

	// 2. Charlie syncs contributions with Bob and Alice
	node3.localSrv.SyncContributions(context.Background())
	node2.localSrv.SyncContributions(context.Background())
	node1.localSrv.SyncContributions(context.Background())

	// 3. Verify convergence across all 3 independent peers
	for idx, n := range []*testNode{node1, node2, node3} {
		w := n.doJSON("GET", "/api/v1/contributions/leaderboard", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("node %d leaderboard failed: %d", idx+1, w.Code)
		}
		var lbResp struct {
			Leaderboard []contributions.LeaderboardEntry `json:"leaderboard"`
			Disclaimer  string                           `json:"disclaimer"`
		}
		_ = json.NewDecoder(w.Body).Decode(&lbResp)
		if len(lbResp.Leaderboard) != 1 {
			t.Fatalf("node %d expected 1 entry, got %d", idx+1, len(lbResp.Leaderboard))
		}
		entry := lbResp.Leaderboard[0]
		if entry.HostMemberID != bobMemberID || entry.Score != 1 || entry.DistinctMembersHelped != 1 {
			t.Fatalf("node %d unexpected entry: %+v", idx+1, entry)
		}
		if lbResp.Disclaimer == "" {
			t.Fatalf("node %d missing social recognition disclaimer", idx+1)
		}
	}

	// 4. Replay test: ingesting same receipt again does not increase score
	_ = node3.store.SaveReceipt(peerapi.ReceiptRecord(receipt, "local"))

	w = node3.doJSON("GET", "/api/v1/contributions/leaderboard", nil)
	var lbDedupe struct {
		Leaderboard []contributions.LeaderboardEntry `json:"leaderboard"`
	}
	_ = json.NewDecoder(w.Body).Decode(&lbDedupe)
	if len(lbDedupe.Leaderboard) != 1 || lbDedupe.Leaderboard[0].Score != 1 {
		t.Fatalf("replayed receipt increased score! got: %+v", lbDedupe.Leaderboard)
	}
}

func TestM5UnilateralAndSelfServiceExclusion(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4251

	node := setupTestNode(t, vNet, "m5-excl-1", peerPort)
	defer node.trans.Close()
	defer node.store.Close()
	node.login(t)

	dev, _ := node.store.GetDeviceIdentity()
	myMemberID := "m-" + dev.DevicePublic[:16]
	myPriv := ed25519.PrivateKey(dev.DevicePrivate)

	w := node.doJSON("POST", "/api/v1/room/host", map[string]string{"room_name": "Exclusion Room"})
	var hostResp struct {
		RoomID string `json:"room_id"`
	}
	_ = json.NewDecoder(w.Body).Decode(&hostResp)

	// 1. Self-service receipt (Host == Requester)
	selfReceipt := contributions.Receipt{
		RequestID:         "req-self-1",
		RoomID:            hostResp.RoomID,
		HostMemberID:      myMemberID,
		RequesterMemberID: myMemberID,
		Timestamp:         time.Now().Unix(),
		Completed:         true,
	}
	_ = selfReceipt.SignHost(myPriv)
	_ = selfReceipt.SignRequester(myPriv)

	_ = node.store.SaveReceipt(store.ContributionReceiptRecord{
		RequestID:          selfReceipt.RequestID,
		RoomID:             selfReceipt.RoomID,
		HostMemberID:       selfReceipt.HostMemberID,
		RequesterMemberID:  selfReceipt.RequesterMemberID,
		Timestamp:          selfReceipt.Timestamp,
		Completed:          selfReceipt.Completed,
		HostSignature:      selfReceipt.HostSignature,
		RequesterSignature: selfReceipt.RequesterSignature,
	})

	// 2. Incomplete receipt (Completed == false)
	incompReceipt := contributions.Receipt{
		RequestID:         "req-incomp-1",
		RoomID:            hostResp.RoomID,
		HostMemberID:      myMemberID,
		RequesterMemberID: "m-charlie",
		Timestamp:         time.Now().Unix(),
		Completed:         false,
	}
	_ = incompReceipt.SignHost(myPriv)

	_ = node.store.SaveReceipt(store.ContributionReceiptRecord{
		RequestID:          incompReceipt.RequestID,
		RoomID:             incompReceipt.RoomID,
		HostMemberID:       incompReceipt.HostMemberID,
		RequesterMemberID:  incompReceipt.RequesterMemberID,
		Timestamp:          incompReceipt.Timestamp,
		Completed:          false,
		HostSignature:      incompReceipt.HostSignature,
		RequesterSignature: incompReceipt.HostSignature,
	})

	// Check leaderboard: both self-service and incomplete are excluded!
	w = node.doJSON("GET", "/api/v1/contributions/leaderboard", nil)
	var lbResp struct {
		Leaderboard []contributions.LeaderboardEntry `json:"leaderboard"`
	}
	_ = json.NewDecoder(w.Body).Decode(&lbResp)

	if len(lbResp.Leaderboard) != 0 {
		t.Fatalf("expected empty leaderboard for self-service/incomplete receipts, got: %+v", lbResp.Leaderboard)
	}
}

func TestM5DailyCeilingCapAndOptOut(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4252

	node1 := setupTestNode(t, vNet, "m5-cap-1", peerPort)
	defer node1.trans.Close()
	defer node1.store.Close()
	node1.login(t)

	node2 := setupTestNode(t, vNet, "m5-cap-2", peerPort)
	defer node2.trans.Close()
	defer node2.store.Close()
	node2.login(t)

	_ = node1.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Alice"})
	_ = node2.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Bob"})

	w := node1.doJSON("POST", "/api/v1/room/host", map[string]string{"room_name": "Cap Room"})
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

	alicePriv := ed25519.PrivateKey(aliceDev.DevicePrivate)
	bobPriv := ed25519.PrivateKey(bobDev.DevicePrivate)

	now := time.Now().Unix()

	// 1. Host Bob serves Alice 25 times today
	for i := 1; i <= 25; i++ {
		r := contributions.Receipt{
			RequestID:         fmt.Sprintf("req-cap-%d", i),
			RoomID:            hostResp.RoomID,
			HostMemberID:      bobMemberID,
			RequesterMemberID: aliceMemberID,
			Timestamp:         now,
			Completed:         true,
		}
		_ = r.SignHost(bobPriv)
		_ = r.SignRequester(alicePriv)

		_ = node1.store.SaveReceipt(store.ContributionReceiptRecord{
			RequestID:          r.RequestID,
			RoomID:             r.RoomID,
			HostMemberID:       r.HostMemberID,
			RequesterMemberID:  r.RequesterMemberID,
			Timestamp:          r.Timestamp,
			Completed:          r.Completed,
			HostSignature:      r.HostSignature,
			RequesterSignature: r.RequesterSignature,
		})
	}

	// Check that score is capped at 20 (not 25)
	w = node1.doJSON("GET", "/api/v1/contributions/leaderboard", nil)
	var lbCap struct {
		Leaderboard []contributions.LeaderboardEntry `json:"leaderboard"`
	}
	_ = json.NewDecoder(w.Body).Decode(&lbCap)

	if len(lbCap.Leaderboard) != 1 || lbCap.Leaderboard[0].Score != 20 {
		t.Fatalf("expected score capped at 20, got: %+v", lbCap.Leaderboard)
	}

	// 2. Test Opt-Out Setting on Node 1
	w = node1.doJSON("GET", "/api/v1/contributions/settings", nil)
	var settResp struct {
		OptOut bool `json:"opt_out"`
	}
	_ = json.NewDecoder(w.Body).Decode(&settResp)
	if settResp.OptOut {
		t.Fatalf("default opt_out should be false")
	}

	// Save opt-out = true
	w = node1.doJSON("POST", "/api/v1/contributions/settings", map[string]bool{"opt_out": true})
	if w.Code != http.StatusOK {
		t.Fatalf("save contributions settings failed: %d", w.Code)
	}

	w = node1.doJSON("GET", "/api/v1/contributions/settings", nil)
	_ = json.NewDecoder(w.Body).Decode(&settResp)
	if !settResp.OptOut {
		t.Fatalf("expected opt_out to be true after save")
	}
}
