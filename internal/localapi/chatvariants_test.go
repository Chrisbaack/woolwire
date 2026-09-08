package localapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Chrisbaack/woolwire/internal/store"
	"github.com/Chrisbaack/woolwire/internal/transport"
)

// chatVariantNode wires a node to a fake OpenAI backend whose reply changes on
// every call, so a regenerated answer is distinguishable from the original.
func chatVariantNode(t *testing.T) (*testNode, store.HostedModelRecord, store.ConversationRecord) {
	t.Helper()

	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer-%d\"}}]}\n\n", n)
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(backend.Close)

	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "variant-node", 4301)
	node.login(t)
	node.hostRoom(t, "Alice", "Variant Room")

	w := node.doJSON("POST", "/api/v1/hosted-models", map[string]any{
		"name":          "Echo",
		"endpoint_url":  backend.URL + "/v1",
		"context_limit": 8192,
		"max_tokens":    512,
		"published":     true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("register model: %d %s", w.Code, w.Body.String())
	}
	var model store.HostedModelRecord
	_ = json.NewDecoder(w.Body).Decode(&model)

	// The send path refuses a model the catalog has not seen.
	if c := node.doJSON("GET", "/api/v1/catalog", nil); c.Code != http.StatusOK {
		t.Fatalf("catalog refresh: %d", c.Code)
	}

	w = node.doJSON("POST", "/api/v1/chats", map[string]any{"title": "Variants"})
	if w.Code != http.StatusOK {
		t.Fatalf("create chat: %d %s", w.Code, w.Body.String())
	}
	var conv store.ConversationRecord
	_ = json.NewDecoder(w.Body).Decode(&conv)

	return node, model, conv
}

// branchOf reads the visible transcript back through the API.
func branchOf(t *testing.T, node *testNode, convID string) []map[string]any {
	t.Helper()
	w := node.doJSON("GET", "/api/v1/chats/"+convID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get chat: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode chat: %v", err)
	}
	return out.Messages
}

func TestRegenerateKeepsTheEarlierAnswerAsAVariant(t *testing.T) {
	node, model, conv := chatVariantNode(t)

	w := node.doJSON("POST", "/api/v1/chats/"+conv.ID+"/message", map[string]string{
		"content":        "hello",
		"host_member_id": node.memberID,
		"model_id":       model.ID,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}

	branch := branchOf(t, node, conv.ID)
	if len(branch) != 2 {
		t.Fatalf("got %d messages, want user + assistant", len(branch))
	}
	firstAnswerID, _ := branch[1]["ID"].(string)
	if got, _ := branch[1]["Content"].(string); !strings.Contains(got, "answer-1") {
		t.Fatalf("first answer = %q, want answer-1", got)
	}

	w = node.doJSON("POST", "/api/v1/chats/"+conv.ID+"/messages/"+firstAnswerID+"/regenerate", map[string]string{
		"host_member_id": node.memberID,
		"model_id":       model.ID,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("regenerate: %d %s", w.Code, w.Body.String())
	}

	// The transcript still reads as one exchange, now showing the new answer.
	branch = branchOf(t, node, conv.ID)
	if len(branch) != 2 {
		t.Fatalf("got %d messages after regenerate, want the branch to stay one exchange", len(branch))
	}
	if got, _ := branch[1]["Content"].(string); !strings.Contains(got, "answer-2") {
		t.Fatalf("visible answer = %q, want the regenerated answer-2", got)
	}
	if count, _ := branch[1]["VariantCount"].(float64); count != 2 {
		t.Errorf("VariantCount = %v, want 2", count)
	}
	if idx, _ := branch[1]["VariantIndex"].(float64); idx != 2 {
		t.Errorf("VariantIndex = %v, want the newest take to be selected", idx)
	}

	// Nothing was destroyed: both answers are still stored.
	stored, err := node.store.ListMessages(conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 3 {
		t.Fatalf("stored %d messages, want the superseded answer kept", len(stored))
	}

	// Paging back restores the original answer.
	w = node.doJSON("POST", "/api/v1/chats/"+conv.ID+"/messages/"+firstAnswerID+"/select", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("select: %d %s", w.Code, w.Body.String())
	}
	branch = branchOf(t, node, conv.ID)
	if got, _ := branch[1]["Content"].(string); !strings.Contains(got, "answer-1") {
		t.Fatalf("after switching back, visible answer = %q, want answer-1", got)
	}
}

func TestEditQuestionBranchesInsteadOfOverwriting(t *testing.T) {
	node, model, conv := chatVariantNode(t)

	w := node.doJSON("POST", "/api/v1/chats/"+conv.ID+"/message", map[string]string{
		"content":        "first wording",
		"host_member_id": node.memberID,
		"model_id":       model.ID,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}
	branch := branchOf(t, node, conv.ID)
	questionID, _ := branch[0]["ID"].(string)

	w = node.doJSON("POST", "/api/v1/chats/"+conv.ID+"/messages/"+questionID+"/edit", map[string]string{
		"content":        "second wording",
		"host_member_id": node.memberID,
		"model_id":       model.ID,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}

	branch = branchOf(t, node, conv.ID)
	if len(branch) != 2 {
		t.Fatalf("got %d messages, want the edited question and its answer", len(branch))
	}
	if got, _ := branch[0]["Content"].(string); got != "second wording" {
		t.Fatalf("visible question = %q, want the edit", got)
	}
	if count, _ := branch[0]["VariantCount"].(float64); count != 2 {
		t.Errorf("question VariantCount = %v, want 2", count)
	}
	// The original wording and the answer it drew are both still stored.
	stored, _ := node.store.ListMessages(conv.ID)
	if len(stored) != 4 {
		t.Fatalf("stored %d messages, want both questions and both answers", len(stored))
	}
}

// A no-save chat stores nothing, so there is no branch to graft onto. The
// endpoints must say so rather than appearing to work.
func TestVariantEndpointsRejectNoSaveChats(t *testing.T) {
	node, model, _ := chatVariantNode(t)

	w := node.doJSON("POST", "/api/v1/chats", map[string]any{"title": "Private", "no_save": true})
	var conv store.ConversationRecord
	_ = json.NewDecoder(w.Body).Decode(&conv)

	if w := node.doJSON("POST", "/api/v1/chats/"+conv.ID+"/messages/msg-x/regenerate", map[string]string{
		"host_member_id": node.memberID, "model_id": model.ID,
	}); w.Code != http.StatusBadRequest {
		t.Errorf("regenerate in a no-save chat returned %d, want 400", w.Code)
	}
	if w := node.doJSON("POST", "/api/v1/chats/"+conv.ID+"/messages/msg-x/edit", map[string]string{
		"content": "x", "host_member_id": node.memberID, "model_id": model.ID,
	}); w.Code != http.StatusBadRequest {
		t.Errorf("edit in a no-save chat returned %d, want 400", w.Code)
	}
}

// Regenerating a question, or editing an answer, is a client bug rather than a
// meaningful operation.
func TestVariantEndpointsRejectTheWrongRole(t *testing.T) {
	node, model, conv := chatVariantNode(t)

	w := node.doJSON("POST", "/api/v1/chats/"+conv.ID+"/message", map[string]string{
		"content":        "hello",
		"host_member_id": node.memberID,
		"model_id":       model.ID,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("send: %d %s", w.Code, w.Body.String())
	}
	branch := branchOf(t, node, conv.ID)
	questionID, _ := branch[0]["ID"].(string)
	answerID, _ := branch[1]["ID"].(string)

	if w := node.doJSON("POST", "/api/v1/chats/"+conv.ID+"/messages/"+questionID+"/regenerate", map[string]string{
		"host_member_id": node.memberID, "model_id": model.ID,
	}); w.Code != http.StatusBadRequest {
		t.Errorf("regenerating a user message returned %d, want 400", w.Code)
	}
	if w := node.doJSON("POST", "/api/v1/chats/"+conv.ID+"/messages/"+answerID+"/edit", map[string]string{
		"content": "x", "host_member_id": node.memberID, "model_id": model.ID,
	}); w.Code != http.StatusBadRequest {
		t.Errorf("editing an assistant message returned %d, want 400", w.Code)
	}
}
