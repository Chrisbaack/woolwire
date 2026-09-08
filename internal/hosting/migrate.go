package hosting

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Downloads used to land as bare files in the models directory: Woolwire
// listed them, the runner loaded them, and nothing else on the machine could
// see them. Weights now go into the Hugging Face cache layout instead, which
// leaves the files an earlier version installed sitting where they were.
//
// The manifest beside each of those files records the URL it came from, and
// that is all it takes to file it correctly after the fact.

// MigrateFlatDownloads moves weights Woolwire downloaded before it wrote a
// cache layout into that layout, and returns how many it moved. Weights it
// cannot place — a source that is not a Hugging Face repository, a repository
// that no longer answers — are left exactly where they are, so a directory
// that only partly migrates is still a working one.
func (m *ArtifactManager) MigrateFlatDownloads(ctx context.Context) (int, error) {
	if m.readOnly {
		return 0, nil
	}

	m.mu.RLock()
	candidates := make([]ArtifactManifest, 0)
	for _, mf := range m.allManifestsLocked() {
		// A path with no separator is a file at the top of the models
		// directory: one of the flat downloads.
		if strings.Contains(mf.Path, "/") || mf.SourceURL == "" {
			continue
		}
		candidates = append(candidates, mf)
	}
	m.mu.RUnlock()

	// Companions move with their model, and the model's manifest names them
	// by filename alone; the new path for each is collected as it moves.
	moved := make(map[string]string, len(candidates))
	migrated := make([]ArtifactManifest, 0, len(candidates))

	for _, mf := range candidates {
		if ctx.Err() != nil {
			return len(migrated), ctx.Err()
		}
		src, ok := m.parseHFSource(mf.SourceURL)
		if !ok {
			continue
		}
		meta, err := m.fetchHFMetadata(ctx, mf.SourceURL, DestinationPolicy{})
		if err != nil {
			continue
		}
		placement, ok := planHFPlacement(src, meta, mf.SHA256)
		if !ok {
			continue
		}

		m.mu.Lock()
		rel, err := m.commitToCache(filepath.Join(m.modelsDir, filepath.FromSlash(mf.Path)), placement)
		if err == nil {
			// The identifier is deliberately kept: it is the key of whatever
			// hosted-model row already advertises this model, and moving the
			// file is no reason to strand it.
			oldPath := mf.Path
			mf.Path = rel
			mf.RepoID = src.Repo
			if saveErr := m.saveManifestLocked(&mf); saveErr == nil {
				_ = os.Remove(filepath.Join(m.modelsDir, legacyManifestName(oldPath)))
				moved[mf.Filename] = rel
				migrated = append(migrated, mf)
			}
		}
		m.mu.Unlock()
	}

	// A model's companions are recorded by filename, which no longer locates
	// them once they are several directories down.
	for _, mf := range migrated {
		if mf.Role == RoleCompanion || len(mf.Companions) == 0 {
			continue
		}
		changed := false
		for i, c := range mf.Companions {
			if rel, ok := moved[c.Filename]; ok && c.Path != rel {
				mf.Companions[i].Path = rel
				changed = true
			}
		}
		if !changed {
			continue
		}
		m.mu.Lock()
		_ = m.saveManifestLocked(&mf)
		m.mu.Unlock()
	}

	return len(migrated), nil
}
