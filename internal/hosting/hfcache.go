package hosting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Chrisbaack/woolwire/internal/modelpath"
)

// Woolwire is not the only thing that reads the models directory. A Hugging
// Face cache is the layout the rest of the ecosystem already understands —
// huggingface_hub, and everything built on it, finds a model by looking for
// hub/models--org--repo/snapshots/<commit>/<file> — so a download that lands
// as a bare file in the models directory is a file only Woolwire can see.
//
// Downloads are therefore written the way the cache stores them:
//
//	hub/models--org--repo/blobs/<etag>                 the bytes
//	hub/models--org--repo/snapshots/<commit>/<file>    a symlink to the blob
//	hub/models--org--repo/refs/main                    the commit a ref names
//
// A blob is named by the file's ETag, which for the LFS files a GGUF
// repository is made of is its SHA-256 — the hash the download already
// verifies. Content addressing is the point of the layout: two revisions
// shipping the same weights share one copy of them.
//
// Nothing here is required for Woolwire to work. A download whose URL says
// nothing about a repository, or whose commit cannot be established, still
// lands as a flat file and is still found by the scan in discovery.go.

const (
	// hfCacheDir is the cache root within the models directory. A models
	// directory pointed at an HF_HOME already has one.
	hfCacheDir = "hub"
	// hfBlobsDir holds the bytes, named by content.
	hfBlobsDir = "blobs"
	// hfSnapshotsDir holds one directory per commit, of symlinks into blobs.
	hfSnapshotsDir = "snapshots"
	// hfRefsDir maps a branch name to the commit it currently points at.
	hfRefsDir = "refs"
)

var (
	// hfCommitPattern matches a resolved commit: a full git object id. A
	// snapshot directory is named by one.
	hfCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// hfBlobNamePattern bounds what may become a name in the blobs directory.
	// The value arrives in a response header, so it is validated rather than
	// trusted.
	hfBlobNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	// hfRevisionPattern is what may appear as the revision in a download URL.
	// A branch name may contain "/", but such a URL cannot be told apart from
	// one naming a file in a subdirectory, so those take the flat path.
	hfRevisionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// hfSource is what a Hugging Face download URL says about where the file it
// names belongs in a cache.
type hfSource struct {
	// Repo is "org/name".
	Repo string
	// Revision is the ref the URL resolves through, usually "main".
	Revision string
	// Path is the file's location within the repository, in slash form.
	Path string
}

// parseHFSource recognizes the "resolve" URL a Hugging Face repository serves
// files from. Anything else — a mirror, a link straight to a CDN, a file on
// someone's web server — names no repository and no revision to file under,
// and reports false.
func (m *ArtifactManager) parseHFSource(rawURL string) (hfSource, bool) {
	return parseHFSource(rawURL, m.hfHost())
}

// hfHost is the host a URL has to name for its file to belong to a repository.
// It is Hugging Face, except under the test override that stands a local
// server in for it.
func (m *ArtifactManager) hfHost() string {
	if u, err := url.Parse(m.hfAPIBase); m.hfAPIBase != "" && err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return "huggingface.co"
}

func parseHFSource(rawURL, wantHost string) (hfSource, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return hfSource{}, false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Host, "."))
	if host != wantHost && host != "www."+wantHost {
		return hfSource{}, false
	}

	// org / name / resolve / revision / path...
	segments := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(segments) < 5 || segments[2] != "resolve" {
		return hfSource{}, false
	}
	for i, s := range segments {
		decoded, err := url.PathUnescape(s)
		if err != nil {
			return hfSource{}, false
		}
		segments[i] = decoded
	}

	org, name, revision := segments[0], segments[1], segments[3]
	if !repoSegmentPattern.MatchString(org) || !repoSegmentPattern.MatchString(name) {
		return hfSource{}, false
	}
	if !hfRevisionPattern.MatchString(revision) {
		return hfSource{}, false
	}
	filePath, err := modelpath.Clean(strings.Join(segments[4:], "/"))
	if err != nil {
		return hfSource{}, false
	}
	return hfSource{Repo: org + "/" + name, Revision: revision, Path: filePath}, true
}

// hfRepoDir is the directory name a cache gives one repository.
func hfRepoDir(repo string) string {
	return "models--" + strings.ReplaceAll(repo, "/", "--")
}

// hfFileMetadata is what the repository says about one of its files: which
// commit serves it, and the cache's name for its content.
type hfFileMetadata struct {
	Commit string
	ETag   string
}

// fetchHFMetadata asks where a file sits before it is downloaded. The commit
// and the file's real ETag ride on the redirect that points at the CDN, not
// on the response that finally delivers the bytes, so they have to be read
// from a request that stops at the redirect.
func (m *ArtifactManager) fetchHFMetadata(ctx context.Context, sourceURL string, policy DestinationPolicy) (hfFileMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, sourceURL, nil)
	if err != nil {
		return hfFileMetadata{}, err
	}

	client := *m.clientFor(policy)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := client.Do(req)
	if err != nil {
		return hfFileMetadata{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode >= 400 {
		return hfFileMetadata{}, fmt.Errorf("Hugging Face returned %d for the file metadata", resp.StatusCode)
	}

	meta := hfFileMetadata{Commit: strings.TrimSpace(resp.Header.Get("X-Repo-Commit"))}
	// X-Linked-Etag is the LFS object's hash; ETag on the same response is
	// the pointer file's, and on the CDN response it is the CDN's own.
	if meta.ETag = cleanETag(resp.Header.Get("X-Linked-Etag")); meta.ETag == "" {
		meta.ETag = cleanETag(resp.Header.Get("ETag"))
	}
	return meta, nil
}

// cleanETag reduces an ETag header to the bare validator. A blob is named
// after it, so the quotes and any weak marker have to go, and what is left
// has to be a name a directory can hold.
func cleanETag(value string) string {
	v := strings.TrimSpace(value)
	v = strings.TrimPrefix(v, "W/")
	v = strings.Trim(v, `"`)
	if !hfBlobNamePattern.MatchString(v) {
		return ""
	}
	return v
}

// hfPlacement is a decided location for one file within the cache.
type hfPlacement struct {
	src    hfSource
	commit string
	blob   string
	// rel is what the runner is given: the snapshot entry, relative to the
	// models directory.
	rel string
}

// planHFPlacement decides where a file belongs. It reports false when the
// answer is not knowable — a file with no commit has no snapshot to live
// under — and the caller then falls back to a flat file, which still loads
// and is still listed.
func planHFPlacement(src hfSource, meta hfFileMetadata, sha256Hex string) (hfPlacement, bool) {
	commit := meta.Commit
	if !hfCommitPattern.MatchString(commit) {
		// A URL may name the commit outright instead of a branch.
		if !hfCommitPattern.MatchString(src.Revision) {
			return hfPlacement{}, false
		}
		commit = src.Revision
	}

	blob := meta.ETag
	if blob == "" {
		// Every GGUF in a repository is an LFS file, and an LFS file's ETag
		// is the SHA-256 the download computes on the way past.
		blob = sha256Hex
	}
	if !hfBlobNamePattern.MatchString(blob) {
		return hfPlacement{}, false
	}

	rel := path.Join(hfCacheDir, hfRepoDir(src.Repo), hfSnapshotsDir, commit, src.Path)
	// The runner has to accept the path this produces, or the model would be
	// listed and then refuse to load.
	if _, err := modelpath.Clean(rel); err != nil {
		return hfPlacement{}, false
	}
	return hfPlacement{src: src, commit: commit, blob: blob, rel: rel}, true
}

// commitToCache moves a finished file into the cache and returns the path the
// runner is to load it by. The bytes become the blob and the snapshot entry
// naming them is a symlink, exactly as huggingface_hub writes it.
func (m *ArtifactManager) commitToCache(sourcePath string, p hfPlacement) (string, error) {
	repoDir := filepath.Join(m.modelsDir, hfCacheDir, hfRepoDir(p.src.Repo))
	blobPath := filepath.Join(repoDir, hfBlobsDir, p.blob)
	snapPath := filepath.Join(repoDir, hfSnapshotsDir, p.commit, filepath.FromSlash(p.src.Path))

	for _, dir := range []string{filepath.Dir(blobPath), filepath.Dir(snapPath)} {
		// The cache is shared with whatever else reads the models directory,
		// so its directories are traversable rather than private.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}

	if existing, err := os.Stat(blobPath); err == nil && existing.Mode().IsRegular() {
		// The same content is already cached, under another revision or by
		// another tool. The copy on disk is the one to keep.
		_ = os.Remove(sourcePath)
	} else {
		if err := os.Rename(sourcePath, blobPath); err != nil {
			return "", fmt.Errorf("move weights into the cache: %w", err)
		}
		// Downloads stage at 0600, and a blob the other tools sharing the
		// cache cannot read defeats the point of writing one.
		_ = os.Chmod(blobPath, 0o644)
	}

	target, err := filepath.Rel(filepath.Dir(snapPath), blobPath)
	if err != nil {
		return "", err
	}
	// An entry left by an interrupted attempt would make the symlink fail.
	if err := os.Remove(snapPath); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Symlink(target, snapPath); err != nil {
		// A filesystem without symlinks still holds a cache readable by
		// everything that reads one; it just cannot share a blob for free.
		if linkErr := os.Link(blobPath, snapPath); linkErr != nil {
			return "", fmt.Errorf("link weights into the snapshot: %w", err)
		}
	}

	// The ref is how a tool that asked for "main" finds this snapshot.
	if !hfCommitPattern.MatchString(p.src.Revision) {
		refPath := filepath.Join(repoDir, hfRefsDir, p.src.Revision)
		if err := os.MkdirAll(filepath.Dir(refPath), 0o755); err == nil {
			_ = os.WriteFile(refPath, []byte(p.commit), 0o644)
		}
	}
	return p.rel, nil
}

// removeFromCache deletes weights Woolwire put in the cache: the snapshot
// entry, then the blob behind it once nothing else names it, then whatever
// directories that empties. Only paths inside the cache are touched, and a
// blob another snapshot still references is left alone — a cache is shared,
// and removing one model from it must not damage another.
func (m *ArtifactManager) removeFromCache(rel string) error {
	parts := strings.Split(rel, "/")
	if len(parts) < 5 || parts[0] != hfCacheDir || parts[2] != hfSnapshotsDir {
		return nil // not a cache path; the caller removes it as a plain file
	}
	repoDir := filepath.Join(m.modelsDir, parts[0], parts[1])
	snapPath := filepath.Join(m.modelsDir, filepath.FromSlash(rel))

	blobPath, err := filepath.EvalSymlinks(snapPath)
	if err != nil {
		blobPath = ""
	}
	if err := os.Remove(snapPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	if blobPath != "" && strings.HasPrefix(blobPath, filepath.Join(repoDir, hfBlobsDir)+string(filepath.Separator)) {
		if !blobReferenced(filepath.Join(repoDir, hfSnapshotsDir), blobPath) {
			_ = os.Remove(blobPath)
		}
	}

	// Prune what the removal emptied, up to the repository directory. Remove
	// refuses a directory that is not empty, which is exactly the test.
	dir := filepath.Dir(snapPath)
	for strings.HasPrefix(dir, repoDir+string(filepath.Separator)) {
		if os.Remove(dir) != nil {
			break
		}
		dir = filepath.Dir(dir)
	}
	_ = os.Remove(filepath.Join(repoDir, hfSnapshotsDir))
	_ = os.Remove(filepath.Join(repoDir, hfBlobsDir))
	return nil
}

// blobReferenced reports whether any snapshot entry still resolves to a blob.
func blobReferenced(snapshotsRoot, blobPath string) bool {
	referenced := false
	_ = filepath.WalkDir(snapshotsRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if resolved, rErr := filepath.EvalSymlinks(p); rErr == nil && resolved == blobPath {
			referenced = true
			return filepath.SkipAll
		}
		return nil
	})
	return referenced
}

// manifestsDir holds Woolwire's record of what it installed. It sits beside
// the weights rather than among them: the models directory is very often a
// cache shared with other tools, and their bookkeeping is not ours to add to.
const manifestsDir = ".woolwire/manifests"

// manifestNameFor names a manifest after the path it describes. Naming it
// after the file would collide: half the GGUF repositories on Hugging Face
// publish a file called mmproj-F16.gguf.
func manifestNameFor(relPath string) string {
	sum := sha256.Sum256([]byte(relPath))
	return hex.EncodeToString(sum[:16]) + ".json"
}

// legacyManifestName is where a manifest was written before models could live
// in subdirectories: beside the weights, named after them.
func legacyManifestName(relPath string) string {
	return filepath.FromSlash(relPath) + ".manifest.json"
}
