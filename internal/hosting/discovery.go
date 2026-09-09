package hosting

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Chrisbaack/woolwire/internal/modelpath"
)

// Weights arrive two ways: Woolwire downloads them and writes a manifest
// beside them, or they are already on disk because something else put them
// there. The second case is now the common one — a Hugging Face cache is where
// most people's GGUF files already live — so the models directory is scanned
// for weights rather than assumed to be a flat directory Woolwire filled.
//
// A cache is nested and shared: hub/models--org--repo/snapshots/<rev>/x.gguf,
// where the snapshot entry is a symlink into a sibling blobs directory and the
// same blob is reachable through every revision that references it.

const (
	// maxScanDepth bounds the walk. A Hugging Face cache needs four levels.
	maxScanDepth = 12
	// maxScanCandidates bounds how many GGUF files one scan will inspect.
	maxScanCandidates = 1024
)

// splitShardPattern matches the naming llama.cpp gives the pieces of a model
// too large for one file. Only the first piece is a model to load: the engine
// finds the rest itself.
var splitShardPattern = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

// hfRepoPattern matches the directory a Hugging Face cache gives one repo.
var hfRepoPattern = regexp.MustCompile(`^models--(.+?)--(.+)$`)

// ggufCache remembers parsed headers so listing the models directory does not
// re-read every header on every poll. A file is re-read when its size or
// modification time changes.
type ggufCache struct {
	mu      sync.Mutex
	entries map[string]ggufCacheEntry
}

type ggufCacheEntry struct {
	size    int64
	modTime time.Time
	md      ggufMetadata
	err     error
}

func newGGUFCache() *ggufCache {
	return &ggufCache{entries: make(map[string]ggufCacheEntry)}
}

func (c *ggufCache) lookup(absPath string, info os.FileInfo) (ggufMetadata, error) {
	c.mu.Lock()
	entry, ok := c.entries[absPath]
	c.mu.Unlock()
	if ok && entry.size == info.Size() && entry.modTime.Equal(info.ModTime()) {
		return entry.md, entry.err
	}

	md, err := readGGUFMetadata(absPath)
	c.mu.Lock()
	// The cache tracks a directory the owner controls, not attacker input, but
	// it still must not grow without bound across long uptimes.
	if len(c.entries) > 4*maxScanCandidates {
		c.entries = make(map[string]ggufCacheEntry, maxScanCandidates)
	}
	c.entries[absPath] = ggufCacheEntry{size: info.Size(), modTime: info.ModTime(), md: md, err: err}
	c.mu.Unlock()
	return md, err
}

// hfRepoID turns "hub/models--unsloth--Qwen3-8B-GGUF/snapshots/<rev>/x.gguf"
// into "unsloth/Qwen3-8B-GGUF". It returns "" for a path that is not inside a
// Hugging Face cache.
func hfRepoID(rel string) string {
	for _, elem := range strings.Split(path.Dir(rel), "/") {
		if m := hfRepoPattern.FindStringSubmatch(elem); m != nil {
			// A cache encodes "/" as "--", and an org name never contains one.
			return m[1] + "/" + strings.ReplaceAll(m[2], "--", "/")
		}
	}
	return ""
}

// shardInfo reports whether a filename is one piece of a split model, and if
// so which piece it is.
func shardInfo(name string) (base string, index int, total int, isShard bool) {
	m := splitShardPattern.FindStringSubmatch(name)
	if m == nil {
		return "", 0, 0, false
	}
	index, _ = strconv.Atoi(m[2])
	total, _ = strconv.Atoi(m[3])
	return m[1], index, total, true
}

// displayName is what a person picks from in the model list. The file stem is
// used rather than the header's general.name because the stem carries the
// quantization, which is the part that distinguishes the four copies of one
// model a cache usually holds.
func displayName(fileName string, md ggufMetadata) string {
	stem := strings.TrimSuffix(fileName, ".gguf")
	if base, _, _, isShard := shardInfo(fileName); isShard {
		stem = base
	}
	if stem == "" {
		return md.Name
	}
	return stem
}

// defaultContextLimit picks the context a discovered model is advertised with.
// A model trained for 262144 tokens will happily allocate a KV cache that size
// and exhaust the host, so the trained length is an upper bound rather than
// the default.
func defaultContextLimit(md ggufMetadata) int {
	const fallback = 4096
	const cap = 8192
	if md.ContextLength <= 0 {
		return fallback
	}
	if md.ContextLength > cap {
		return cap
	}
	return md.ContextLength
}

// discoverModels walks the models directory and returns a manifest for every
// GGUF language model it finds. Unreadable directories, unparseable files and
// GGUF files that are not language models are skipped rather than failing the
// listing: one bad file in a large cache must not hide the rest.
func (m *ArtifactManager) discoverModels() []ArtifactManifest {
	root := m.modelsDir
	found := make([]ArtifactManifest, 0, 8)
	seen := make(map[string]bool)
	candidates := 0
	var companions []companionCandidate

	_ = filepath.WalkDir(root, func(absPath string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is skipped; the walk continues.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		rel, relErr := filepath.Rel(root, absPath)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			if absPath == root {
				return nil
			}
			name := d.Name()
			// Hidden directories are Woolwire's own staging area and the
			// cache's bookkeeping (.locks, .no_exist). A cache's blobs
			// directory holds the same files the snapshots link to, under
			// content-hash names.
			if strings.HasPrefix(name, ".") || name == "blobs" || name == "refs" {
				return fs.SkipDir
			}
			if strings.Count(rel, "/")+1 >= maxScanDepth {
				return fs.SkipDir
			}
			return nil
		}

		if !strings.HasSuffix(strings.ToLower(d.Name()), ".gguf") {
			return nil
		}
		if candidates >= maxScanCandidates {
			return filepath.SkipAll
		}
		candidates++

		if _, index, _, isShard := shardInfo(d.Name()); isShard && index != 1 {
			return nil
		}
		// Projectors and draft modules belong to a model. They are collected
		// below and attached to one, not offered as models themselves — a
		// projector cannot serve a request, and an MTP module in a cache is
		// meaningless without the weights it drafts for.
		if classifyCompanion(rel) != "" {
			companions = append(companions, companionCandidate{Path: rel, Kind: classifyCompanion(rel)})
			return nil
		}
		// Reject anything the runner would refuse to load anyway, so the list
		// never offers a model that cannot be selected.
		if _, err := modelpath.Clean(rel); err != nil {
			return nil
		}

		info, err := os.Stat(absPath)
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}

		md, err := m.ggufCache.lookup(absPath, info)
		if err != nil {
			return nil
		}

		// Every revision of a cached repo links to the same blob, so the same
		// weights are reachable by several paths.
		realPath, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			realPath = absPath
		}
		if seen[realPath] {
			return nil
		}
		seen[realPath] = true

		size := info.Size()
		if base, _, total, isShard := shardInfo(d.Name()); isShard {
			size = totalShardSize(filepath.Dir(absPath), base, total)
		}

		found = append(found, ArtifactManifest{
			ID:               discoveredArtifactID(rel),
			Name:             displayName(d.Name(), md),
			Filename:         d.Name(),
			Path:             rel,
			RepoID:           hfRepoID(rel),
			Architecture:     md.Architecture,
			SupportsThinking: md.SupportsThinking,
			Source:           SourceCache,
			Role:             RoleModel,
			SizeBytes:        size,
			ContextLimit:     defaultContextLimit(md),
			InstalledAt:      info.ModTime().Unix(),
		})
		return nil
	})

	// A companion sits with the model it belongs to: the same directory, or
	// an MTP subdirectory of it.
	for i := range found {
		dir := path.Dir(found[i].Path)
		var local []companionCandidate
		for _, c := range companions {
			cDir := path.Dir(c.Path)
			if cDir == dir || path.Dir(cDir) == dir {
				if info, err := os.Stat(filepath.Join(root, filepath.FromSlash(c.Path))); err == nil {
					c.Size = info.Size()
				}
				local = append(local, c)
			}
		}
		quant := ""
		if match := quantPattern.FindStringSubmatch(found[i].Filename); match != nil {
			quant = strings.ToUpper(match[1])
		}
		for _, c := range chooseCompanions(local, quant) {
			found[i].Companions = append(found[i].Companions, ArtifactCompanion{
				Kind:     c.Kind,
				Path:     c.Path,
				Filename: path.Base(c.Path),
			})
		}
	}

	sort.Slice(found, func(i, j int) bool {
		if found[i].Name == found[j].Name {
			return found[i].Path < found[j].Path
		}
		return found[i].Name < found[j].Name
	})
	return found
}

// totalShardSize sums the pieces of a split model so the list shows what the
// model actually costs rather than the size of its first shard.
func totalShardSize(dir, base string, total int) int64 {
	var sum int64
	for i := 1; i <= total; i++ {
		name := fmt.Sprintf("%s-%05d-of-%05d.gguf", base, i, total)
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sum += info.Size()
	}
	return sum
}
