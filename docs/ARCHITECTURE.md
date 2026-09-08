# Woolwire architecture

> Design reference: this document includes intended controls and deferred work.
> It is not a claim that every requirement has shipped. See [Features](FEATURES.md),
> [Configuration](CONFIGURATION.md), and [Validation status](STATUS.md) for the
> implemented interface and remaining verification.

Status: proposed design, September 5, 2026. Product decisions reflect the latest agreed room-based experience; earlier proposals requiring Tailscale accounts, expiring invitations, or daily membership renewal are superseded.

## 1. Product and trust model

Target a private group of 5–30 technical friends. One local owner and one room per installation in v1. Members can use models without offering compute. The room creator has the same personal app as everyone else plus room administration.

| Screen | Responsibility and ownership |
|---|---|
| Welcome | Host a room or paste a join code; set your display name. |
| Dashboard | Your current view of reachable members and available models; select a host/model. |
| My chats | Your LLM conversations and history, stored locally; never room-replicated. |
| Community | Room channels and replies; your own unread state, notifications, and local mute preferences. |
| Contributions | Room leaderboard derived from acknowledged hosting receipts. |
| Settings | Your hosted models, external endpoints, limits, availability, profile and local data controls. |
| Room administration | Creator-only room name, invitation rotation, approval mode, removals and moderation. |

A room creator cannot read private conversations through the app or alter another member's hosting configuration. A model host necessarily receives the prompts it processes. Room membership never grants access to shell commands, host files, model installation, or container administration.

## 2. Topology and stack

See [the standalone diagram](architecture.svg) and [editable Mermaid source](architecture.mmd).

- **Go backend:** embedded Tailcat, local UI/API, authenticated peer API, membership, model catalog, scheduler, adapters and event synchronization.
- **React + TypeScript:** built static assets served by the Go app; browser communication remains with the local installation. SSE carries streaming output and live UI updates.
- **SQLite:** local transactional state with migrations. Identity secrets live in restricted files in the persistent data volume, separate from replicated records.
- **Tailcat:** encrypted backend-to-backend connections, discovery bootstrapping through DERP and direct connections where possible. Use its Go library through a small transport adapter, with a pinned reviewed revision.
- **llama.cpp:** managed GGUF inference through a separate runner container. External adapters initially support the text chat-completions subset of OpenAI-compatible local servers, including compatible Ollama/llama.cpp deployments.
- **HTTP/JSON:** versioned application protocol; streaming inference over authenticated connections. No public DHT, blockchain, distributed inference, or mandatory application server.

Tailcat documents account-free userspace operation, saved keys, direct encrypted connections, DERP fallback, and configurable relays. Those are transport capabilities, not room membership or administration. Woolwire implements the latter. The first milestone verifies the exact pinned library API and behavior inside a container before dependent implementation. Podman is the accepted validation runtime; a separate Docker Engine/Desktop run is not required for M0. [Tailcat README](https://github.com/tailscale/tailcat/blob/main/README.md)

DERP operators can observe connection metadata even though application traffic is encrypted. Public relays have capacity limits; expose a custom relay setting in advanced configuration and qualify the relay used for any pilot. No promise of anonymity or public relay service availability. [Tailcat service description](https://tailscale.com/tailcat)

## 3. Joining, identity and discovery

### Creation and invitation

Creating a room generates a room signing key, a member/device identity, and a persistent bootstrap listener. Persist all required Tailcat key and rendezvous configuration so ordinary container restarts preserve reachability.

The join code is a versioned, self-contained, URL-safe encoded invitation containing the room ID, pinned room public key, bootstrap connection information, invitation ID and a cryptographically random admission secret of at least 128 bits. It is copy/paste-friendly, not a short numeric code requiring a hosted lookup service. Treat it as sensitive; never put it in logs, analytics, or URL query strings.

The code is reusable and has **no automatic expiration**. Only the creator issues, disables or replaces it. Existing memberships survive invitation rotation. Code rotation changes the admission secret; do not assume a raw Tailcat address alone can implement independent invitation revocation.

### Admission

1. A joining app validates the code format and bounded size before initiating a connection.
2. It connects to the creator and verifies the room identity pinned in the code.
3. It proves possession of a newly generated device identity and presents the invitation secret over the encrypted authenticated channel.
4. The creator admits it immediately by default, or records a pending request if optional approval mode is enabled.
5. The creator signs a membership credential binding room, member/device public key and membership ID, then returns the signed membership state and peer bootstrap addresses.
6. The member saves its identity, establishes authenticated sessions with other peers, and requests current catalogs.

Use standard Ed25519 signatures and TLS 1.3 mutual authentication above the Tailcat stream for the application identity boundary. TLS certificates prove possession of device keys; a custom verifier must additionally validate the pinned room authority, membership binding and locally known revocation state. Transport encryption alone is insufficient. Certificate/key rotation is automatic application maintenance and does not expire invitations or require daily creator contact.

Initial enrollment uses a separate server-authenticated bootstrap endpoint; only bounded enrollment/status operations are available there. Normal peer methods require membership authentication. Do not expose a generic TCP proxy or Tailcat SSH/exit-node functionality.

### Continued operation and removal

Peers persist known peer addresses and exchange signed address announcements. They reconnect directly without routing inference through the creator. Discovery polls known peers every 30 seconds with jitter; an advertisement becomes stale after 90 seconds without refresh. A successful connection and current model status are required before advertising a model as usable.

The creator signs monotonically versioned membership updates. Peers retain the highest accepted version and propagate updates. Newly learned removals cancel affected active sessions and requests. Peer-local blocking works immediately on that installation.

Existing memberships do not require periodic online renewal. During a partition, a removed member can still interact with peers that have not learned the removal. This is an explicit availability tradeoff, with no claimed maximum revocation delay. Once connected, peers exchange membership state before ordinary application operations, but cannot prove no newer unseen update exists elsewhere.

Removing a member offers **Remove and replace invitation** by default: someone holding a still-valid code could otherwise return with a new identity. The UI must explain that this prevents reuse of that code, not all future identity abuse.

The creator must be online for new admissions and administrative changes. If unreachable, joining shows “Room creator is offline — retry.” Existing members can continue if they can reach one another. Restore creator identity from a protected backup after device loss; there is no silent takeover or automatic admin election in v1. Lost transport reachability may require a new invitation or an out-of-band reconnect code.

## 4. Inference and data flow

Model advertisements identify `(room ID, host member ID, model ID, revision)` and include name, quantization, context limit, availability, queue estimate and managed/external status. Hardware details are optional. Advertisements are signed, versioned, bounded and refreshable; they never expose endpoint secrets or local paths.

A user selects a concrete host/model. Their app sends only the chosen conversation context to that host. The host authenticates membership, checks local permissions and limits, queues fairly, invokes its runner/adapter, and streams results back. The requester's app stores conversation history locally by default, with a per-conversation no-save option. The host avoids storing prompt/output bodies; requester-side persistence and external backend retention are distinct policies.

Changing hosts requires explicit selection before transmitting prior history. No silent cross-host failover, automatic request replay, or automatic room-wide conversation synchronization. Surface partial output and unknown execution status after a disconnect.

The default host queue has one active request, at most one queued request per member and ten queued overall, with round-robin member fairness. Defaults: five-minute queue expiry, ten-minute execution timeout, 1,024 generated tokens and a 1 MiB HTTP request-body cap, further constrained by the selected model's context capacity. Host-local policy may tighten or deliberately increase configurable limits. Validate token/context limits before expensive inference where tokenizer support exists; the engine remains subject to an execution deadline.

Cancellation is request-owner scoped. When a compute window ends or the owner pauses, reject new work, discard queued work and stop active managed execution. External endpoints only guarantee that Woolwire stops forwarding and requests cancellation where supported; backend work may continue.

## 5. Deployment and managed hosting boundary

**Base installation:** one non-root app container, persistent data volume and web UI published to host loopback only, e.g. `127.0.0.1:7070`. Tailcat runs inside the backend; no host networking, TUN device, Docker socket or privileged container is required by the intended base design. Verify those claims in the feasibility milestone rather than compensating with extra privileges.

**Managed hosting:** a supplied Compose package adds a fixed runner container on a private internal network. The runner has its own model/cache volume and only the GPU devices needed by the selected deployment profile. Its controller launches the pinned llama.cpp binary with approved arguments. It has no app database, membership keys or external endpoint credentials. Neither container gets the Docker socket. This is one launch command with two security boundaries, not an arbitrary container orchestrator exposed through Settings.

The app fetches approved artifacts into a bounded staging/model volume; the runner mounts finalized weights read-only. The controller accepts authenticated requests from the local app only. Its fixed API supports approved model IDs, load/unload, infer/cancel and status. It rejects arbitrary paths, executable arguments and URLs. The private runner-control credential never enters the room protocol.

Deployment defines hard container CPU/RAM ceilings and GPU access. Settings control model selection, application storage budget, lower operating targets, concurrency and schedules within those ceilings. Increasing hard container ceilings requires a Compose restart; the UI must say so. GPU percentage and VRAM estimates are advisory unless hardware-specific enforcement has been validated. Do not label watchdog-based thresholds as hard OS quotas.

Model installation is owner-only. Support approved HTTPS artifact sources and explicit local imports; record source, license metadata and SHA-256. A locally computed digest identifies an artifact but does not prove it safe. Reserve staging plus final storage, reject unsafe paths/symlinks, bound download sizes, and check destination addresses/redirects to prevent SSRF. No model repository scripts or remote-code execution.

Disable llama.cpp administrative/file/slot/RPC features on the exposed inference path. Do not forward arbitrary engine routes. Disable persistent prompt caches; serialize inference and restart the managed engine between different requesting members in v1 to release application process state, accepting reload latency. This is not a promise of GPU-memory forensic erasure.

The runner boundary contains the controller and inference engine together: an engine compromise may compromise that runner and falsify its status. Container-enforced ceilings remain the hard boundary. A runner has no general internet egress and no connection path to unrelated host services. Test actual Docker isolation rather than relying on diagram labels. Docker's shared-kernel and GPU-driver exposure remain residual risks. [Docker security](https://docs.docker.com/engine/security/), [llama.cpp security policy](https://github.com/ggml-org/llama.cpp/security/policy)

External endpoints are configured locally and referenced by opaque adapter/model IDs. Accept only fixed destinations; deny redirects, peer-supplied URLs, metadata addresses and arbitrary proxying. Private/loopback destinations are allowed only as explicit owner configuration. Revalidate DNS destinations against that approved target policy at connection time. Plain HTTP is limited to the local machine or isolated local container network; off-machine backends require authenticated TLS. Document Docker host-reachability configuration without publishing the engine publicly.

## 6. Shared and private state

| Data | Stored locally | Shared with peers |
|---|---|---|
| Private LLM chats | On requester, unless no-save selected | Only selected inference host receives needed context |
| Hosting settings and endpoint credentials | On host | Only sanitized model advertisements |
| Room membership and removals | Each peer | Creator-signed updates |
| Presence and models | Short-lived cache | Relevant room peers |
| Community messages | Retained room history | Approved members, including new members |
| Unread, muted users, notification preferences | Current user only | No |
| Contribution receipts | Each peer | Minimal signed room records |
| Invitation secrets and private keys | Creator/device only as needed | Invitation secret only via deliberately shared code and enrollment |

Community uses signed immutable events, unique IDs, per-author sequences and paginated missing-event exchange. Sign exact encoded bytes; verify bounded envelopes before materializing views. Author edits/deletions and creator moderation are additional events. Sort deterministically and deduplicate; quarantine conflicting events with the same author sequence.

Default community retention is 30 days and 250 MiB. Preserve compact tombstones for the synchronization horizon; reject expired events rather than resurrecting deleted history. A locally saved message is not guaranteed replicated: show local/pending/replicated status. Offline catch-up requires some reachable peer to retain a copy. Members may keep copies indefinitely outside the app, so deletion cannot guarantee erasure.

Contribution receipts are jointly signed by requester and host, with a random request ID, timestamp, completion status and minimal counters. Do not include content or prompt hashes. One acknowledged completed request earns one point, capped at 20 per requester-host member pair per UTC day; exclude self-service. Rank over 30 days. Receipt deduplication prevents replay scoring, not collusion or multiple-identity gaming. Points never determine access or have financial value. Participants can opt out of public receipts and earn no public points for those requests. Receipts reveal a requester-host relationship to room members.

## 7. Security requirements across components

- The local UI requires local-owner authentication. Bootstrap with a one-time setup secret shown locally, exchange it for an HttpOnly SameSite session cookie, then invalidate it. Pin Host/Origin, check CSRF and avoid permissive CORS. Bootstrap secrets are the exception to ordinary secret-free logs and must be printed only in the explicitly local setup output, never shared telemetry.
- Local-owner and peer HTTP routers use distinct listeners. Room-admin methods additionally check possession of the creator authority; UI hiding is not authorization.
- Sanitize Markdown; disable raw HTML, remote image loading and executable tool output. No shell/tool execution, MCP, browsing, attachments or arbitrary file fetches in v1.
- Rate-limit bootstrap, peer sessions, requests, event synchronization and downloads. Bound decoded message size, queue size, fan-out, stream duration and storage before costly work.
- Protect persisted keys through filesystem permissions and encrypted backups; recommend host full-disk encryption. Disable telemetry and payload-bearing crash dumps by default; do not claim that app file permissions protect against host administrators.
- Pin reviewed dependencies and images, publish signed artifacts and an SBOM, scan advisories, and provide backup/rollback instructions. Tailcat is a deliberate early dependency; do not silently substitute a different transport if validation fails.

## 8. Deferred scope

Public rooms/discovery, financial credits, cross-machine model sharding, training, executable agents, private member-to-member messaging, attachments, multiple rooms per installation, multi-device identity linking, administrator succession, and managed native macOS/Windows GPU execution are outside v1. Docker Desktop clients and external endpoints remain part of the base-app validation matrix.
