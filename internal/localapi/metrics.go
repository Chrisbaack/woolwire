package localapi

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/nacl/secretbox"
)

type MetricsResponse struct {
	Connectivity struct {
		TailcatAddr string `json:"tailcat_addr"`
		KnownPeers  int    `json:"known_peers"`
	} `json:"connectivity"`
	Storage struct {
		DatabaseSizeBytes  int64 `json:"database_size_bytes"`
		ArtifactsSizeBytes int64 `json:"artifacts_size_bytes"`
		ArtifactCount      int   `json:"artifact_count"`
	} `json:"storage"`
	Engine struct {
		Configured   bool   `json:"configured"`
		RunnerStatus string `json:"runner_status"`
		LoadedModel  string `json:"loaded_model,omitempty"`
	} `json:"engine"`
	Queue struct {
		Active int `json:"active"`
		Queued int `json:"queued"`
	} `json:"queue"`
	Denials struct {
		UnauthorizedCount  int64 `json:"unauthorized_count"`
		QueueOverflowCount int64 `json:"queue_overflow_count"`
	} `json:"denials"`
	TelemetryEnabled bool `json:"telemetry_enabled"`
}

func (s *Server) handleGetMetrics(w http.ResponseWriter, r *http.Request) {
	var resp MetricsResponse
	resp.TelemetryEnabled = false // Telemetry is strictly disabled

	// The transport's public address, never the persisted node private key
	// that also lives on the device identity row.
	resp.Connectivity.TailcatAddr = s.trans.Address()
	peers, _ := s.store.ListPeerAddresses()
	resp.Connectivity.KnownPeers = len(peers)

	// Storage
	resp.Storage.DatabaseSizeBytes = s.store.DatabaseSize()
	if s.artifactMgr != nil {
		manifests, err := s.artifactMgr.ListArtifacts()
		if err == nil {
			resp.Storage.ArtifactCount = len(manifests)
			var totalSize int64
			for _, m := range manifests {
				totalSize += m.SizeBytes
			}
			resp.Storage.ArtifactsSizeBytes = totalSize
		}
	}

	// Engine
	if s.runnerClient != nil {
		resp.Engine.Configured = true
		health, err := s.runnerClient.Health(r.Context())
		if err == nil && health != nil {
			resp.Engine.RunnerStatus = health.Status
			resp.Engine.LoadedModel = health.LoadedModelID
		} else {
			resp.Engine.RunnerStatus = "unreachable"
		}
	} else {
		resp.Engine.Configured = false
		resp.Engine.RunnerStatus = "unconfigured"
	}

	resp.Queue.Active, resp.Queue.Queued = s.infer.Queue().Stats()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// backupMagic identifies an encrypted Woolwire export. It is deliberately not
// a SQLite header: the plaintext database carries the device private key, the
// Tailcat node key, the session token, and every endpoint API key.
var backupMagic = []byte("WOOLWIRE-BACKUP-1\n")

const (
	backupSaltSize  = 16
	backupNonceSize = 24
	// Argon2id parameters sized for an interactive passphrase prompt on a
	// small home server.
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 4
)

func (s *Server) handleExportBackup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if len(body.Passphrase) < 12 {
		http.Error(w, "a passphrase of at least 12 characters is required to encrypt the backup", http.StatusBadRequest)
		return
	}

	// Backups live under the state directory. Writing to a path relative to
	// the working directory put container backups outside the mounted volume,
	// where they vanished with the container.
	backupDir := s.backupDir()
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		http.Error(w, "failed to create backup directory", http.StatusInternalServerError)
		return
	}

	timestamp := time.Now().Format("20060102-150405")
	plainPath := filepath.Join(backupDir, fmt.Sprintf(".woolwire-%s.sqlite", timestamp))
	backupPath := filepath.Join(backupDir, fmt.Sprintf("woolwire-backup-%s.age", timestamp))

	if err := s.store.Backup(plainPath); err != nil {
		http.Error(w, fmt.Sprintf("backup failed: %v", err), http.StatusInternalServerError)
		return
	}
	// The plaintext snapshot exists only long enough to be encrypted.
	defer os.Remove(plainPath)

	if err := encryptBackup(plainPath, backupPath, body.Passphrase); err != nil {
		_ = os.Remove(backupPath)
		http.Error(w, fmt.Sprintf("encrypt backup failed: %v", err), http.StatusInternalServerError)
		return
	}

	info, _ := os.Stat(backupPath)
	var size int64
	if info != nil {
		size = info.Size()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":          true,
		"backup_path": backupPath,
		"size_bytes":  size,
		"encrypted":   true,
		"created_at":  time.Now().Unix(),
	})
}

func (s *Server) backupDir() string {
	base := s.stateDir
	if base == "" {
		base = "."
	}
	return filepath.Join(base, "backups")
}

// encryptBackup seals the snapshot with a passphrase-derived key. The file
// layout is magic || salt || nonce || secretbox ciphertext.
func encryptBackup(plainPath, destPath, passphrase string) error {
	plaintext, err := os.ReadFile(plainPath)
	if err != nil {
		return err
	}

	salt := make([]byte, backupSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	var nonce [backupNonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}

	var key [32]byte
	copy(key[:], argon2.IDKey([]byte(passphrase), salt, argonTime, argonMemory, argonThreads, 32))

	out := make([]byte, 0, len(backupMagic)+len(salt)+len(nonce)+len(plaintext)+secretbox.Overhead)
	out = append(out, backupMagic...)
	out = append(out, salt...)
	out = append(out, nonce[:]...)
	out = secretbox.Seal(out, plaintext, &nonce, &key)

	return os.WriteFile(destPath, out, 0o600)
}

// DecryptBackup reverses encryptBackup. It is exported so the restore path and
// its tests share exactly one implementation.
func DecryptBackup(r io.Reader, passphrase string) ([]byte, error) {
	blob, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	header := len(backupMagic) + backupSaltSize + backupNonceSize
	if len(blob) < header || string(blob[:len(backupMagic)]) != string(backupMagic) {
		return nil, errors.New("not a Woolwire encrypted backup")
	}

	salt := blob[len(backupMagic) : len(backupMagic)+backupSaltSize]
	var nonce [backupNonceSize]byte
	copy(nonce[:], blob[len(backupMagic)+backupSaltSize:header])

	var key [32]byte
	copy(key[:], argon2.IDKey([]byte(passphrase), salt, argonTime, argonMemory, argonThreads, 32))

	plaintext, ok := secretbox.Open(nil, blob[header:], &nonce, &key)
	if !ok {
		return nil, errors.New("backup passphrase is incorrect or the file is corrupt")
	}
	return plaintext, nil
}
