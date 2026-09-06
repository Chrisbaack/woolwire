package localapi

import (
	"bufio"
	"bytes"
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
		receipts = append(receipts, contributions.Receipt{
			RequestID:          rec.RequestID,
			RoomID:             rec.RoomID,
			HostMemberID:       rec.HostMemberID,
			RequesterMemberID:  rec.RequesterMemberID,
			Timestamp:          rec.Timestamp,
			Completed:          rec.Completed,
			HostSignature:      rec.HostSignature,
			RequesterSignature: rec.RequesterSignature,
			ReplicatedStatus:   rec.ReplicatedStatus,
		})
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
		if device != nil && len(device.DevicePublic) >= 16 {
			myMemberID := "m-" + device.DevicePublic[:16]
			optOutMap[myMemberID] = true
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
	_ = s.store.SaveReceipt(store.ContributionReceiptRecord{
		RequestID:          rec.RequestID,
		RoomID:             rec.RoomID,
		HostMemberID:       rec.HostMemberID,
		RequesterMemberID:  rec.RequesterMemberID,
		Timestamp:          rec.Timestamp,
		Completed:          rec.Completed,
		HostSignature:      rec.HostSignature,
		RequesterSignature: rec.RequesterSignature,
		ReplicatedStatus:   "local",
	})

	// Acknowledge back to host asynchronously
	go func() {
		b, _ := json.Marshal(rec)
		dialCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, dialErr := s.trans.Dial(dialCtx, hostTailcatAddr, s.peerPort)
		if dialErr != nil {
			return
		}
		defer conn.Close()
		req, _ := http.NewRequestWithContext(dialCtx, "POST", "http://woolwire-peer/peer/v1/contributions/ack", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		_ = req.Write(conn)
	}()
}

func (s *Server) SyncContributions(ctx context.Context) {
	roomRec, err := s.store.GetRoomState()
	if err != nil || roomRec == nil {
		return
	}
	device, err := s.store.GetDeviceIdentity()
	if err != nil || device == nil {
		return
	}
	myMemberID := "m-" + device.DevicePublic[:16]

	allReceipts, _ := s.store.ListReceipts(roomRec.RoomID)
	knownIDs := make([]string, 0, len(allReceipts))
	var pushReceipts []contributions.Receipt
	for _, rec := range allReceipts {
		knownIDs = append(knownIDs, rec.RequestID)
		if rec.ReplicatedStatus == "local" {
			pushReceipts = append(pushReceipts, contributions.Receipt{
				RequestID:          rec.RequestID,
				RoomID:             rec.RoomID,
				HostMemberID:       rec.HostMemberID,
				RequesterMemberID:  rec.RequesterMemberID,
				Timestamp:          rec.Timestamp,
				Completed:          rec.Completed,
				HostSignature:      rec.HostSignature,
				RequesterSignature: rec.RequesterSignature,
			})
		}
	}

	peerAddrs, _ := s.store.ListPeerAddresses()
	for _, pa := range peerAddrs {
		if pa.MemberID == myMemberID {
			continue
		}
		dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		conn, dialErr := s.trans.Dial(dialCtx, pa.TailcatAddr, s.peerPort)
		if dialErr != nil {
			cancel()
			continue
		}
		reqPayload := peerapi.ContributionsSyncRequest{
			RoomID:          roomRec.RoomID,
			MemberID:        myMemberID,
			KnownRequestIDs: knownIDs,
			PushReceipts:    pushReceipts,
		}
		b, _ := json.Marshal(reqPayload)
		httpReq, _ := http.NewRequestWithContext(dialCtx, "POST", "http://woolwire-peer/peer/v1/contributions/sync", bytes.NewReader(b))
		httpReq.Header.Set("Content-Type", "application/json")
		_ = httpReq.Write(conn)

		resp, readErr := http.ReadResponse(bufio.NewReader(conn), httpReq)
		if readErr == nil {
			if resp.StatusCode == http.StatusOK {
				var syncResp peerapi.ContributionsSyncResponse
				if json.NewDecoder(resp.Body).Decode(&syncResp) == nil {
					for _, pr := range syncResp.PullReceipts {
						if pr.HostMemberID == pr.RequesterMemberID || !pr.Completed {
							continue
						}
						hostMember, _ := s.store.GetMember(pr.HostMemberID)
						reqMember, _ := s.store.GetMember(pr.RequesterMemberID)
						if hostMember == nil || reqMember == nil {
							continue
						}
						hostPubBytes, _ := identity.DecodeToken(hostMember.DevicePublic, ed25519.PublicKeySize)
						reqPubBytes, _ := identity.DecodeToken(reqMember.DevicePublic, ed25519.PublicKeySize)
						if pr.VerifyBoth(ed25519.PublicKey(hostPubBytes), ed25519.PublicKey(reqPubBytes)) != nil {
							continue
						}
						_ = s.store.SaveReceipt(store.ContributionReceiptRecord{
							RequestID:          pr.RequestID,
							RoomID:             pr.RoomID,
							HostMemberID:       pr.HostMemberID,
							RequesterMemberID:  pr.RequesterMemberID,
							Timestamp:          pr.Timestamp,
							Completed:          pr.Completed,
							HostSignature:      pr.HostSignature,
							RequesterSignature: pr.RequesterSignature,
							ReplicatedStatus:   "replicated",
						})
					}
				}
			}
			resp.Body.Close()
		}
		conn.Close()
		cancel()
	}

	for _, pr := range pushReceipts {
		rec, _ := s.store.GetReceipt(pr.RequestID)
		if rec != nil {
			rec.ReplicatedStatus = "replicated"
			_ = s.store.SaveReceipt(*rec)
		}
	}
}
