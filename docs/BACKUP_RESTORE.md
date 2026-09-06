# Backup and Disaster Recovery Guide

## 1. Overview & Storage Architecture

Woolwire stores all state in a single SQLite database located by default at:
- Container deployment: `/data/woolwire.db`
- Local CLI deployment: `~/.local/share/woolwire/woolwire.db` (or custom `-db-path`)

The database uses Write-Ahead Logging (`journal_mode=WAL`) and foreign key enforcement. State stored in this file includes:
1. **Device Identity & Keys:** Local private/public keypair, Tailcat private key, and certificate DER.
2. **Room Credentials:** Authority keys (for room creators), membership roster, and admission secret verifiers.
3. **Local Settings:** Display name, session authentication token, contribution opt-out preferences, and local model configs.
4. **Conversations & Messages:** Private requester-side LLM chats (never synchronized across peers).
5. **Community Events:** Signed channel messages, edits, deletions, tombstones, and local read state.
6. **Contribution Receipts:** Jointly signed completion receipts.

Large GGUF model artifacts are stored separately in `/models` and can be re-downloaded or backed up independently.

---

## 2. Backup Procedures

### Method A: Live Export via Local API (Recommended)

While Woolwire is running, an authenticated local request to `POST /api/v1/backup/export` performs a non-blocking SQLite `VACUUM INTO` snapshot:

```bash
curl -X POST http://127.0.0.1:7070/api/v1/backup/export \
  -H "Cookie: woolwire_session=<session_token>"
```

Output:
```json
{
  "ok": true,
  "backup_path": "data/backups/woolwire-backup-20260905-153000.db",
  "size_bytes": 147456,
  "created_at": 1788635400
}
```

This snapshot is crash-consistent, compacts unused pages, and flushes the WAL log into a single self-contained `.db` file.

### Method B: Offline Cold Backup

1. Stop the Woolwire service:
   ```bash
   docker compose -f deploy/base/compose.yaml down
   ```
2. Copy the entire data directory including WAL files:
   ```bash
   tar -czvf woolwire-backup-$(date +%Y%m%d).tar.gz /path/to/woolwire-data/
   ```
3. Restart the service:
   ```bash
   docker compose -f deploy/base/compose.yaml up -d
   ```

### Method C: SQLite CLI Hot Snapshot

```bash
sqlite3 /data/woolwire.db ".backup /data/backups/woolwire-hot-backup.db"
```

---

## 3. Restore Procedures

### Restoring to an Existing or New Installation

1. Stop the running Woolwire instance:
   ```bash
   docker compose -f deploy/base/compose.yaml down
   ```
2. Verify integrity of the backup file:
   ```bash
   sqlite3 /path/to/backup.db "PRAGMA integrity_check;"
   # Expected output: ok
   ```
3. Replace the target database file with the backup:
   ```bash
   cp /path/to/backup.db /data/woolwire.db
   # Clean up any leftover stale WAL or SHM files
   rm -f /data/woolwire.db-wal /data/woolwire.db-shm
   ```
4. Set strict file permissions (contains private Ed25519 keys):
   ```bash
   chmod 600 /data/woolwire.db
   ```
5. Start Woolwire:
   ```bash
   docker compose -f deploy/base/compose.yaml up -d
   ```
6. Verify status by requesting `GET /api/v1/state` or opening the web dashboard at `http://127.0.0.1:7070`.

---

## 4. Disaster Recovery Scenarios

### Scenario 1: Creator Machine Offline or Hardware Failure
- **Non-creator members:** Existing admitted members continue communicating, exchanging community events, and serving inference directly to one another without the creator present.
- **Roster & Invitations:** The room invitation code cannot issue new admissions until the creator restores their database.
- **Recovery:** Restore the creator's backup `.db` to a new host with the same network connectivity. When the creator restarts, peers re-establish contact over Tailcat and roster synchronization resumes automatically.

### Scenario 2: Database File Corruption
If the database fails `PRAGMA integrity_check`:
1. Attempt SQLite recovery to a fresh file:
   ```bash
   sqlite3 /data/woolwire.db ".recover" | sqlite3 /data/woolwire-recovered.db
   ```
2. If recovery succeeds, replace `woolwire.db`.
3. If recovery fails, revert to the most recent daily `woolwire-backup-*.db`.

### Scenario 3: Accidental Deletion of Model Weights
Model GGUF files in `/models` do not contain private keys. If lost:
1. Open the **Settings** panel in the Web UI.
2. In **Managed Models**, re-download the desired model weights using the verified HTTPS URL and SHA-256 hash.
3. The manifest and download manager will automatically verify the checksum and restore the model runner.
