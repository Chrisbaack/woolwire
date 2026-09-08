# Troubleshooting

Use the same Compose file, environment file, and project name you used to start
the app. Check `logs woolwire` for the app and `logs runner` for managed inference.
Redact setup secrets, invitation codes, and credentials before sharing output.

## I cannot open the UI

- Open `http://127.0.0.1:7070` on the server itself. A different device needs a
  LAN bind as described in [Getting started](GETTING_STARTED.md#access-from-another-device).
- Check that the process/container is running and port `7070` is free. The base
  and managed profiles both use the name `woolwire-app`; stop the previous one
  before switching, and preserve its state volume.
- Startup initializes Tailcat before starting the HTTP listener. If logs stop at
  transport initialization, check outbound rendezvous/DERP connectivity.
- A managed app needs both its egress and internal networks. Attaching it only to
  an internal network prevents the intended outbound connectivity.
- `invalid host header` means the browser hostname is not allowed. Configure
  `WOOLWIRE_ALLOWED_HOSTS` for that name; changing the bind address alone does
  not allow arbitrary hostnames.

## I lost the setup secret and all signed-in browsers

If a signed-in browser still exists, mint a new secret with Connect Device.
Otherwise, an operator with control of the process can use the startup override.
For a native installation:

```sh
# Stop the existing process first; keep the existing state directory.
export WOOLWIRE_SETUP_TOKEN="$(openssl rand -hex 32)"
printf 'Temporary setup secret: %s\n' "$WOOLWIRE_SETUP_TOKEN"
./bin/woolwire -state ./state
# Sign in with the temporary secret, then stop the app with Ctrl+C.
unset WOOLWIRE_SETUP_TOKEN
./bin/woolwire -state ./state
```

Do this in a private terminal. Removing the override prevents the same secret
being re-armed on the next restart. For containers, temporarily add
`WOOLWIRE_SETUP_TOKEN` to the app service environment, recreate, pair, then
remove the variable and recreate again. An entry in `.env` alone is insufficient
unless the Compose service passes it through. Do not delete your database to
reset browser access.

`410 Gone` during pairing means the secret was already redeemed. `429` indicates
a setup rate limit; follow `Retry-After`. A fresh setup secret does not revoke
existing signed-in sessions.

## Joining fails or remains pending

Check that the creator is online, the invitation is current, and each process
can reach the transport. With approval mode enabled, the creator must approve
the device in Room Admin and the joiner must retry. Rotating invitations does
not disconnect existing members, but invalidates the old code for new joins.

If an existing peer cannot connect after a reinstall, verify that the original
state directory was restored. A new empty state directory creates a new
identity. Do not run two live installations from copies of the same identity.

## The Meadow is empty or a model disappears

Join or host a room first. Registering an external endpoint requires the model
to be enabled and published. Managed weights must be loaded successfully, not
just present on disk. Clear the search and Ready-only filter.

Unavailable or stale advertisements drop out of the catalog. Check the actual
host and runner status and refresh. Loading a different managed model replaces
the previous one. After a runner restart, load the model again if it is not
resident. A room creator being online does not mean another member's model is.

## An external endpoint test fails

The endpoint is contacted from the app process/container. Container loopback is
not host loopback. Use the correct bridge/host address and confirm the model
server listens there. See [container networking](GETTING_STARTED.md#external-servers-and-container-networking).

Private addresses and some ports require **Allow Private Network** for that
specific endpoint. The setting permits private HTTP as well; prefer TLS for
traffic leaving the machine. Redirecting API URLs are refused. Discovery and
inference have different routes, so use the backend base URL and exact model
identifier rather than a full `/chat/completions` URL.

A test that returns HTTP 200 can still contain `{"ok":false,"error":"..."}`;
inspect the JSON result.

## The runner cannot start, load, or see a GPU

- `WOOLWIRE_RUNNER_TOKEN` is required by managed Compose. Use the intended `.env`
  with `--env-file`; never paste the token into an issue.
- The runner reads `RUNNER_TOKEN`; it must equal the app's token.
- An NVIDIA reservation fails before startup on hosts without a configured GPU
  runtime. Follow the [CPU/Podman variants](GETTING_STARTED.md#cpu-and-podman-gpu-variants).
- App-side hardware detection may show no GPU even though the runner has one.
  Inspect runner health/logs; GPU access is assigned only to that container.
- Check that the runner can read the same relative filename under its models
  root. Use an artifact's `path`, not an absolute host path.
- Container defaults cap memory at 8192 MiB. Increase ceilings deliberately or
  use smaller weights/context. A model fitting in RAM may still be partly
  offloaded if VRAM is insufficient.
- `gpu_layers: 0` explicitly disables offload; omit it for automatic selection.
- A native runner also needs a valid `-engine-path` and free engine port.

## Downloads fail or disk usage looks wrong

A read-only models mount permits serving but not downloading. Inspect
`GET /api/v1/managed-models/storage` for `configured`, `read_only`, `used_bytes`,
and `budget_bytes`. The budget is not free host disk space, and usage can report
zero if its scan fails. Check permissions and the actual filesystem too.

Verify the source URL, available disk, and checksum. The Hugging Face resolver
is for publicly accessible repositories; gated/private repository credentials
are not an exposed resolver option. Do not assume interrupted downloads resume
across process restarts. Discovered files cannot be deleted through the app;
manage shared caches with their owning tools.

## API calls return 401 or 403

Owner `/api/v1` calls require the `woolwire_session` cookie. The compatible `/v1`
routes instead require the local API bearer token. Neither accepts a room
invitation as authentication.

State-changing local calls, **including `/v1/chat/completions` and setup**, need
`X-Woolwire-Request: 1` or a matching Origin. JSON bodies need
`Content-Type: application/json`. The [API examples](API.md) include all headers.
A model client that only lets you set a URL and API key may need custom-header
support to work with this interface.

## Streaming stalls or cancellation seems late

Use `curl -N` for streaming clients. A reverse proxy must forward SSE without
buffering and allow a response to remain open for the configured queue and
execution timeouts. Blank lines delimit events; do not parse HTTP chunks as
complete SSE messages. Read named error events even after HTTP 200.

Queued requests may wait for another member's work. Cancellation asks the host
to stop a request but cannot guarantee an external backend aborts computation.
No `[DONE]` after a disconnect means completion is uncertain; avoid blindly
replaying the same prompt.

## My UI changes do not appear

The Go binary embeds frontend assets at build time. Run
`npm --prefix web run build`, rebuild the Go binary or container, and restart it.
Vite's standalone dev/preview commands do not proxy `/api` or `/v1` in the current
configuration. Use the [documented development loop](../CONTRIBUTING.md#frontend-development).

## Database or permission problems

Check the state directory ownership for the container's UID/GID and account for
rootless UID mapping and SELinux labeling. Keep SQLite WAL/SHM files with the
state directory when making a cold copy. Back up before restoring or changing
versions; use the [recovery guide](BACKUP_RESTORE.md) rather than replacing a live
database. Restoring should bring back the original transport identity, not run
another copy of it at the same time.
