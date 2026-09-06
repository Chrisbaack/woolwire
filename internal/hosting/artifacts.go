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
)

var (
	ErrDiskBudgetExceeded = errors.New("model download exceeds configured storage budget")
	ErrHashMismatch       = errors.New("downloaded artifact SHA-256 hash mismatch")
	ErrPathTraversal      = errors.New("invalid filename: path traversal prohibited")
)

type ArtifactManifest struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Filename     string `json:"filename"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA256       string `json:"sha256"`
	SourceURL    string `json:"source_url,omitempty"`
	ContextLimit int    `json:"context_limit"`
	InstalledAt  int64  `json:"installed_at"`
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

type ArtifactManager struct {
	modelsDir  string
	stagingDir string
	maxBudget  int64 // max total bytes allowed for models

	mu        sync.RWMutex
	reserved  int64 // bytes promised to in-flight downloads
	downloads map[string]*downloadJob

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
	return guardedClient(policy, 60*time.Second)
}

func NewArtifactManager(modelsDir string, maxBudget int64) (*ArtifactManager, error) {
	if modelsDir == "" {
		return nil, errors.New("models directory required")
	}
	if maxBudget <= 0 {
		maxBudget = 50 * (1 << 30) // default 50 GB
	}

	stagingDir := filepath.Join(modelsDir, ".staging")
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}

	return &ArtifactManager{
		modelsDir:  modelsDir,
		stagingDir: stagingDir,
		maxBudget:  maxBudget,
		downloads:  make(map[string]*downloadJob),
	}, nil
}

func (m *ArtifactManager) GetUsedDiskSpace() (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.getUsedSpaceLocked()
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

	reservation := maxSizeBytes
	if reservation <= 0 {
		reservation = 0
	}
	if err := m.reserve(reservation); err != nil {
		return nil, err
	}
	defer m.release(reservation)

	stagingPath := filepath.Join(m.stagingDir, fmt.Sprintf("dl-%d-%s", time.Now().UnixNano(), cleanName))
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

	budgetHeadroom, err := m.remainingBudget()
	if err != nil {
		return nil, err
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
			totalDownloaded += int64(n)
			if maxSizeBytes > 0 && totalDownloaded > maxSizeBytes {
				return nil, ErrDiskBudgetExceeded
			}
			if totalDownloaded > budgetHeadroom {
				return nil, ErrDiskBudgetExceeded
			}

			if _, err := outFile.Write(buf[:n]); err != nil {
				return nil, fmt.Errorf("write staging file: %w", err)
			}
			hasher.Write(buf[:n])

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
		SizeBytes:    totalDownloaded,
		SHA256:       actualHash,
		SourceURL:    sourceURL,
		ContextLimit: 4096,
		InstalledAt:  time.Now().Unix(),
	}
	if renameErr == nil {
		_ = m.saveManifestLocked(manifest)
	}
	m.mu.Unlock()

	if renameErr != nil {
		return nil, fmt.Errorf("finalize model move: %w", renameErr)
	}
	return manifest, nil
}

// reserve books budget for an in-flight download so a second one starting at
// the same moment sees the space as already spoken for.
func (m *ArtifactManager) reserve(bytes int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	used, err := m.getUsedSpaceLocked()
	if err != nil {
		return err
	}
	if bytes > 0 && used+m.reserved+bytes > m.maxBudget {
		return ErrDiskBudgetExceeded
	}
	m.reserved += bytes
	return nil
}

func (m *ArtifactManager) release(bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reserved -= bytes
	if m.reserved < 0 {
		m.reserved = 0
	}
}

func (m *ArtifactManager) remainingBudget() (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	used, err := m.getUsedSpaceLocked()
	if err != nil {
		return 0, err
	}
	remaining := m.maxBudget - used
	if remaining < 0 {
		return 0, nil
	}
	return remaining, nil
}

// StartDownload runs a download in the background and returns immediately with
// a job id the UI polls. A synchronous multi-gigabyte transfer inside an HTTP
// handler is not something a browser or a reverse proxy will wait through.
func (m *ArtifactManager) StartDownload(sourceURL, filename, expectedSHA256 string, maxSizeBytes int64) (string, error) {
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", err
	}
	id := "dl-" + hex.EncodeToString(idBytes)

	ctx, cancel := context.WithCancel(context.Background())
	job := &downloadJob{
		state: DownloadState{
			ID:         id,
			Filename:   filename,
			SourceURL:  sourceURL,
			Status:     "downloading",
			TotalBytes: maxSizeBytes,
			StartedAt:  time.Now().Unix(),
		},
		cancel: cancel,
	}

	m.mu.Lock()
	m.downloads[id] = job
	m.mu.Unlock()

	go func() {
		defer cancel()
		_, err := m.DownloadArtifact(ctx, sourceURL, filename, expectedSHA256, maxSizeBytes, func(n int64) {
			m.mu.Lock()
			job.state.BytesDownloaded = n
			m.mu.Unlock()
		})

		m.mu.Lock()
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

	used, _ := m.getUsedSpaceLocked()
	if used+info.Size() > m.maxBudget {
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
		SizeBytes:    written,
		SHA256:       actualHash,
		ContextLimit: 4096,
		InstalledAt:  time.Now().Unix(),
	}

	_ = m.saveManifestLocked(manifest)
	return manifest, nil
}

func (m *ArtifactManager) DeleteArtifact(filename string) error {
	cleanName := filepath.Base(filename)
	if cleanName != filename || strings.Contains(filename, "..") {
		return ErrPathTraversal
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	target := filepath.Join(m.modelsDir, cleanName)
	_ = os.Remove(target)
	_ = os.Remove(filepath.Join(m.modelsDir, cleanName+".manifest.json"))
	return nil
}

func (m *ArtifactManager) ListArtifacts() ([]ArtifactManifest, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries, err := os.ReadDir(m.modelsDir)
	if err != nil {
		return nil, err
	}

	manifests := make([]ArtifactManifest, 0)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".manifest.json") {
			path := filepath.Join(m.modelsDir, e.Name())
			if b, err := os.ReadFile(path); err == nil {
				var mf ArtifactManifest
				if json.Unmarshal(b, &mf) == nil {
					// Check if weight file still exists
					if _, statErr := os.Stat(filepath.Join(m.modelsDir, mf.Filename)); statErr == nil {
						manifests = append(manifests, mf)
					}
				}
			}
		}
	}
	return manifests, nil
}

// getUsedSpaceLocked counts finished weights and partially written staging
// files alike. Excluding staging let two concurrent downloads each believe the
// whole remaining budget was theirs.
func (m *ArtifactManager) getUsedSpaceLocked() (int64, error) {
	var total int64
	for _, dir := range []string{m.modelsDir, m.stagingDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if dir == m.modelsDir {
				return 0, err
			}
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
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
