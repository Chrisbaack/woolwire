package hosting

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCachedModel reproduces how a Hugging Face cache stores one file: the
// bytes live in the repo's blobs directory under a content hash, and every
// snapshot that references them holds a relative symlink.
func writeCachedModel(t *testing.T, root, repoDir, revision, name, blobName string, kvs ...ggufKV) string {
	t.Helper()

	blobDir := filepath.Join(root, repoDir, "blobs")
	snapshotDir := filepath.Join(root, repoDir, "snapshots", revision)
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatal(err)
	}

	blob := filepath.Join(blobDir, blobName)
	if _, err := os.Stat(blob); err != nil {
		writeGGUF(t, blob, kvs...)
	}

	link := filepath.Join(snapshotDir, name)
	target, err := filepath.Rel(snapshotDir, blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// newCacheFixture builds a models directory shaped like a real Hugging Face
// cache: one repo with several quantizations, a second revision pointing at
// the same weights, a split model, a vision projector, and files belonging to
// another tool entirely.
func newCacheFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	const repo = "hub/models--unsloth--Qwen3-8B-GGUF"

	writeCachedModel(t, root, repo, "rev1", "Qwen3-8B-UD-Q4_K_S.gguf", "blob-q4",
		languageModelKVs("qwen3", "Qwen3 8B", 40960)...)
	writeCachedModel(t, root, repo, "rev1", "Qwen3-8B-UD-IQ3_XXS.gguf", "blob-iq3",
		languageModelKVs("qwen3", "Qwen3 8B", 40960)...)

	// A newer revision of the same repo links to weights already seen. The
	// same model must not be listed twice.
	writeCachedModel(t, root, repo, "rev2", "Qwen3-8B-UD-Q4_K_S.gguf", "blob-q4")

	// A vision projector sits beside the weights it belongs to and is not a
	// model anyone can load on its own.
	writeCachedModel(t, root, repo, "rev1", "mmproj-F16.gguf", "blob-mmproj",
		kvString("general.architecture", "clip"),
		kvU32("clip.vision.block_count", 24))

	// A split model: only the first piece is loadable, and the size shown
	// should be the whole model.
	const bigRepo = "hub/models--unsloth--Qwen3-Next-GGUF"
	writeCachedModel(t, root, bigRepo, "rev1", "Qwen3-Next-UD-IQ4_XS-00001-of-00003.gguf", "blob-split-1",
		languageModelKVs("qwen3next", "Qwen3 Next", 8192)...)
	writeCachedModel(t, root, bigRepo, "rev1", "Qwen3-Next-UD-IQ4_XS-00002-of-00003.gguf", "blob-split-2",
		languageModelKVs("qwen3next", "Qwen3 Next", 8192)...)
	writeCachedModel(t, root, bigRepo, "rev1", "Qwen3-Next-UD-IQ4_XS-00003-of-00003.gguf", "blob-split-3",
		languageModelKVs("qwen3next", "Qwen3 Next", 8192)...)

	// Another tool's diffusion weights, in the same container format.
	writeGGUF(t, filepath.Join(root, "comfyui", "shape", "flow_dit_Q8_0.gguf"),
		kvString("general.architecture", "flux"),
		kvU32("flux.in_channels", 64))

	return root
}

func manifestByName(t *testing.T, list []ArtifactManifest, name string) ArtifactManifest {
	t.Helper()
	for _, mf := range list {
		if mf.Name == name {
			return mf
		}
	}
	t.Fatalf("model %q not listed; got %v", name, manifestNames(list))
	return ArtifactManifest{}
}

func manifestNames(list []ArtifactManifest) []string {
	names := make([]string, 0, len(list))
	for _, mf := range list {
		names = append(names, mf.Name)
	}
	return names
}

func TestListArtifactsDiscoversHuggingFaceCache(t *testing.T) {
	root := newCacheFixture(t)
	mgr, err := NewArtifactManager(root, 50<<30)
	if err != nil {
		t.Fatal(err)
	}

	list, err := mgr.ListArtifacts()
	if err != nil {
		t.Fatalf("ListArtifacts returned %v", err)
	}

	want := []string{"Qwen3-8B-UD-IQ3_XXS", "Qwen3-8B-UD-Q4_K_S", "Qwen3-Next-UD-IQ4_XS"}
	if got := manifestNames(list); len(got) != len(want) {
		t.Fatalf("listed %v, want exactly %v", got, want)
	}
	for _, name := range want {
		manifestByName(t, list, name)
	}

	q4 := manifestByName(t, list, "Qwen3-8B-UD-Q4_K_S")
	if q4.RepoID != "unsloth/Qwen3-8B-GGUF" {
		t.Fatalf("repo id = %q", q4.RepoID)
	}
	if q4.Source != SourceCache {
		t.Fatalf("source = %q, want %q", q4.Source, SourceCache)
	}
	if q4.Architecture != "qwen3" {
		t.Fatalf("architecture = %q", q4.Architecture)
	}
	if !strings.HasPrefix(q4.Path, "hub/models--unsloth--Qwen3-8B-GGUF/snapshots/") {
		t.Fatalf("path = %q, want the cache path", q4.Path)
	}
	if q4.Filename != "Qwen3-8B-UD-Q4_K_S.gguf" {
		t.Fatalf("filename = %q", q4.Filename)
	}
	// A model trained for 40960 tokens is advertised at the safe default
	// rather than at a context that would size the KV cache to match.
	if q4.ContextLimit != 8192 {
		t.Fatalf("context limit = %d, want 8192", q4.ContextLimit)
	}
	// IDs travel in URL path segments, so a nested path must not leak into one.
	if strings.Contains(q4.ID, "/") {
		t.Fatalf("id %q contains a separator", q4.ID)
	}
}

func TestDiscoverySumsSplitShards(t *testing.T) {
	root := newCacheFixture(t)
	mgr, err := NewArtifactManager(root, 50<<30)
	if err != nil {
		t.Fatal(err)
	}
	list, err := mgr.ListArtifacts()
	if err != nil {
		t.Fatal(err)
	}

	split := manifestByName(t, list, "Qwen3-Next-UD-IQ4_XS")
	if !strings.HasSuffix(split.Path, "-00001-of-00003.gguf") {
		t.Fatalf("split model resolved to %q, want the first shard", split.Path)
	}

	var shardTotal int64
	for i := 1; i <= 3; i++ {
		name := filepath.Join(root, "hub", "models--unsloth--Qwen3-Next-GGUF", "snapshots", "rev1",
			[]string{"", "Qwen3-Next-UD-IQ4_XS-00001-of-00003.gguf", "Qwen3-Next-UD-IQ4_XS-00002-of-00003.gguf", "Qwen3-Next-UD-IQ4_XS-00003-of-00003.gguf"}[i])
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		shardTotal += info.Size()
	}
	if split.SizeBytes != shardTotal {
		t.Fatalf("split size = %d, want the sum of every shard (%d)", split.SizeBytes, shardTotal)
	}
}

// A downloaded model is in the models directory too, so the scan finds it as
// well. Its manifest is the better record and has to win.
func TestListArtifactsPrefersInstalledManifest(t *testing.T) {
	root := t.TempDir()
	writeGGUF(t, filepath.Join(root, "downloaded.gguf"), languageModelKVs("llama", "Llama", 4096)...)

	mgr, err := NewArtifactManager(root, 50<<30)
	if err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	err = mgr.saveManifestLocked(&ArtifactManifest{
		ID: "art-downloaded.gguf", Name: "downloaded", Filename: "downloaded.gguf",
		Path: "downloaded.gguf", Source: SourceDownload, SHA256: "abc123", ContextLimit: 4096,
	})
	mgr.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	list, err := mgr.ListArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %v, want one entry", manifestNames(list))
	}
	if list[0].Source != SourceDownload || list[0].SHA256 != "abc123" {
		t.Fatalf("scan overwrote the installed manifest: %+v", list[0])
	}
}

// Deleting inside a shared cache would corrupt it for every other tool using
// it, so only weights Woolwire installed may be removed.
func TestDeleteRefusesDiscoveredModels(t *testing.T) {
	root := newCacheFixture(t)
	mgr, err := NewArtifactManager(root, 50<<30)
	if err != nil {
		t.Fatal(err)
	}
	list, err := mgr.ListArtifacts()
	if err != nil {
		t.Fatal(err)
	}

	discovered := manifestByName(t, list, "Qwen3-8B-UD-Q4_K_S")
	if err := mgr.DeleteArtifact(discovered.Path); err != ErrNotManaged {
		t.Fatalf("DeleteArtifact on a discovered model returned %v, want ErrNotManaged", err)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(discovered.Path))); err != nil {
		t.Fatalf("discovered model was removed: %v", err)
	}
	if err := mgr.DeleteArtifact("../escape.gguf"); err != ErrPathTraversal {
		t.Fatalf("DeleteArtifact on an escape returned %v, want ErrPathTraversal", err)
	}
}

// A cache mounted read-only is a reasonable way to run this, and it should
// cost downloads rather than the model list.
func TestReadOnlyModelsDirectoryStillLists(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := newCacheFixture(t)
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	mgr, err := NewArtifactManager(root, 50<<30)
	if err != nil {
		t.Fatalf("NewArtifactManager on a read-only directory returned %v", err)
	}
	if !mgr.ReadOnly() {
		t.Fatal("expected the manager to report a read-only models directory")
	}
	list, err := mgr.ListArtifacts()
	if err != nil || len(list) == 0 {
		t.Fatalf("ListArtifacts on a read-only directory returned %d models, %v", len(list), err)
	}
	if _, err := mgr.StartDownload("https://example.com/m.gguf", "m.gguf", "", 0); err != ErrReadOnlyModels {
		t.Fatalf("StartDownload returned %v, want ErrReadOnlyModels", err)
	}
}

// The budget bounds what Woolwire downloads. Weights it found are served but
// not charged, or a cache larger than any sensible budget would block every
// download.
func TestStorageBudgetCountsOnlyInstalledWeights(t *testing.T) {
	root := newCacheFixture(t)
	// A file the models directory happens to contain, belonging to something
	// else entirely.
	if err := os.WriteFile(filepath.Join(root, "token"), []byte("not a model"), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewArtifactManager(root, 50<<30)
	if err != nil {
		t.Fatal(err)
	}
	used, err := mgr.GetUsedDiskSpace()
	if err != nil {
		t.Fatal(err)
	}
	if used != 0 {
		t.Fatalf("used space = %d, want 0 before anything is installed", used)
	}

	writeGGUF(t, filepath.Join(root, "installed.gguf"), languageModelKVs("llama", "Llama", 4096)...)
	info, err := os.Stat(filepath.Join(root, "installed.gguf"))
	if err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	err = mgr.saveManifestLocked(&ArtifactManifest{
		ID: "art-installed.gguf", Name: "installed", Filename: "installed.gguf",
		Path: "installed.gguf", Source: SourceDownload, SizeBytes: info.Size(),
	})
	mgr.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	used, err = mgr.GetUsedDiskSpace()
	if err != nil {
		t.Fatal(err)
	}
	if used != info.Size() {
		t.Fatalf("used space = %d, want the installed weight's size (%d)", used, info.Size())
	}
}
