# Woolwire

**A private room for shared intelligence.**

Woolwire is a working name for a peer-to-peer platform where friends share their locally hosted language models. Wool nods to the llama mascot; wire hints at the seamless connections between members. The name is provisional and has not been checked for trademark or domain availability.

## The experience

1. Launch the container app with Podman or Docker and open its local web UI.
2. Choose **Host a room** or **Join a room**.
3. Hosting produces a reusable join code. Joining means pasting that code.
4. See connected friends and currently available models on your dashboard.
5. Select a model and start your own conversation.
6. Configure your models, endpoints, limits, and schedule in **Settings**.

Every person runs their own app. Hosting a room means creating it; hosting a model means offering compute. Either role can exist without the other.

Your conversations and settings belong to your installation. The creator has a separate room administration panel, not access to everyone else's settings or LLM conversations. Community messages are intentionally shared with the room.

## Planning documents

- [Architecture and security boundaries](docs/ARCHITECTURE.md)
- [Architecture diagram — standalone SVG](docs/architecture.svg)
- [Architecture diagram — editable Mermaid](docs/architecture.mmd)
- [Implementation plan and acceptance gates](docs/IMPLEMENTATION_PLAN.md)
- [M0 feasibility evidence](docs/M0_FEASIBILITY.md)
- [M0 networking and relay operations](docs/M0_NETWORKING.md)

![Woolwire architecture](docs/architecture.svg)

## Status

- **M0 (Transport Feasibility):** Completed and accepted. Pinned Tailcat `v0.6.0`, mutual TLS 1.3, Ed25519 room-authority root certificate pinning, live proof of creator-offline continuation, address regeneration, and substituted peer rejection.
- **M1 (Local App & Room Onboarding):** Completed and accepted. Pure-Go SQLite migrations (`modernc.org/sqlite`), Ed25519 identity, separate loopback-only local API (`127.0.0.1:7070`) and authenticated peer listeners, embedded React 18 / TypeScript SPA, copyable persistent join codes, roster synchronization, and creator room administration panel.
- **M2 (Live Dashboard & External Inference):** Completed and accepted. Cryptographically signed model advertisements with opaque IDs and 90s freshness expiry, SSRF protection against cloud metadata/redirects, round-robin per-member fair queue with execution deadlines, "My Chats" interface with live SSE streaming, cancellation, no-save privacy mode, and local OpenAI compatibility (`/v1/models`, `/v1/chat/completions`).
- **M3 (Managed Models & Resource Controls):** Completed and accepted. Companion runner controller protocol (`/runner/v1`), CPU cores/RAM and NVIDIA GPU hardware detection, GGUF artifact download manager with streaming SHA-256 verification and disk budget limits, deployment CPU/RAM ceilings, and non-privileged Compose profiles (`deploy/base`, `deploy/managed`).
- **M4 (Personal Community Experience):** Completed and accepted. Room channels with signed immutable events, monotonic author sequence tracking, quarantine of sequence conflicts, deterministic sorting `(timestamp, author_seq, id)`, author edits/deletions, creator moderation tombstones, HTML/tracking image sanitization, and replication status indicators.
- **M5 (Contributions & Social Recognition):** Completed and accepted. Jointly-signed completion receipts, zero prompt/content leakage, 20 points per member-pair per UTC day cap, self-service exclusion, 30-day rolling leaderboard, and participant opt-out preference.
- **M6 (Release Hardening & Verification):** Completed and accepted. Local metrics endpoint (`GET /api/v1/metrics`), non-blocking SQLite backup export (`POST /api/v1/backup/export`), reproducible pure-Go build instructions, Software Bill of Materials ([docs/SBOM.md](docs/SBOM.md)), backup and disaster recovery guide ([docs/BACKUP_RESTORE.md](docs/BACKUP_RESTORE.md)), and full automated test suite with race detector enabled (`go test -race ./...`).

## Building and running

Run all commands directly from the project root:

```sh
# Run full automated test suite with race detector
go test -v -race ./...
go vet ./...

# Build frontend static assets (embedded into Go binary)
npm --prefix web run build

# Build standalone binaries (reproducible pure-Go, no CGO)
CGO_ENABLED=0 go build -trimpath -o bin/woolwire ./cmd/woolwire
CGO_ENABLED=0 go build -trimpath -o bin/woolwire-runner ./cmd/woolwire-runner

# Run standalone Woolwire instance
./bin/woolwire -listen 127.0.0.1:7070 -data ./data
```

## Deployment Profiles

### Base Deployment (External Endpoints & Inference Requester)
Runs the lightweight single container app, with zero model weights or GPU requirements:
```sh
podman compose -f deploy/base/compose.yaml up -d
# or docker compose -f deploy/base/compose.yaml up -d
```

### Managed Deployment (Local GGUF Weights & Hardware Acceleration)
Runs the app paired with the isolated companion runner:
```sh
podman compose -f deploy/managed/compose.yaml up -d
# or docker compose -f deploy/managed/compose.yaml up -d
```

Key Deployment Properties:
- **No Docker Socket:** Neither container mounts `/var/run/docker.sock`.
- **Non-Root User:** Runs as non-root UID:GID `1000:1000`.
- **Read-Only Model Mount:** Runner mounts model weights as read-only (`:ro`).
- **Ceilings Enforced:** Container resource limits prevent host resource exhaustion.

## Trust Model & Privacy Guarantees

1. **Private LLM Conversations:** Conversations and prompt history belong exclusively to the requester's device. Prompts are never stored on the host's disk or sent to the room creator.
2. **Creator is Not a Relay:** The room creator acts as an admission and admin authority. Once admitted, peers communicate directly over encrypted transport; inference continues uninterrupted if the creator goes offline.
3. **No Arbitrary Proxying (SSRF Protection):** External model connections strictly prohibit cloud metadata addresses (`169.254.169.254`), disallow non-loopback plain HTTP, reject redirect chains, and pin allowed destination ports.
4. **Social Recognition:** Contribution points provide informal community acknowledgement and never confer financial value or access control.

## Documentation Index

- [Architecture & Security Boundaries](docs/ARCHITECTURE.md)
- [Implementation Plan & Acceptance Gates](docs/IMPLEMENTATION_PLAN.md)
- [Software Bill of Materials (SBOM)](docs/SBOM.md)
- [Backup and Disaster Recovery Guide](docs/BACKUP_RESTORE.md)
- [M0 Feasibility Evidence](docs/M0_FEASIBILITY.md)
- [M0 Networking & Relay Operations](docs/M0_NETWORKING.md)

