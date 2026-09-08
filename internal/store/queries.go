package store

import (
	"database/sql"
	"errors"
)

// GetMemberByDevicePublic resolves a device key to its roster row. It backs
// the peer TLS verifier, which only ever sees the certificate key and must
// derive the member identity from local, authority-signed state.
func (s *Store) GetMemberByDevicePublic(devicePublic string) (*MemberRecord, error) {
	if devicePublic == "" {
		return nil, ErrNotFound
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var m MemberRecord
	err := s.db.QueryRow(`
		SELECT member_id, room_id, device_public, display_name, status,
		       roster_version, signature, created_at, updated_at, sig_version
		FROM members WHERE device_public = ?
	`, devicePublic).Scan(
		&m.MemberID, &m.RoomID, &m.DevicePublic, &m.DisplayName, &m.Status,
		&m.RosterVersion, &m.Signature, &m.CreatedAt, &m.UpdatedAt, &m.SigVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// CountRoomStates reports how many room rows exist. Hosting or joining a
// second room while one is active is refused rather than quietly adding a row
// that GetRoomState would then pick between.
func (s *Store) CountRoomStates() (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM room_state").Scan(&n)
	return n, err
}

// AuthorCursors returns the highest author_seq this node holds for each
// author in the room. It is the sync request payload that replaces sending
// every known event ID.
func (s *Store) AuthorCursors(roomID string) (map[string]int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT author_member_id, MAX(author_seq)
		FROM community_events WHERE room_id = ?
		GROUP BY author_member_id
	`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cursors := make(map[string]int64)
	for rows.Next() {
		var author string
		var maxSeq int64
		if err := rows.Scan(&author, &maxSeq); err != nil {
			return nil, err
		}
		cursors[author] = maxSeq
	}
	return cursors, rows.Err()
}

// AuthorGaps returns sequence ranges that are missing for each author.
// A range [start, end] means sequences start..end are missing locally.
func (s *Store) AuthorGaps(roomID string) (map[string][][2]int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT author_member_id, author_seq
		FROM community_events WHERE room_id = ?
		ORDER BY author_member_id ASC, author_seq ASC
	`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	gaps := make(map[string][][2]int64)
	var currentAuthor string
	var expectedSeq int64 = 1

	for rows.Next() {
		var author string
		var seq int64
		if err := rows.Scan(&author, &seq); err != nil {
			return nil, err
		}
		if author != currentAuthor {
			currentAuthor = author
			expectedSeq = 1
		}
		if seq > expectedSeq {
			gaps[author] = append(gaps[author], [2]int64{expectedSeq, seq - 1})
		}
		if seq >= expectedSeq {
			expectedSeq = seq + 1
		}
	}
	return gaps, rows.Err()
}

// ListEventIDs returns all community event IDs held for the room.
func (s *Store) ListEventIDs(roomID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT id FROM community_events WHERE room_id = ?`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func inGaps(seq int64, ranges [][2]int64) bool {
	for _, r := range ranges {
		if seq >= r[0] && seq <= r[1] {
			return true
		}
	}
	return false
}

// ListEventsAfterCursor returns events the requester is missing, ordered by
// (author, seq) so paging with the returned high-water marks converges.
// It includes events in missing ranges (gaps) and conflicting events at
// sequences already seen by the requester.
func (s *Store) ListEventsAfterCursor(
	roomID string,
	cursors map[string]int64,
	gaps map[string][][2]int64,
	knownIDs map[string]bool,
	limit int,
) ([]EventRecord, error) {
	if limit <= 0 {
		limit = 500
	}

	// Find sequences where the local store holds multiple events (known conflicts).
	multiEvents := make(map[string]map[int64]bool)
	if multiRows, err := s.db.Query(`
		SELECT author_member_id, author_seq
		FROM community_events
		WHERE room_id = ?
		GROUP BY author_member_id, author_seq
		HAVING COUNT(*) > 1
	`, roomID); err == nil {
		for multiRows.Next() {
			var author string
			var seq int64
			if multiRows.Scan(&author, &seq) == nil {
				if multiEvents[author] == nil {
					multiEvents[author] = make(map[int64]bool)
				}
				multiEvents[author][seq] = true
			}
		}
		multiRows.Close()
	}

	rows, err := s.db.Query(`
		SELECT id, room_id, channel_id, author_member_id, author_seq,
		       event_type, COALESCE(target_event_id, ''), content, timestamp, signature,
		       replicated_status, created_at, sig_version
		FROM community_events
		WHERE room_id = ?
		ORDER BY author_member_id ASC, author_seq ASC, id ASC
	`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]EventRecord, 0, limit)
	for rows.Next() {
		var e EventRecord
		if err := rows.Scan(
			&e.ID, &e.RoomID, &e.ChannelID, &e.AuthorMemberID, &e.AuthorSeq,
			&e.EventType, &e.TargetEventID, &e.Content, &e.Timestamp, &e.Signature,
			&e.ReplicatedStatus, &e.CreatedAt, &e.SigVersion,
		); err != nil {
			return nil, err
		}
		if knownIDs != nil && knownIDs[e.ID] {
			continue
		}
		cursor, hasCursor := cursors[e.AuthorMemberID]
		isNewSeq := !hasCursor || e.AuthorSeq > cursor
		isGap := inGaps(e.AuthorSeq, gaps[e.AuthorMemberID])
		isConflict := multiEvents[e.AuthorMemberID] != nil && multiEvents[e.AuthorMemberID][e.AuthorSeq]

		if !isNewSeq && !isGap && !isConflict {
			if knownIDs != nil && len(knownIDs) <= 500 && !knownIDs[e.ID] {
				isConflict = true
			}
		}

		if !isNewSeq && !isGap && !isConflict {
			continue
		}
		events = append(events, e)
		if len(events) >= limit {
			break
		}
	}
	return events, rows.Err()
}

// HasEvent reports whether an event ID is already stored. Divergent events
// share (author, seq) but never share an ID, so this is the duplicate check.
func (s *Store) HasEvent(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var one int
	err := s.db.QueryRow("SELECT 1 FROM community_events WHERE id = ?", id).Scan(&one)
	return err == nil
}

// EventStorageBytes approximates the space community events occupy. Content
// dominates; the fixed columns are counted at a flat per-row overhead.
func (s *Store) EventStorageBytes(roomID string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var total sql.NullInt64
	err := s.db.QueryRow(`
		SELECT SUM(LENGTH(content) + LENGTH(id) + LENGTH(signature) + 256)
		FROM community_events WHERE room_id = ?
	`, roomID).Scan(&total)
	if err != nil {
		return 0, err
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Int64, nil
}

// PurgeEventsOverCap deletes the oldest non-tombstone events until the room's
// stored events fit within capBytes. Tombstones are retained so a moderation
// decision is never lost to a storage sweep.
func (s *Store) PurgeEventsOverCap(roomID string, capBytes int64) (int64, error) {
	if capBytes <= 0 {
		return 0, nil
	}

	total, err := s.EventStorageBytes(roomID)
	if err != nil {
		return 0, err
	}
	if total <= capBytes {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(`
		SELECT id, LENGTH(content) + LENGTH(id) + LENGTH(signature) + 256
		FROM community_events
		WHERE room_id = ? AND event_type != 'tombstone'
		ORDER BY timestamp ASC, id ASC
	`, roomID)
	if err != nil {
		return 0, err
	}

	var doomed []string
	remaining := total
	for rows.Next() && remaining > capBytes {
		var id string
		var size int64
		if err := rows.Scan(&id, &size); err != nil {
			rows.Close()
			return 0, err
		}
		doomed = append(doomed, id)
		remaining -= size
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var deleted int64
	for _, id := range doomed {
		res, err := s.db.Exec("DELETE FROM community_events WHERE id = ?", id)
		if err != nil {
			return deleted, err
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	return deleted, nil
}

// ReceiptCursor returns the newest receipt timestamp this node holds for a
// (host, requester) pair, which is the unit contributions sync pages over.
func (s *Store) ReceiptCursors(roomID string) (map[string]int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT host_member_id, requester_member_id, MAX(timestamp)
		FROM contribution_receipts WHERE room_id = ?
		GROUP BY host_member_id, requester_member_id
	`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cursors := make(map[string]int64)
	for rows.Next() {
		var host, requester string
		var maxTS int64
		if err := rows.Scan(&host, &requester, &maxTS); err != nil {
			return nil, err
		}
		cursors[host+"/"+requester] = maxTS
	}
	return cursors, rows.Err()
}

type ReceiptCursorKey struct {
	Timestamp int64  `json:"timestamp"`
	RequestID string `json:"request_id"`
}

// ListReceiptIDs returns all receipt request IDs stored in the room.
func (s *Store) ListReceiptIDs(roomID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT request_id FROM contribution_receipts WHERE room_id = ?`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListReceiptsAfterCursor returns receipts the requester is missing for each
// (host, requester) pair, bounded by limit. Uses unique tie-breaker (timestamp, request_id)
// for pagination and checks known receipt IDs to recover receipts learned out of order.
func (s *Store) ListReceiptsAfterCursor(
	roomID string,
	cursors map[string]int64,
	pageCursors map[string]ReceiptCursorKey,
	knownIDs map[string]bool,
	limit int,
) ([]ContributionReceiptRecord, error) {
	if limit <= 0 {
		limit = 500
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(`
		SELECT request_id, room_id, host_member_id, requester_member_id,
		       timestamp, completed, host_signature, requester_signature,
		       replicated_status, created_at, sig_version
		FROM contribution_receipts WHERE room_id = ?
		ORDER BY host_member_id ASC, requester_member_id ASC, timestamp ASC, request_id ASC
	`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	receipts := make([]ContributionReceiptRecord, 0, limit)
	for rows.Next() {
		var r ContributionReceiptRecord
		var completedInt int
		if err := rows.Scan(
			&r.RequestID, &r.RoomID, &r.HostMemberID, &r.RequesterMemberID,
			&r.Timestamp, &completedInt, &r.HostSignature, &r.RequesterSignature,
			&r.ReplicatedStatus, &r.CreatedAt, &r.SigVersion); err != nil {
			return nil, err
		}
		r.Completed = completedInt != 0

		if knownIDs != nil && knownIDs[r.RequestID] {
			continue
		}

		pair := r.HostMemberID + "/" + r.RequesterMemberID
		if pageCursors != nil {
			if pc, ok := pageCursors[pair]; ok {
				if r.Timestamp < pc.Timestamp || (r.Timestamp == pc.Timestamp && r.RequestID <= pc.RequestID) {
					continue
				}
			}
		} else if cursor, ok := cursors[pair]; ok && (knownIDs == nil || len(knownIDs) == 0) && r.Timestamp <= cursor {
			continue
		}

		receipts = append(receipts, r)
		if len(receipts) >= limit {
			break
		}
	}
	return receipts, rows.Err()
}
