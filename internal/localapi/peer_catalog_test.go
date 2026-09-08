package localapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Chrisbaack/woolwire/internal/hosting"
	"github.com/Chrisbaack/woolwire/internal/transport"
)

func TestCatalogDiscoversHostThatJoinedAfterClient(t *testing.T) {
	network := transport.NewMemoryNetwork()
	creator := setupTestNode(t, network, "creator", 4242)
	client := setupTestNode(t, network, "client", 4242)
	host := setupTestNode(t, network, "host", 4242)
	for _, node := range []*testNode{creator, client, host} {
		node.login(t)
	}
	_, invitation := creator.hostRoom(t, "Creator", "Discovery")
	if w := client.joinRoom(t, "Client", invitation); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if w := host.joinRoom(t, "Host", invitation); w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w := host.doJSON("POST", "/api/v1/hosted-models", map[string]any{
		"name": "Shared model", "endpoint_url": "http://127.0.0.1:8080", "published": true,
	})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	w = client.doJSON("GET", "/api/v1/catalog", nil)
	var items []CatalogItem
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "Shared model" {
		t.Fatalf("late-joining host missing from client catalog: %s", w.Body.String())
	}
}

func TestPeerDiscoversManagedModelInCatalog_ModelLoadedBeforeJoin(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node1 := setupTestNode(t, vNet, "node-1", 4242)
	node2 := setupTestNode(t, vNet, "node-2", 4242)

	node1.login(t)
	node2.login(t)

	_ = node1.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "HostNode"})
	_ = node2.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "PeerNode"})

	_, invitation := node1.hostRoom(t, "HostNode", "TestRoom")

	var loadedModelID string
	runnerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/runner/v1/models/load":
			var body struct {
				ModelID string `json:"model_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			loadedModelID = body.ModelID
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "model_id": loadedModelID})
		case "/runner/v1/health":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "loaded_model_id": loadedModelID})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer runnerSrv.Close()

	node1.localSrv.runnerClient = hosting.NewRunnerClient(runnerSrv.URL, "")

	// Load model BEFORE node2 joins
	w := node1.doJSON("POST", "/api/v1/managed-models/load", map[string]any{
		"model_id":  "model-qwen",
		"name":      "Qwen-Test",
		"filename":  "qwen.gguf",
		"published": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("load managed model failed: %d, %s", w.Code, w.Body.String())
	}

	joinW := node2.joinRoom(t, "PeerNode", invitation)
	if joinW.Code != http.StatusOK {
		t.Fatalf("join failed: %d, %s", joinW.Code, joinW.Body.String())
	}

	// Now Node 2 fetches /api/v1/catalog
	w = node2.doJSON("GET", "/api/v1/catalog", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("node2 get catalog failed: %d, %s", w.Code, w.Body.String())
	}

	var items []CatalogItem
	if err := json.NewDecoder(w.Body).Decode(&items); err != nil {
		t.Fatalf("decode catalog response: %v", err)
	}

	t.Logf("Node 2 catalog items: %+v", items)
	var found bool
	for _, it := range items {
		if it.ModelID == "model-qwen" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Node 2 failed to discover Node 1's managed model in catalog: %#v", items)
	}
}
