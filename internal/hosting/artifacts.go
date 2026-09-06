package hosting

import (
	"context"
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

type ArtifactManager struct {
	modelsDir    string
	stagingDir   string
	maxBudget    int64 // max total bytes allowed for models
	mu           sync.RWMutex
	httpClient   *http.Client
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

	client := &http.Client{
		Timeout: 0, // bounded by context
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errors.New("redirects prohibited during model download")
		},
	}

	return &ArtifactManager{
		modelsDir:  modelsDir,
		stagingDir: stagingDir,
		maxBudget:  maxBudget,
		httpClient: client,
	}, nil
}

func (m *ArtifactManager) GetUsedDiskSpace() (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var total int64
	entries, err := os.ReadDir(m.modelsDir)
	if err != nil {
		return 0, err
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

func (m *ArtifactManager) DownloadArtifact(
	ctx context.Context,
	sourceURL string,
	filename string,
	expectedSHA256 string,
	maxSizeBytes int64,
	onProgress func(bytesDownloaded int64),
) (*ArtifactManifest, error) {
	// 1. Destination validation to prevent SSRF
	if err := ValidateDestination(sourceURL); err != nil {
		return nil, fmt.Errorf("source URL validation failed: %w", err)
	}
	if !strings.HasPrefix(sourceURL, "https://") {
		return nil, errors.New("model downloads must use HTTPS")
	}

	// 2. Filename validation
	cleanName := filepath.Base(filename)
	if cleanName != filename || strings.Contains(filename, "..") || strings.Contains(filename, "/") || strings.Contains(filename, "\\") {
		return nil, ErrPathTraversal
	}
	if !strings.HasSuffix(strings.ToLower(cleanName), ".gguf") {
		return nil, errors.New("only .gguf model weights are accepted")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 3. Storage budget check
	used, _ := m.getUsedSpaceLocked()
	if maxSizeBytes > 0 && used+maxSizeBytes > m.maxBudget {
		return nil, ErrDiskBudgetExceeded
	}

	// 4. Create staging file
	stagingPath := filepath.Join(m.stagingDir, fmt.Sprintf("dl-%d-%s", time.Now().UnixNano(), cleanName))
	outFile, err := os.OpenFile(stagingPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create staging file: %w", err)
	}
	defer func() {
		_ = outFile.Close()
		_ = os.Remove(stagingPath) // clean up staging file if not renamed
	}()

	req, err := http.NewRequestWithContext(ctx, "GET", sourceURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download server returned %d", resp.StatusCode)
	}

	// 5. Stream download, hash, and enforce budget limit
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
			if used+totalDownloaded > m.maxBudget {
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

	_ = outFile.Close()

	// 6. Atomic move into finalized model directory
	finalPath := filepath.Join(m.modelsDir, cleanName)
	if err := os.Rename(stagingPath, finalPath); err != nil {
		return nil, fmt.Errorf("finalize model move: %w", err)
	}

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

	_ = m.saveManifestLocked(manifest)
	return manifest, nil
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

func (m *ArtifactManager) getUsedSpaceLocked() (int64, error) {
	var total int64
	entries, err := os.ReadDir(m.modelsDir)
	if err != nil {
		return 0, err
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

func (m *ArtifactManager) saveManifestLocked(mf *ArtifactManifest) error {
	path := filepath.Join(m.modelsDir, mf.Filename+".manifest.json")
	b, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
