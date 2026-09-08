package hosting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Resolving a repository means asking Hugging Face what is in it.
//
// This used to be a guess: the repo name with "-Q4_K_M.gguf" appended. It was
// wrong twice over — a repo is conventionally named "<model>-GGUF" while its
// files are named "<model>-<quant>.gguf", so the guess kept the "-GGUF" and
// 404ed, and no guess can know which quantizations a repo actually published.

const (
	// huggingFaceAPIBase is the public API. No credential is sent: only
	// public repositories can be resolved.
	huggingFaceAPIBase = "https://huggingface.co"
	// maxRepoListingBytes bounds the listing we are willing to read.
	maxRepoListingBytes = 8 << 20
	// maxRepoFiles bounds how many files one repository may contribute.
	maxRepoFiles = 2000
)

var (
	// ErrInvalidRepoRef is returned for input that is not a repository
	// reference.
	ErrInvalidRepoRef = errors.New("expected a Hugging Face repository, as a URL or as org/name")
	// ErrRepoNotFound is returned when the repository does not exist or is
	// not public.
	ErrRepoNotFound = errors.New("repository not found, or it is private")
	// ErrNoWeightsInRepo is returned for a repository holding no GGUF files.
	ErrNoWeightsInRepo = errors.New("repository contains no .gguf weight files")
)

// repoSegmentPattern is what Hugging Face allows in an owner or repo name.
// The reference is interpolated into a URL this process then fetches, so it is
// validated rather than escaped-and-hoped.
var repoSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)

// quantPattern recovers the quantization from a weight filename, which is the
// part people choose by.
var quantPattern = regexp.MustCompile(`(?i)-((?:UD-)?(?:IQ|Q)\d[A-Z0-9_]*|BF16|FP16|F16|F32)(?:-\d{5}-of-\d{5})?\.gguf$`)

// RepoWeight is one downloadable GGUF file in a Hugging Face repository.
type RepoWeight struct {
	// Path is the file's location within the repository, which may include
	// directories.
	Path     string `json:"path"`
	Filename string `json:"filename"`
	// SizeBytes is the whole model's size: for a split model, every shard.
	SizeBytes    int64  `json:"size_bytes"`
	URL          string `json:"url"`
	Quantization string `json:"quantization,omitempty"`
	// ShardCount is above one when the model is split across files. Every
	// shard has to be present before the model can be loaded.
	ShardCount int `json:"shard_count,omitempty"`
	// Companions are the files that belong with this model — a projector, a
	// draft module — chosen for it rather than offered as choices.
	Companions []RepoCompanion `json:"companions,omitempty"`
	// TotalBytes is the download: this model plus its companions.
	TotalBytes int64 `json:"total_bytes"`
}

// RepoCompanion is a file downloaded alongside a model.
type RepoCompanion struct {
	Kind      CompanionKind `json:"kind"`
	Path      string        `json:"path"`
	Filename  string        `json:"filename"`
	SizeBytes int64         `json:"size_bytes"`
	URL       string        `json:"url"`
}

// ParseRepoRef accepts what someone is likely to paste — a repository URL, a
// URL pointing into its file tree, or a bare "org/name" — and returns the
// canonical reference.
func ParseRepoRef(input string) (string, error) {
	ref := strings.TrimSpace(input)
	if ref == "" {
		return "", ErrInvalidRepoRef
	}

	// Tolerate a full URL with or without a scheme.
	for _, prefix := range []string{"https://huggingface.co/", "http://huggingface.co/", "huggingface.co/"} {
		if rest, found := strings.CutPrefix(ref, prefix); found {
			ref = rest
			break
		}
	}
	if u, err := url.Parse(ref); err == nil && u.Scheme != "" {
		// Any other host is not a Hugging Face repository.
		return "", ErrInvalidRepoRef
	}
	if idx := strings.IndexAny(ref, "?#"); idx >= 0 {
		ref = ref[:idx]
	}

	parts := strings.Split(strings.Trim(ref, "/"), "/")
	// Everything after "org/name" is the branch and path within the repo,
	// which a listing does not need.
	if len(parts) > 2 {
		parts = parts[:2]
	}
	if len(parts) != 2 {
		return "", ErrInvalidRepoRef
	}
	for _, p := range parts {
		if !repoSegmentPattern.MatchString(p) {
			return "", ErrInvalidRepoRef
		}
	}
	return parts[0] + "/" + parts[1], nil
}

// treeEntry is the part of Hugging Face's tree listing we use.
type treeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ResolveHuggingFaceRepo lists the GGUF weights a public repository publishes.
// Split models collapse into one entry — their first shard, sized across every
// shard — because that is the file an engine is pointed at.
func (m *ArtifactManager) ResolveHuggingFaceRepo(ctx context.Context, repoRef string) ([]RepoWeight, error) {
	repo, err := ParseRepoRef(repoRef)
	if err != nil {
		return nil, err
	}

	base := m.hfAPIBase
	if base == "" {
		base = huggingFaceAPIBase
	}
	listURL := fmt.Sprintf("%s/api/models/%s/tree/main?recursive=true", base, repo)

	policy := DestinationPolicy{}
	if err := ValidateDestinationWithPolicy(listURL, policy); err != nil {
		return nil, fmt.Errorf("repository listing URL rejected: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", listURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := m.clientFor(policy).Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach Hugging Face: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized:
		return nil, ErrRepoNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("Hugging Face returned %d", resp.StatusCode)
	}

	var entries []treeEntry
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRepoListingBytes)).Decode(&entries); err != nil {
		return nil, fmt.Errorf("could not read the repository listing: %w", err)
	}
	if len(entries) > maxRepoFiles {
		entries = entries[:maxRepoFiles]
	}

	return collectRepoWeights(repo, entries), nil
}

// collectRepoWeights turns a repository listing into the weights it offers.
func collectRepoWeights(repo string, entries []treeEntry) []RepoWeight {
	// Shards are summed onto their first piece, so a split model is offered
	// once, at its real size.
	shardTotals := make(map[string]int64)
	shardCounts := make(map[string]int)
	for _, e := range entries {
		if e.Type == "directory" || !strings.HasSuffix(strings.ToLower(e.Path), ".gguf") {
			continue
		}
		if base, _, _, isShard := shardInfo(path.Base(e.Path)); isShard {
			key := path.Join(path.Dir(e.Path), base)
			shardTotals[key] += e.Size
			shardCounts[key]++
		}
	}

	// Companions belong to a model, not beside it in a list of choices.
	var candidates []companionCandidate
	for _, e := range entries {
		if e.Type == "directory" || !strings.HasSuffix(strings.ToLower(e.Path), ".gguf") {
			continue
		}
		if kind := classifyCompanion(e.Path); kind != "" {
			candidates = append(candidates, companionCandidate{Path: e.Path, Size: e.Size, Kind: kind})
		}
	}

	weights := make([]RepoWeight, 0, len(entries))
	for _, e := range entries {
		if e.Type == "directory" || !strings.HasSuffix(strings.ToLower(e.Path), ".gguf") {
			continue
		}
		if classifyCompanion(e.Path) != "" {
			continue
		}

		name := path.Base(e.Path)
		size := e.Size
		shards := 0
		if base, index, total, isShard := shardInfo(name); isShard {
			if index != 1 {
				continue // the engine finds the remaining pieces itself
			}
			key := path.Join(path.Dir(e.Path), base)
			size = shardTotals[key]
			shards = total
		}

		w := RepoWeight{
			Path:       e.Path,
			Filename:   name,
			SizeBytes:  size,
			ShardCount: shards,
			URL:        repoFileURL(repo, e.Path),
		}
		if match := quantPattern.FindStringSubmatch(name); match != nil {
			w.Quantization = strings.ToUpper(match[1])
		}

		w.TotalBytes = w.SizeBytes
		for _, c := range chooseCompanions(candidates, w.Quantization) {
			w.Companions = append(w.Companions, RepoCompanion{
				Kind:      c.Kind,
				Path:      c.Path,
				Filename:  path.Base(c.Path),
				SizeBytes: c.Size,
				URL:       repoFileURL(repo, c.Path),
			})
			w.TotalBytes += c.Size
		}
		weights = append(weights, w)
	}

	sort.Slice(weights, func(i, j int) bool {
		if weights[i].SizeBytes != weights[j].SizeBytes {
			return weights[i].SizeBytes < weights[j].SizeBytes
		}
		return weights[i].Path < weights[j].Path
	})
	return weights
}

// repoFileURL builds the direct download URL for one file in a repository.
func repoFileURL(repo, filePath string) string {
	segments := strings.Split(filePath, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("%s/%s/resolve/main/%s", huggingFaceAPIBase, repo, strings.Join(segments, "/"))
}
