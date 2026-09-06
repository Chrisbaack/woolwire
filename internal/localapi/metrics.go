package localapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
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
	Denials struct {
		UnauthorizedCount  int64 `json:"unauthorized_count"`
		QueueOverflowCount int64 `json:"queue_overflow_count"`
	} `json:"denials"`
	TelemetryEnabled bool `json:"telemetry_enabled"`
}

func (s *Server) handleGetMetrics(w http.ResponseWriter, r *http.Request) {
	var resp MetricsResponse
	resp.TelemetryEnabled = false // Telemetry is strictly disabled

	// Connectivity
	dev, _ := s.store.GetDeviceIdentity()
	if dev != nil {
		resp.Connectivity.TailcatAddr = dev.TailcatKey
	}
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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleExportBackup(w http.ResponseWriter, r *http.Request) {
	backupDir := "data/backups"
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		http.Error(w, "failed to create backup directory", http.StatusInternalServerError)
		return
	}

	timestamp := time.Now().Format("20060102-150405")
	backupPath := filepath.Join(backupDir, fmt.Sprintf("woolwire-backup-%s.db", timestamp))

	if err := s.store.Backup(backupPath); err != nil {
		http.Error(w, fmt.Sprintf("backup failed: %v", err), http.StatusInternalServerError)
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
		"created_at":  time.Now().Unix(),
	})
}
