package hosting

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testCommit = "fc034cfff751157913579611efad8462ac1be606"

// sha256Of is the hash a Hugging Face cache names a blob by: for the LFS files
// a GGUF repository is made of, the ETag is the file's SHA-256.
func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeLegacyManifest writes the sidecar manifest an older Woolwire left
// beside a downloaded file.
func writeLegacyManifest(t *testing.T, modelsDir string, mf ArtifactManifest) {
	t.Helper()
	b, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelsDir, mf.Filename+".manifest.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ggufBytes is a GGUF file as a byte slice, so a test server can serve one.
func ggufBytes(kvs ...ggufKV) []byte {
	var buf bytes.Buffer
	buf.WriteString("GGUF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(3))
	_ = binary.Write(&buf, binary.LittleEndian, uint64(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint64(len(kvs)))
	for _, kv := range kvs {
		_ = binary.Write(&buf, binary.LittleEndian, uint64(len(kv.key)))
		buf.WriteString(kv.key)
		_ = binary.Write(&buf, binary.LittleEndian, kv.typ)
		writeGGUFValue(&buf, kv.typ, kv.val)
	}
	return buf.Bytes()
}

// newHFServer stands in for Hugging Face: it serves a repository's files from
// "resolve" URLs and answers a HEAD with the commit and the LFS hash, which is
// where a real one puts them.
func newHFServer(t *testing.T, files map[string][]byte, commit string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, "/org/Model-GGUF/resolve/main/")
		body, found := files[rest]
		if !ok || !found {
			http.NotFound(w, r)
			return
		}
		if commit != "" {
			w.Header().Set("X-Repo-Commit", commit)
			w.Header().Set("X-Linked-Etag", `"`+sha256Of(body)+`"`)
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newHFManager points a manager at a stand-in for Hugging Face, so a download
// URL from it is recognized as a repository file.
func newHFManager(t *testing.T, srv *httptest.Server) (*ArtifactManager, string) {
	t.Helper()
	modelsDir := t.TempDir()
	mgr, err := NewArtifactManager(modelsDir, 50<<30)
	if err != nil {
		t.Fatal(err)
	}
	mgr.httpClient = srv.Client()
	mgr.hfAPIBase = srv.URL
	return mgr, modelsDir
}

// awaitDownload blocks until a download job settles.
func awaitDownload(t *testing.T, mgr *ArtifactManager, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		states, err := mgr.DownloadStatus(id)
		if err != nil {
			t.Fatal(err)
		}
		switch states[0].Status {
		case "complete":
			return
		case "failed", "cancelled":
			t.Fatalf("download %s: %s", states[0].Status, states[0].Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("download stuck in %q", states[0].Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestParseHFSourceRecognizesRepositoryFiles(t *testing.T) {
	cases := []struct {
		url  string
		want hfSource
		ok   bool
	}{
		{
			url:  "https://huggingface.co/unsloth/gemma-4-12b-it-GGUF/resolve/main/gemma-4-12b-it-Q8_0.gguf",
			want: hfSource{Repo: "unsloth/gemma-4-12b-it-GGUF", Revision: "main", Path: "gemma-4-12b-it-Q8_0.gguf"},
			ok:   true,
		},
		{
			// A companion in a subdirectory keeps its subdirectory.
			url:  "https://huggingface.co/unsloth/gemma-4-12b-it-GGUF/resolve/main/MTP/mtp-Q8_0.gguf",
			want: hfSource{Repo: "unsloth/gemma-4-12b-it-GGUF", Revision: "main", Path: "MTP/mtp-Q8_0.gguf"},
			ok:   true,
		},
		{
			// Escaped separators are decoded before the path is trusted.
			url: "https://huggingface.co/org/repo/resolve/main/%2E%2E%2Fescape.gguf",
			ok:  false,
		},
		// A CDN link carries no repository, and neither does a mirror.
		{url: "https://cdn-lfs.huggingface.co/repos/ab/cd/model.gguf", ok: false},
		{url: "https://example.com/org/repo/resolve/main/model.gguf", ok: false},
		{url: "https://huggingface.co/org/repo/blob/main/model.gguf", ok: false},
		{url: "http://huggingface.co/org/repo/resolve/main/model.gguf", ok: false},
	}

	for _, tc := range cases {
		got, ok := parseHFSource(tc.url, "huggingface.co")
		if ok != tc.ok {
			t.Fatalf("%s: recognized=%v, want %v", tc.url, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Fatalf("%s: parsed %+v, want %+v", tc.url, got, tc.want)
		}
	}
}

// A download has to land where the rest of the machine looks for models, not
// in a flat directory only Woolwire reads.
func TestDownloadWritesHuggingFaceCacheLayout(t *testing.T) {
	model := ggufBytes(languageModelKVs("llama", "Model", 8192)...)
	projector := ggufBytes(kvString("general.architecture", "clip"))
	srv := newHFServer(t, map[string][]byte{
		"Model-Q4_K_M.gguf": model,
		"mmproj-F16.gguf":   projector,
	}, testCommit)
	mgr, modelsDir := newHFManager(t, srv)

	id, err := mgr.StartDownloadSet(DownloadRequest{
		SourceURL: srv.URL + "/org/Model-GGUF/resolve/main/Model-Q4_K_M.gguf",
		Filename:  "Model-Q4_K_M.gguf",
		Companions: []CompanionRequest{{
			Kind:      CompanionProjector,
			SourceURL: srv.URL + "/org/Model-GGUF/resolve/main/mmproj-F16.gguf",
			Filename:  "mmproj-F16.gguf",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitDownload(t, mgr, id)

	repoDir := filepath.Join(modelsDir, "hub", "models--org--Model-GGUF")
	snapshot := filepath.Join(repoDir, "snapshots", testCommit)

	// The bytes are a blob named by the file's hash, and the snapshot entry
	// naming them is a symlink — the layout huggingface_hub writes.
	for name, want := range map[string][]byte{"Model-Q4_K_M.gguf": model, "mmproj-F16.gguf": projector} {
		link := filepath.Join(snapshot, name)
		info, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("%s is not in the snapshot: %v", name, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is a %s, want a symlink into blobs", name, info.Mode())
		}
		blob, err := filepath.EvalSymlinks(link)
		if err != nil {
			t.Fatalf("%s dangles: %v", name, err)
		}
		if filepath.Dir(blob) != filepath.Join(repoDir, "blobs") {
			t.Fatalf("%s points at %s, want a blob", name, blob)
		}
		if filepath.Base(blob) != sha256Of(want) {
			t.Fatalf("%s blob is named %s, want the file hash", name, filepath.Base(blob))
		}
		if got, err := os.ReadFile(link); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s does not read back: %v", name, err)
		}
	}

	// A tool asking for "main" finds the snapshot through the ref.
	ref, err := os.ReadFile(filepath.Join(repoDir, "refs", "main"))
	if err != nil || string(ref) != testCommit {
		t.Fatalf("refs/main is %q (%v), want the commit", ref, err)
	}

	// Nothing was left in the models directory itself.
	if _, err := os.Stat(filepath.Join(modelsDir, "Model-Q4_K_M.gguf")); err == nil {
		t.Fatal("the model was also dumped flat in the models directory")
	}

	list, err := mgr.ListArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %v, want only the model", manifestNames(list))
	}
	wantPath := "hub/models--org--Model-GGUF/snapshots/" + testCommit + "/Model-Q4_K_M.gguf"
	if list[0].Path != wantPath {
		t.Fatalf("model path is %q, want %q", list[0].Path, wantPath)
	}
	if list[0].RepoID != "org/Model-GGUF" {
		t.Fatalf("model repo is %q, want org/Model-GGUF", list[0].RepoID)
	}

	// The companion has to be loadable by the path it actually sits at.
	companions := mgr.CompanionsFor(wantPath)
	if len(companions) != 1 {
		t.Fatalf("CompanionsFor returned %d entries, want the projector", len(companions))
	}
	if _, err := os.Stat(filepath.Join(modelsDir, filepath.FromSlash(companions[0].Path))); err != nil {
		t.Fatalf("recorded projector path %q does not exist: %v", companions[0].Path, err)
	}
}

// A repository that will not say which commit served a file leaves nowhere to
// file it. That is a reason to skip the layout, not to fail the download.
func TestDownloadFallsBackToFlatWithoutACommit(t *testing.T) {
	model := ggufBytes(languageModelKVs("llama", "Model", 8192)...)
	srv := newHFServer(t, map[string][]byte{"Model-Q4_K_M.gguf": model}, "")
	mgr, modelsDir := newHFManager(t, srv)

	id, err := mgr.StartDownload(srv.URL+"/org/Model-GGUF/resolve/main/Model-Q4_K_M.gguf", "Model-Q4_K_M.gguf", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	awaitDownload(t, mgr, id)

	if _, err := os.Stat(filepath.Join(modelsDir, "Model-Q4_K_M.gguf")); err != nil {
		t.Fatalf("the model was not installed at all: %v", err)
	}
	list, err := mgr.ListArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Path != "Model-Q4_K_M.gguf" {
		t.Fatalf("listed %v, want the flat file", manifestNames(list))
	}
}

// installOne downloads a single model into a fresh cache and returns the
// manager, the models directory, and the path the model was filed at.
func installOne(t *testing.T, body []byte) (*ArtifactManager, string, string) {
	t.Helper()
	srv := newHFServer(t, map[string][]byte{"Model-Q4_K_M.gguf": body}, testCommit)
	mgr, modelsDir := newHFManager(t, srv)

	id, err := mgr.StartDownload(srv.URL+"/org/Model-GGUF/resolve/main/Model-Q4_K_M.gguf", "Model-Q4_K_M.gguf", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	awaitDownload(t, mgr, id)
	return mgr, modelsDir, "hub/models--org--Model-GGUF/snapshots/" + testCommit + "/Model-Q4_K_M.gguf"
}

// Deleting a model Woolwire installed has to take the blob with it. Removing
// the snapshot entry alone leaves the whole file on the disk, unreachable and
// uncounted.
func TestDeleteRemovesTheSnapshotEntryAndItsBlob(t *testing.T) {
	model := ggufBytes(languageModelKVs("llama", "Model", 8192)...)
	mgr, modelsDir, rel := installOne(t, model)
	repoDir := filepath.Join(modelsDir, "hub", "models--org--Model-GGUF")

	if err := mgr.DeleteArtifact(rel); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(modelsDir, filepath.FromSlash(rel))); err == nil {
		t.Fatal("the snapshot entry survived the delete")
	}
	if _, err := os.Stat(filepath.Join(repoDir, "blobs", sha256Of(model))); err == nil {
		t.Fatal("the blob outlived every reference to it")
	}
	// The directories the removal emptied go too, so a repository does not
	// accumulate empty snapshots.
	if _, err := os.Stat(filepath.Join(repoDir, "snapshots", testCommit)); err == nil {
		t.Fatal("the emptied snapshot directory was left behind")
	}
	if list, err := mgr.ListArtifacts(); err != nil || len(list) != 0 {
		t.Fatalf("listed %v after the delete, want nothing", manifestNames(list))
	}
}

// A cache is shared. A blob some other revision still names is not Woolwire's
// to remove, however much of it Woolwire downloaded.
func TestDeleteKeepsABlobAnotherRevisionReferences(t *testing.T) {
	model := ggufBytes(languageModelKVs("llama", "Model", 8192)...)
	mgr, modelsDir, rel := installOne(t, model)
	repoDir := filepath.Join(modelsDir, "hub", "models--org--Model-GGUF")
	blob := filepath.Join(repoDir, "blobs", sha256Of(model))

	other := filepath.Join(repoDir, "snapshots", "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0", "Model-Q4_K_M.gguf")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "blobs", sha256Of(model)), other); err != nil {
		t.Fatal(err)
	}

	if err := mgr.DeleteArtifact(rel); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(blob); err != nil {
		t.Fatalf("the blob went with the entry another revision still names: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("the other revision was damaged by the delete: %v", err)
	}
}

// Weights an earlier Woolwire dumped in the models directory have to end up
// where the rest of the machine looks, without being downloaded again.
func TestMigrateFlatDownloadsFilesThemInTheCache(t *testing.T) {
	model := ggufBytes(languageModelKVs("llama", "Model", 8192)...)
	projector := ggufBytes(kvString("general.architecture", "clip"))
	srv := newHFServer(t, map[string][]byte{
		"Model-Q4_K_M.gguf": model,
		"mmproj-F16.gguf":   projector,
	}, testCommit)
	mgr, modelsDir := newHFManager(t, srv)
	base := srv.URL + "/org/Model-GGUF/resolve/main/"

	// The state an older version left behind: bare files at the top of the
	// models directory, each with a sidecar manifest beside it.
	for _, f := range []struct {
		name string
		body []byte
		mf   ArtifactManifest
	}{
		{"Model-Q4_K_M.gguf", model, ArtifactManifest{
			ID: "art-Model-Q4_K_M.gguf", Name: "Model-Q4_K_M.gguf", Filename: "Model-Q4_K_M.gguf",
			Path: "Model-Q4_K_M.gguf", Source: SourceDownload, SHA256: sha256Of(model),
			SourceURL:  base + "Model-Q4_K_M.gguf",
			Companions: []ArtifactCompanion{{Kind: CompanionProjector, Filename: "mmproj-F16.gguf"}},
		}},
		{"mmproj-F16.gguf", projector, ArtifactManifest{
			ID: "art-mmproj-F16.gguf", Name: "mmproj-F16.gguf", Filename: "mmproj-F16.gguf",
			Path: "mmproj-F16.gguf", Source: SourceDownload, Role: RoleCompanion,
			SHA256: sha256Of(projector), SourceURL: base + "mmproj-F16.gguf",
		}},
	} {
		if err := os.WriteFile(filepath.Join(modelsDir, f.name), f.body, 0o600); err != nil {
			t.Fatal(err)
		}
		writeLegacyManifest(t, modelsDir, f.mf)
	}

	moved, err := mgr.MigrateFlatDownloads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if moved != 2 {
		t.Fatalf("migrated %d files, want the model and its projector", moved)
	}

	snapshot := filepath.Join(modelsDir, "hub", "models--org--Model-GGUF", "snapshots", testCommit)
	for _, name := range []string{"Model-Q4_K_M.gguf", "mmproj-F16.gguf"} {
		if _, err := os.Stat(filepath.Join(snapshot, name)); err != nil {
			t.Fatalf("%s did not reach the cache: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(modelsDir, name)); err == nil {
			t.Fatalf("%s was left behind in the models directory", name)
		}
		if _, err := os.Stat(filepath.Join(modelsDir, name+".manifest.json")); err == nil {
			t.Fatalf("the sidecar manifest for %s was left behind", name)
		}
	}

	list, err := mgr.ListArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %v, want only the model", manifestNames(list))
	}
	// The identifier is what any hosted-model row already refers to.
	if list[0].ID != "art-Model-Q4_K_M.gguf" {
		t.Fatalf("the model was renamed to %q by the move", list[0].ID)
	}
	wantPath := "hub/models--org--Model-GGUF/snapshots/" + testCommit + "/Model-Q4_K_M.gguf"
	if list[0].Path != wantPath {
		t.Fatalf("model path is %q, want %q", list[0].Path, wantPath)
	}
	companions := mgr.CompanionsFor(wantPath)
	if len(companions) != 1 || companions[0].Path != filepath.ToSlash(filepath.Join("hub/models--org--Model-GGUF/snapshots", testCommit, "mmproj-F16.gguf")) {
		t.Fatalf("the projector is recorded at %+v, want its path in the cache", companions)
	}

	// Running it again has nothing to do and must not undo anything.
	if moved, err := mgr.MigrateFlatDownloads(context.Background()); err != nil || moved != 0 {
		t.Fatalf("a second migration moved %d files (%v), want none", moved, err)
	}
}
