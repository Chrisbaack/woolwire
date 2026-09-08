package hosting

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// gemmaRepoEntries mirrors unsloth/gemma-4-12b-it-GGUF: many quantizations,
// three projector precisions, and MTP modules both at the top level and in a
// subdirectory.
func gemmaRepoEntries() []map[string]any {
	return []map[string]any{
		{"type": "file", "path": "README.md", "size": 1000},
		{"type": "file", "path": "Model-Q4_K_M.gguf", "size": 7 << 30},
		{"type": "file", "path": "Model-Q8_0.gguf", "size": 12 << 30},
		{"type": "file", "path": "Model-UD-Q4_K_XL.gguf", "size": 7 << 30},
		{"type": "file", "path": "mmproj-F32.gguf", "size": 214 << 20},
		{"type": "file", "path": "mmproj-F16.gguf", "size": 171 << 20},
		{"type": "file", "path": "mmproj-BF16.gguf", "size": 171 << 20},
		{"type": "file", "path": "mtp-Model.gguf", "size": 460 << 20},
		{"type": "file", "path": "MTP/mtp-Model-Q8_0.gguf", "size": 460 << 20},
		{"type": "file", "path": "MTP/mtp-Model-F16.gguf", "size": 860 << 20},
		{"type": "file", "path": "MTP/mtp-Model-BF16.gguf", "size": 860 << 20},
	}
}

// Choosing a quantization should not also mean choosing a projector precision
// and an MTP variant: those follow from the model.
func TestResolveAttachesCompanionsToEachQuant(t *testing.T) {
	srv := newRepoServer(t, gemmaRepoEntries())
	mgr, err := NewArtifactManager(t.TempDir(), 50<<30)
	if err != nil {
		t.Fatal(err)
	}
	mgr.httpClient = srv.Client()
	mgr.hfAPIBase = srv.URL

	weights, err := mgr.ResolveHuggingFaceRepo(context.Background(), "org/Model-GGUF")
	if err != nil {
		t.Fatal(err)
	}

	// The projectors and MTP modules are not models, so they are not offered
	// as ones.
	if len(weights) != 3 {
		names := make([]string, len(weights))
		for i, w := range weights {
			names[i] = w.Filename
		}
		t.Fatalf("offered %v, want only the three quantizations", names)
	}

	for _, w := range weights {
		if len(w.Companions) != 2 {
			t.Fatalf("%s carries %d companions, want a projector and a draft module", w.Filename, len(w.Companions))
		}
		byKind := map[CompanionKind]RepoCompanion{}
		for _, c := range w.Companions {
			byKind[c.Kind] = c
		}
		// F16 is the conventional projector, and the same size as BF16 here.
		if got := byKind[CompanionProjector].Filename; got != "mmproj-F16.gguf" {
			t.Fatalf("%s took projector %q, want mmproj-F16.gguf", w.Filename, got)
		}
		// A draft module only buys speed, so the cheapest precision wins.
		if got := byKind[CompanionDraft].Filename; got != "mtp-Model-Q8_0.gguf" && got != "mtp-Model.gguf" {
			t.Fatalf("%s took draft %q", w.Filename, got)
		}
		if w.TotalBytes <= w.SizeBytes {
			t.Fatalf("%s reports %d total against %d for the weights alone", w.Filename, w.TotalBytes, w.SizeBytes)
		}
	}

	// An exact quantization match wins when one exists.
	var q8 RepoWeight
	for _, w := range weights {
		if w.Filename == "Model-Q8_0.gguf" {
			q8 = w
		}
	}
	for _, c := range q8.Companions {
		if c.Kind == CompanionDraft && c.Path != "MTP/mtp-Model-Q8_0.gguf" {
			t.Fatalf("Q8_0 weights took draft %q, want the Q8_0 module", c.Path)
		}
	}
}

func TestClassifyCompanion(t *testing.T) {
	cases := map[string]CompanionKind{
		"mmproj-F16.gguf":                     CompanionProjector,
		"Qwen3-27B-mmproj-F16.gguf":           CompanionProjector,
		"mtp-gemma-4-12b-it.gguf":             CompanionDraft,
		"MTP/mtp-Model-Q8_0.gguf":             CompanionDraft,
		"MTP/anything.gguf":                   CompanionDraft,
		"gemma-4-12b-it-Q4_K_M.gguf":          "",
		"hub/snap/Qwen3.8-27B-UD-Q4_K_S.gguf": "",
		// A model whose name merely mentions the words is still a model.
		"my-mtptuned-model-Q4_K_M.gguf": "",
	}
	for in, want := range cases {
		if got := classifyCompanion(in); got != want {
			t.Fatalf("classifyCompanion(%q) = %q, want %q", in, got, want)
		}
	}
}

// The cache scanner has the same job: a projector or MTP module already on
// disk belongs to a model rather than being one.
func TestDiscoveryAttachesCompanionsAndHidesThem(t *testing.T) {
	root := t.TempDir()
	const repo = "hub/models--unsloth--gemma-4-12b-it-GGUF"

	writeCachedModel(t, root, repo, "rev1", "gemma-4-12b-it-Q4_K_M.gguf", "blob-q4",
		languageModelKVs("gemma4", "Gemma 4 12B", 8192)...)
	writeCachedModel(t, root, repo, "rev1", "mmproj-F16.gguf", "blob-mmproj",
		kvString("general.architecture", "clip"))
	writeCachedModel(t, root, repo, "rev1", "mmproj-F32.gguf", "blob-mmproj32",
		kvString("general.architecture", "clip"))
	// An MTP module carries a tokenizer, so nothing but its name and location
	// separates it from a model.
	writeCachedModel(t, root, repo, "rev1", "mtp-gemma-4-12b-it.gguf", "blob-mtp",
		languageModelKVs("gemma4", "Gemma 4 MTP", 8192)...)

	mgr, err := NewArtifactManager(root, 50<<30)
	if err != nil {
		t.Fatal(err)
	}
	list, err := mgr.ListArtifacts()
	if err != nil {
		t.Fatal(err)
	}

	if len(list) != 1 {
		t.Fatalf("listed %v, want only the model", manifestNames(list))
	}
	model := list[0]
	if len(model.Companions) != 2 {
		t.Fatalf("model carries %d companions, want the projector and the MTP module", len(model.Companions))
	}

	byKind := map[CompanionKind]ArtifactCompanion{}
	for _, c := range model.Companions {
		byKind[c.Kind] = c
	}
	if got := byKind[CompanionProjector].Filename; got != "mmproj-F16.gguf" {
		t.Fatalf("chose projector %q, want mmproj-F16.gguf", got)
	}
	if got := byKind[CompanionDraft].Filename; got != "mtp-gemma-4-12b-it.gguf" {
		t.Fatalf("chose draft %q", got)
	}

	// And a load can find them without anyone naming one.
	companions := mgr.CompanionsFor(model.Path)
	if len(companions) != 2 {
		t.Fatalf("CompanionsFor returned %d entries", len(companions))
	}
	for _, c := range companions {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(c.Path))); err != nil {
			t.Fatalf("companion %q does not resolve: %v", c.Path, err)
		}
	}
}

// TestDownloadSetInstallsCompanionsWithoutListingThem covers the download
// side: the projector and draft module arrive with the model, as one job, and
// none of them shows up as something to load.
func TestDownloadSetInstallsCompanionsWithoutListingThem(t *testing.T) {
	files := map[string][]byte{
		"/Model-Q4_K_M.gguf": nil,
		"/mmproj-F16.gguf":   nil,
		"/mtp-Model.gguf":    nil,
	}
	// Real GGUF bodies, so the scanner has to rely on the naming rather than
	// on the files being unreadable.
	dir := t.TempDir()
	for name := range files {
		p := filepath.Join(dir, filepath.Base(name))
		writeGGUF(t, p, languageModelKVs("gemma4", "Gemma", 8192)...)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = b
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	modelsDir := t.TempDir()
	mgr, err := NewArtifactManager(modelsDir, 50<<30)
	if err != nil {
		t.Fatal(err)
	}
	mgr.httpClient = srv.Client()

	id, err := mgr.StartDownloadSet(DownloadRequest{
		SourceURL: srv.URL + "/Model-Q4_K_M.gguf",
		Filename:  "Model-Q4_K_M.gguf",
		Companions: []CompanionRequest{
			{Kind: CompanionProjector, SourceURL: srv.URL + "/mmproj-F16.gguf", Filename: "mmproj-F16.gguf"},
			{Kind: CompanionDraft, SourceURL: srv.URL + "/mtp-Model.gguf", Filename: "mtp-Model.gguf"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		states, err := mgr.DownloadStatus(id)
		if err != nil {
			t.Fatal(err)
		}
		if states[0].Status == "complete" {
			break
		}
		if states[0].Status == "failed" {
			t.Fatalf("download failed: %s", states[0].Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("download stuck in %q", states[0].Status)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Every file arrived.
	for _, name := range []string{"Model-Q4_K_M.gguf", "mmproj-F16.gguf", "mtp-Model.gguf"} {
		if _, err := os.Stat(filepath.Join(modelsDir, name)); err != nil {
			t.Fatalf("%s was not installed: %v", name, err)
		}
	}

	list, err := mgr.ListArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %v, want only the model", manifestNames(list))
	}
	if len(list[0].Companions) != 2 {
		t.Fatalf("the model records %d companions, want 2", len(list[0].Companions))
	}

	companions := mgr.CompanionsFor("Model-Q4_K_M.gguf")
	if len(companions) != 2 {
		t.Fatalf("CompanionsFor returned %d entries", len(companions))
	}
	for _, c := range companions {
		if c.Path == "" {
			t.Fatalf("companion %q has no path to load from", c.Filename)
		}
	}
}
