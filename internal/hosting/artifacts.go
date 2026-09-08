package hosting

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cbaack/woolwire/internal/modelpath"
)

var (
	ErrDiskBudgetExceeded = errors.New("model download exceeds configured storage budget")
	ErrHashMismatch       = errors.New("downloaded artifact SHA-256 hash mismatch")
	ErrPathTraversal      = errors.New("invalid filename: path traversal prohibited")
	// ErrNotManaged guards weights Woolwire found but did not install.
	// Deleting inside a Hugging Face cache would corrupt it for every other
	// tool sharing it.
	ErrNotManaged = errors.New("model was found in the models directory, not installed by Woolwire; remove it with the tool that put it there")
	// ErrReadOnlyModels is returned when a download is attempted against a
	// models directory Woolwire cannot write to.
	ErrReadOnlyModels = errors.New("models directory is read-only; downloads are disabled")
)

type ArtifactManifest struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Filename string `json:"filename"`
	// Path locates the weights relative to the models directory, in slash
	// form. It is what the runner is asked to load. For a model Woolwire
	// downloaded it is just the filename; for one found in a cache it is the
	// nested path the cache stores it at.
	Path         string `json:"path"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA256       string `json:"sha256"`
	SourceURL    string `json:"source_url,omitempty"`
	ContextLimit int    `json:"context_limit"`
	InstalledAt  int64  `json:"installed_at"`
	// Source distinguishes weights Woolwire installed, and may therefore
	// delete, from weights it merely found.
	Source string `json:"source,omitempty"`
	// RepoID is the "org/repo" a Hugging Face cache filed the model under.
	RepoID string `json:"repo_id,omitempty"`
	// Architecture is the model family reported by the GGUF header.
	Architecture string `json:"architecture,omitempty"`
	// Role separates models from the files that support one.
	Role string `json:"role,omitempty"`
	// Companions are the supporting files installed with this model.
	Companions []ArtifactCompanion `json:"companions,omitempty"`
}

// ArtifactCompanion is a supporting file recorded against a model.
type ArtifactCompanion struct {
	Kind CompanionKind `json:"kind"`
	// Path locates the file relative to the models directory.
	Path     string `json:"path,omitempty"`
	Filename string `json:"filename"`
}

// Artifact roles.
const (
	// RoleModel is a model that can be loaded. It is the default, so an older
	// manifest without a role is one of these.
	RoleModel = "model"
	// RoleCompanion is a projector or draft module: needed alongside a model,
	// never loaded as one.
	RoleCompanion = "companion"
)

// Artifact sources.
const (
	// SourceDownload marks weights Woolwire downloaded and wrote a manifest
	// for. Only these may be deleted through the API.
	SourceDownload = "download"
	// SourceCache marks weights discovered in the models directory.
	SourceCache = "cache"
)

// discoveredArtifactID derives a stable identifier from a model's path. A
// nested path cannot be used as an ID directly: IDs travel in URL path
// segments and become hosted-model row keys.
func discoveredArtifactID(relPath string) string {
	sum := sha256.Sum256([]byte(relPath))
	return "art-" + hex.EncodeToString(sum[:8])
}

// DownloadState is the observable progress of one background download.
type DownloadState struct {
	ID              string `json:"id"`
	Filename        string `json:"filename"`
	SourceURL       string `json:"source_url"`
	Status          string `json:"status"` // downloading, complete, failed, cancelled
	BytesDownloaded int64  `json:"bytes_downloaded"`
	TotalBytes      int64  `json:"total_bytes,omitempty"`
	Error           string `json:"error,omitempty"`
	StartedAt       int64  `json:"started_at"`
	FinishedAt      int64  `json:"finished_at,omitempty"`
}

type downloadJob struct {
	state  DownloadState
	cancel context.CancelFunc
}

type transferState struct {
	reservation int64
	staged      int64
	stagingPath string
}

type ArtifactManager struct {
	modelsDir string
	// readOnly records that the models directory could not be written to.
	// Pointing Woolwire at a cache mounted read-only is a reasonable thing to
	// do, and it should cost downloads, not the whole model list.
	readOnly  bool
	ggufCache *ggufCache
	// hfAPIBase overrides the Hugging Face endpoint. It exists so tests can
	// point at a local server; production leaves it empty.
	hfAPIBase  string
	stagingDir string
	maxBudget  int64 // max total bytes allowed for models

	mu              sync.RWMutex
	activeTransfers map[string]*transferState
	downloads       map[string]*downloadJob

	// httpClient overrides the SSRF-guarded client. It exists so tests can
	// trust a self-signed local server; production leaves it nil.
	httpClient *http.Client
}

func (m *ArtifactManager) clientFor(policy DestinationPolicy) *http.Client {
	m.mu.RLock()
	override := m.httpClient
	m.mu.RUnlock()
	if override != nil {
		return override
	}
	return guardedDownloadClient(policy, 60*time.Second)
}

func NewArtifactManager(modelsDir string, maxBudget int64) (*ArtifactManager, error) {
	if modelsDir == "" {
		return nil, errors.New("models directory required")
	}
	if maxBudget <= 0 {
		maxBudget = 50 * (1 << 30) // default 50 GB
	}

	// The models directory itself has to exist and be readable; anything less
	// and there is nothing to serve.
	info, err := os.Stat(modelsDir)
	if err != nil {
		return nil, fmt.Errorf("models directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("models directory %q is not a directory", modelsDir)
	}

	// A read-only models directory is a supported configuration — a shared
	// model cache mounted read-only is the obvious example — so failing to
	// create the staging area disables downloads instead of the manager.
	stagingDir := filepath.Join(modelsDir, ".staging")
	readOnly := false
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		readOnly = true
	} else if probe, err := os.CreateTemp(stagingDir, ".writable-*"); err != nil {
		// MkdirAll succeeds on an existing directory even under a read-only
		// mount, so the directory existing proves nothing. Writing does.
		readOnly = true
	} else {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
	}

	return &ArtifactManager{
		modelsDir:       modelsDir,
		readOnly:        readOnly,
		ggufCache:       newGGUFCache(),
		stagingDir:      stagingDir,
		maxBudget:       maxBudget,
		activeTransfers: make(map[string]*transferState),
		downloads:       make(map[string]*downloadJob),
	}, nil
}

// CompanionsFor returns the supporting files recorded or found beside one
// model, so a load can pass them to the engine without anyone having chosen
// them.
func (m *ArtifactManager) CompanionsFor(relPath string) []ArtifactCompanion {
	artifacts, err := m.ListArtifacts()
	if err != nil {
		return nil
	}
	for _, a := range artifacts {
		if a.Path != relPath {
			continue
		}
		out := make([]ArtifactCompanion, 0, len(a.Companions))
		for _, c := range a.Companions {
			// A downloaded companion is recorded by filename, at the top
			// level beside its model.
			if c.Path == "" {
				c.Path = c.Filename
			}
			out = append(out, c)
		}
		return out
	}
	return nil
}

// ReadOnly reports whether the models directory can be written to.
func (m *ArtifactManager) ReadOnly() bool { return m.readOnly }

func (m *ArtifactManager) GetUsedDiskSpace() (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.getUsedSpaceLocked()
}

func (m *ArtifactManager) MaxBudget() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.maxBudget
}

// DownloadArtifact fetches one model weight file. The storage budget is
// reserved under the lock and then released; the multi-gigabyte transfer runs
// with the lock free, so ListArtifacts and the metrics endpoint keep answering
// while a download is in flight.
func (m *ArtifactManager) DownloadArtifact(
	ctx context.Context,
	sourceURL string,
	filename string,
	expectedSHA256 string,
	maxSizeBytes int64,
	onProgress func(bytesDownloaded int64),
) (*ArtifactManifest, error) {
	policy := DestinationPolicy{}
	if err := ValidateDestinationWithPolicy(sourceURL, policy); err != nil {
		return nil, fmt.Errorf("source URL validation failed: %w", err)
	}
	if !strings.HasPrefix(sourceURL, "https://") {
		return nil, errors.New("model downloads must use HTTPS")
	}

	cleanName := filepath.Base(filename)
	if cleanName != filename || strings.Contains(filename, "..") || strings.Contains(filename, "/") || strings.Contains(filename, "\\") {
		return nil, ErrPathTraversal
	}
	if !strings.HasSuffix(strings.ToLower(cleanName), ".gguf") {
		return nil, errors.New("only .gguf model weights are accepted")
	}

	transferID := fmt.Sprintf("tr-%d-%s", time.Now().UnixNano(), cleanName)
	stagingPath := filepath.Join(m.stagingDir, fmt.Sprintf("dl-%d-%s", time.Now().UnixNano(), cleanName))

	reservation := maxSizeBytes
	if reservation <= 0 {
		reservation = 0
	}
	if err := m.startTransfer(transferID, stagingPath, reservation); err != nil {
		return nil, err
	}
	defer m.finishTransfer(transferID)

	outFile, err := os.OpenFile(stagingPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create staging file: %w", err)
	}
	defer func() {
		_ = outFile.Close()
		_ = os.Remove(stagingPath) // no-op once the file has been renamed
	}()

	req, err := http.NewRequestWithContext(ctx, "GET", sourceURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := m.clientFor(policy).Do(req)
	if err != nil {
		return nil, fmt.Errorf("http download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download server returned %d", resp.StatusCode)
	}

	hasher := sha256.New()
	var totalDownloaded int64
	buf := make([]byte, 64*1024)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if err := m.chargeTransfer(transferID, int64(n), maxSizeBytes); err != nil {
				return nil, err
			}

			if _, err := outFile.Write(buf[:n]); err != nil {
				return nil, fmt.Errorf("write staging file: %w", err)
			}
			hasher.Write(buf[:n])
			totalDownloaded += int64(n)

			if onProgress != nil {
				onProgress(totalDownloaded)
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("download read error: %w", readErr)
		}
	}

	actualHash := hex.EncodeToString(hasher.Sum(nil))
	if expectedSHA256 != "" && !strings.EqualFold(actualHash, expectedSHA256) {
		return nil, fmt.Errorf("%w: expected %s, got %s", ErrHashMismatch, expectedSHA256, actualHash)
	}

	if err := outFile.Close(); err != nil {
		return nil, fmt.Errorf("flush staging file: %w", err)
	}

	m.mu.Lock()
	finalPath := filepath.Join(m.modelsDir, cleanName)
	renameErr := os.Rename(stagingPath, finalPath)
	manifest := &ArtifactManifest{
		ID:           "art-" + cleanName,
		Name:         cleanName,
		Filename:     cleanName,
		Path:         cleanName,
		Source:       SourceDownload,
		SizeBytes:    totalDownloaded,
		SHA256:       actualHash,
		SourceURL:    sourceURL,
		ContextLimit: 4096,
		InstalledAt:  time.Now().Unix(),
	}
	if renameErr == nil {
		_ = m.saveManifestLocked(manifest)
		m.finishTransferLocked(transferID)
	}
	m.mu.Unlock()

	if renameErr != nil {
		return nil, fmt.Errorf("finalize model move: %w", renameErr)
	}
	return manifest, nil
}

func (m *ArtifactManager) startTransfer(transferID, stagingPath string, reservation int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	committed, err := m.totalCommittedLocked()
	if err != nil {
		return err
	}
	if reservation > 0 && committed+reservation > m.maxBudget {
		return ErrDiskBudgetExceeded
	}
	if reservation <= 0 && committed >= m.maxBudget {
		return ErrDiskBudgetExceeded
	}

	m.activeTransfers[transferID] = &transferState{
		reservation: reservation,
		staged:      0,
		stagingPath: stagingPath,
	}
	return nil
}

func (m *ArtifactManager) chargeTransfer(transferID string, delta int64, maxSizeBytes int64) error {
	if delta <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.activeTransfers[transferID]
	if !ok {
		return errors.New("transfer not found")
	}

	newStaged := t.staged + delta
	if maxSizeBytes > 0 && newStaged > maxSizeBytes {
		return ErrDiskBudgetExceeded
	}

	currCommitted := t.staged
	if t.reservation > currCommitted {
		currCommitted = t.reservation
	}
	newCommitted := newStaged
	if t.reservation > newCommitted {
		newCommitted = t.reservation
	}
	extraNeeded := newCommitted - currCommitted

	if extraNeeded > 0 {
		committed, err := m.totalCommittedLocked()
		if err != nil {
			return err
		}
		if committed+extraNeeded > m.maxBudget {
			return ErrDiskBudgetExceeded
		}
	}

	t.staged = newStaged
	return nil
}

func (m *ArtifactManager) finishTransfer(transferID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finishTransferLocked(transferID)
}

func (m *ArtifactManager) finishTransferLocked(transferID string) {
	delete(m.activeTransfers, transferID)
}

func (m *ArtifactManager) totalCommittedLocked() (int64, error) {
	total, err := m.installedSizeLocked()
	if err != nil {
		return 0, err
	}

	stagingEntries, err := os.ReadDir(m.stagingDir)
	if err == nil {
		activePaths := make(map[string]bool, len(m.activeTransfers))
		for _, t := range m.activeTransfers {
			if t.stagingPath != "" {
				activePaths[filepath.Clean(t.stagingPath)] = true
			}
		}
		for _, e := range stagingEntries {
			if e.IsDir() {
				continue
			}
			fullPath := filepath.Clean(filepath.Join(m.stagingDir, e.Name()))
			if !activePaths[fullPath] {
				if info, err := e.Info(); err == nil {
					total += info.Size()
				}
			}
		}
	}

	for _, t := range m.activeTransfers {
		committed := t.staged
		if t.reservation > committed {
			committed = t.reservation
		}
		total += committed
	}

	return total, nil
}

// StartDownload runs a download in the background and returns immediately with
// a job id the UI polls. A synchronous multi-gigabyte transfer inside an HTTP
// handler is not something a browser or a reverse proxy will wait through.
func (m *ArtifactManager) StartDownload(sourceURL, filename, expectedSHA256 string, maxSizeBytes int64) (string, error) {
	return m.StartDownloadSet(DownloadRequest{
		SourceURL:      sourceURL,
		Filename:       filename,
		ExpectedSHA256: expectedSHA256,
		MaxSizeBytes:   maxSizeBytes,
	})
}

// DownloadRequest is one model and the files that have to arrive with it.
type DownloadRequest struct {
	SourceURL      string
	Filename       string
	ExpectedSHA256 string
	MaxSizeBytes   int64
	// Companions are fetched after the weights and recorded against them. A
	// projector or draft module is worthless on its own, so they are part of
	// one download rather than something to remember to fetch separately.
	Companions []CompanionRequest
}

// CompanionRequest is one companion file to fetch beside a model.
type CompanionRequest struct {
	Kind         CompanionKind
	SourceURL    string
	Filename     string
	MaxSizeBytes int64
}

// StartDownloadSet downloads a model and its companions as one job, reported
// as one progress figure, because to the person waiting it is one download.
func (m *ArtifactManager) StartDownloadSet(req DownloadRequest) (string, error) {
	if m.readOnly {
		return "", ErrReadOnlyModels
	}

	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", err
	}
	id := "dl-" + hex.EncodeToString(idBytes)

	total := req.MaxSizeBytes
	for _, c := range req.Companions {
		total += c.MaxSizeBytes
	}

	ctx, cancel := context.WithCancel(context.Background())
	job := &downloadJob{
		state: DownloadState{
			ID:         id,
			Filename:   req.Filename,
			SourceURL:  req.SourceURL,
			Status:     "downloading",
			TotalBytes: total,
			StartedAt:  time.Now().Unix(),
		},
		cancel: cancel,
	}

	m.mu.Lock()
	m.downloads[id] = job
	m.mu.Unlock()

	go func() {
		defer cancel()

		// Progress is reported across the whole set, so a 6 GB model followed
		// by a 160 MB projector reads as one bar rather than two.
		var completed int64
		progress := func(n int64) {
			m.mu.Lock()
			job.state.BytesDownloaded = completed + n
			m.mu.Unlock()
		}

		manifest, err := m.DownloadArtifact(ctx, req.SourceURL, req.Filename, req.ExpectedSHA256, req.MaxSizeBytes, progress)
		if err == nil && manifest != nil {
			completed = manifest.SizeBytes

			var installed []ArtifactCompanion
			for _, c := range req.Companions {
				m.mu.Lock()
				job.state.Filename = c.Filename
				m.mu.Unlock()

				companionManifest, cErr := m.DownloadArtifact(ctx, c.SourceURL, c.Filename, "", c.MaxSizeBytes, progress)
				if cErr != nil {
					err = fmt.Errorf("companion %s: %w", c.Filename, cErr)
					break
				}
				completed += companionManifest.SizeBytes
				installed = append(installed, ArtifactCompanion{Kind: c.Kind, Filename: c.Filename})

				// A companion is not a model. Recording it as one would put a
				// projector in the list of things to load.
				m.mu.Lock()
				companionManifest.Role = RoleCompanion
				_ = m.saveManifestLocked(companionManifest)
				m.mu.Unlock()
			}

			if err == nil && len(installed) > 0 {
				m.mu.Lock()
				manifest.Companions = installed
				_ = m.saveManifestLocked(manifest)
				m.mu.Unlock()
			}
		}

		m.mu.Lock()
		job.state.Filename = req.Filename
		job.state.FinishedAt = time.Now().Unix()
		switch {
		case err == nil:
			job.state.Status = "complete"
		case errors.Is(err, context.Canceled):
			job.state.Status = "cancelled"
		default:
			job.state.Status = "failed"
			job.state.Error = err.Error()
		}
		m.mu.Unlock()
	}()

	return id, nil
}

// DownloadStatus reports one job, or all of them when id is empty.
func (m *ArtifactManager) DownloadStatus(id string) ([]DownloadState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if id != "" {
		job, ok := m.downloads[id]
		if !ok {
			return nil, errors.New("download job not found")
		}
		return []DownloadState{job.state}, nil
	}

	states := make([]DownloadState, 0, len(m.downloads))
	for _, job := range m.downloads {
		states = append(states, job.state)
	}
	return states, nil
}

// CancelDownload stops an in-flight job.
func (m *ArtifactManager) CancelDownload(id string) error {
	m.mu.RLock()
	job, ok := m.downloads[id]
	m.mu.RUnlock()
	if !ok {
		return errors.New("download job not found")
	}
	job.cancel()
	return nil
}

func (m *ArtifactManager) ImportLocalArtifact(sourcePath, filename string) (*ArtifactManifest, error) {
	cleanName := filepath.Base(filename)
	if cleanName != filename || strings.Contains(filename, "..") || strings.Contains(filename, "/") {
		return nil, ErrPathTraversal
	}
	if !strings.HasSuffix(strings.ToLower(cleanName), ".gguf") {
		return nil, errors.New("only .gguf model weights are accepted")
	}

	info, err := os.Stat(sourcePath)
	if err != nil || info.IsDir() {
		return nil, errors.New("source file not found or is a directory")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	committed, err := m.totalCommittedLocked()
	if err != nil {
		return nil, err
	}
	if committed+info.Size() > m.maxBudget {
		return nil, ErrDiskBudgetExceeded
	}

	src, err := os.Open(sourcePath)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	destPath := filepath.Join(m.modelsDir, cleanName)
	dst, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer dst.Close()

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(dst, hasher), src)
	if err != nil {
		_ = os.Remove(destPath)
		return nil, err
	}

	actualHash := hex.EncodeToString(hasher.Sum(nil))
	manifest := &ArtifactManifest{
		ID:           "art-" + cleanName,
		Name:         cleanName,
		Filename:     cleanName,
		Path:         cleanName,
		Source:       SourceDownload,
		SizeBytes:    written,
		SHA256:       actualHash,
		ContextLimit: 4096,
		InstalledAt:  time.Now().Unix(),
	}

	_ = m.saveManifestLocked(manifest)
	return manifest, nil
}

// DeleteArtifact removes weights Woolwire installed. Weights it only
// discovered are refused: the models directory may be a cache shared with
// other tools, and deleting a file out of one corrupts it for all of them.
func (m *ArtifactManager) DeleteArtifact(ref string) error {
	rel, err := modelpath.Clean(ref)
	if err != nil {
		return ErrPathTraversal
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	manifestPath := filepath.Join(m.modelsDir, filepath.FromSlash(rel)+".manifest.json")
	if _, err := os.Stat(manifestPath); err != nil {
		return ErrNotManaged
	}

	if err := os.Remove(filepath.Join(m.modelsDir, filepath.FromSlash(rel))); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(manifestPath)
	return nil
}

// ListArtifacts returns every model the runner could be asked to load: the
// ones Woolwire downloaded, which carry a manifest, and the ones already in
// the models directory, which are found by scanning it.
func (m *ArtifactManager) ListArtifacts() ([]ArtifactManifest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	installed, err := m.installedArtifactsLocked()
	if err != nil {
		return nil, err
	}

	// A downloaded model is also sitting in the models directory, so the scan
	// finds it too. Its manifest is the better record — it has the verified
	// hash and the source URL — so it wins.
	claimed := make(map[string]bool, len(installed))
	for _, mf := range installed {
		if real, err := filepath.EvalSymlinks(filepath.Join(m.modelsDir, filepath.FromSlash(mf.Path))); err == nil {
			claimed[real] = true
		}
	}

	manifests := installed
	for _, mf := range m.discoverModels() {
		real, err := filepath.EvalSymlinks(filepath.Join(m.modelsDir, filepath.FromSlash(mf.Path)))
		if err == nil && claimed[real] {
			continue
		}
		manifests = append(manifests, mf)
	}
	return manifests, nil
}

// installedArtifactsLocked reads the sidecar manifests Woolwire writes beside
// the weights it downloads.
func (m *ArtifactManager) installedArtifactsLocked() ([]ArtifactManifest, error) {
	entries, err := os.ReadDir(m.modelsDir)
	if err != nil {
		return nil, err
	}

	manifests := make([]ArtifactManifest, 0)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".manifest.json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.modelsDir, e.Name()))
		if err != nil {
			continue
		}
		var mf ArtifactManifest
		if json.Unmarshal(b, &mf) != nil {
			continue
		}
		// Manifests written before models could live in subdirectories carry
		// only a filename.
		if mf.Path == "" {
			mf.Path = mf.Filename
		}
		if mf.Source == "" {
			mf.Source = SourceDownload
		}
		// A projector or draft module is installed alongside a model and
		// listed as part of it, never as something to load.
		if mf.Role == RoleCompanion {
			continue
		}
		// Check if weight file still exists
		if _, statErr := os.Stat(filepath.Join(m.modelsDir, filepath.FromSlash(mf.Path))); statErr != nil {
			continue
		}
		manifests = append(manifests, mf)
	}
	return manifests, nil
}

// getUsedSpaceLocked counts finished weights and partially written staging
// files alike. Excluding staging let two concurrent downloads each believe the
// whole remaining budget was theirs.
func (m *ArtifactManager) getUsedSpaceLocked() (int64, error) {
	total, err := m.installedSizeLocked()
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(m.stagingDir)
	if err != nil {
		return total, nil
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total, nil
}

// installedSizeLocked sums the weights Woolwire downloaded. The budget bounds
// what Woolwire puts on the disk, so weights it merely found are not charged
// against it — a shared model cache is routinely larger than any budget worth
// setting, and counting it would block every download.
func (m *ArtifactManager) installedSizeLocked() (int64, error) {
	entries, err := os.ReadDir(m.modelsDir)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".manifest.json") {
			continue
		}
		weight := strings.TrimSuffix(e.Name(), ".manifest.json")
		if info, err := os.Stat(filepath.Join(m.modelsDir, weight)); err == nil {
			total += info.Size()
		}
	}
	return total, nil
}

func (m *ArtifactManager) saveManifestLocked(mf *ArtifactManifest) error {
	path := filepath.Join(m.modelsDir, mf.Filename+".manifest.json")
	b, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
