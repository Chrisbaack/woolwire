# Dependency inventory and build provenance

This is a human-maintained inventory of primary components. It is **not** a
complete machine-readable SBOM. The checked-in manifests/lockfiles are the
version sources; release artifacts still need provenance, image digests, and
an actual generated SBOM.

## Primary components

| Component | Version source / current value | Purpose |
|---|---|---|
| Go | [go.mod](../go.mod), `go 1.27.1` | Backend and controller toolchain |
| Tailcat | `github.com/tailscale/tailcat v0.6.0` in go.mod | Peer transport |
| SQLite driver | `modernc.org/sqlite v1.58.0` in go.mod | Embedded pure-Go SQLite |
| x/crypto | `golang.org/x/crypto v0.56.0` in go.mod | Backup encryption/key derivation |
| x/term | `golang.org/x/term v0.45.0` in go.mod | Interactive backup decryption prompt |
| React / React DOM | `18.3.1` in [package-lock.json](../web/package-lock.json) | UI |
| Vite | `5.4.21` in package-lock.json | Frontend build |
| TypeScript | `5.9.3` in package-lock.json | Frontend type checking |
| qrcode / jsqr | `1.5.4` / `1.4.0` in package-lock.json | Room invitation QR rendering/scanning |
| llama.cpp runtime | `ghcr.io/ggml-org/llama.cpp:server-cuda` in [Dockerfile.runner](../Dockerfile.runner) | Managed engine, CUDA-capable image |
| App build/runtime images | `node:20-alpine`, `golang:1.27-alpine`, `alpine:3.20` in [Dockerfile](../Dockerfile) | Container build and runtime |

`go.sum` and `package-lock.json` record dependency checksums. Semver ranges in
`package.json` are not the installed versions. Use `npm ci` for local repeatable
installs. The app Dockerfile currently uses `npm install`, and image tags are
mutable; neither Dockerfile pins all base images to digests. The runner image
comment describing it as pinned does not make its tag immutable.

Node 20 builds the current container frontend; use Node **22.6+** to run the
repository's TypeScript-stripping frontend test command outside that builder.
## License review

Woolwire itself is [MIT](../LICENSE). The dependencies actually linked into a
build were reviewed for license compatibility on 2026-09-07, at the versions
recorded in `go.mod`/`go.sum` and `web/package-lock.json`:

| Set | How it was enumerated | Result |
|---|---|---|
| Go modules | `go list -deps ./...` mapped to modules, then the `LICENSE` file in each module cache directory | 48 modules: BSD-3-Clause, BSD-2-Clause, MIT, ISC (`github.com/coder/websocket`), Apache-2.0 |
| npm packages | `license` fields in `web/package-lock.json` | 148 packages: MIT, ISC, Apache-2.0, BSD-3-Clause, CC-BY-4.0 |

No copyleft license (GPL, LGPL, AGPL, MPL, SSPL) appears in either set, so MIT
carries no reciprocal obligation from a dependency. Note that `go list -m all`
reports the full module *graph* (over 600 modules), most of which are never
built; the linked set above is the one that matters for distribution.

This is a point-in-time check, not a continuously enforced policy. Re-run it
when dependencies change. Model weight and external backend licenses are
separate and must be reviewed for the models actually used; Woolwire does not
evaluate them for you.

## Build from a reviewed checkout

```sh
npm --prefix web ci
npm --prefix web run build
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/woolwire ./cmd/woolwire
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/woolwire-runner ./cmd/woolwire-runner
sha256sum bin/woolwire bin/woolwire-runner
go version -m bin/woolwire
```

`CGO_ENABLED=0` removes the C toolchain requirement for the app/controller build;
llama-server is a separate native runtime. `-trimpath` removes local source paths.
Stripping debug symbols reduces size; it does not by itself prove identical
build output. Record the source commit, Go/Node/npm versions, OS/architecture,
flags, dependency locks, and base-image digests to reproduce release artifacts.

No `main.version` release field is currently defined for linker injection; do
not claim a version was embedded by passing an unused `-X main.version=...` flag.
Only the app and runner controller are primary product binaries. Tools under
`hack/`, including backup decryption, are built separately when needed.

## Advisory checks for a release

With the corresponding scanners installed and their advisory data current:

```sh
govulncheck ./...
npm --prefix web audit
# Build explicitly named local images before scanning them:
docker build -t woolwire:local .
docker build -f Dockerfile.runner -t woolwire-runner:local .
trivy image woolwire:local
trivy image woolwire-runner:local
```

Record scan dates and assess findings against the exact artifact. These commands
are procedures, not a statement that scans are currently clean. Dependency
updates can require code changes and new regression/acceptance runs.

## Cryptographic and privacy mechanisms

Peer identity uses Ed25519 and membership-aware TLS 1.3. Signed protocol records
use domain-separated payload encodings with a signature-version field. Model
artifact checksums use SHA-256. Setup authentication compares a secret hash;
the separate local API bearer token is checked in constant time.

Backups use NaCl secretbox with an Argon2id-derived key, documented in
[Backup and restore](BACKUP_RESTORE.md). Product usage telemetry is disabled;
network activity for Tailcat, peers, configured backends, and requested downloads
still occurs. See [Security](../SECURITY.md) for the limits of these controls.
