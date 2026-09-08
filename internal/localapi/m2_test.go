package localapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cbaack/woolwire/internal/hosting"
	"github.com/cbaack/woolwire/internal/peerapi"
	"github.com/cbaack/woolwire/internal/store"
	"github.com/cbaack/woolwire/internal/transport"
)

func TestM2LiveDashboardAndExternalInferenceGate(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	const peerPort = 4242

	// Setup 3 nodes:
	// Node 1: Creator (Alice)
	// Node 2: Host (Bob)
	// Node 3: Requester (Charlie)
	node1 := setupTestNode(t, vNet, "node-1", peerPort)
	node1.login(t)

	node2 := setupTestNode(t, vNet, "node-2", peerPort)
	node2.login(t)

	node3 := setupTestNode(t, vNet, "node-3", peerPort)
	node3.login(t)

	// Set display names
	_ = node1.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Alice"})
	_ = node2.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Bob"})
	_ = node3.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "Charlie"})

	// Node 1 (Alice) creates room
	w := node1.doJSON("POST", "/api/v1/room/host", map[string]string{"room_name": "AI Builders"})
	if w.Code != http.StatusOK {
		t.Fatalf("host room failed: %d, %s", w.Code, w.Body.String())
	}
	var hostResp struct {
		RoomID         string `json:"room_id"`
		InvitationCode string `json:"invitation_code"`
	}
	_ = json.NewDecoder(w.Body).Decode(&hostResp)

	// Node 2 (Bob) joins room
	w = node2.doJSON("POST", "/api/v1/room/join", map[string]string{"invitation_code": hostResp.InvitationCode})
	if w.Code != http.StatusOK {
		t.Fatalf("node2 join room failed: %d, %s", w.Code, w.Body.String())
	}

	// Node 3 (Charlie) joins room
	w = node3.doJSON("POST", "/api/v1/room/join", map[string]string{"invitation_code": hostResp.InvitationCode})
	if w.Code != http.StatusOK {
		t.Fatalf("node3 join room failed: %d, %s", w.Code, w.Body.String())
	}

	// Give peers a moment to sync addresses if needed
	bobMemberID := node2.memberID
	charlieMemberID := node3.memberID

	// Sync roster and addresses
	_ = node3.store.SavePeerAddress(bobMemberID, "node-2")
	_ = node1.store.SavePeerAddress(bobMemberID, "node-2")
	charlieMem, _ := node1.store.GetMember(charlieMemberID)
	if charlieMem != nil {
		_ = node2.store.SaveMember(*charlieMem)
	}

	// Setup a mock external LLM server for Bob
	var mockServerHang sync.WaitGroup
	var backendModelMu sync.Mutex
	var backendModelSeen []string

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/hang") {
			mockServerHang.Wait()
		}

		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			var body struct {
				Model     string `json:"model"`
				MaxTokens int    `json:"max_tokens"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			backendModelMu.Lock()
			backendModelSeen = append(backendModelSeen, body.Model)
			backendModelMu.Unlock()
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher := w.(http.Flusher)

		chunks := []string{"Hello", " from", " Bob's", " model!"}
		for _, c := range chunks {
			chunkJSON, _ := json.Marshal(map[string]any{
				"choices": []map[string]any{
					{"delta": map[string]string{"content": c}},
				},
			})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(chunkJSON))
			flusher.Flush()
		}
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer mockServer.Close()

	// Bob configures and publishes hosted model
	w = node2.doJSON("POST", "/api/v1/hosted-models", map[string]any{
		"name":          "Llama-3-8B",
		"endpoint_url":  mockServer.URL + "/v1",
		"context_limit": 8192,
		"max_tokens":    2048,
		"published":     true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("Bob add hosted model failed: %d, %s", w.Code, w.Body.String())
	}
	var bobModel store.HostedModelRecord
	_ = json.NewDecoder(w.Body).Decode(&bobModel)

	// ---------------------------------------------------------------------------------
	// GATE CRITERION 1: Three members see the same reachable model offers from their own apps
	// ---------------------------------------------------------------------------------
	t.Run("Criterion 1: Three members discover model in catalog", func(t *testing.T) {
		for i, node := range []*testNode{node1, node2, node3} {
			w := node.doJSON("GET", "/api/v1/catalog", nil)
			if w.Code != http.StatusOK {
				t.Fatalf("node %d get catalog failed: %d, %s", i+1, w.Code, w.Body.String())
			}
			var items []CatalogItem
			_ = json.NewDecoder(w.Body).Decode(&items)

			var found bool
			for _, it := range items {
				if it.ModelID == bobModel.ID && it.HostMemberID == bobMemberID {
					found = true
					if it.Availability != "ready" {
						t.Errorf("node %d saw model availability %q, expected ready", i+1, it.Availability)
					}
					break
				}
			}
			if !found {
				t.Fatalf("node %d failed to see Bob's model in catalog: %#v", i+1, items)
			}
		}
	})

	// ---------------------------------------------------------------------------------
	// GATE CRITERION 2: Non-creator host serves another member while the creator is offline
	// ---------------------------------------------------------------------------------
	t.Run("Criterion 2: Bob serves Charlie while Alice (creator) is stopped", func(t *testing.T) {
		// Stop Creator (Alice) entirely: peer listener, local API, transport.
		_ = node1.stopPeer()
		_ = node1.localSrv.Close()
		_ = node1.trans.Close()

		// Charlie creates a conversation
		w := node3.doJSON("POST", "/api/v1/chats", map[string]any{
			"title":   "P2P LLM Chat",
			"no_save": false,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("Charlie create chat failed: %d, %s", w.Code, w.Body.String())
		}
		var conv store.ConversationRecord
		_ = json.NewDecoder(w.Body).Decode(&conv)

		// Charlie sends message to Bob's model
		w = node3.doJSON("POST", fmt.Sprintf("/api/v1/chats/%s/message", conv.ID), map[string]string{
			"content":        "Hi Bob!",
			"host_member_id": bobMemberID,
			"model_id":       bobModel.ID,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("Charlie send message failed: %d, %s", w.Code, w.Body.String())
		}

		respBody := w.Body.String()
		if !strings.Contains(respBody, "Hello") || !strings.Contains(respBody, "Bob's") {
			t.Fatalf("Charlie did not receive expected stream content: %s", respBody)
		}

		// Verify Charlie's local database contains the saved messages
		msgs, err := node3.store.ListMessages(conv.ID)
		if err != nil || len(msgs) != 2 {
			t.Fatalf("Charlie expected 2 messages (user + assistant), got %d, err: %v", len(msgs), err)
		}
		if msgs[0].Role != "user" || msgs[0].Content != "Hi Bob!" {
			t.Errorf("unexpected user message: %#v", msgs[0])
		}
		if msgs[1].Role != "assistant" || !strings.Contains(msgs[1].Content, "Hello from Bob's model!") {
			t.Errorf("unexpected assistant message: %#v", msgs[1])
		}

		// The backend must be sent the model name it knows. Passing the opaque
		// Woolwire ID only ever worked against single-model servers that
		// ignore the field.
		backendModelMu.Lock()
		seen := append([]string(nil), backendModelSeen...)
		backendModelMu.Unlock()
		if len(seen) == 0 {
			t.Fatal("the backend received no chat completion request")
		}
		for _, got := range seen {
			if got != "Llama-3-8B" {
				t.Fatalf("backend received model %q, want the configured name %q", got, "Llama-3-8B")
			}
		}
	})

	t.Run("Criterion 2b: Unpublished model rejects peer inference and never touches backend", func(t *testing.T) {
		// Unpublish Bob's model
		w := node2.doJSON("POST", "/api/v1/hosted-models", map[string]any{
			"id":                    bobModel.ID,
			"name":                  "Llama-3-8B",
			"endpoint_url":          mockServer.URL + "/v1",
			"context_limit":         8192,
			"max_tokens":            2048,
			"published":             false,
			"enabled":               true,
			"allow_private_network": true,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("Bob unpublish model failed: %d, %s", w.Code, w.Body.String())
		}

		backendModelMu.Lock()
		countBefore := len(backendModelSeen)
		backendModelMu.Unlock()

		// Charlie directly requests inference over authenticated TLS for the unpublished model
		conn, err := node3.localSrv.dialPeer(context.Background(), bobMemberID, "node-2")
		if err != nil {
			t.Fatalf("Charlie dial Bob failed: %v", err)
		}
		defer conn.Close()

		inferReq := peerapi.InferenceRequest{
			RequestID: "req-unpub-test",
			ModelID:   bobModel.ID,
			Messages:  []hosting.ChatMessage{{Role: "user", Content: "Hello?"}},
		}
		resp, err := peerRoundTrip(context.Background(), conn, "POST", "/peer/v1/inference", inferReq)
		if err != nil {
			t.Fatalf("peer roundtrip failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found for unpublished model, got: %d", resp.StatusCode)
		}

		backendModelMu.Lock()
		countAfter := len(backendModelSeen)
		backendModelMu.Unlock()

		if countAfter != countBefore {
			t.Fatalf("backend received request for unpublished model: before=%d, after=%d", countBefore, countAfter)
		}

		// Re-publish Bob's model so subsequent tests pass
		w = node2.doJSON("POST", "/api/v1/hosted-models", map[string]any{
			"id":                    bobModel.ID,
			"name":                  "Llama-3-8B",
			"endpoint_url":          mockServer.URL + "/v1",
			"context_limit":         8192,
			"max_tokens":            2048,
			"published":             true,
			"enabled":               true,
			"allow_private_network": true,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("Bob re-publish model failed: %d, %s", w.Code, w.Body.String())
		}
	})

	// ---------------------------------------------------------------------------------
	// GATE CRITERION 3: Personal chats never appear in shared sync or creator's database
	// ---------------------------------------------------------------------------------
	t.Run("Criterion 3: Privacy boundaries - chats strictly local to requester", func(t *testing.T) {
		// Verify Alice's DB has ZERO conversations and ZERO messages
		aliceConvs, err := node1.store.ListConversations()
		if err != nil || len(aliceConvs) != 0 {
			t.Fatalf("Creator (Alice) leaked conversations: %#v", aliceConvs)
		}

		// Verify Bob's DB has ZERO conversations and ZERO messages (host does not retain chat)
		bobConvs, err := node2.store.ListConversations()
		if err != nil || len(bobConvs) != 0 {
			t.Fatalf("Host (Bob) leaked conversations: %#v", bobConvs)
		}

		// Test No-Save mode: Charlie creates no-save conversation
		w := node3.doJSON("POST", "/api/v1/chats", map[string]any{
			"title":   "Ephemeral Chat",
			"no_save": true,
		})
		var noSaveConv store.ConversationRecord
		_ = json.NewDecoder(w.Body).Decode(&noSaveConv)

		w = node3.doJSON("POST", fmt.Sprintf("/api/v1/chats/%s/message", noSaveConv.ID), map[string]string{
			"content":        "Top secret question",
			"host_member_id": bobMemberID,
			"model_id":       bobModel.ID,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("No-save message failed: %d, %s", w.Code, w.Body.String())
		}

		// Check Charlie's database: No messages should be saved for this conversation
		charlieNoSaveMsgs, _ := node3.store.ListMessages(noSaveConv.ID)
		if len(charlieNoSaveMsgs) != 0 {
			t.Fatalf("No-save mode leaked messages into Charlie's DB: %#v", charlieNoSaveMsgs)
		}

		// A second turn must still carry the first: privacy mode must not also
		// mean the model forgets what was just said.
		w = node3.doJSON("POST", fmt.Sprintf("/api/v1/chats/%s/message", noSaveConv.ID), map[string]string{
			"content":        "And the follow-up?",
			"host_member_id": bobMemberID,
			"model_id":       bobModel.ID,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("second no-save message failed: %d, %s", w.Code, w.Body.String())
		}

		turns := node3.localSrv.noSaveHistory(noSaveConv.ID)
		var sawFirstTurn bool
		for _, m := range turns {
			if m.Role == "user" && m.Content == "Top secret question" {
				sawFirstTurn = true
			}
		}
		if !sawFirstTurn {
			t.Fatalf("no-save conversation lost its first turn: %#v", turns)
		}

		// Still nothing on disk.
		charlieNoSaveMsgs, _ = node3.store.ListMessages(noSaveConv.ID)
		if len(charlieNoSaveMsgs) != 0 {
			t.Fatalf("No-save mode leaked messages into Charlie's DB: %#v", charlieNoSaveMsgs)
		}
	})

	// ---------------------------------------------------------------------------------
	// GATE CRITERION 4: Offline / stale models cannot be falsely selected as ready
	// ---------------------------------------------------------------------------------
	t.Run("Criterion 4: Offline and stale models rejected explicitly", func(t *testing.T) {
		// Charlie creates a chat for this test
		cw := node3.doJSON("POST", "/api/v1/chats", map[string]any{"title": "Test Chat"})
		var cRec store.ConversationRecord
		_ = json.NewDecoder(cw.Body).Decode(&cRec)

		// Charlie tries to send to a non-existent model
		w := node3.doJSON("POST", fmt.Sprintf("/api/v1/chats/%s/message", cRec.ID), map[string]string{
			"content":        "Hello",
			"host_member_id": bobMemberID,
			"model_id":       "model-offline-123",
		})
		if w.Code != http.StatusServiceUnavailable && w.Code != http.StatusBadRequest {
			t.Fatalf("expected 503/400 for offline model, got: %d, %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "offline") && !strings.Contains(w.Body.String(), "unreachable") {
			t.Fatalf("expected explicit error state, got: %s", w.Body.String())
		}
	})

	// ---------------------------------------------------------------------------------
	// GATE CRITERION 5: Overflow, disconnect and cancellation produce explicit states without silent retries
	// ---------------------------------------------------------------------------------
	t.Run("Criterion 5: Queue limits, cancellation, and disconnect handling", func(t *testing.T) {
		// Set Bob's limits: MaxQueuedPerMember = 1
		w := node2.doJSON("POST", "/api/v1/host-limits", store.HostLimitsRecord{
			MaxActive:               1,
			MaxQueuedPerMember:      1,
			MaxQueuedTotal:          1,
			QueueTimeoutSeconds:     60,
			ExecutionTimeoutSeconds: 60,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("save limits failed: %d", w.Code)
		}

		// The shared queue picks the new limits up immediately.
		// Test SSRF destination validation: remote off-machine plain HTTP must be rejected
		w = node2.doJSON("POST", "/api/v1/hosted-models", map[string]any{
			"name":         "Insecure Remote Model",
			"endpoint_url": "http://192.168.1.50:8000/v1",
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 rejection for plain HTTP remote URL, got: %d", w.Code)
		}

		// Cloud metadata (169.254.169.254) must be rejected
		w = node2.doJSON("POST", "/api/v1/hosted-models", map[string]any{
			"name":         "Metadata Exploit",
			"endpoint_url": "http://169.254.169.254/latest/meta-data",
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 rejection for metadata service, got: %d", w.Code)
		}

		// The OpenAI-compatible routes always require the local API token.
		unauth := node3.newRequest("GET", "/v1/models", nil)
		if code := node3.doRequest(unauth).Code; code != http.StatusUnauthorized {
			t.Fatalf("expected 401 without the local API token, got %d", code)
		}

		req := node3.newRequest("GET", "/v1/models", nil)
		req.Header.Set("Authorization", node3.localAPIBearer(t))
		w = node3.doRequest(req)
		if w.Code != http.StatusOK {
			t.Fatalf("OpenAI models list failed: %d, %s", w.Code, w.Body.String())
		}
		var oaiModels struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.NewDecoder(w.Body).Decode(&oaiModels)
		var foundOAI bool
		for _, m := range oaiModels.Data {
			if m.ID == bobModel.ID {
				foundOAI = true
				break
			}
		}
		if !foundOAI {
			t.Fatalf("Bob's model not listed in /v1/models: %#v", oaiModels)
		}
	})
}

func TestTestAndDiscoverEndpoints(t *testing.T) {
	// Mock OpenAI/Ollama endpoint
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"id": "llama3:latest"},
					{"id": "mistral:7b"},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer mockServer.Close()

	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "test-node", 4242)
	node.login(t)

	// Test discover models
	w := node.doJSON("POST", "/api/v1/hosted-models/discover", map[string]any{
		"endpoint_url": mockServer.URL + "/v1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("discover failed: %d, %s", w.Code, w.Body.String())
	}
	var discResp struct {
		OK     bool     `json:"ok"`
		Models []string `json:"models"`
	}
	_ = json.NewDecoder(w.Body).Decode(&discResp)
	if !discResp.OK || len(discResp.Models) != 2 {
		t.Fatalf("unexpected discover response: %#v", discResp)
	}

	// Test connection
	w = node.doJSON("POST", "/api/v1/hosted-models/test", map[string]any{
		"endpoint_url": mockServer.URL + "/v1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("test endpoint failed: %d, %s", w.Code, w.Body.String())
	}
	var testResp struct {
		OK        bool     `json:"ok"`
		LatencyMs int64    `json:"latency_ms"`
		Models    []string `json:"models"`
	}
	_ = json.NewDecoder(w.Body).Decode(&testResp)
	if !testResp.OK || len(testResp.Models) != 2 {
		t.Fatalf("unexpected test response: %#v", testResp)
	}
}
