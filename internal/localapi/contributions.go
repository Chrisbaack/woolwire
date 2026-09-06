package localapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"time"

	"github.com/cbaack/woolwire/internal/contributions"
	"github.com/cbaack/woolwire/internal/identity"
	"github.com/cbaack/woolwire/internal/peerapi"
	"github.com/cbaack/woolwire/internal/store"
)

func (s *Server) handleGetLeaderboard(w http.ResponseWriter, r *http.Request) {
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"leaderboard": []contributions.LeaderboardEntry{},
			"disclaimer":  "Social recognition within your room. Points never determine access or carry financial value.",
		})
		return
	}

	records, err := s.store.ListReceipts(roomRec.RoomID)
	if err != nil {
		http.Error(w, "failed to query receipts", http.StatusInternalServerError)
		return
	}

	var receipts []contributions.Receipt
	for _, rec := range records {
		receipts = append(receipts, peerapi.ReceiptFromRecord(rec))
	}

	memberNames := make(map[string]string)
	members, _ := s.store.ListMembers(roomRec.RoomID)
	for _, m := range members {
		memberNames[m.MemberID] = m.DisplayName
	}

	optOutMap := make(map[string]bool)
	localOptOut, _ := s.store.GetSetting("contributions_opt_out")
	if localOptOut == "true" {
		device, _ := s.store.GetDeviceIdentity()
		if device != nil {
			if myMemberID, err := peerapi.MemberIDForDevicePublic(device.DevicePublic); err == nil {
				optOutMap[myMemberID] = true
			}
		}
	}

	entries := contributions.CalculateLeaderboard(receipts, memberNames, optOutMap, time.Now().Unix())

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"leaderboard": entries,
		"disclaimer":  "Social recognition within your room. Points never determine access or carry financial value.",
	})
}

func (s *Server) handleGetContributionsSettings(w http.ResponseWriter, r *http.Request) {
	optOut, _ := s.store.GetSetting("contributions_opt_out")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"opt_out": optOut == "true",
	})
}

func (s *Server) handleSaveContributionsSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OptOut bool `json:"opt_out"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	val := "false"
	if body.OptOut {
		val = "true"
	}
	if err := s.store.SetSetting("contributions_opt_out", val); err != nil {
		http.Error(w, "failed to save setting", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) handleInboundReceipt(ctx context.Context, rec contributions.Receipt, hostMemberID, hostTailcatAddr string) {
	optOut, _ := s.store.GetSetting("contributions_opt_out")
	if optOut == "true" {
		return // participant opted out
	}

	device, err := s.store.GetDeviceIdentity()
	if err != nil || device == nil {
		return
	}
	myPriv := ed25519.PrivateKey(device.DevicePrivate)

	hostMember, err := s.store.GetMember(hostMemberID)
	if err != nil || hostMember == nil {
		return
	}
	hostPubBytes, err := identity.DecodeToken(hostMember.DevicePublic, ed25519.PublicKeySize)
	if err != nil {
		return
	}
	if rec.VerifyHost(ed25519.PublicKey(hostPubBytes)) != nil {
		return
	}

	// Sign as requester
	if err := rec.SignRequester(myPriv); err != nil {
		return
	}

	// Save locally
	_ = s.store.SaveReceipt(peerapi.ReceiptRecord(rec, "local"))

	// Acknowledge back to the host asynchronously over the authenticated peer
	// connection, so the host learns the requester counter-signed.
	go func() {
		dialCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.peerJSON(dialCtx, hostMemberID, hostTailcatAddr, "POST", "/peer/v1/contributions/ack", rec, nil)
	}()
}

// SyncContributions exchanges receipts with peers using per-pair cursors, so
// the request body no longer grows with the number of receipts held.
func (s *Server) SyncContributions(ctx context.Context) {
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		return
	}
	device, err := s.store.GetDeviceIdentity()
	if err != nil || device == nil {
		return
	}
	myMemberID, err := peerapi.MemberIDForDevicePublic(device.DevicePublic)
	if err != nil {
		return
	}

	allReceipts, _ := s.store.ListReceipts(roomRec.RoomID)
	var pushReceipts []contributions.Receipt
	for _, rec := range allReceipts {
		if rec.ReplicatedStatus != "local" {
			continue
		}
		pushReceipts = append(pushReceipts, peerapi.ReceiptFromRecord(rec))
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
		s.syncReceiptsWithPeer(ctx, roomRec.RoomID, pa, pushReceipts)
	}

	for _, pr := range pushReceipts {
		if rec, err := s.store.GetReceipt(pr.RequestID); err == nil && rec != nil {
			rec.ReplicatedStatus = "replicated"
			_ = s.store.SaveReceipt(*rec)
		}
	}
}

func (s *Server) syncReceiptsWithPeer(
	ctx context.Context,
	roomID string,
	peer store.PeerAddressRecord,
	pushReceipts []contributions.Receipt,
) {
	const maxPages = 200

	for page := 0; page < maxPages; page++ {
		dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)

		cursors, _ := s.store.ReceiptCursors(roomID)
		req := peerapi.ContributionsSyncRequest{
			RoomID:  roomID,
			Cursors: cursors,
		}
		if page == 0 {
			req.PushReceipts = pushReceipts
		}

		var resp peerapi.ContributionsSyncResponse
		err := s.peerJSON(dialCtx, peer.MemberID, peer.TailcatAddr, "POST", "/peer/v1/contributions/sync", req, &resp)
		cancel()
		if err != nil {
			return
		}

		applied := 0
		for _, pr := range resp.PullReceipts {
			if !peerapi.VerifyReceipt(s.store, pr) {
				continue
			}
			_ = s.store.SaveReceipt(peerapi.ReceiptRecord(pr, "replicated"))
			applied++
		}

		if len(resp.PullReceipts) == 0 || applied == 0 {
			return
		}
	}
}
