# Configuration reference

Command-line flags override their environment defaults. Relative paths resolve
from the process's working directory. Containers in the supplied profiles use
explicit `/state` and `/models` mounts; native processes default to local paths.

## App flags and environment

Source: [cmd/woolwire/main.go](../cmd/woolwire/main.go).

| Flag | Environment fallback | Native default | Meaning |
|---|---|---|---|
| `-listen` | `WOOLWIRE_LISTEN` | `127.0.0.1:7070` | UI and local HTTP API bind address |
| `-state` | `WOOLWIRE_STATE_DIR` | `./state` | Database and encrypted backup directory |
| `-peer-port` | None | `4242` | Protocol port inside the Tailcat transport; bootstrap uses port + 1 |
| `-models` | `WOOLWIRE_MODELS_DIR` | `./models` | Root for downloaded and discovered weights |
| `-runner-url` | `WOOLWIRE_RUNNER_URL` | Empty | Runner controller base URL; empty disables managed inference |
| `-runner-token` | `WOOLWIRE_RUNNER_TOKEN` | Empty | App's bearer token for the configured runner |
| `-allowed-hosts` | `WOOLWIRE_ALLOWED_HOSTS` | Empty | Comma-separated additional HTTP hostnames |
| `-setup-token` | `WOOLWIRE_SETUP_TOKEN` | Empty | Explicit owner pairing secret, at least 12 characters |

`-setup-token` is an owner recovery/bootstrap override: leaving it configured
re-arms that same pairing secret on every process restart. Remove it after
recovery. Prefer an environment variable over putting a secret in a process's
command-line arguments. If no override is supplied, the app generates a secret
on first boot, prints it once, and does not reprint or replace it on restart.

Loopback hosts and private/link-local IP literals are accepted by the HTTP Host
check. Additional hostnames must be allowlisted. The implementation accepts `*`,
but explicit names preserve the Host check's purpose. This is separate from
which network interfaces `-listen` binds and which peers are room members.

The artifact manager currently defaults to a **50 GiB** directory budget. It is
an implementation default, not a supported CLI setting. Query the storage endpoint for the active
budget; no storage-budget setter is exposed by the current local API.

## Runner flags and environment

Source: [cmd/woolwire-runner/main.go](../cmd/woolwire-runner/main.go).

| Flag | Environment fallback | Default | Meaning |
|---|---|---|---|
| `-listen` | `RUNNER_LISTEN` | `0.0.0.0:8080` | Runner controller bind address; use loopback for native operation |
| `-models` | `MODELS_DIR` | `/models` | Weight directory shared with the app |
| `-token` | `RUNNER_TOKEN` | Empty; required | Bearer token on every runner route, including health |
| `-engine-path` | `ENGINE_PATH` | `llama-server` | Engine executable; container sets `/app/llama-server` |
| `-engine-port` | None | `8081` | Internal loopback engine port |

The app uses `WOOLWIRE_RUNNER_TOKEN`; the runner uses `RUNNER_TOKEN`. They must
contain the same value. The runner accepts load parameters through its API,
not arbitrary engine command-line arguments. `gpu_layers` omitted means
hardware-based selection; `0` requests CPU execution.

## Compose variables

| Variable | Profiles | Default | Meaning |
|---|---|---|---|
| `WOOLWIRE_HOST_PORT` | Base, managed | `127.0.0.1:7070` | Host IP and port mapped to container `7070` |
| `WOOLWIRE_RUNNER_TOKEN` | Managed | Required | Interpolated into both services' respective token variables |
| `WOOLWIRE_MODELS` | Managed | `woolwire_models` named volume | Host model directory or volume source |

Use `--env-file` explicitly in scripts. A Compose `.env` file provides values
for interpolation; variables reach the app only if the service environment
passes them through. [Docker documents this distinction](https://docs.docker.com/compose/how-tos/environment-variables/variable-interpolation/).

The base profile's app has a writable root filesystem. The managed profile sets
both roots read-only, drops capabilities, supplies writable `/tmp`, and sets
CPU/RAM limits. Neither profile mounts a Docker socket. Confirm enforcement with
your runtime/provider rather than assuming every Compose key is honored.

## Ports and storage

| Endpoint | Default | Exposure |
|---|---|---|
| UI and owner/client API | TCP `7070` | Host loopback in supplied Compose profiles |
| Peer API | Transport port `4242` | Authenticated Tailcat stream, not a host TCP port to publish |
| Creator bootstrap | Transport port `4243` | Admission protocol, authority-pinned TLS |
| Runner controller | TCP `8080` | Internal managed network; no published host port |
| llama-server | TCP `8081` | Runner loopback |

`/state/woolwire.db` stores the installation's private keys, room records,
settings, and saved data. SQLite may create `-wal` and `-shm` sidecars. Encrypted
exports go under `/state/backups`. Model files are separate from this database.
Native defaults place these under `./state` and `./models`.

Keep the state directory persistent. Both containers run as UID/GID `1000:1000`
in the supplied profiles. A bind mount uses the host's ownership and, where
applicable, SELinux labeling. The managed app reads/writes the models mount;
the runner reads it only. If you deliberately mount the app's models read-only,
existing models remain usable but downloading is disabled.

Pointing `WOOLWIRE_MODELS` at an existing `HF_HOME` is the intended case: the
directory holding `hub/` is read as-is, and downloads are written into it in
the same layout, under `hub/models--org--repo/`. Woolwire's own bookkeeping
stays out of the cache, in `.woolwire/`, and it never deletes weights it did
not install.

## In-app host limits

`GET` and `POST /api/v1/host-limits` use these **PascalCase** keys, reflecting the
current Go record. These are not snake_case environment variables.

| JSON field | Default | Meaning |
|---|---|---|
| `MaxActive` | `1` | Concurrent active requests |
| `MaxQueuedPerMember` | `1` | Waiting requests per member |
| `MaxQueuedTotal` | `10` | Waiting requests overall |
| `QueueTimeoutSeconds` | `300` | Maximum time waiting in the queue |
| `ExecutionTimeoutSeconds` | `600` | Maximum execution time |

Non-positive values are replaced with defaults when saving. Posting a partial
object resets omitted fields to defaults, so read, edit, and send the complete
record. Limits apply to the shared queue immediately and do not change the
container's OS-level ceilings. Per-model context/output defaults are `4096` and
`1024` tokens when no positive values are supplied at registration/load.

## Credentials

| Credential | Purpose | Where it comes from |
|---|---|---|
| Setup secret | Exchange once for owner access to an installation | First boot, Connect Device, Settings, or explicit recovery override |
| `woolwire_session` cookie | Owner browser/local administration API | `POST /api/v1/setup` |
| Room invitation | Admit a separate installation to a room | Creator's room setup/admin panel |
| Local API bearer token | Access `/v1/models` and `/v1/chat/completions` | Settings or authenticated `/api/v1/local-api-token` |
| Runner bearer token | App-to-runner control/inference | Deployment configuration |
| External backend API key | Authenticate to your configured model server | That backend's operator |

Pairing a browser does not create a new room member. Rotating an API or runner
token does not rotate the room invitation. Keep all of these out of issues,
example configuration, screenshots, and Git history.
