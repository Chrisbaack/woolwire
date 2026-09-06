package contributions

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"github.com/cbaack/woolwire/internal/identity"
)

func TestReceiptVerificationAndTampering(t *testing.T) {
	hostPub, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	reqPub, reqPriv, _ := ed25519.GenerateKey(rand.Reader)

	hostMemberID := "m-" + identity.EncodeToken(hostPub)[:16]
	reqMemberID := "m-" + identity.EncodeToken(reqPub)[:16]

	r := Receipt{
		RequestID:         "req-abc-123",
		RoomID:            "room-test",
		HostMemberID:      hostMemberID,
		RequesterMemberID: reqMemberID,
		Timestamp:         time.Now().Unix(),
		Completed:         true,
	}

	if err := r.SignHost(hostPriv); err != nil {
		t.Fatalf("sign host failed: %v", err)
	}
	if err := r.SignRequester(reqPriv); err != nil {
		t.Fatalf("sign requester failed: %v", err)
	}

	if err := r.VerifyBoth(hostPub, reqPub); err != nil {
		t.Fatalf("verify both failed: %v", err)
	}

	// Tampered request id fails
	tampered := r
	tampered.RequestID = "req-tampered"
	if err := tampered.VerifyBoth(hostPub, reqPub); err == nil {
		t.Fatal("expected failure on tampered request id")
	}

	// Tampered timestamp fails
	tamperedTime := r
	tamperedTime.Timestamp = r.Timestamp + 10
	if err := tamperedTime.VerifyBoth(hostPub, reqPub); err == nil {
		t.Fatal("expected failure on tampered timestamp")
	}

	// Missing requester signature fails
	unilateral := r
	unilateral.RequesterSignature = ""
	if err := unilateral.VerifyBoth(hostPub, reqPub); err == nil {
		t.Fatal("expected failure on missing requester signature")
	}
}

func TestLeaderboardRules(t *testing.T) {
	now := time.Now().Unix()
	nowUTC := time.Unix(now, 0).UTC()

	names := map[string]string{
		"host-alice": "Alice Host",
		"host-bob":   "Bob Host",
	}

	var receipts []Receipt

	// 1. Host Alice serves Requester 1 25 times today -> should cap at 20 points!
	for i := 1; i <= 25; i++ {
		receipts = append(receipts, Receipt{
			RequestID:         fmt.Sprintf("req-alice-req1-%d", i),
			RoomID:            "room-1",
			HostMemberID:      "host-alice",
			RequesterMemberID: "req-1",
			Timestamp:         now,
			Completed:         true,
		})
	}

	// 2. Host Alice serves Requester 2 5 times today -> +5 points (total 25)
	for i := 1; i <= 5; i++ {
		receipts = append(receipts, Receipt{
			RequestID:         fmt.Sprintf("req-alice-req2-%d", i),
			RoomID:            "room-1",
			HostMemberID:      "host-alice",
			RequesterMemberID: "req-2",
			Timestamp:         now,
			Completed:         true,
		})
	}

	// 3. Replayed receipt for Alice -> should not increase score
	receipts = append(receipts, Receipt{
		RequestID:         "req-alice-req2-1", // duplicate of earlier
		RoomID:            "room-1",
		HostMemberID:      "host-alice",
		RequesterMemberID: "req-2",
		Timestamp:         now,
		Completed:         true,
	})

	// 4. Self-service request for Alice -> should be excluded (0 points)
	receipts = append(receipts, Receipt{
		RequestID:         "req-alice-self",
		RoomID:            "room-1",
		HostMemberID:      "host-alice",
		RequesterMemberID: "host-alice",
		Timestamp:         now,
		Completed:         true,
	})

	// 5. Incomplete request for Alice -> excluded (0 points)
	receipts = append(receipts, Receipt{
		RequestID:         "req-alice-incomplete",
		RoomID:            "room-1",
		HostMemberID:      "host-alice",
		RequesterMemberID: "req-3",
		Timestamp:         now,
		Completed:         false,
	})

	// 6. Old request for Alice (> 30 days) -> excluded
	oldTimestamp := nowUTC.AddDate(0, 0, -35).Unix()
	receipts = append(receipts, Receipt{
		RequestID:         "req-alice-old",
		RoomID:            "room-1",
		HostMemberID:      "host-alice",
		RequesterMemberID: "req-3",
		Timestamp:         oldTimestamp,
		Completed:         true,
	})

	// 7. Host Bob serves Requester 1 10 times
	for i := 1; i <= 10; i++ {
		receipts = append(receipts, Receipt{
			RequestID:         fmt.Sprintf("req-bob-req1-%d", i),
			RoomID:            "room-1",
			HostMemberID:      "host-bob",
			RequesterMemberID: "req-1",
			Timestamp:         now,
			Completed:         true,
		})
	}

	// Compute leaderboard without opt-outs
	lb := CalculateLeaderboard(receipts, names, map[string]bool{}, now)

	if len(lb) != 2 {
		t.Fatalf("expected 2 leaderboard entries, got %d", len(lb))
	}

	// Alice: 20 (from req1) + 5 (from req2) = 25 points, 2 distinct members helped
	if lb[0].HostMemberID != "host-alice" || lb[0].Score != 25 || lb[0].DistinctMembersHelped != 2 {
		t.Fatalf("unexpected Alice score: %+v", lb[0])
	}

	// Bob: 10 points, 1 distinct member helped
	if lb[1].HostMemberID != "host-bob" || lb[1].Score != 10 || lb[1].DistinctMembersHelped != 1 {
		t.Fatalf("unexpected Bob score: %+v", lb[1])
	}

	// Compute leaderboard with Alice opted out
	lbOptOut := CalculateLeaderboard(receipts, names, map[string]bool{"host-alice": true}, now)
	if len(lbOptOut) != 1 || lbOptOut[0].HostMemberID != "host-bob" {
		t.Fatalf("opt-out failed, expected only Bob, got: %+v", lbOptOut)
	}
}
