package hosting

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseRepoRefAcceptsWhatPeoplePaste(t *testing.T) {
	const want = "NANI-Nithin/K2-Horizon-7B-GGUF"
	for _, in := range []string{
		"https://huggingface.co/NANI-Nithin/K2-Horizon-7B-GGUF",
		"https://huggingface.co/NANI-Nithin/K2-Horizon-7B-GGUF/",
		"https://huggingface.co/NANI-Nithin/K2-Horizon-7B-GGUF/tree/main",
		"https://huggingface.co/NANI-Nithin/K2-Horizon-7B-GGUF/resolve/main/K2-Horizon-7B-Q4_K_M.gguf",
		"huggingface.co/NANI-Nithin/K2-Horizon-7B-GGUF",
		"NANI-Nithin/K2-Horizon-7B-GGUF",
		"  NANI-Nithin/K2-Horizon-7B-GGUF  ",
		"https://huggingface.co/NANI-Nithin/K2-Horizon-7B-GGUF?library=true",
	} {
		got, err := ParseRepoRef(in)
		if err != nil {
			t.Fatalf("ParseRepoRef(%q) returned %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseRepoRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRepoRefRejectsAnythingElse(t *testing.T) {
	// The reference is interpolated into a URL this process then fetches, so
	// anything that is not plainly a repository is refused.
	for _, in := range []string{
		"", "   ", "single", "/", "//",
		"https://evil.example.com/org/repo",
		"ftp://huggingface.co/org/repo",
		"../../etc/passwd",
		"org/../../etc",
		"org/repo\nHost: evil",
		"org name/repo",
		"-leading/repo",
	} {
		if got, err := ParseRepoRef(in); err == nil {
			t.Fatalf("ParseRepoRef(%q) = %q, want an error", in, got)
		}
	}
}

// newRepoServer serves a Hugging Face style tree listing.
func newRepoServer(t *testing.T, entries []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/org/Model-GGUF/tree/main" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveHuggingFaceRepoListsRealFiles(t *testing.T) {
	// The shape that used to 404: a repo named "<model>-GGUF" whose files are
	// named "<model>-<quant>.gguf", published in many quantizations.
	srv := newRepoServer(t, []map[string]any{
		{"type": "file", "path": ".gitattributes", "size": 3355},
		{"type": "file", "path": "README.md", "size": 1200},
		{"type": "file", "path": "Model-Q4_K_M.gguf", "size": 5 << 30},
		{"type": "file", "path": "Model-Q2_K.gguf", "size": 3 << 30},
		{"type": "file", "path": "Model-BF16.gguf", "size": 16 << 30},
		{"type": "directory", "path": "UD-IQ4_XS"},
	})

	mgr, err := NewArtifactManager(t.TempDir(), 50<<30)
	if err != nil {
		t.Fatal(err)
	}
	mgr.httpClient = srv.Client()
	mgr.hfAPIBase = srv.URL

	weights, err := mgr.ResolveHuggingFaceRepo(context.Background(), "https://huggingface.co/org/Model-GGUF")
	if err != nil {
		t.Fatalf("ResolveHuggingFaceRepo returned %v", err)
	}
	if len(weights) != 3 {
		t.Fatalf("listed %d weights, want the 3 gguf files", len(weights))
	}

	// Smallest first, so the list opens on something that fits.
	if weights[0].Filename != "Model-Q2_K.gguf" {
		t.Fatalf("first entry is %q", weights[0].Filename)
	}
	if weights[0].Quantization != "Q2_K" {
		t.Fatalf("quantization = %q", weights[0].Quantization)
	}
	if weights[0].URL != "https://huggingface.co/org/Model-GGUF/resolve/main/Model-Q2_K.gguf" {
		t.Fatalf("download URL = %q", weights[0].URL)
	}
	// The guess this replaced asked for "Model-GGUF-Q4_K_M.gguf".
	for _, w := range weights {
		if w.Filename == "Model-GGUF-Q4_K_M.gguf" {
			t.Fatal("listing invented a filename instead of reading one")
		}
	}
}

func TestResolveHuggingFaceRepoCollapsesSplitModels(t *testing.T) {
	srv := newRepoServer(t, []map[string]any{
		{"type": "file", "path": "UD-IQ4_XS/Model-UD-IQ4_XS-00001-of-00003.gguf", "size": 10},
		{"type": "file", "path": "UD-IQ4_XS/Model-UD-IQ4_XS-00002-of-00003.gguf", "size": 20},
		{"type": "file", "path": "UD-IQ4_XS/Model-UD-IQ4_XS-00003-of-00003.gguf", "size": 30},
	})

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
	if len(weights) != 1 {
		t.Fatalf("listed %d entries, want one per model", len(weights))
	}
	w := weights[0]
	if w.ShardCount != 3 {
		t.Fatalf("shard count = %d, want 3", w.ShardCount)
	}
	if w.SizeBytes != 60 {
		t.Fatalf("size = %d, want every shard summed (60)", w.SizeBytes)
	}
	if w.Path != "UD-IQ4_XS/Model-UD-IQ4_XS-00001-of-00003.gguf" {
		t.Fatalf("path = %q, want the first shard", w.Path)
	}
	if w.URL != "https://huggingface.co/org/Model-GGUF/resolve/main/UD-IQ4_XS/Model-UD-IQ4_XS-00001-of-00003.gguf" {
		t.Fatalf("download URL = %q", w.URL)
	}
}

func TestResolveHuggingFaceRepoReportsMissingRepos(t *testing.T) {
	srv := newRepoServer(t, nil)
	mgr, err := NewArtifactManager(t.TempDir(), 50<<30)
	if err != nil {
		t.Fatal(err)
	}
	mgr.httpClient = srv.Client()
	mgr.hfAPIBase = srv.URL

	if _, err := mgr.ResolveHuggingFaceRepo(context.Background(), "org/Missing"); !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("missing repository returned %v, want ErrRepoNotFound", err)
	}
}
