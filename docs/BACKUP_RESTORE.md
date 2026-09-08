# Backup and restore

Woolwire keeps its installation in `woolwire.db` under `-state` /
`WOOLWIRE_STATE_DIR`: normally `./state` natively or `/state` inside a container.
The database includes device and transport keys, room authority material on the
creator, owner sessions, endpoint credentials, settings, saved chats, shared
room data, and receipts. Protect a backup as you would the installation itself.

GGUF weights live separately in the models directory. Back them up independently
or keep enough source information to download them again. No-save transcript
bodies are not in a database backup.

## Live encrypted export

Use the authenticated cookie jar from the [API guide](API.md#pair-an-api-session).
This Bash example reads a passphrase without putting its literal value in shell
history and posts it to the owner API:

```bash
read -r -s -p 'Backup passphrase (at least 12 characters): ' WOOLWIRE_BACKUP_PASSPHRASE
printf '\n'
printf '%s' "$WOOLWIRE_BACKUP_PASSPHRASE" |
  python3 -c 'import json,sys; print(json.dumps({"passphrase":sys.stdin.read()}))' |
  curl --fail-with-body -sS -b "$WOOLWIRE_COOKIE_JAR" \
    -H 'X-Woolwire-Request: 1' -H 'Content-Type: application/json' \
    --data-binary @- "$WOOLWIRE_URL/api/v1/backup/export"
unset WOOLWIRE_BACKUP_PASSPHRASE
```

The response includes `backup_path`, `size_bytes`, `created_at`, and
`encrypted:true`. It names a file on the **server filesystem**, for example
`/state/backups/woolwire-backup-20260907-120000.age`. It does not download the file
to the browser or API client.

For a supplied container profile, copy that encrypted file off the volume:

```sh
mkdir -p backups
# Replace the filename with the actual backup_path returned by the API.
docker cp woolwire-app:/state/backups/woolwire-backup-20260907-120000.age ./backups/
```

Use `podman cp` for Podman. Copy native exports from the configured state
folder's `backups/` subdirectory. Keep a separate copy outside the host and keep
the passphrase elsewhere. There is no passphrase recovery.

The export uses SQLite `VACUUM INTO` to create a consistent snapshot before
sealing it. The file's `.age` suffix is historical: **this is not the age format**.
Its layout is `WOOLWIRE-BACKUP-1\n`, a 16-byte salt, a 24-byte nonce, and NaCl
secretbox ciphertext. Argon2id uses time 3, memory 64 MiB, and 4 threads.

## Offline copy

Stop the app before copying a plain state directory. SQLite uses WAL mode;
include the entire directory rather than copying only a live `.db` file.
For a base Compose installation:

```sh
docker compose -f deploy/base/compose.yaml stop woolwire
mkdir -p backups
docker cp woolwire-app:/state ./backups/cold-state
docker compose -f deploy/base/compose.yaml start woolwire
```

Choose a fresh destination for each copy. Use the managed file and environment
options if that is your installation. For a native app, stop the process and
copy the whole configured state directory. An offline copy is **unencrypted**;
store it on protected/encrypted storage. Never commit it.

## Decrypt and inspect

From a source checkout with Go installed:

```sh
go run ./hack/backup-decrypt \
  -in ./backups/woolwire-backup-20260907-120000.age \
  -out ./backups/woolwire-restored.db
# The helper prompts for the passphrase.
sqlite3 ./backups/woolwire-restored.db 'PRAGMA integrity_check;'
```

Expected integrity result: `ok`. Use a fresh output filename: the helper overwrites an existing
output file. The restored `.db` is sensitive plaintext. Use a secure temporary
location and remove unneeded plaintext copies after the restore is verified.
The SQLite CLI is a separate tool; the app does not require it for normal use.

## Restore a native installation

1. Stop the app and keep a cold copy of its current state before replacing it.
2. Verify the decrypted backup's integrity.
3. Copy the restored database into the **actual state directory**. For the
   default native location, with the app stopped:

   ```sh
   cp ./backups/woolwire-restored.db ./state/woolwire.db
   rm -f ./state/woolwire.db-wal ./state/woolwire.db-shm
   chmod 600 ./state/woolwire.db
   ```

4. Ensure the directory and file belong to the user that runs Woolwire.
5. Start the matching application version against that state directory. Open the
   UI and verify the room, saved chats, and connectivity.

The stale WAL/SHM removal above applies only after replacing the database with a
consistent snapshot while stopped. Never delete sidecars from a live database.

## Restore a Compose installation

Use the same Compose project/file that owns the state volume. The container must
exist for `docker cp`; use `stop`, not `down`, in this procedure. For the base
profile:

```sh
docker compose -f deploy/base/compose.yaml stop woolwire
# Keep the old stopped state in a fresh destination before replacing it.
docker cp woolwire-app:/state ./backups/pre-restore-state
docker cp ./backups/woolwire-restored.db woolwire-app:/state/woolwire.db
# Run only a maintenance shell with the profile's state volume mounted.
docker compose -f deploy/base/compose.yaml run --rm --no-deps \
  --user 0:0 --entrypoint sh woolwire -c \
  'rm -f /state/woolwire.db-wal /state/woolwire.db-shm && chown 1000:1000 /state/woolwire.db && chmod 600 /state/woolwire.db'
docker compose -f deploy/base/compose.yaml start woolwire
```

For managed hosting, use
`--env-file deploy/managed/.env -f deploy/managed/compose.yaml` consistently in
these commands and stop the runner during recovery too. Restart both after
verification. Use equivalent `podman` commands for that runtime. The maintenance
shell fixes file ownership because copying into a container can create a file
owned by root; it does not start the Woolwire server.

Do not change the Compose project name or create a fresh state volume while
trying to restore an existing installation. A bind-mounted state directory can
instead be restored directly on the host, accounting for rootless UID mappings.

## Recovery limits

A database restore brings back the original transport and room identity. Do not
run the original and restored copies simultaneously. The creator's backup is
needed to recover admission authority; another member cannot automatically take
over. Existing admitted members can continue without the creator while reachable.

An older backup may contain stale membership, invitations, or credentials.
Reconnect and verify current state before relying on it. Tokens present in a
compromised backup should be treated as exposed. A browser that no longer has a
valid owner session can be paired again using the
[operator recovery procedure](TROUBLESHOOTING.md#i-lost-the-setup-secret-and-all-signed-in-browsers).

For version rollback, use the older application with its matching pre-upgrade
backup. Database migration downgrade is not supported. A backup is useful only
if you have exercised the restore procedure on your deployment.
