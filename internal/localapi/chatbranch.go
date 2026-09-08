package localapi

import (
	"sort"

	"github.com/Chrisbaack/woolwire/internal/store"
)

// A conversation is stored as a tree. Regenerating an answer or editing a
// question adds a sibling rather than overwriting, so earlier attempts stay
// readable and the reader can page between them. Exactly one child of each
// parent is active, and the chain of active messages from the root is the
// transcript that is displayed and sent to the model.

// branchView is one message on the visible branch, together with where it sits
// among its alternatives.
type branchView struct {
	store.MessageRecord
	// VariantIndex is this message's 1-based position among its siblings, and
	// VariantCount how many there are. A message with no alternatives reports
	// 1 of 1.
	VariantIndex int
	VariantCount int
	// SiblingIDs lists every alternative for this turn, oldest first, so the
	// client can switch to one by id.
	SiblingIDs []string
}

// childrenByParent groups messages by their parent, preserving the stored
// order (oldest first) within each group.
func childrenByParent(msgs []store.MessageRecord) map[string][]store.MessageRecord {
	byParent := make(map[string][]store.MessageRecord, len(msgs))
	for _, m := range msgs {
		byParent[m.ParentID] = append(byParent[m.ParentID], m)
	}
	for parent := range byParent {
		group := byParent[parent]
		sort.SliceStable(group, func(i, j int) bool {
			return group[i].CreatedAt < group[j].CreatedAt
		})
		byParent[parent] = group
	}
	return byParent
}

// pickActive returns the active child of a group. A group with no message
// flagged active falls back to the newest, so a transcript written before the
// active column existed, or one left inconsistent, still renders.
func pickActive(group []store.MessageRecord) (store.MessageRecord, bool) {
	if len(group) == 0 {
		return store.MessageRecord{}, false
	}
	for _, m := range group {
		if m.Active {
			return m, true
		}
	}
	return group[len(group)-1], true
}

// activeBranch walks from the root along active children and returns the
// visible transcript. A parent_id that points at a missing message would
// otherwise strand its subtree, so orphaned roots are treated as roots.
func activeBranch(msgs []store.MessageRecord) []branchView {
	if len(msgs) == 0 {
		return nil
	}
	byParent := childrenByParent(msgs)
	known := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		known[m.ID] = true
	}
	// Anything whose parent no longer exists starts its own branch at the root
	// rather than disappearing from the transcript.
	for parent, group := range byParent {
		if parent != "" && !known[parent] {
			byParent[""] = append(byParent[""], group...)
			delete(byParent, parent)
		}
	}
	if roots := byParent[""]; len(roots) > 1 {
		sort.SliceStable(roots, func(i, j int) bool { return roots[i].CreatedAt < roots[j].CreatedAt })
		byParent[""] = roots
	}

	var branch []branchView
	visited := make(map[string]bool, len(msgs))
	parent := ""
	for {
		group := byParent[parent]
		current, ok := pickActive(group)
		if !ok || visited[current.ID] {
			// visited guards against a parent cycle in corrupted data, which
			// would otherwise loop here forever.
			break
		}
		visited[current.ID] = true

		siblings := make([]string, 0, len(group))
		index := 0
		for i, sib := range group {
			siblings = append(siblings, sib.ID)
			if sib.ID == current.ID {
				index = i + 1
			}
		}
		branch = append(branch, branchView{
			MessageRecord: current,
			VariantIndex:  index,
			VariantCount:  len(group),
			SiblingIDs:    siblings,
		})
		parent = current.ID
	}
	return branch
}

// branchThrough returns the visible transcript truncated at messageID
// inclusive. It is what a regeneration or an edit is given as context: the
// conversation as it stood at that point, with nothing from the branch being
// replaced.
func branchThrough(msgs []store.MessageRecord, messageID string) []branchView {
	if messageID == "" {
		return nil
	}
	// The requested message may sit on a branch that is not currently active,
	// so walk ancestors from it back to the root instead of down from the top.
	byID := make(map[string]store.MessageRecord, len(msgs))
	for _, m := range msgs {
		byID[m.ID] = m
	}
	var reversed []store.MessageRecord
	seen := make(map[string]bool, len(msgs))
	for id := messageID; id != ""; {
		m, ok := byID[id]
		if !ok || seen[id] {
			break
		}
		seen[id] = true
		reversed = append(reversed, m)
		id = m.ParentID
	}
	if len(reversed) == 0 {
		return nil
	}
	byParent := childrenByParent(msgs)
	path := make([]branchView, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		m := reversed[i]
		group := byParent[m.ParentID]
		siblings := make([]string, 0, len(group))
		index := 0
		for j, sib := range group {
			siblings = append(siblings, sib.ID)
			if sib.ID == m.ID {
				index = j + 1
			}
		}
		path = append(path, branchView{
			MessageRecord: m,
			VariantIndex:  index,
			VariantCount:  len(group),
			SiblingIDs:    siblings,
		})
	}
	return path
}
