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
	"github.com/Chrisbaack/woolwire/internal/store"
	"github.com/Chrisbaack/woolwire/internal/transport"
)

func TestManagedModelReplacementRetainsUnloadedAvailability(t *testing.T) {
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

	// Replacing the loaded model releases its weights but keeps the prepared
	// row enabled and published, so peers can still demand-load it later.
	mA, err = node.store.GetHostedModel("model-a")
	if err != nil || mA == nil {
		t.Fatalf("expected model-a in store: %v", err)
	}
	if !mA.Enabled || !mA.Published {
		t.Fatalf("expected replaced model-a to remain enabled and published")
	}

	// Check model B is enabled in store
	mB, err := node.store.GetHostedModel("model-b")
	if err != nil || mB == nil {
		t.Fatalf("expected model-b in store: %v", err)
	}
	if !mB.Enabled {
		t.Fatalf("expected model-b to be enabled")
	}

	// Both models remain selectable: A is unloaded, B is ready.
	ads = node.localSrv.catalog.ListAvailable()
	if len(ads) != 2 {
		t.Fatalf("expected both models in catalog, got: %v", ads)
	}
	availability := map[string]string{}
	for _, ad := range ads {
		availability[ad.ModelID] = ad.Availability
	}
	if availability["model-a"] != "unloaded" || availability["model-b"] != "ready" {
		t.Fatalf("unexpected replacement availability: %v", availability)
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

	// A failed replacement does not withdraw the existing published rows.
	mA, _ = node.store.GetHostedModel("model-a")
	mB, _ = node.store.GetHostedModel("model-b")
	if !mA.Enabled || !mB.Enabled {
		t.Fatalf("expected both managed models to remain enabled after failure, A=%v, B=%v", mA.Enabled, mB.Enabled)
	}

	// Model B is still loaded and model A remains demand-loadable.
	ads = node.localSrv.catalog.ListAvailable()
	if len(ads) != 2 {
		t.Fatalf("expected catalog to retain existing models after failure, got: %v", ads)
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

func TestManagedModelMaxTokensDefault(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "node-1", 4242)
	node.login(t)

	runnerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "idle"})
	}))
	defer runnerSrv.Close()
	node.localSrv.runnerClient = hosting.NewRunnerClient(runnerSrv.URL, "")

	w := node.doJSON("POST", "/api/v1/managed-models/load", map[string]any{
		"model_id":      "art-qwen-8k",
		"filename":      "Qwen-8k.gguf",
		"context_limit": 8192,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("load returned %d: %s", w.Code, w.Body.String())
	}

	model, err := node.store.GetHostedModel("art-qwen-8k")
	if err != nil || model == nil {
		t.Fatalf("model not found in store: %v", err)
	}
	if model.MaxTokens != 4096 {
		t.Errorf("got MaxTokens %d, want 4096 for 8192 context limit", model.MaxTokens)
	}
}

func TestPrepareManagedModelDoesNotLoadAndIsAdvertisedUnloaded(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "node-prepare", 4242)
	node.login(t)
	node.hostRoom(t, "Host", "Room")

	loads := 0
	runnerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/runner/v1/models/load":
			loads++
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		case "/runner/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "idle"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		}
	}))
	defer runnerSrv.Close()
	node.localSrv.runnerClient = hosting.NewRunnerClient(runnerSrv.URL, "")

	w := node.doJSON("POST", "/api/v1/managed-models/prepare", map[string]any{
		"model_id": "custom-model", "name": "Prepared", "filename": "prepared.gguf",
		"threads": 6, "gpu_layers": 12, "extra_args": []string{"--flash-attn=on"}, "published": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("prepare returned %d: %s", w.Code, w.Body.String())
	}
	if loads != 0 {
		t.Fatalf("prepare started the runner %d times", loads)
	}
	model, err := node.store.GetHostedModel("custom-model")
	if err != nil {
		t.Fatal(err)
	}
	if model.Filename != "prepared.gguf" || model.Threads != 6 || model.GPULayers == nil || *model.GPULayers != 12 || !model.Published {
		t.Fatalf("prepared config was not persisted: %#v", model)
	}
	ad, ok := node.localSrv.catalog.Get(node.memberID, "custom-model")
	if !ok || ad.Availability != "unloaded" {
		t.Fatalf("prepared model availability = %#v, present=%v", ad, ok)
	}
}

func TestManagedSettingsEditPreservesPreparedLoadConfiguration(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "node-edit", 4242)
	node.login(t)
	layers := 9
	if err := node.store.SaveHostedModel(store.HostedModelRecord{ID: "custom-model", Name: "Prepared", ModelType: "managed", EndpointURL: managedEndpointSentinel, BackendModel: "Prepared", ContextLimit: 4096, MaxTokens: 1024, Enabled: true, Published: true, Revision: 1, Filename: "prepared.gguf", Threads: 5, GPULayers: &layers, Projector: "mmproj.gguf", ExtraArgs: []string{"--no-mmap"}}); err != nil {
		t.Fatal(err)
	}
	w := node.doJSON("POST", "/api/v1/hosted-models", map[string]any{
		"id": "custom-model", "name": "Prepared", "model_type": "managed", "published": false, "enabled": true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("managed edit returned %d: %s", w.Code, w.Body.String())
	}
	model, err := node.store.GetHostedModel("custom-model")
	if err != nil {
		t.Fatal(err)
	}
	if model.Filename != "prepared.gguf" || model.Threads != 5 || model.GPULayers == nil || *model.GPULayers != layers || model.Projector != "mmproj.gguf" || len(model.ExtraArgs) != 1 || model.Published {
		t.Fatalf("managed edit lost load config or sharing state: %#v", model)
	}
}

// TestInventoryScanNamesModelsWithoutTheFileExtension covers naming a
// downloaded model. A model Woolwire installed records the bare file name in
// its manifest, extension and all, so the scan has to trim it: correcting a
// row named that way by an earlier scan, and never touching a name the owner
// chose for themselves.
func TestInventoryScanNamesModelsWithoutTheFileExtension(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "node-names", 4242)
	node.login(t)

	modelsDir := t.TempDir()
	manifestDir := filepath.Join(modelsDir, ".woolwire", "manifests")
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Three installed models, as a download would have recorded them.
	for _, f := range []string{"gemma-4-12b-it-Q8_0.gguf", "mine.gguf", "fresh.gguf"} {
		if err := os.WriteFile(filepath.Join(modelsDir, f), []byte("weights"), 0o600); err != nil {
			t.Fatal(err)
		}
		mf := map[string]any{"id": "art-" + strings.TrimSuffix(f, ".gguf"), "name": f,
			"filename": f, "path": f, "context_limit": 8192, "source": "download"}
		b, err := json.Marshal(mf)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(manifestDir, f+".json"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mgr, err := hosting.NewArtifactManager(modelsDir, 50<<30)
	if err != nil {
		t.Fatal(err)
	}
	node.localSrv.artifactMgr = mgr

	// One row as an older scan would have written it, suffix and all, and one
	// the owner renamed. "fresh" has no row yet.
	stale := store.HostedModelRecord{ID: "art-gemma-4-12b-it-Q8_0", Name: "gemma-4-12b-it-Q8_0.gguf",
		ModelType: "managed", EndpointURL: managedEndpointSentinel, BackendModel: "gemma-4-12b-it-Q8_0.gguf",
		ContextLimit: 8192, MaxTokens: 2048, Enabled: true, Revision: 1, Filename: "gemma-4-12b-it-Q8_0.gguf"}
	owned := store.HostedModelRecord{ID: "art-mine", Name: "Chris's favourite",
		ModelType: "managed", EndpointURL: managedEndpointSentinel, BackendModel: "Chris's favourite",
		ContextLimit: 8192, MaxTokens: 2048, Enabled: true, Revision: 1, Filename: "mine.gguf"}
	for _, rec := range []store.HostedModelRecord{stale, owned} {
		if err := node.store.SaveHostedModel(rec); err != nil {
			t.Fatal(err)
		}
	}

	node.localSrv.syncManagedInventory()

	corrected, err := node.store.GetHostedModel(stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if corrected.Name != "gemma-4-12b-it-Q8_0" || corrected.BackendModel != "gemma-4-12b-it-Q8_0" {
		t.Fatalf("a scanner-derived name kept its extension: %q / %q", corrected.Name, corrected.BackendModel)
	}
	if corrected.Revision <= stale.Revision {
		t.Fatalf("a renamed row must advertise a new revision, got %d", corrected.Revision)
	}

	untouched, err := node.store.GetHostedModel(owned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if untouched.Name != owned.Name || untouched.Revision != owned.Revision {
		t.Fatalf("the owner's own name was rewritten: %q rev %d", untouched.Name, untouched.Revision)
	}

	created, err := node.store.GetHostedModel("art-fresh")
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "fresh" {
		t.Fatalf("a newly scanned model is named %q", created.Name)
	}
}
