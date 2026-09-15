# Getting started

Woolwire runs one app per participant. Container examples run the published
images from `ghcr.io/chrisbaack`; [building them from source](#building-images-from-source)
is covered separately. Commands start in the repository root unless a `cd` is
shown, except the [single container](#single-container) route, which needs no
checkout at all.

## Prerequisites

| Route | Requirements |
|---|---|
| Single container | Docker or Podman on linux/amd64 or linux/arm64 (Docker Desktop on Apple Silicon included); persistent disk space |
| Base Compose | Git; Docker with Compose or Podman with a Compose provider; persistent disk space |
| Managed Compose | Base requirements on **amd64** (the CUDA runner image has no arm64 build), RAM for your chosen model, and the NVIDIA driver and Container Toolkit for the supplied GPU reservation |
| Native source build | Git, Go **1.27.1** (see `go.mod`), Node.js **22.6+**, npm |
| Native managed runner | Native build requirements plus your own working `llama-server` executable |

The first start pulls the images: under 20 MB to download for the app, and several GB for
the managed runner's CUDA image. App startup initializes
Tailcat and needs access to its rendezvous/DERP services, even if you intend to
start by using a local model. See [networking](M0_NETWORKING.md) for the transport
background. A host GPU is optional when requesting models from friends.

The Compose profiles and example environment files live in the repository, so
clone once:

```sh
git clone https://github.com/Chrisbaack/woolwire.git
cd woolwire
```

## Base Compose

### Docker

```sh
docker compose -f deploy/base/compose.yaml up -d
docker compose -f deploy/base/compose.yaml logs -f woolwire
```

Open **http://127.0.0.1:7070**. Find the setup secret in the first-start output,
then follow [first login](#first-login-and-your-first-room). Exit log following
with Ctrl+C; the detached container continues running.

The app runs as UID/GID `1000:1000` and stores its database in a named volume
mounted at `/state`. No runner or model weights are needed to join a room and
request another member's model.

To customize the published address, copy `deploy/base/.env.example` to
`deploy/base/.env`, edit it, and pass it explicitly:

```sh
docker compose --env-file deploy/base/.env -f deploy/base/compose.yaml up -d
```

### Podman

`podman compose` delegates to an installed Compose provider. Install a provider
such as `podman-compose` first, then use:

```sh
podman compose -f deploy/base/compose.yaml up -d
podman compose -f deploy/base/compose.yaml logs -f woolwire
```

You can also invoke `podman-compose` directly. Provider support for GPU device
reservations and resource settings varies; inspect the resulting containers
when using managed hosting. See the [Podman Compose documentation](https://docs.podman.io/en/latest/markdown/podman-compose.1.html).

Rootless Podman maps the container's UID 1000 to a subordinate UID on the host,
so a state volume or bind-mounted models directory owned by your account is
unreadable inside the container. Set this in the profile's `.env` before the
first `up`:

```sh
WOOLWIRE_USERNS_MODE=keep-id:uid=1000,gid=1000
```

Leave it unset for Docker. See
[database and permission problems](TROUBLESHOOTING.md#database-or-permission-problems)
if you have already started the stack without it.

## Managed Compose

This starts the app plus an isolated runner. The supplied profile requests an
NVIDIA GPU and pulls the CUDA-based llama.cpp runner image, which is published
for amd64 only. Configure the host using
[NVIDIA's Container Toolkit documentation](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)
and [Docker's GPU Compose guide](https://docs.docker.com/compose/how-tos/gpu-support/).

### Configure the runner secret

If you do not already have a managed `.env`, create it:

```sh
cp -n deploy/managed/.env.example deploy/managed/.env
chmod 600 deploy/managed/.env
openssl rand -hex 32
```

Put the generated value after `WOOLWIRE_RUNNER_TOKEN=` in that file. This secret
must match between app and runner. Keep an existing token when restarting an
existing installation; copying an example should not replace your local config.

Optionally set `WOOLWIRE_MODELS` to an **absolute host directory** containing
GGUF weights, or a Hugging Face cache directory containing `hub/`. Omit it to
use a named volume. The app needs write access for downloads; the runner's mount
is read-only. Bind-mounted directories must be readable by the container user,
with write access for the app if downloads are desired. Avoid making your model
collection world-writable to work around an ownership mismatch.

### Start and load a model

```sh
docker compose --env-file deploy/managed/.env -f deploy/managed/compose.yaml up -d
docker compose --env-file deploy/managed/.env -f deploy/managed/compose.yaml logs -f woolwire runner
```

Use the setup secret from the **woolwire** service, not the runner token, to sign
in at **http://127.0.0.1:7070**. Create or join a room, then open **Settings**:

1. Check runner status.
2. Select discovered weights, or use the Hugging Face repository resolver to
   choose a GGUF download. Wait for the download job to complete.
3. Prepare the model for sharing, setting context and generation limits that
   fit your hardware. Preparing saves its configuration without loading weights.
4. Return to the Meadow and start a conversation. An unloaded model starts on
   its host when a request arrives; the first response includes that startup time.

Both services default to a 4-CPU / 8192-MiB container ceiling in this profile.
Larger weights or context windows may require changing those limits in Compose
and recreating the affected container. Only the runner receives a GPU. It has
no published host port and is connected only to the internal network; the app
also has an egress network for peer connectivity and downloads.

### CPU and Podman GPU variants

For **CPU-only managed Compose**, make a local copy next to the original:

```sh
cp -n deploy/managed/compose.yaml deploy/managed/compose.cpu.local.yaml
```

In the copy, remove the runner's `deploy.resources.reservations` block containing
`devices`, `driver: nvidia`, `count: all`, and `capabilities: [gpu]`. Keep
`deploy.resources.limits`. Then start with:

```sh
docker compose --env-file deploy/managed/.env -f deploy/managed/compose.cpu.local.yaml up -d
```

The CUDA image can fall back to CPU when no GPU is exposed, but the unmodified
GPU reservation can stop Compose before the runner starts. The
[native runner](#native-runner) is another CPU option and avoids downloading the
CUDA image. Do not run these variants alongside the original managed profile;
they share container names and persistent volume names within the project.

For **Podman with NVIDIA**, use a provider that supports the reservation, or
make a `compose.podman.local.yaml` copy in the same directory. Remove the NVIDIA
reservation as above and add this at the runner service level:

```yaml
    devices:
      - nvidia.com/gpu=all
```

This requires NVIDIA CDI configuration on the host. Follow the
[NVIDIA CDI guide](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/cdi-support.html)
for your driver/toolkit version, then invoke `podman compose` with the explicit
`.env` and local Compose file. SELinux and rootless device permissions may need
host-specific configuration. These variants require end-to-end verification on
your host; they are not recorded as a tested hardware matrix.

## Single container

Run the published base image directly. No checkout is needed:

```sh
docker run -d --name woolwire-app --restart unless-stopped \
  -p 127.0.0.1:7070:7070 \
  -v woolwire_state:/state \
  --security-opt no-new-privileges=true \
  ghcr.io/chrisbaack/woolwire:0.1
docker logs -f woolwire-app
```

This is the [README quick start](../README.md#quick-start) with the
`no-new-privileges` hardening the Compose profiles also apply. Use `podman` in
place of `docker` for the same workflow; the named volume is created owned by
the container user, so rootless Podman needs no UID mapping here. Stop and start
the container with `docker stop woolwire-app` and `docker start woolwire-app`.

`:0.1` follows patch releases of 0.1, `:0.1.0` pins exactly one release, and
`:latest` follows the newest release of any version. To upgrade, pull and
recreate the container; the `woolwire_state` volume carries your data across:

```sh
docker pull ghcr.io/chrisbaack/woolwire:0.1
docker rm -f woolwire-app
# then rerun the docker run command above
```

For managed hosting, prefer the Compose profile, which defines the runner's
separate network, token, and model mount.

## Building images from source

Each Compose profile has a `compose.source.yaml` that builds the images from
your checkout (tagged `woolwire:local` and `woolwire-runner:local`) instead of
pulling them. Layer it after `compose.yaml` so the project name, and with it
your named volumes, stays the same:

```sh
docker compose -f deploy/base/compose.yaml -f deploy/base/compose.source.yaml up -d --build
docker compose --env-file deploy/managed/.env \
  -f deploy/managed/compose.yaml -f deploy/managed/compose.source.yaml up -d --build
```

`scripts/build.sh stack` does this for you. For a single container, build with
`docker build -t woolwire:local .` and run `woolwire:local` in place of the
published image name.

## Native binary

Build the UI before compiling Go so the binary embeds the current assets:

```sh
npm --prefix web ci
npm --prefix web run build
CGO_ENABLED=0 go build -trimpath -o bin/woolwire ./cmd/woolwire
./bin/woolwire -listen 127.0.0.1:7070 -state ./state -models ./models
```

Open **http://127.0.0.1:7070** and use the setup secret printed in the terminal.
Ctrl+C stops the app. Restart with the same state directory to retain identity,
room membership, and chats. A GPU is unnecessary for the base app. Linux is the
primary development target; native platform support beyond that requires
verification, especially for the optional inference engine.

For backend development, `go run ./cmd/woolwire -state ./state` is also usable
after the frontend build. [Contributing](../CONTRIBUTING.md) covers the UI loop.

## Native runner

Install a `llama-server` binary suitable for your CPU/GPU using the upstream
[llama.cpp build instructions](https://github.com/ggml-org/llama.cpp/blob/master/docs/build.md).
The runner controller does not install the engine for you.

Build both Woolwire binaries, then generate one shared token in a terminal:

```sh
CGO_ENABLED=0 go build -trimpath -o bin/woolwire-runner ./cmd/woolwire-runner
export WOOLWIRE_RUNNER_TOKEN="$(openssl rand -hex 32)"
printf '%s\n' 'Runner token set for this shell.'
```

Start the runner in the background, then the app from that same shell:

```sh
mkdir -p models
RUNNER_TOKEN="$WOOLWIRE_RUNNER_TOKEN" ./bin/woolwire-runner \
  -listen 127.0.0.1:8080 \
  -models "$PWD/models" \
  -engine-path /absolute/path/to/llama-server &
WOOLWIRE_RUNNER_PID=$!
./bin/woolwire -state ./state -models "$PWD/models" \
  -runner-url http://127.0.0.1:8080
# After stopping the app with Ctrl+C:
kill "$WOOLWIRE_RUNNER_PID"
unset WOOLWIRE_RUNNER_TOKEN WOOLWIRE_RUNNER_PID
```

Create the models directory and populate it or use the app's downloader. The
app and runner must use the same models directory. The engine listens on its
own loopback port, `8081` by default. Keep the controller on loopback too; native
processes do not provide the managed container's network isolation.

## First login and your first room

The first boot prints a setup secret once. Redeeming it creates the browser's
owner session and invalidates the secret. Restarting does not print a fresh
secret. Use **Connect Device** or **Settings** from an authenticated browser to
mint another one. If you lost all sessions, see
[setup recovery](TROUBLESHOOTING.md#i-lost-the-setup-secret-and-all-signed-in-browsers).

Choose a display name, then:

- **Host a room:** choose a room name and share its invitation privately with
  friends. Room Admin manages invitations and membership.
- **Join a room:** paste the invitation supplied by the creator. The creator
  must be online. With approval enabled, retry after approval.

A setup secret pairs with your installation; a room invitation joins the group
from another installation. They are not interchangeable.

## External servers and container networking

The model URL is resolved by Woolwire, not by your browser. A model server on
host loopback is not at container loopback.

| Where the model server runs | Approach |
|---|---|
| Same host as a native Woolwire app | Use its loopback base URL, e.g. `http://127.0.0.1:8081` |
| Docker Desktop host | Use `host.docker.internal` with the backend's port and allow-private-network setting |
| Linux Docker host | Add `host.docker.internal:host-gateway` to the app's `extra_hosts`; the backend must listen on an address reachable from the bridge |
| Podman host | Check `host.containers.internal` resolution; the backend must be reachable from the container network |
| Another LAN machine | Use its address and enable the endpoint's private-network opt-in; prefer TLS |
| Another container | Put app and backend on an appropriate shared network and use the backend service name |

A hostname alias does not make a loopback-only Linux service reachable on the
bridge gateway. One Linux base-profile alternative is host networking: replace
`ports` with `network_mode: host` and set `WOOLWIRE_LISTEN=127.0.0.1:7070` in the
container environment. Host networking changes isolation; do not use this as a
drop-in edit for the managed profile's split networks.

See the [Docker Desktop networking reference](https://docs.docker.com/desktop/features/networking/)
and [Podman run reference](https://docs.podman.io/en/latest/markdown/podman-run.1.html)
for host aliases and runtime-specific behavior.

Enable **Allow Private Network** only for the endpoint you intend to reach.
The option also permits private-network HTTP; it does not make that traffic
TLS-encrypted. Metadata and other prohibited destinations remain blocked.

## Access from another device

For Compose, set `WOOLWIRE_HOST_PORT=0.0.0.0:7070` in its `.env`, then recreate
the app using the same `--env-file` and `-f` options. Open
`http://YOUR_SERVER_LAN_IP:7070` from your device. For a native process, use
`-listen 0.0.0.0:7070`. Restrict access to your trusted network; the local HTTP
server does not terminate TLS.

Private IP Host headers are accepted. For a hostname such as `woolwire.local`,
pass `-allowed-hosts woolwire.local` or `WOOLWIRE_ALLOWED_HOSTS=woolwire.local`.
In Compose, add that environment variable under the app service; an unused key
in `.env` is not automatically forwarded to the container.

Use **Connect Device** on an already signed-in browser to mint a new setup
secret. Some browsers disable clipboard/camera APIs on plain HTTP LAN origins;
manual copy/paste remains available. For remote administration, use a protected
tunnel or configure a TLS reverse proxy that preserves the host and streams
responses without buffering. Public internet hosting is not the default setup.

## Stop, upgrade, and recover

Use the same profile, environment file, and project name each time. Base and
managed profiles both specify `woolwire-app`; they cannot run side by side
unchanged. Their Compose project names may also select different state volumes.
Back up before changing deployment modes.

For a base installation:

```sh
docker compose -f deploy/base/compose.yaml stop
# Restart the existing containers:
docker compose -f deploy/base/compose.yaml start
# Upgrade: pull the newest image for your tag, then recreate the container.
git pull
docker compose -f deploy/base/compose.yaml pull
docker compose -f deploy/base/compose.yaml up -d
```

For managed hosting, use the same commands with
`--env-file deploy/managed/.env -f deploy/managed/compose.yaml`. Prepared models
retain their saved load settings across restarts. Weights load on the next
request, so restarting the runner does not require keeping GPU memory occupied.

The profiles default to the `0.1` tag, which follows patch releases. To move to
a new minor version, or to pin an exact one such as `0.1.0`, set
`WOOLWIRE_IMAGE_TAG` in the profile's `.env`. If you built from source, upgrade
with the [source build](#building-images-from-source) command after updating
your checkout.

Take a [backup](BACKUP_RESTORE.md) before an upgrade. `down` removes containers
and networks; `down --volumes` also removes named data volumes. For rollback,
keep the backup together with the exact version you upgraded from, and set
`WOOLWIRE_IMAGE_TAG` back to it; database migration downgrade is not a
supported operation, so restore that backup rather than running old code on a
migrated database.

[Configuration reference](CONFIGURATION.md) · [Troubleshooting](TROUBLESHOOTING.md)
