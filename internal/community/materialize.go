package community

import (
	"encoding/json"
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

// ChannelAnnouncement is a channel materialized from the signed event log.
// Channels replicate this way so a channel created on one node is visible and
// readable on every other, rather than existing only where it was created.
type ChannelAnnouncement struct {
	ID             string `json:"id"`
	RoomID         string `json:"room_id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	AuthorMemberID string `json:"author_member_id"`
	CreatedAt      int64  `json:"created_at"`
}

// ChannelPayload is the JSON body of a channel event's Content field.
type ChannelPayload struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Result is everything one pass over the event log produces.
type Result struct {
	Messages []MaterializedMessage
	Channels []ChannelAnnouncement
	// ConflictedAuthors names authors that published two different events
	// under the same author_seq. Both events are stored; the deterministic
	// winner is the lexically smaller event ID so every peer converges on the
	// same view, and the author is surfaced so the divergence is visible
	// rather than silently absorbed.
	ConflictedAuthors []string
}

func MaterializeEvents(events []Event) []MaterializedMessage {
	return Materialize(events).Messages
}

func Materialize(events []Event) Result {
	if len(events) == 0 {
		return Result{Messages: []MaterializedMessage{}, Channels: []ChannelAnnouncement{}}
	}

	// 1. Deduplicate by event ID, then resolve author-sequence conflicts.
	//    Two peers can legitimately receive divergent events in opposite
	//    orders, so the winner cannot be "whichever arrived first".
	seenIDs := make(map[string]bool)
	bySeq := make(map[string]Event)
	conflicted := make(map[string]bool)

	for _, e := range events {
		if seenIDs[e.ID] {
			continue
		}
		seenIDs[e.ID] = true

		seqKey := fmt.Sprintf("%s:%d", e.AuthorMemberID, e.AuthorSeq)
		existing, exists := bySeq[seqKey]
		if !exists {
			bySeq[seqKey] = e
			continue
		}
		if existing.ID == e.ID {
			continue
		}
		conflicted[e.AuthorMemberID] = true
		if e.ID < existing.ID {
			bySeq[seqKey] = e
		}
	}

	uniqueEvents := make([]Event, 0, len(bySeq))
	for _, e := range bySeq {
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

	// 3. Materialize message and channel records
	msgMap := make(map[string]*MaterializedMessage)
	var orderedList []*MaterializedMessage
	channels := make([]ChannelAnnouncement, 0)
	seenChannels := make(map[string]bool)

	for _, e := range uniqueEvents {
		switch e.EventType {
		case EventChannel:
			if seenChannels[e.ChannelID] {
				continue
			}
			var payload ChannelPayload
			if json.Unmarshal([]byte(e.Content), &payload) != nil || payload.Name == "" {
				continue
			}
			seenChannels[e.ChannelID] = true
			channels = append(channels, ChannelAnnouncement{
				ID:             e.ChannelID,
				RoomID:         e.RoomID,
				Name:           payload.Name,
				Description:    payload.Description,
				AuthorMemberID: e.AuthorMemberID,
				CreatedAt:      e.Timestamp,
			})

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
				// Only the original author can edit their message
				if target.AuthorMemberID == e.AuthorMemberID && !target.Deleted && !target.Tombstoned {
					target.Content = SanitizeContent(e.Content)
					target.Edited = true
				}
			}

		case EventDelete:
			if target, exists := msgMap[e.TargetEventID]; exists {
				// Only the original author can delete their message
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

	messages := make([]MaterializedMessage, 0, len(orderedList))
	for _, m := range orderedList {
		messages = append(messages, *m)
	}

	authors := make([]string, 0, len(conflicted))
	for author := range conflicted {
		authors = append(authors, author)
	}
	sort.Strings(authors)

	return Result{Messages: messages, Channels: channels, ConflictedAuthors: authors}
}
