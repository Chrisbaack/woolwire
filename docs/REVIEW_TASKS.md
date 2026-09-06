# Woolwire review task list

Source: code review of the initial commit (`3645338`) against `docs/ARCHITECTURE.md` and `docs/IMPLEMENTATION_PLAN.md`, September 5, 2026.

Context for whoever picks this up:

- Go monorepo. Local owner API in `internal/localapi`, peer API in `internal/peerapi`, Tailcat wrapper in `internal/transport`, SQLite in `internal/store`, React SPA in `web/src`.
- `go vet ./...` and `go test -race ./...` are green today. The tests use `httptest` recorders and the in-memory transport, so they do not cover most of the items below. Every task should add or extend a test that would have caught it.
- The disposable `internal/m0` package contains a working mutual-TLS-over-Tailcat implementation with room-authority pinning. It was never promoted into the v1 seam. Reuse it rather than rewriting.
- Do not change the documented trust model. Where the code contradicts a doc claim, fix the code. Where a doc claims a feature the code lacks, either implement it or edit the doc status line so it stops claiming acceptance.

Priority legend: P0 = security or data-exposure, fix before any pilot. P1 = core flow broken. P2 = correctness, robustness, or doc drift.

Work the P0 items in order. Tasks 1 and 2 unblock most of the others.

---

## P0: Peer API authentication and admission

### 1. Authenticate every peer connection with mutual TLS and derive member identity from the certificate

- **Problem:** `internal/peerapi/server.go` serves plain HTTP over the Tailcat stream. Every handler trusts a caller-supplied `member_id` in the JSON body: inference at `internal/peerapi/server.go:337`, community sync at `:465`, contributions sync at `:611`, cancel at `:425`. Anyone who can reach a node's Tailcat address can impersonate any admitted member.
- **Fix:** Wrap the listener returned by `transport.Listen` in a `tls.Server` using the device certificate from `identity.Device.TLSCertificate()`. Require client certs. The verifier must check that the peer cert's Ed25519 key matches an admitted member's `device_public` in the local roster and that the membership signature verifies against the pinned room authority. On the dial side, `localapi` must present the device cert and verify the remote presents an admitted member cert for the member ID it intends to talk to. Set the authenticated member ID in the request context and delete every `MemberID` field from `SyncRequest`, `InferenceRequest`, `CommunitySyncRequest`, and `ContributionsSyncRequest`.
- **Reference implementation:** `internal/m0/peer.go` and `internal/m0/endpoint.go` already do authority-signed peer credentials and pinning. See `TestAdmittedPeersExchangeDataWithCreatorStopped` and `TestSubstitutedPeerRejectedByClientTLS` in `internal/m0`.
- **Verify:** A test where node C, holding a valid Tailcat address for node B but no membership, gets a TLS handshake failure on every `/peer/v1/*` route. A test where an admitted member cannot submit a request claiming another member's ID.

### 2. Bootstrap join must prove key possession and only the creator may admit

- **Problem:** `internal/peerapi/server.go:103` `handleJoin`:
  - `:141` derives the member ID from the first 16 characters of a caller-supplied `device_public` without decoding it. A 32-byte key is never validated. Strings shorter than 16 characters panic the handler. A joiner can submit the creator's public key and overwrite the creator's roster row via the `SaveMember` upsert.
  - `:153` saves an unsigned `admitted` membership when `s.authority` is nil. Members store the invitation code in their own room record at `internal/localapi/server.go:597`, so any member's bootstrap endpoint admits new devices with the leaked code, bypassing the creator and bypassing rotation.
  - A removed member whose key is unchanged can rejoin and flip back to `admitted` if the code was not rotated.
- **Fix:**
  - Run `/bootstrap/v1/join` over server-authenticated TLS where the client presents its device cert. Derive `device_public` from the cert, never from the body.
  - Return 403 from `handleJoin` unless `s.authority != nil`. Non-creators must not store the invitation code at all; drop it from the member-side `RoomRecord`.
  - Reject joins whose derived member ID already exists with status `removed` or `admitted`. Decide whether re-admission of a removed key is allowed and document it.
  - Validate `device_public` decodes to exactly `ed25519.PublicKeySize` bytes before slicing.
- **Verify:** Tests for: join via a member node fails; join with a malformed key returns 400 rather than panicking; join reusing the creator's public key is rejected; a removed member's rejoin is rejected.

### 3. Implement membership sync so removals actually propagate

- **Problem:** `/peer/v1/membership/sync` is served at `internal/peerapi/server.go:214` but nothing calls it. Grep confirms zero callers. Removed members keep full access on every peer except the creator. The M1 test at `internal/localapi/m1_test.go:283` only inspects the creator's own table.
- **Fix:** Add a poller in `localapi` that, on startup and on a 30 second jittered interval, dials each known peer, sends `known_version`, verifies every returned `Membership` with `room.Membership.Verify` against the pinned authority, applies only records with a higher `roster_version`, and persists. Also call it before every inference request to the target host. Newly learned removals must cancel that member's active queue items and close their connections.
- **Verify:** Extend the M1 gate test: creator removes Bob, Alice syncs, Alice's roster shows Bob removed, Bob's inference request to Alice returns 403.

### 4. Stop accepting peer address updates from unauthenticated bodies

- **Problem:** `internal/peerapi/server.go:219` saves any `member_id -> tailcat_addr` pair. An attacker rebinds a host's ID to their own node; other members then send full conversation context to the attacker.
- **Fix:** After task 1, only accept an address update for the member ID bound to the TLS session. Better: have members sign `(room_id, member_id, tailcat_addr, timestamp)` with their device key as the architecture's "signed address announcements", and verify on receipt.
- **Verify:** Test that a sync from member A carrying an address for member B is ignored.

### 5. Scope cancellation to the request owner

- **Problem:** `internal/peerapi/server.go:425` cancels any request ID from any caller.
- **Fix:** `FairQueue.Cancel(requestID, memberID)` and refuse if the item's `memberID` differs.
- **Verify:** Unit test in `internal/inference`.

---

## P0: Local API exposure

### 6. Make the setup secret one-time and rate-limited

- **Problem:** The setup token is persistent, re-printed to stdout on every boot (`cmd/woolwire/main.go`), returned to the UI by `GET /api/v1/setup/info` at `internal/localapi/server.go:321`, and may be a 4-character PIN (`:341`). `POST /api/v1/setup` has no rate limiting. With `WOOLWIRE_HOST_PORT=0.0.0.0` this is brute-forceable from the LAN in seconds. Docs require a one-time secret invalidated after exchange.
- **Fix:** Generate a fresh 128-bit token on first boot only, print it once, invalidate it on successful exchange. Provide an explicit "reset setup secret" path from an authenticated session. Enforce a minimum of 12 characters for any custom secret. Add a per-IP exponential backoff on `/api/v1/setup`. Remove the QR/URL-fragment login flow at `web/src/App.tsx:286` or replace it with a short-lived one-time link minted by an authenticated session.
- **Verify:** Test that the token stops working after first use and that 10 failed attempts are delayed.

### 7. Add CSRF protection

- **Problem:** The session cookie is `SameSite=Lax` (`internal/localapi/server.go:313`) with no Origin check. Any page on `127.0.0.1` at another port is same-site, and handlers ignore `Content-Type`, so a `text/plain` POST from another local web app can rotate invitations, delete chats, or remove members.
- **Fix:** In `securityMiddleware`, for every non-GET request require an `Origin` header that matches the request's own scheme and host, or a custom header such as `X-Woolwire-Request: 1` that the SPA always sets. Reject `Content-Type` other than `application/json` on JSON routes. Use `SameSite=Strict`.
- **Verify:** Test that a POST with `Origin: http://127.0.0.1:3000` and a valid cookie is rejected.

### 8. Require a token on the OpenAI-compatible routes

- **Problem:** `internal/localapi/openai.go:29` allows unauthenticated access when `local_api_token` is unset, and nothing in the codebase can set it. `/v1/chat/completions` accepts a simple cross-origin POST from any web page and drives inference on peers' hardware. Comparison at `:33` is not constant-time.
- **Fix:** Generate `local_api_token` on first boot, expose it read-only in Settings with a regenerate button, and require it always. Use `subtle.ConstantTimeCompare`.
- **Verify:** Test that `/v1/models` returns 401 without a bearer token.

### 9. Stop leaking the Tailcat private key from metrics

- **Problem:** `internal/localapi/metrics.go:41` returns `dev.TailcatKey`, which is the persisted node private key, under `connectivity.tailcat_addr`.
- **Fix:** Return `s.trans.Address()` instead.
- **Verify:** Test that the metrics body does not contain the string `privkey:`.

---

## P1: Core flows that do not work

### 10. Persist the Tailcat preshared key so the address survives restarts

- **Problem:** Tailcat embeds the WireGuard preshared key in the address. `tailcat.Server.PresharedKey` docs: "A persistent server must restore this value along with Key so its address remains usable across restarts." `cmd/woolwire/main.go:86` restores only the node key, so every restart yields a new address, invalidating the invitation code and every peer's stored address for the node.
- **Fix:** Add a `tailcat_psk` column to `device_identity`, generate once with `tailcat.NewPresharedKey()`, pass it in `transport.TailcatConfig.PresharedKey`. Also persist the resolved region so the address is stable.
- **Verify:** Integration test that restarts the transport with the same store and asserts `Address()` is unchanged. `internal/m0/state.go` already persists the PSK; mirror it.

### 11. Remove the 15 second write timeout from streaming routes

- **Problem:** `internal/localapi/server.go:109` sets `WriteTimeout: 15 * time.Second` on the local HTTP server. Chat SSE, OpenAI streaming, and the synchronous artifact download all die after 15 seconds. Tests never hit this because they use `httptest.NewRecorder`.
- **Fix:** Set `WriteTimeout: 0` and use `http.NewResponseController(w).SetWriteDeadline(...)` per handler, or keep a global timeout and clear it in streaming handlers. Add idle keepalive comments (`: ping\n\n`) every 15 seconds on SSE routes.
- **Verify:** Test using a real `net/http` server with a backend that streams for 20 seconds.

### 12. Send the backend model name, not the opaque Woolwire ID

- **Problem:** `internal/peerapi/server.go:370`, `internal/localapi/chats.go:219`, and both call sites in `internal/localapi/openai.go` pass `model.ID` (e.g. `model-3f2a...`) as the OpenAI `model` field. The Settings UI puts the discovered backend name into `name`. Only single-model servers that ignore the field work.
- **Fix:** Pass `model.Name`. Consider adding a distinct `backend_model` column so display name and backend identifier can differ.
- **Verify:** Extend `internal/localapi/m2_test.go` so the fake backend asserts the received model field equals the configured name.

### 13. Wire managed models end to end

- **Problem:** Loading a GGUF into the runner at `web/src/components/Settings.tsx:466` creates no hosted-model record, and `hosting.RunnerClient.StreamChat` has no callers. Managed models are never advertised or served. In `internal/runner/controller.go`:
  - `:211` marks the engine `ready` immediately after `cmd.Start()`, before llama-server has loaded weights.
  - The child process is never waited on unless explicitly stopped, so a crashed engine leaves status `ready` and a zombie.
  - Restart at `:266` drops the context, threads, and GPU-layer arguments that load at `:188` used.
  - `handleInference` at `:305` proxies the raw client body to llama-server unvalidated.
  - Concurrent inference calls overwrite `cancelActive`.
  - No "restart engine between different requesting members" as the architecture requires.
- **Fix:** On successful load, create or update a `hosted_models` row with `model_type = managed` and a sentinel endpoint. In `peerapi.handleInference`, branch on `model_type`: managed models go through `RunnerClient.StreamChat`. In the controller: poll `/health` on the engine port before flipping to `ready`; run `cmd.Wait()` in a goroutine that sets `error` on exit; store load args and reuse them on restart; build the llama-server request from a typed struct rather than proxying bytes; serialize inference with a mutex; track last requester and restart when it changes.
- **Verify:** Add `internal/runner` tests with a fake engine binary. Extend `m3_test.go` so a peer requests inference against a managed model.

### 14. Fix the managed Compose profile

- **Problem:** `deploy/managed/compose.yaml:64` attaches the app only to an `internal: true` network. Tailcat cannot reach DERP or `tailcat.dev`, and Docker does not publish ports for containers on internal-only networks, so the UI is unreachable too. The runner token is hardcoded at `:21` and `:44`.
- **Fix:** Attach the app to both the default bridge network and `woolwire_internal`; keep the runner internal-only. Generate the token at first `up` via an `.env` file or an entrypoint, or read it from a Docker secret. Add `cap_drop: [ALL]` and `read_only: true` with tmpfs as `deploy/m0` already does.
- **Verify:** Bring the profile up with Podman and confirm the UI answers on `127.0.0.1:7070` and the app can print a Tailcat address.

### 15. Replicate community channels

- **Problem:** `community_channels` rows exist only on the creating node. Events for a custom channel arrive at peers but are invisible because `handleListChannels` lists local rows only, and `handleGetChannelMessages` is keyed by channel ID.
- **Fix:** Model channel creation as a signed community event (`event_type = channel`) so it replicates through the existing sync, and materialize the channel list from events. Alternatively, restrict channel creation to the creator and sign it with the authority key.
- **Verify:** Extend `m4_test.go`: node A creates `#ops`, node B syncs and can list and read `#ops`.

### 16. Replace full-ID-list sync with per-author cursors

- **Problem:** `internal/localapi/community.go:524` sends every known event ID on every sync under a 512 KiB cap at `internal/peerapi/server.go`. Around 15k events makes sync fail permanently with 400. Pulls are capped at 100 per call (`internal/peerapi/server.go:532`) with no cursor, so catch-up is slow. Contributions sync has the same shape.
- **Fix:** Sync request carries `map[author_member_id]max_seen_seq`. Responder returns events with `author_seq` greater than the cursor, ordered, paged with a `next` cursor. Loop until empty. Same for receipts keyed by `(host, requester, timestamp)`.
- **Verify:** Test with 20k events that sync converges and each request body stays under the cap.

### 17. Implement retention and storage caps

- **Problem:** `store.PurgeExpiredEvents` at `internal/store/store.go:901` has no callers. The 30 day / 250 MiB policy in the architecture is unimplemented. Author-controlled timestamps mean future-dated events would also never expire.
- **Fix:** Background job every hour: purge non-tombstone events older than 30 days, then enforce the byte cap oldest-first. Reject inbound events whose timestamp is more than 5 minutes in the future or older than the retention horizon.
- **Verify:** Store test for purge; sync test that a future-dated event is rejected.

### 18. Keep conversation context in no-save mode

- **Problem:** `internal/localapi/chats.go:185` sends only the current turn when `no_save` is set, so privacy mode also disables multi-turn conversation.
- **Fix:** Keep the no-save conversation's messages in an in-memory map on the server keyed by conversation ID, with eviction on delete or restart. The architecture already says queued prompt bodies stay in memory.
- **Verify:** Test that the second turn in a no-save chat includes the first turn in the backend request and that nothing is written to `messages`.

### 19. Enforce token and context limits, and queue local requests

- **Problem:** `max_tokens` and `context_limit` are stored but never sent to the backend or checked. `hosting.ExternalAdapter.StreamChat` sends only `model`, `messages`, `stream`. Local chats and OpenAI-compat requests bypass `FairQueue` entirely, so the host's own usage is invisible to remote fairness and limits.
- **Fix:** Pass `max_tokens` in the request body; estimate prompt size (4 chars per token is acceptable for v1) and reject above `context_limit` before dispatch. Route local and OpenAI-compat requests for self-hosted models through the same `FairQueue` instance. Move the queue out of `peerapi.Server` into a shared `inference` component that both servers use.
- **Verify:** Test that a prompt over the context limit is rejected without calling the backend and that a local request occupies the active slot.

### 20. Fix the fair queue slot leak and double-decrement

- **Problem:** `internal/inference/queue.go:133`. When `dispatchNextLocked` closes `ready` at the same instant the timeout or cancel branch fires, Go's `select` may choose the timeout branch. The item has already been counted active, so `activeCount` is never decremented and the host stops serving with the default of one slot. `Cancel` on a queued item also triggers `ctx.Done()` in `Submit`, which calls `dequeue` a second time and decrements `memberQueued` twice. Fairness is FIFO, not the documented round-robin.
- **Fix:** After the timeout or cancel branch wins, re-check under the lock whether the item is in `activeItems`; if so, either run it or release the slot and dispatch the next. Make `dequeue` idempotent by tracking a `removed` flag on the item. Implement round-robin by keeping a per-member FIFO and rotating across members.
- **Verify:** A stress test with `-race` that submits and cancels thousands of items and asserts `activeCount` returns to zero, plus a fairness test with two members where one has a higher per-member limit.

---

## P2: Correctness, robustness, and doc drift

### 21. SSRF policy is thinner than the README claims

- **Problem:** `internal/hosting/adapter.go:63` checks two literal hosts. No DNS resolution check, no IPv6 metadata (`fd00:ec2::254`), no alternate encodings (`0xa9fea9fe`, `2852039166`), no port pinning, and `:70` allows plain HTTP to any single-label hostname, which resolves to LAN hosts under search domains.
- **Fix:** Resolve the host at connection time via a custom `DialContext`, and reject link-local, multicast, and any non-loopback private range unless the owner explicitly checked an "allow private network" box for that model. Restrict ports to an allowlist (80, 443, 8080, 8000, 11434, 1234, plus explicit owner override). Apply the same dialer to `ArtifactManager`.
- **Verify:** Table-driven test covering each bypass form.

### 22. Backups: location, secrecy, and permissions

- **Problem:** `internal/localapi/metrics.go:80` writes to `data/backups`, relative to CWD rather than the state directory, so container backups land outside the volume. The file contains the device private key, Tailcat key, session token, setup token, and endpoint API keys in plaintext. Docs promise encrypted backups. The SQLite file is created with default umask.
- **Fix:** Write under `<state dir>/backups`. Encrypt the export with a passphrase-derived key (age or NaCl secretbox with Argon2id). `chmod 0600` the database after `Open`.
- **Verify:** Test that the export path is under the state dir and the output is not a valid SQLite header.

### 23. Artifact manager holds the lock for the whole download

- **Problem:** `internal/hosting/artifacts.go:117` takes `m.mu.Lock()` before a multi-GB transfer, blocking `ListArtifacts` and metrics for the duration. The download also runs synchronously inside an HTTP handler.
- **Fix:** Reserve budget under the lock, release it, download without the lock, retake it to finalize. Run downloads as background jobs with a status endpoint the UI polls. Include `.staging` contents in used-space accounting.
- **Verify:** Test that `ListArtifacts` returns while a download is in flight.

### 24. Canonicalize signed payloads

- **Problem:** All four `Payload()` functions colon-join free-text fields: `internal/room/membership.go:29`, `internal/community/event.go:37`, `internal/catalog/ad.go:30`, `internal/contributions/receipt.go:25`. Display names, model names, and message content contain colons. Currently unexploitable because trailing fields are typed, but fragile.
- **Fix:** Sign a canonical encoding: length-prefixed fields or deterministic JSON with sorted keys and no whitespace. Bump a `sig_version` field so old records can be recognized.
- **Verify:** Test that two structs with different fields never produce the same payload bytes.

### 25. Author-sequence conflicts are dropped, not quarantined

- **Problem:** `internal/store/store.go:786` `ON CONFLICT(room_id, author_member_id, author_seq) DO UPDATE SET replicated_status` keeps whichever event arrived first and updates its status, so peers can diverge. The quarantine branch in `community.MaterializeEvents` is unreachable.
- **Fix:** Drop the unique constraint or make it `(room_id, author_member_id, author_seq, id)`. Store both, let the materializer quarantine deterministically (for example, keep the lexically smaller ID and flag the author).
- **Verify:** Test that two peers receiving conflicting events in opposite order converge.

### 26. Double-escaped community text

- **Problem:** `internal/community/sanitize.go:33` HTML-escapes content that React then escapes again, so `&` renders as `&amp;`. The tag-stripping regex also deletes `<3` and `a < b > c`.
- **Fix:** Sanitize for structure only (drop remote image markdown) and let React escape for display. If Markdown rendering is added later, use a sanitizer that operates on the rendered tree, not regexes.
- **Verify:** Snapshot test that `a < b & c` renders unchanged.

### 27. Room-state edge cases

- **Problem:**
  - `handleHostRoom` at `internal/localapi/server.go:413` does not refuse when already in a room; `room_state` gets a second row and `GetRoomState` uses `LIMIT 1` with no ordering.
  - `handleLeaveRoom` at `:645` clears tables but leaves the peer server running and tells no one.
  - Members get a hardcoded room name at `:594`.
  - `:628` derives the creator's member ID from the authority key, which never matches the creator's real member ID, so the creator's address is stored under a phantom ID.
  - Approval mode has no toggle and no approve endpoint, so pending members are stuck forever and re-joining creates a new pending row each time.
  - `handleHostRoom` does not require a display name, and `Membership.Verify` rejects empty names, so a creator without a name produces a roster record peers cannot verify.
- **Fix:** Reject host/join when a room row exists. Stop the peer server on leave. Send `room_name` and the creator's member ID in `JoinResponse`. Either implement approval (toggle in room-admin, `GET /room-admin/pending`, `POST /room-admin/members/{id}/approve`) or remove `approval_mode` from the schema and UI.
- **Verify:** Tests for each bullet.

### 28. Restart-safety of `startPeer` and shutdown

- **Problem:** `cmd/woolwire/main.go` closes over `peerServer` and mutates it from HTTP handler goroutines without synchronization. Shutdown builds a context it never uses and calls `Close` rather than `Shutdown`.
- **Fix:** Guard with a mutex; use `Shutdown(ctx)` on both servers.

### 29. Docs and README drift

- README run command uses `-data`; the flag is `-state`.
- README and `IMPLEMENTATION_PLAN.md` mark M1 through M6 as accepted. Until tasks 1 through 20 land, change those status lines to "implemented, gate not yet passed" and list the missing checks. The security claims in the README trust-model section (port pinning, redirect chains, removal propagation, one-time setup secret, encrypted backups) must match code.
- `docs/openapi.yaml` should be regenerated or hand-checked after the peer API changes in tasks 1 through 5.
- `internal/m0` and `cmd/m0probe` should either be deleted after task 1 reuses them, or moved under `hack/` with a note that they are not built into the product.

---

## Test-suite gaps to close alongside the above

- Add a real-network integration test package that starts two or three `cmd/woolwire` processes with the in-process Tailcat transport and exercises join, removal propagation, inference, and restart. Mark it with a build tag so unit runs stay fast.
- Run the local API through a real `net/http` server in at least one test so timeouts and cookies are exercised.
- Add fuzz tests for `room.ParseInvitation`, `community.Event.Verify`, and `hosting.ValidateDestination`.
- Add `internal/peerapi` and `internal/runner` test files; both packages have none.
