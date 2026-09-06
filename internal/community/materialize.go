package community

import (
	"fmt"
	"sort"
)

type MaterializedMessage struct {
	ID               string `json:"id"`
	RoomID           string `json:"room_id"`
	ChannelID        string `json:"channel_id"`
	AuthorMemberID   string `json:"author_member_id"`
	AuthorSeq        int64  `json:"author_seq"`
	Content          string `json:"content"`
	Timestamp        int64  `json:"timestamp"`
	Edited           bool   `json:"edited"`
	Deleted          bool   `json:"deleted"`
	Tombstoned       bool   `json:"tombstoned"`
	TombstoneReason  string `json:"tombstone_reason,omitempty"`
	ReplicatedStatus string `json:"replicated_status"`
}

func MaterializeEvents(events []Event) []MaterializedMessage {
	if len(events) == 0 {
		return []MaterializedMessage{}
	}

	// 1. Deduplicate by event ID
	seenIDs := make(map[string]bool)
	uniqueEvents := make([]Event, 0, len(events))
	// Quarantine check: detect conflicting events with same author and same seq
	seenSeqs := make(map[string]string) // key: author+seq -> eventID

	for _, e := range events {
		if seenIDs[e.ID] {
			continue
		}
		seenIDs[e.ID] = true

		seqKey := fmt.Sprintf("%s:%d", e.AuthorMemberID, e.AuthorSeq)
		if existingID, exists := seenSeqs[seqKey]; exists && existingID != e.ID {
			// Conflicting sequence from same author! Quarantine: skip conflicting event
			continue
		}
		seenSeqs[seqKey] = e.ID
		uniqueEvents = append(uniqueEvents, e)
	}

	// 2. Sort deterministically: timestamp ASC, author_seq ASC, ID ASC
	sort.SliceStable(uniqueEvents, func(i, j int) bool {
		if uniqueEvents[i].Timestamp != uniqueEvents[j].Timestamp {
			return uniqueEvents[i].Timestamp < uniqueEvents[j].Timestamp
		}
		if uniqueEvents[i].AuthorSeq != uniqueEvents[j].AuthorSeq {
			return uniqueEvents[i].AuthorSeq < uniqueEvents[j].AuthorSeq
		}
		return uniqueEvents[i].ID < uniqueEvents[j].ID
	})

	// 3. Materialize message records
	msgMap := make(map[string]*MaterializedMessage)
	var orderedList []*MaterializedMessage

	for _, e := range uniqueEvents {
		switch e.EventType {
		case EventMessage:
			status := e.ReplicatedStatus
			if status == "" {
				status = "replicated"
			}
			msg := &MaterializedMessage{
				ID:               e.ID,
				RoomID:           e.RoomID,
				ChannelID:        e.ChannelID,
				AuthorMemberID:   e.AuthorMemberID,
				AuthorSeq:        e.AuthorSeq,
				Content:          SanitizeContent(e.Content),
				Timestamp:        e.Timestamp,
				ReplicatedStatus: status,
			}
			msgMap[e.ID] = msg
			orderedList = append(orderedList, msg)

		case EventEdit:
			if target, exists := msgMap[e.TargetEventID]; exists {
				// Only original author can edit their message
				if target.AuthorMemberID == e.AuthorMemberID && !target.Deleted && !target.Tombstoned {
					target.Content = SanitizeContent(e.Content)
					target.Edited = true
				}
			}

		case EventDelete:
			if target, exists := msgMap[e.TargetEventID]; exists {
				// Only original author can delete their message
				if target.AuthorMemberID == e.AuthorMemberID && !target.Tombstoned {
					target.Deleted = true
					target.Content = "[Message deleted by author]"
				}
			}

		case EventTombstone:
			if target, exists := msgMap[e.TargetEventID]; exists {
				target.Tombstoned = true
				target.TombstoneReason = e.Content
				target.Content = "[Message removed by room moderator: " + e.Content + "]"
			}
		}
	}

	result := make([]MaterializedMessage, 0, len(orderedList))
	for _, m := range orderedList {
		result = append(result, *m)
	}

	return result
}
