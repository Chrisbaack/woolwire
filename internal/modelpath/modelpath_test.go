package modelpath

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanAcceptsNestedReferences(t *testing.T) {
	cases := map[string]string{
		"model.gguf": "model.gguf",
		"hub/models--unsloth--Qwen3-8B-GGUF/snapshots/abc/model.gguf": "hub/models--unsloth--Qwen3-8B-GGUF/snapshots/abc/model.gguf",
		"sub/model.gguf": "sub/model.gguf",
	}
	for in, want := range cases {
		got, err := Clean(in)
		if err != nil {
			t.Fatalf("Clean(%q) returned %v", in, err)
		}
		if got != want {
			t.Fatalf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanRejectsEscapes(t *testing.T) {
	// Each of these either leaves the models directory or is a shape we never
	// write, so accepting it could only ever help an attacker.
	for _, ref := range []string{
		"",
		"   ",
		"..",
		"../etc/passwd",
		"sub/../../etc/passwd",
		"/etc/passwd",
		"/models/x.gguf",
		`..\windows`,
		`sub\model.gguf`,
		"sub//model.gguf",
		"sub/./model.gguf",
		"sub/model.gguf/",
		"a/b/c/d/e/f/g/h/i/j/k/l/m/model.gguf",
	} {
		if got, err := Clean(ref); err == nil {
			t.Fatalf("Clean(%q) = %q, want an error", ref, got)
		}
	}
}

func TestResolveFollowsSymlinksInsideModelsDir(t *testing.T) {
	// A Hugging Face cache stores every snapshot entry as a symlink into a
	// sibling blobs directory, so resolution has to follow one.
	root := t.TempDir()
	blobs := filepath.Join(root, "repo", "blobs")
	snapshot := filepath.Join(root, "repo", "snapshots", "rev1")
	for _, dir := range []string{blobs, snapshot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	blob := filepath.Join(blobs, "deadbeef")
	if err := os.WriteFile(blob, []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(snapshot, "model.gguf")
	if err := os.Symlink(blob, link); err != nil {
		t.Fatal(err)
	}

	rel, abs, err := Resolve(root, "repo/snapshots/rev1/model.gguf")
	if err != nil {
		t.Fatalf("Resolve returned %v", err)
	}
	if rel != "repo/snapshots/rev1/model.gguf" {
		t.Fatalf("unexpected rel %q", rel)
	}
	if abs != link {
		t.Fatalf("unexpected abs %q, want %q", abs, link)
	}
}

func TestResolveRejectsDirectoriesAndMissingFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, _, err := Resolve(root, "sub"); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("Resolve on a directory returned %v, want ErrNotRegular", err)
	}
	if _, _, err := Resolve(root, "ghost.gguf"); !os.IsNotExist(err) {
		t.Fatalf("Resolve on a missing file returned %v, want not-exist", err)
	}
	if _, _, err := Resolve(root, "../escape.gguf"); !errors.Is(err, ErrEscapes) {
		t.Fatalf("Resolve on an escape returned %v, want ErrEscapes", err)
	}
}
