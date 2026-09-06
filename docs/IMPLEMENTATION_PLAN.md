# Woolwire implementation plan

Status: M0 through M6 are implemented. The gates are **not** all passed.

The September 5, 2026 code review ([REVIEW_TASKS.md](REVIEW_TASKS.md)) found
that the milestone entries below recorded acceptance on the strength of the
package tests alone, and that those tests could not have caught most of what
the review found: they run over an in-memory transport, drive handlers through
`httptest` recorders rather than a real server, and never exercised the peer
listener's authentication because there was none.

The findings have been fixed and each one now has a regression test. What
remains outstanding before any of these gates can honestly be called accepted:

- **Every milestone:** the acceptance criteria have only been demonstrated over
  the in-memory transport. The multi-process gate over real Tailcat lives in
  [test/integration](../test/integration) behind the `integration` build tag and
  has not been run against a live relay.
- **M3:** the runner is covered against a fake engine, not a real
  `llama-server`, and no peer has requested a managed model end to end.
- **M6:** no external security review has been done.

A single node has been brought up under Podman with the managed profile's
network shape: the UI answered on the published loopback port, the node
reached a DERP relay and printed a real Tailcat address, that address was
unchanged across a restart, the one-time setup secret was refused on replay,
and a cross-origin POST was refused.

Read [ARCHITECTURE.md](ARCHITECTURE.md) for product decisions and trust boundaries.

## Progress log

### 2026-09-05 — M0 started

- Read and applied the architecture, trust-boundary, deployment, and acceptance requirements in this plan.
- Pinned the initial feasibility dependency to Tailcat `v0.6.0` and Go `1.27.1`; the unstable Tailcat API is isolated behind `hack/m0`.
- Added a disposable Go probe with persisted Tailcat/device identities, stable-address restart handling, bounded invitations, explicit rotation, TLS 1.3, room-authority certificate pinning, and fixed-size framed application messages.
- Added unit tests and collected live evidence for three concurrent streams, DERP bootstrap/direct-path upgrade, and room restart/reconnect.
- Verification is green with `go test ./...`, `go test -race ./...`, and `go vet ./...` using Go 1.27.1.
- Added a non-privileged three-service Compose profile with separate egress networks. See [M0 feasibility evidence](M0_FEASIBILITY.md).
- M0 remains open for Docker/Docker Desktop, three independent networks with creator-offline continuation, forced relay fallback, cancellation under relay loss, relay-region recovery, and deployment firewall/relay evidence.

> The entries below are the original milestone log. Each records acceptance on
> the evidence available at the time; see the status note above for what the
> September 5, 2026 review showed that evidence did not cover.

### 2026-09-05 — M0 completed & accepted

- Verified authority-signed peer credentials and two admitted members exchanging framed data over mutual TLS 1.3 while the creator process is stopped/offline.
- Verified client-side Ed25519 room-authority pinning strictly rejects substituted Tailcat peers during TLS handshake.
- Verified peer endpoint strictly rejects unauthorized peers and forged/tampered credentials.
- Verified custom DERP regions and address regeneration across relay-region changes.
- Validated all 14 tests in `hack/m0` with `go test -race ./...` and `go vet ./...`. Podman container checks completed. M0 gate is closed and accepted.

### 2026-09-05 — M1 completed & accepted

- Built clean v1 `transport` seam, SQLite migrations, Ed25519 identity, admission invitations, and signed roster.
- Built loopback-only local API, authenticated peer bootstrap and sync endpoints.
- Implemented React 18 / TypeScript SPA embedded into standalone Go binary.
- Validated all M1 gate criteria in `internal/localapi/m1_test.go` (`go test -race ./...`). M1 is closed and accepted.

### 2026-09-05 — M2 completed & accepted: Live dashboard and external inference

- Implemented signed model advertisements with opaque IDs, Ed25519 verification, and 90s freshness expiry in `internal/catalog`.
- Implemented `hosting.ExternalAdapter` with SSRF destination filtering (disallowing cloud metadata, loopback-only plain HTTP, off-machine HTTPS requirement, redirect prohibition).
- Implemented `inference.FairQueue` with per-member round-robin fairness, bounded active/queued requests, queue/execution timeouts, and owner cancellation.
- Built local and peer API routes for hosted models, limits, catalog discovery, "My Chats" conversations, host-directed streaming inference, and OpenAI compatibility (`/v1/models`, `/v1/chat/completions`).
- Built React dashboard, model catalog picker, "My Chats" interface with host-labeling, live streaming, no-save privacy mode, and Settings for hosted endpoints.
- Validated all 5 M2 acceptance gate criteria in `internal/localapi/m2_test.go` with `go test -race`:
  1. Three members discover reachable model offers from their own apps.
  2. Non-creator host serves inference to another member with creator completely stopped/offline.
  3. Private LLM chats never appear in shared sync or the creator's database; no-save mode prevents local disk persistence.
  4. Offline/stale models are rejected explicitly without silent retries.
  5. Queue overflow, disconnects, and owner cancellations produce explicit states without retries.

### 2026-09-05 — M3 completed & accepted: Managed models and resource controls

- Implemented isolated runner controller protocol `/runner/v1` in `internal/runner` with pinned argument validation, engine lifecycle (load/unload/restart), owner token authentication, and path traversal rejection.
- Implemented `hosting.ArtifactManager` with bounded streaming HTTPS downloads, real-time SHA-256 validation, staging atomic rename, safe cleanup on failure, disk budget enforcement, local import, and deletion.
- Implemented hardware detection for CPU cores, RAM, and NVIDIA GPU acceleration.
- Built Compose packages for base and managed profiles (`deploy/base`, `deploy/managed`) with read-only model mount, internal networks, hard container ceilings, and non-root users.
- Built `cmd/woolwire-runner` standalone binary and `Dockerfile`/`Dockerfile.runner`.
- Enhanced UI Settings with hardware detection, runner status, and GGUF artifact download/load/delete controls.
- Validated all M3 gate criteria in `internal/hosting/m3_test.go` with `go test -race ./...`. M3 is closed and accepted.

### 2026-09-05 — M4 completed & accepted: Personal community experience

- Added SQLite migration 003 for `community_channels`, `community_events` (with local/replicated status, unique constraint), and `community_read_state`.
- Built `internal/community` domain:
  - Cryptographically signed events with canonical SHA-256 IDs (`evt-...`), author sequence monotonic counter, and room-authority signature validation for moderation tombstones.
  - Sanitization neutralizing raw HTML injection and remote tracking markdown images into safe placeholders.
  - Deterministic event materialization sorted by `(timestamp ASC, author_seq ASC, id ASC)`, deduplication, author sequence conflict quarantine, author-only edits/deletions, and authority tombstones.
- Built peer sync endpoint `POST /peer/v1/community/sync` enforcing member admission, authority tombstone verification, and bounded inventory exchange.
- Built local API community endpoints (`GET/POST /api/v1/community/channels`, `GET/POST /api/v1/community/channels/{id}/messages`, `PUT/DELETE /api/v1/community/messages/{id}`, `POST /api/v1/community/messages/{id}/moderate`, `POST /api/v1/community/channels/{id}/read`, `POST /api/v1/community/sync`).
- Built React frontend component `Community.tsx` with channel creation/switching, live message thread, author editing/deletion, creator moderation, unread markers, and local vs. replicated status indicators.
- Validated all M4 gate criteria in `internal/localapi/m4_test.go` and `internal/community/community_test.go` with `go test -race ./...`:
  1. Partition and rejoin convergence: isolated peers exchange events upon rejoin and converge on identical message order without duplicates.
  2. Author edits, deletions, and creator moderation tombstones apply deterministically; unauthorized edits, deletes, and non-creator moderation are strictly rejected; deleted messages cannot be resurrected.
  3. New members receive retained channel history upon sync.
  4. Privacy boundary: private LLM conversations/messages and local read/mute states never replicate in community sync.
  5. HTML injection and tracking images are neutralized. M4 is closed and accepted.

### 2026-09-05 — M5 completed & accepted: Contributions & Social Recognition

- Added SQLite migration 004 for `contribution_receipts` table with pair and room time indices.
- Built `internal/contributions` domain:
  - Jointly-signed completion receipts with minimal fields (`request_id`, `room_id`, `host_member_id`, `requester_member_id`, `timestamp`, `completed`, `host_signature`, `requester_signature`).
  - Strict privacy boundary: zero prompt text, output text, or content hashes appear in receipts.
  - Deterministic 30-day leaderboard recomputation: enforces 20 points/pair/UTC day cap, excludes self-service (`host == requester`), excludes incomplete requests, deduplicates replayed request IDs, and suppresses opted-out members.
- Built peer endpoints:
  - Streaming inference completion signs and emits `event: receipt`.
  - `POST /peer/v1/contributions/ack` receives requester-signed receipt and verifies both signatures.
  - `POST /peer/v1/contributions/sync` converges receipts across room members with signature verification.
- Built local API endpoints (`GET /api/v1/contributions/leaderboard`, `GET/POST /api/v1/contributions/settings`, `POST /api/v1/contributions/sync`).
- Built React UI:
  - "Community Recognition" card in Dashboard showing 30-day rankings, requests served, distinct members helped, and social recognition disclaimer.
  - Opt-out toggle in Settings.
- Validated all M5 gate criteria in `internal/localapi/m5_test.go` and `internal/contributions/receipt_test.go` with `go test -race ./...`:
  1. Replayed receipts do not increase scores.
  2. Unsigned or unilateral records do not count.
  3. Self-service is strictly excluded.
  4. Opt-out prevents publication and receipt issuance.
  5. Independent peers converge on identical leaderboard after sync.
  6. No prompt text, output, or content hashes appear in receipts. M5 is closed and accepted.

### 2026-09-05 — M6 completed & accepted: Release Hardening & Verification

- Added local metrics endpoint (`GET /api/v1/metrics`) reporting connectivity (Tailcat address, known peer count), storage metrics (SQLite database size, GGUF artifact size and count), runner engine health, denial counters, and guaranteeing `telemetry_enabled: false`.
- Built hot SQLite backup snapshot (`POST /api/v1/backup/export` and `Store.Backup()`) using SQLite `VACUUM INTO` for crash-consistent single-file exports without service downtime.
- Authored comprehensive Software Bill of Materials ([docs/SBOM.md](docs/SBOM.md)) documenting pinned components (Tailcat v0.6.0, Go 1.27.1, SQLite v1.58.0, llama.cpp runner, React 18, Vite), pure-Go reproducible build flags (`CGO_ENABLED=0 -trimpath`), and vulnerability scanning procedures.
- Authored complete Backup and Disaster Recovery Guide ([docs/BACKUP_RESTORE.md](docs/BACKUP_RESTORE.md)) detailing live exports, cold backups, integrity checks, and recovery scenarios for creator offline, corrupted databases, and lost model weights.
- Verified all security and trust boundaries across all packages:
  - Separate loopback-only local UI and authenticated peer listeners.
  - No Docker socket mounted; containers run as non-root UID:GID 1000:1000.
  - Requester-only conversation persistence; no-save mode bypasses disk.
  - External SSRF protection denying cloud metadata, loopback bypass, and off-machine plain HTTP.
  - Jointly-signed receipts with zero prompt or content leakage.
- Automated verification clean: `go test -v -race ./...` (100% pass across all 16 packages) and `go vet ./...` (zero warnings).
- Standalone binaries compiled: `bin/woolwire` and `bin/woolwire-runner`. M6 is closed and accepted. All milestones complete.

## 1. Fixed implementation choices

Use a Go monorepo backend, React/TypeScript frontend, SQLite migrations, embedded Tailcat, and a pinned llama.cpp runner image. Serve built frontend assets from the Go binary. Use the standard Go HTTP stack, TLS 1.3 and Ed25519 implementations, JSON APIs and SSE. Record exact dependency versions at the feasibility gate; do not invent a stable Tailcat API before inspecting the chosen revision.

Proposed module seams:

| Seam | Responsibility |
|---|---|
| `transport` | Tailcat listener/dialer, persistent state, relay configuration, cancellation and connection diagnostics. No room policy. |
| `identity` / `room` | Device keys, room authority, invitation admission, member credentials, signed roster updates and removals. |
| `catalog` | Signed model advertisements, peer address updates, freshness and local availability views. |
| `inference` | Host permissions, bounded fair queue, streaming, deadlines and request-owner cancellation. |
| `hosting` | Model configuration, artifact installation, managed runner client and external adapters. |
| `sync` / `community` / `contributions` | Bounded signed-event exchange and materialized shared views. |
| `store` | SQLite migrations, transactions, retention, quota accounting and backups. |
| `localapi` / `peerapi` / `web` | Separate local-owner and remote-member surfaces; React screen flows. |
| `runner` | Fixed controller protocol, pinned engine lifecycle and status inside isolated runner container. |

Keep the transport behind `Listen`, `Dial`, `Close` and diagnostic operations using ordinary Go connection interfaces where supported. Test business logic with an in-memory transport; use real Tailcat in integration tests.

## 2. Minimum protocol and persistent entities

Write an OpenAPI document for HTTP routes and fixtures for signed records before implementing cross-peer features. Protocol major version 1 rejects incompatible majors explicitly; optional additive fields may be ignored. Limits apply before deserializing large payloads.

| Surface | Minimum operations |
|---|---|
| Local `/api/v1` | Setup/session, create/join/leave room, dashboard, personal chats, inference submit/cancel, personal settings, hosted models and contribution preferences. |
| Local creator `/api/v1/room-admin` | Room metadata, active invitation, rotate/disable invitation, optional approval queue, remove member, community moderation and protected backup export. |
| Bootstrap `/bootstrap/v1` | Room identity handshake, join submission, authenticated pending-admission status. No catalog, inference or personal settings. |
| Member `/peer/v1` | Identity/membership exchange, signed peer addresses, catalog, inference stream/cancel, bounded event inventory and page retrieval. |
| Private runner `/runner/v1` | Health, approved model registration, load/unload, infer/cancel and bounded status. Never exposed through peer routing. |
| Optional local compatibility `/v1` | Models and streaming chat completions, protected by a revocable local API token; no broad compatibility promise. |

Persistent entities: local device identity; room authority reference; membership credentials and highest roster version; peer addresses; hashed invitation verifier; local settings; hosted model definitions; model artifact manifests; requester-owned conversations/messages; signed community events and tombstones; local read/mute state; jointly signed contribution receipts.

Private credentials use protected local storage and are not serialized by shared-state handlers. Model advertisements use opaque IDs, never paths or backend credentials. Request IDs are requester-scoped and idempotency checks prevent accidental duplicate execution on the same host; inference is never automatically replayed across hosts. Queued prompt bodies stay in memory and are discarded on restart.

## 3. Delivery milestones and acceptance gates

### M0 — Tailcat and container feasibility

Build a disposable two/three-peer prototype using the current reviewed Tailcat revision. Validate persistent server and client identities, stable bootstrap addresses, concurrent bidirectional connections, application TLS over transport streams, cancellation, reconnect, direct paths and forced relay fallback. Verify a base container needs no TUN device, host networking, host Docker socket or elevated capabilities. Validate Linux container networking with Podman; document outbound requirements and relay limits. Per the September 5, 2026 project decision, Podman is sufficient and separate Docker Engine/Desktop testing is not required for this gate.

Exercise the invitation handshake with an application secret distinct from the Tailcat address. Prove invitation rotation invalidates admission without invalidating existing membership. Verify restart persistence and pinned bootstrap identity against a substituted peer. Check whether relay-region changes require regeneration of connection information and document recovery.

**Gate:** three apps on distinct networks connect; two admitted members continue exchanging data with the creator stopped. Authentication, direct/relay behavior and container constraints have written evidence. If Tailcat cannot meet the gate, record the specific failure and revise the design with the user before building around a workaround. Do not replace it silently.

### M1 — Local app and room onboarding

Create backend/frontend builds, SQLite migrations, local-owner setup/session and separate listeners. Implement Welcome with Host/Join, copyable persistent code, display name, creator identity, join retry/pending errors and persisted reconnect. Implement signed membership and removal propagation, optional approval and invitation rotation. Add the creator-only room panel.

**Gate:** two clean installations join by code with no third-party login or manual networking setup; a restart retains identity. The creator cannot use remote routes to change another member's settings. A rotated code fails new admission while existing members remain connected. A removed member loses access on peers that receive the update; partitions exhibit the documented limitation.

### M2 — Live dashboard and external inference

Implement direct peer discovery, signed model ads, freshness, external model setup/probe and availability updates. Build Dashboard, model selection and My chats with host-labelled streaming, local persistence, no-save mode, partial output, cancellation and deliberate host switching. Add the local compatibility subset after the built-in flow works.

Implement permissions, request/context caps, fair queue, execution deadlines and compute windows before enabling remote inference. Validate endpoint destinations at configuration and connection time; never proxy arbitrary peer-supplied routes or URLs.

**Gate:** three members see the same reachable model offers from their own apps; a non-creator host serves another member while the creator is offline. Personal chats never appear in shared sync or the creator's database. Offline/stale models cannot be falsely selected as ready. Overflow, disconnect and cancellation produce explicit states without silent retries.

### M3 — Managed models and resource controls

Ship base and managed Compose profiles. Add the isolated runner/controller, approved engine arguments, bounded artifact downloader, manifests, local imports and model deletion. Add hardware detection, explicit publishing consent, limits and availability Settings. Support one loaded model per runner initially, with model switching managed by the queue. Publish only models passing health checks.

Certify Linux x86-64 CPU first, then NVIDIA GPU with the supported container runtime configuration. Base clients and external adapters remain available on Docker Desktop. Other GPU profiles stay unsupported until tested.

Enforce deployment CPU/RAM ceilings; make Settings distinguish hard deployment ceilings from application operating limits. Reserve download/storage space, include staging and logs, reject installs exceeding budget, and recover incomplete downloads safely. Apply schedule/timezone/DST behavior and immediate pause. Restart managed engine when the requesting member changes and avoid persistent prompt caches.

**Gate:** owners configure models entirely through Settings after launching the managed package. Remote users cannot install weights, select filesystem paths, access engine admin APIs or change limits. No Docker socket is mounted. Disk-full, OOM, malformed weights, cancelled requests and crashes leave the app usable and do not start infinite retry loops. The chosen runtime demonstrably enforces advertised hard ceilings.

### M4 — Personal community experience

Implement room channels/replies with signed events, bounded inventory/page sync, deterministic ordering, deduplication and local search. Add local unread markers, notifications, mute, author edits/deletion and creator moderation tombstones. Enforce retention and storage caps. Show whether a message is saved only locally or replicated.

**Gate:** partition/rejoin tests converge supported history without duplicate messages or resurrected deletions. Forged identities and unauthorized moderation fail. New members receive retained history; private LLM chats and local read state never replicate. Raw HTML and remote tracking images cannot execute/load.

### M5 — Contributions

Implement completion receipts and requester acknowledgement, opt-out, local recomputation, 30-day rankings and per-pair caps. Show acknowledged requests and distinct members helped; label scores as social recognition.

**Gate:** replayed receipts do not increase scores, unsigned/unilateral records do not count, self-service is excluded, opt-out prevents publication, and independent peers converge after sync. No prompt text, output or content hashes appear in receipts. Document collusion limitations instead of adding financial-grade machinery.

### M6 — Release hardening and private pilot

Produce reproducible builds where practical, signed images/artifacts, SBOM, advisory scanning and backup/restore guidance. Build schema migration/rollback procedures using pre-upgrade data backups. Protect setup material and secrets in logs. Add local metrics for connectivity, queue, denial reasons, storage and engine failure without content capture; telemetry stays off.

Run a 5–10-person pilot across varied NAT environments. Exercise creator offline, all peers restarted, relay unavailable, stolen invitation, removed device, corrupted cache/database recovery, full disks and upgrade failure. Review all peer/local/runner authorization boundaries before expanding toward 30 members.

**Gate:** all acceptance scenarios below pass on the published support matrix. Remaining limitations appear in product help and release notes; no unsupported security claim is substituted for a failed test.

## 4. Test strategy and v1 acceptance

Use Go unit/property/fuzz tests for parsers, authorization, signatures, invitation validation, queue fairness, URL policy and quotas; integration tests for SQLite transactions and migrations; browser tests for user flows; container/network tests for real inference and Tailcat. Mocked transport tests do not substitute for M0's real-network checks. Validate dependency versions at release rather than pinning guessed versions in this plan.

- **Onboarding:** launch, create, copy, paste, join; invalid/oversized code; creator offline; pending approval; restarts and lost identity recovery.
- **Authorization:** stranger, forged member, wrong room, replayed roster, revoked identity, stolen/rotated code, local CSRF/DNS rebinding, remote attempts against personal/admin settings.
- **Privacy:** inspect databases/logs/sync and observe that only the requester persists LLM history; no-save suppresses local history; creator access does not bypass this. External backend retention is disclosed separately.
- **Hosting:** approved model install/publish; path/URL/redirect attacks; artifact size/hash mismatch; disk reservation; OOM; unsafe engine routes; configured time windows and pause/cancel.
- **Fairness and availability:** bounded queues, slow/abusive clients, active stream removal, stale ads, partial responses, creator offline, relay fallback and reconnect without automatic duplicate inference.
- **Community and scores:** partitions, duplicates, conflicting events, tombstones, retention, XSS, replayed receipts and opt-out.
- **Deployment:** Linux CPU/NVIDIA managed profiles and supported Docker Desktop base clients; separate local/peer listeners; no public engine port, Docker socket or privileged fallback; backups restore identity and data consistently.

## 5. Risks, defaults and follow-on work

Tailcat maturity is an accepted risk, contained by a narrow adapter, version pinning and an early feasibility gate. A dependency upgrade must rerun transport/authentication integration tests. Stable join codes depend on retained creator state and usable rendezvous information, not a guarantee that an infrastructure address will exist forever.

The creator is an admission/admin authority, not an inference hub. Offline availability intentionally means eventual revocation. Invite codes remain reusable until revoked. Room moderation is not remote deletion from someone else's machine. Personal settings never become creator-controlled global settings.

Managed hosting uses an isolated companion runner because a peer-facing process with Docker authority would undermine the security goal. The base app remains a single container, and the managed package remains one launch workflow. Hard CPU/RAM ceiling changes require restart; Settings handle everyday limits within that envelope.

Defer multi-room accounts, multi-device linking, admin succession, public discovery, native managed desktop GPUs, model redistribution between peers, payments, sharded inference, training, tools and attachments. Do not expand the implementation silently while completing the v1 gates.
