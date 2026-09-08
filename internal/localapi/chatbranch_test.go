package localapi

import (
	"testing"

	"github.com/Chrisbaack/woolwire/internal/store"
)

func msg(id, parent, role, content string, at int64, active bool) store.MessageRecord {
	return store.MessageRecord{
		ID: id, ConversationID: "c1", Role: role, Content: content,
		CreatedAt: at, ParentID: parent, Active: active,
	}
}

func ids(branch []branchView) []string {
	out := make([]string, 0, len(branch))
	for _, b := range branch {
		out = append(out, b.ID)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestActiveBranchFollowsTheSelectedAlternative(t *testing.T) {
	// u1 -> a1 (superseded) / a2 (active) -> u2 -> a3
	msgs := []store.MessageRecord{
		msg("u1", "", "user", "hi", 1, true),
		msg("a1", "u1", "assistant", "first try", 2, false),
		msg("a2", "u1", "assistant", "second try", 3, true),
		msg("u2", "a2", "user", "more", 4, true),
		msg("a3", "u2", "assistant", "answer", 5, true),
	}

	branch := activeBranch(msgs)
	if got := ids(branch); !equal(got, []string{"u1", "a2", "u2", "a3"}) {
		t.Fatalf("active branch = %v, want the a2 line", got)
	}
	if branch[1].VariantCount != 2 || branch[1].VariantIndex != 2 {
		t.Errorf("a2 reported as variant %d of %d, want 2 of 2", branch[1].VariantIndex, branch[1].VariantCount)
	}
	if !equal(branch[1].SiblingIDs, []string{"a1", "a2"}) {
		t.Errorf("siblings = %v, want [a1 a2] oldest first", branch[1].SiblingIDs)
	}
	// A turn with no alternatives still reports a coherent 1 of 1.
	if branch[0].VariantIndex != 1 || branch[0].VariantCount != 1 {
		t.Errorf("u1 reported as %d of %d, want 1 of 1", branch[0].VariantIndex, branch[0].VariantCount)
	}
}

// Transcripts written before the tree existed have no message flagged active.
// They must still render rather than collapsing to nothing.
func TestActiveBranchFallsBackWhenNothingIsFlagged(t *testing.T) {
	msgs := []store.MessageRecord{
		msg("u1", "", "user", "hi", 1, false),
		msg("a1", "u1", "assistant", "answer", 2, false),
	}
	if got := ids(activeBranch(msgs)); !equal(got, []string{"u1", "a1"}) {
		t.Fatalf("branch = %v, want the whole linear transcript", got)
	}
}

// A message whose parent is missing must not silently vanish from the chat.
func TestActiveBranchKeepsOrphanedMessages(t *testing.T) {
	msgs := []store.MessageRecord{
		msg("u9", "gone", "user", "orphan", 1, true),
		msg("a9", "u9", "assistant", "answer", 2, true),
	}
	if got := ids(activeBranch(msgs)); !equal(got, []string{"u9", "a9"}) {
		t.Fatalf("branch = %v, want the orphan promoted to a root", got)
	}
}

// Corrupted data must not hang the request. A parent cycle has no root, so
// both messages are promoted to roots and the walk still terminates; the point
// of the test is that it returns at all.
func TestActiveBranchTerminatesOnACycle(t *testing.T) {
	msgs := []store.MessageRecord{
		msg("m1", "m2", "user", "a", 1, true),
		msg("m2", "m1", "assistant", "b", 2, true),
	}
	if got := ids(activeBranch(msgs)); len(got) > len(msgs) {
		t.Fatalf("branch = %v, want no message repeated", got)
	}
}

func TestBranchThroughStopsAtTheRequestedMessage(t *testing.T) {
	msgs := []store.MessageRecord{
		msg("u1", "", "user", "hi", 1, true),
		msg("a1", "u1", "assistant", "first", 2, true),
		msg("u2", "a1", "user", "more", 3, true),
		msg("a2", "u2", "assistant", "second", 4, true),
	}
	if got := ids(branchThrough(msgs, "u2")); !equal(got, []string{"u1", "a1", "u2"}) {
		t.Fatalf("context = %v, want everything up to u2", got)
	}
}

// Regenerating reads context from the branch the target sits on, which is not
// necessarily the visible one.
func TestBranchThroughWalksAnInactiveBranch(t *testing.T) {
	msgs := []store.MessageRecord{
		msg("u1", "", "user", "hi", 1, true),
		msg("a1", "u1", "assistant", "old", 2, false),
		msg("a2", "u1", "assistant", "new", 3, true),
		msg("u2", "a1", "user", "follow-up on the old one", 4, true),
	}
	if got := ids(branchThrough(msgs, "u2")); !equal(got, []string{"u1", "a1", "u2"}) {
		t.Fatalf("context = %v, want the inactive a1 line", got)
	}
}

func TestBranchThroughUnknownMessageIsEmpty(t *testing.T) {
	msgs := []store.MessageRecord{msg("u1", "", "user", "hi", 1, true)}
	if got := branchThrough(msgs, "nope"); len(got) != 0 {
		t.Fatalf("context = %v, want empty for an unknown id", ids(got))
	}
}
