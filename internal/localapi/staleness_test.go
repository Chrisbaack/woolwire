package localapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Chrisbaack/woolwire/internal/catalog"
	"github.com/Chrisbaack/woolwire/internal/hosting"
	"github.com/Chrisbaack/woolwire/internal/identity"
	"github.com/Chrisbaack/woolwire/internal/transport"
)

// ageLocalAd re-signs a node's own advertisement with an old timestamp, which
// is what an advertisement looks like after nothing has refreshed it for
// longer than the freshness window.
func ageLocalAd(t *testing.T, node *testNode, modelID string, age time.Duration) {
	t.Helper()

	ad, ok := node.localSrv.catalog.Get(node.memberID, modelID)
	if !ok {
		t.Fatalf("model %q was never advertised", modelID)
	}

	device, err := node.store.GetDeviceIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pubBytes, err := identity.DecodeToken(device.DevicePublic, ed25519.PublicKeySize)
	if err != nil {
		t.Fatal(err)
	}

	ad.Timestamp = time.Now().Add(-age).Unix()
	ad.SigVersion = identity.SigVersionCanonical
	ad.Signature = identity.EncodeToken(ed25519.Sign(ed25519.PrivateKey(device.DevicePrivate), ad.Payload()))
	if err := node.localSrv.catalog.Upsert(ad, ed25519.PublicKey(pubBytes)); err != nil {
		t.Fatal(err)
	}
}

// TestOwnModelStaysReachableWhileTheRunnerServesIt is the regression for a
// chat refusing a model the runner was serving. A node's own advertisement was
// only re-signed inside the catalog handler, so a model went "offline" 90
// seconds after a browser last polled the dashboard — while Settings, reading
// live runner health, still showed it ready.
func TestOwnModelStaysReachableWhileTheRunnerServesIt(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "node-1", 4242)
	node.login(t)
	node.hostRoom(t, "Host", "Room")

	loaded := ""
	runnerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/runner/v1/models/load" {
			var body struct {
				ModelID string `json:"model_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			loaded = body.ModelID
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "loaded_model_id": loaded})
	}))
	defer runnerSrv.Close()
	node.localSrv.runnerClient = hosting.NewRunnerClient(runnerSrv.URL, "")

	if w := node.doJSON("POST", "/api/v1/managed-models/load", map[string]any{
		"model_id": "art-abc", "name": "Qwen", "filename": "m.gguf", "context_limit": 8192,
	}); w.Code != http.StatusOK {
		t.Fatalf("load model: %d %s", w.Code, w.Body.String())
	}

	ageLocalAd(t, node, "art-abc", (catalog.StaleAdTimeoutSeconds+30)*time.Second)

	// Nothing about the host changed: it is this node, and its runner still
	// holds the weights. Refreshing its own advertisement must not depend on
	// a browser having polled recently.
	node.localSrv.advertiseLocalModels()

	ad, ok := node.localSrv.catalog.Get(node.memberID, "art-abc")
	if !ok {
		t.Fatal("own model disappeared from the catalog")
	}
	if ad.Availability != "ready" {
		t.Fatalf("own model reads as %q while its runner is serving it", ad.Availability)
	}
	if age := time.Now().Unix() - ad.Timestamp; age > catalog.StaleAdTimeoutSeconds {
		t.Fatalf("advertisement is still %ds old after a refresh", age)
	}
}

// TestCatalogLoopRefreshesOwnAdvertisements covers the timer that keeps the
// fix working while nobody is looking at the UI.
func TestCatalogLoopRefreshesOwnAdvertisements(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "node-1", 4242)
	node.login(t)
	node.hostRoom(t, "Host", "Room")

	loaded := ""
	runnerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/runner/v1/models/load" {
			var body struct {
				ModelID string `json:"model_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			loaded = body.ModelID
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "loaded_model_id": loaded})
	}))
	defer runnerSrv.Close()
	node.localSrv.runnerClient = hosting.NewRunnerClient(runnerSrv.URL, "")

	if w := node.doJSON("POST", "/api/v1/managed-models/load", map[string]any{
		"model_id": "art-abc", "name": "Qwen", "filename": "m.gguf", "context_limit": 8192,
	}); w.Code != http.StatusOK {
		t.Fatalf("load model: %d %s", w.Code, w.Body.String())
	}
	ageLocalAd(t, node, "art-abc", (catalog.StaleAdTimeoutSeconds+30)*time.Second)

	// The loop refreshes once on entry, which is what covers the window
	// between start-up and the first tick.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		node.localSrv.catalogLoop(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		ad, ok := node.localSrv.catalog.Get(node.memberID, "art-abc")
		if ok && ad.Availability == "ready" {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the catalog loop never refreshed this node's own advertisement")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	<-done

	// The refresh interval has to stay inside the freshness window, or a model
	// goes offline between ticks.
	if catalogRefreshInterval >= catalog.StaleAdTimeoutSeconds*time.Second {
		t.Fatalf("refresh interval %s does not fit inside the %ds freshness window",
			catalogRefreshInterval, catalog.StaleAdTimeoutSeconds)
	}
}

// TestManagedModelIsWithdrawnWhenTheRunnerDropsIt is the other half of keeping
// advertisements honest: the hosted_models row outlives a runner restart, so a
// refresh that trusted the row alone would advertise weights nothing holds.
func TestManagedModelIsWithdrawnWhenTheRunnerDropsIt(t *testing.T) {
	vNet := transport.NewMemoryNetwork()
	node := setupTestNode(t, vNet, "node-1", 4242)
	node.login(t)
	node.hostRoom(t, "Host", "Room")

	loaded := ""
	runnerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/runner/v1/models/load" {
			var body struct {
				ModelID string `json:"model_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			loaded = body.ModelID
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "loaded_model_id": loaded})
	}))
	defer runnerSrv.Close()
	node.localSrv.runnerClient = hosting.NewRunnerClient(runnerSrv.URL, "")

	if w := node.doJSON("POST", "/api/v1/managed-models/load", map[string]any{
		"model_id": "art-abc", "name": "Qwen", "filename": "m.gguf", "context_limit": 8192,
	}); w.Code != http.StatusOK {
		t.Fatalf("load model: %d %s", w.Code, w.Body.String())
	}
	if ad, _ := node.localSrv.catalog.Get(node.memberID, "art-abc"); ad.Availability != "ready" {
		t.Fatalf("a loaded model reads as %q", ad.Availability)
	}

	// The runner restarts and comes back empty, exactly as it does when its
	// container is replaced.
	loaded = ""
	node.localSrv.advertiseLocalModels()

	ad, ok := node.localSrv.catalog.Get(node.memberID, "art-abc")
	if !ok {
		t.Fatal("the advertisement disappeared entirely")
	}
	if ad.Availability != "offline" {
		t.Fatalf("a model the runner no longer holds reads as %q", ad.Availability)
	}
}
