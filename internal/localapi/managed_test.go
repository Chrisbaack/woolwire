package localapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Chrisbaack/woolwire/internal/hosting"
	"github.com/Chrisbaack/woolwire/internal/transport"
)

func TestManagedModelReplacementAndFailureWithdrawal(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "node-1", 4242)
	node.login(t)

	// Set display name and host room
	_ = node.doJSON("POST", "/api/v1/profile", map[string]string{"display_name": "HostNode"})
	hostW := node.doJSON("POST", "/api/v1/room/host", map[string]string{"room_name": "ManagedRoom"})
	if hostW.Code != http.StatusOK {
		t.Fatalf("host room failed: %d", hostW.Code)
	}

	var loadedModelID string
	var failLoad bool

	// Fake runner server
	runnerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/runner/v1/models/load":
			if failLoad {
				http.Error(w, "simulated load failure", http.StatusInternalServerError)
				return
			}
			var body struct {
				ModelID string `json:"model_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			loadedModelID = body.ModelID
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "model_id": loadedModelID})
		case "/runner/v1/models/unload":
			loadedModelID = ""
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/runner/v1/health":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "loaded_model_id": loadedModelID})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer runnerSrv.Close()

	node.localSrv.runnerClient = hosting.NewRunnerClient(runnerSrv.URL, "")

	// 1. Load Model A
	w := node.doJSON("POST", "/api/v1/managed-models/load", map[string]any{
		"model_id":  "model-a",
		"filename":  "model-a.gguf",
		"published": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("load model-a failed: %d, %s", w.Code, w.Body.String())
	}

	mA, err := node.store.GetHostedModel("model-a")
	if err != nil || mA == nil {
		t.Fatalf("expected model-a in store: %v", err)
	}
	if !mA.Enabled || !mA.Published {
		t.Fatalf("expected model-a enabled and published, got: enabled=%v, published=%v", mA.Enabled, mA.Published)
	}

	ads := node.localSrv.catalog.ListAvailable()
	if len(ads) != 1 || ads[0].ModelID != "model-a" {
		t.Fatalf("expected catalog to have model-a, got: %v", ads)
	}

	// 2. Load Model B (replaces A)
	w = node.doJSON("POST", "/api/v1/managed-models/load", map[string]any{
		"model_id":  "model-b",
		"filename":  "model-b.gguf",
		"published": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("load model-b failed: %d, %s", w.Code, w.Body.String())
	}

	// Check model A is disabled in store and revision bumped
	mA, err = node.store.GetHostedModel("model-a")
	if err != nil || mA == nil {
		t.Fatalf("expected model-a in store: %v", err)
	}
	if mA.Enabled {
		t.Fatalf("expected replaced model-a to be disabled in store")
	}
	if mA.Revision < 2 {
		t.Fatalf("expected model-a revision >= 2, got %d", mA.Revision)
	}

	// Check model B is enabled in store
	mB, err := node.store.GetHostedModel("model-b")
	if err != nil || mB == nil {
		t.Fatalf("expected model-b in store: %v", err)
	}
	if !mB.Enabled {
		t.Fatalf("expected model-b to be enabled")
	}

	// Check catalog only contains model B
	ads = node.localSrv.catalog.ListAvailable()
	if len(ads) != 1 || ads[0].ModelID != "model-b" {
		t.Fatalf("expected catalog to have only model-b, got: %v", ads)
	}

	// 3. Failed replacement with Model C
	failLoad = true
	w = node.doJSON("POST", "/api/v1/managed-models/load", map[string]any{
		"model_id":  "model-c",
		"filename":  "model-c.gguf",
		"published": true,
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on load failure, got %d", w.Code)
	}

	// Both model A and model B must now be disabled in store
	mA, _ = node.store.GetHostedModel("model-a")
	mB, _ = node.store.GetHostedModel("model-b")
	if mA.Enabled || mB.Enabled {
		t.Fatalf("expected both managed models to be disabled after failure, A=%v, B=%v", mA.Enabled, mB.Enabled)
	}

	// Catalog must be empty of available models
	ads = node.localSrv.catalog.ListAvailable()
	if len(ads) != 0 {
		t.Fatalf("expected catalog to be empty after failed replacement, got: %v", ads)
	}
}

// TestManagedModelPathsSurviveTheAPI covers pointing the models directory at a
// Hugging Face cache: weights sit several levels down, so a nested reference
// has to survive routing, validation and the hop to the runner intact.
func TestManagedModelPathsSurviveTheAPI(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "node-1", 4242)
	node.login(t)

	modelsDir := t.TempDir()
	nested := "hub/models--org--repo/snapshots/rev1/model.gguf"
	if err := os.MkdirAll(filepath.Join(modelsDir, filepath.FromSlash(path.Dir(nested))), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelsDir, filepath.FromSlash(nested)), []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := hosting.NewArtifactManager(modelsDir, 50<<30)
	if err != nil {
		t.Fatal(err)
	}
	node.localSrv.artifactMgr = mgr

	var loadedFilename string
	runnerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/runner/v1/models/load" {
			var body struct {
				Filename string `json:"filename"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			loadedFilename = body.Filename
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready"})
	}))
	defer runnerSrv.Close()
	node.localSrv.runnerClient = hosting.NewRunnerClient(runnerSrv.URL, "")

	// The nested path reaches the runner unchanged.
	w := node.doJSON("POST", "/api/v1/managed-models/load", map[string]any{
		"model_id": "art-abc123", "filename": nested, "context_limit": 8192,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("nested load returned %d: %s", w.Code, w.Body.String())
	}
	if loadedFilename != nested {
		t.Fatalf("runner was asked for %q, want %q", loadedFilename, nested)
	}

	// A reference leaving the models directory is refused before it is sent.
	w = node.doJSON("POST", "/api/v1/managed-models/load", map[string]any{
		"model_id": "art-evil", "filename": "../../etc/passwd",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("traversal load returned %d, want 400", w.Code)
	}

	// Deleting weights Woolwire only found would corrupt a cache shared with
	// other tools, so the route exists but refuses.
	rec := node.doJSON("DELETE", "/api/v1/managed-models/artifacts/"+nested, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delete of a discovered model returned %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(modelsDir, filepath.FromSlash(nested))); err != nil {
		t.Fatalf("discovered model was deleted: %v", err)
	}
}

// TestLoadFailureReachesTheUI is the regression for the other half of an
// opaque failure: the runner explained itself in the response body and the
// client threw it away, leaving "runner error (500)".
func TestLoadFailureReachesTheUI(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "node-1", 4242)
	node.login(t)

	runnerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/runner/v1/models/load" {
			http.Error(w, "engine exited before becoming ready: error loading model: unknown model architecture: 'k2-horizon'", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "idle"})
	}))
	defer runnerSrv.Close()
	node.localSrv.runnerClient = hosting.NewRunnerClient(runnerSrv.URL, "")

	w := node.doJSON("POST", "/api/v1/managed-models/load", map[string]any{
		"model_id": "art-abc", "filename": "K2-Horizon-7B-Q4_K_M.gguf",
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("load returned %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unknown model architecture") {
		t.Fatalf("the reason never reached the caller: %s", w.Body.String())
	}
}
