// Package modelpath validates references to weight files inside a models
// directory.
//
// The runner and the local API both accept a caller-supplied reference and
// both have to agree on what is in bounds. A reference is always relative to
// the models directory and always uses forward slashes, so the same string
// works on either side of the runner protocol.
//
// References may name a file in a subdirectory: a Hugging Face cache stores
// weights several levels down (models--org--repo/snapshots/<rev>/model.gguf),
// and refusing every reference containing a separator meant a cache could not
// be used as the models directory at all.
package modelpath

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxDepth bounds how far below the models directory a reference may point.
// A Hugging Face cache needs four levels; the rest is headroom.
const maxDepth = 12

var (
	// ErrEmpty is returned for a reference with no path elements.
	ErrEmpty = errors.New("model reference is empty")
	// ErrEscapes is returned for a reference that leaves the models directory.
	ErrEscapes = errors.New("invalid model reference: path traversal prohibited")
	// ErrTooDeep is returned for a reference nested past maxDepth.
	ErrTooDeep = fmt.Errorf("invalid model reference: more than %d levels deep", maxDepth)
	// ErrNotRegular is returned when a reference resolves to something that is
	// not a regular file.
	ErrNotRegular = errors.New("model reference is not a regular file")
)

// Clean validates a reference lexically and returns it in canonical
// slash-separated form. It performs no filesystem access, so the local API can
// check a reference it will only ever hand to the runner.
func Clean(ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", ErrEmpty
	}
	// Backslashes are legal in a POSIX filename but never appear in one we
	// wrote, and accepting them invites Windows-shaped traversal attempts.
	if strings.ContainsAny(ref, `\`+"\x00") {
		return "", ErrEscapes
	}
	if path.IsAbs(ref) || filepath.IsAbs(ref) {
		return "", ErrEscapes
	}

	elems := strings.Split(ref, "/")
	if len(elems) > maxDepth {
		return "", ErrTooDeep
	}
	for _, e := range elems {
		// Rejecting "" also rejects "a//b" and a trailing slash, so the
		// canonical form is exactly what the caller sent.
		if e == "" || e == "." || e == ".." {
			return "", ErrEscapes
		}
	}
	return path.Join(elems...), nil
}

// Resolve validates a reference against a models directory and returns the
// canonical reference together with the absolute path it names.
//
// Symlinks are followed deliberately: a Hugging Face cache stores every
// snapshot entry as a symlink into its blobs directory, so refusing to follow
// them would reject the layout this exists to support. Nothing untrusted
// writes to the models directory — downloads are owner-authenticated and are
// committed under a base name — so its links are the owner's own.
func Resolve(root, ref string) (string, string, error) {
	rel, err := Clean(ref)
	if err != nil {
		return "", "", err
	}

	abs := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil {
		return rel, abs, err
	}
	if !info.Mode().IsRegular() {
		return rel, abs, ErrNotRegular
	}
	return rel, abs, nil
}
