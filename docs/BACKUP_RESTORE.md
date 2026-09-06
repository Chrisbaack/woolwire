# Backup and Disaster Recovery Guide

## 1. Overview & Storage Architecture

Woolwire stores all state in a single SQLite database under the state
directory given by `-state` (or `WOOLWIRE_STATE_DIR`):
- Container deployment: `/state/woolwire.db`
- Local run: `./state/woolwire.db` by default

The database file is created `0600` on every open, because it holds private
keys.

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

### Method A: Live Encrypted Export via Local API (Recommended)

While Woolwire is running, an authenticated local request to
`POST /api/v1/backup/export` takes a non-blocking SQLite `VACUUM INTO`
snapshot and seals it with a key derived from a passphrase you supply.

A passphrase is required, not optional. The plaintext snapshot contains the
device private key, the Tailcat node key, the session token, the room
authority key on a creator, and every configured endpoint API key. An
unencrypted copy of that on a NAS or in a cloud sync folder is a full
compromise of the installation.

```bash
curl -X POST http://127.0.0.1:7070/api/v1/backup/export \
  -H "Cookie: woolwire_session=<session_token>" \
  -H "Content-Type: application/json" \
  -H "X-Woolwire-Request: 1" \
  -d '{"passphrase":"a passphrase of at least 12 characters"}'
```

Output:
```json
{
  "ok": true,
  "backup_path": "/state/backups/woolwire-backup-20260905-153000.age",
  "size_bytes": 147512,
  "encrypted": true,
  "created_at": 1788635400
}
```

The export is written under the **state directory**, so a container backup
lands inside the mounted volume rather than in the container's ephemeral
working directory.

File format: `WOOLWIRE-BACKUP-1\n` || 16-byte salt || 24-byte nonce ||
NaCl secretbox ciphertext. The key is Argon2id over the passphrase and salt
(time 3, memory 64 MiB, 4 threads). The plaintext inside is the crash-
consistent, compacted SQLite snapshot.

Keep the passphrase somewhere the backup is not. Losing it makes the backup
unrecoverable; there is no escrow.

### Method B: Offline Cold Backup

1. Stop the Woolwire service:
   ```bash
   docker compose -f deploy/base/compose.yaml down
   ```
2. Copy the entire data directory including WAL files:
   ```bash
   tar -czvf woolwire-backup-$(date +%Y%m%d).tar.gz /path/to/woolwire-state/
   ```
3. Restart the service:
   ```bash
   docker compose -f deploy/base/compose.yaml up -d
   ```

### Method C: SQLite CLI Hot Snapshot

```bash
sqlite3 /state/woolwire.db ".backup /state/backups/woolwire-hot-backup.db"
```

---

## 3. Restore Procedures

### Restoring to an Existing or New Installation

1. Stop the running Woolwire instance:
   ```bash
   docker compose -f deploy/base/compose.yaml down
   ```
2. Decrypt the export (skip for a Method B or C copy, which is already a
   plain `.db`). Any age-compatible tool will not read this format; use the
   Woolwire helper, which is the same code path the export uses:
   ```bash
   go run ./hack/backup-decrypt -in woolwire-backup-20260905-153000.age \
     -out woolwire-restored.db
   # prompts for the passphrase
   ```
3. Verify integrity of the backup file:
   ```bash
   sqlite3 /path/to/backup.db "PRAGMA integrity_check;"
   # Expected output: ok
   ```
4. Replace the target database file with the backup:
   ```bash
   cp /path/to/restored.db /state/woolwire.db
   # Clean up any leftover stale WAL or SHM files
   rm -f /state/woolwire.db-wal /state/woolwire.db-shm
   ```
5. Set strict file permissions (contains private Ed25519 keys). Woolwire also
   does this itself on every open:
   ```bash
   chmod 600 /state/woolwire.db
   ```
6. Start Woolwire:
   ```bash
   docker compose -f deploy/base/compose.yaml up -d
   ```
7. Verify status by requesting `GET /api/v1/state` or opening the web dashboard at `http://127.0.0.1:7070`.

Restoring the database also restores the transport identity: the node key,
the pre-shared key, and the pinned DERP region all live in it, so the node
comes back at the same Tailcat address and peers' stored addresses and the
invitation code stay valid.

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
   sqlite3 /state/woolwire.db ".recover" | sqlite3 /state/woolwire-recovered.db
   ```
2. If recovery succeeds, replace `woolwire.db`.
3. If recovery fails, revert to the most recent `woolwire-backup-*.age`,
   decrypting it first as in section 3.

### Scenario 3: Accidental Deletion of Model Weights
Model GGUF files in `/models` do not contain private keys. If lost:
1. Open the **Settings** panel in the Web UI.
2. In **Managed Models**, re-download the desired model weights using the verified HTTPS URL and SHA-256 hash.
3. The manifest and download manager will automatically verify the checksum and restore the model runner.
