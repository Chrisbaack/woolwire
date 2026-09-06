# Software Bill of Materials (SBOM) & Release Verification

## 1. Primary Components & Dependencies

| Component | Pinned Version / Revision | Purpose | Verification Mechanism |
|---|---|---|---|
| **Go Toolchain** | `1.27.1` | Backend compiler and runtime | SHA-256 binary hash, Go checksum database (`go.sum`) |
| **Tailcat** | `v0.6.0` (Tailscale) | Peer-to-peer transport & NAT traversal | Pinned in `go.mod`, isolated behind `transport` seam |
| **SQLite Driver** | `modernc.org/sqlite v1.58.0` | Embedded pure-Go SQLite engine | No CGO required; reproducible pure-Go build |
| **golang.org/x/crypto** | `v0.55.0` | Argon2id key derivation and NaCl secretbox for encrypted backups | Pinned in `go.mod`; standard-library-adjacent, no CGO |
| **golang.org/x/term** | `v0.45.0` | Passphrase prompt in `hack/backup-decrypt` | Pinned in `go.mod`; not linked into the product binaries |
| **llama.cpp / Engine** | `ghcr.io/ggerganov/llama.cpp:server` | Isolated local model runner | Pinned container digest; restricted argument parsing |
| **React** | `^18.2.0` | Embedded Single-Page Application UI | Pinned in `package-lock.json`, built via Vite |
| **Vite** | `^5.4.21` | Frontend build pipeline & asset embedder | Built into `internal/localapi/dist` / `web/dist` |
| **TypeScript** | `^5.0.2` | Frontend type verification | Strict compiler checks (`tsc --noEmit`) |
| **qrcode / jsqr** | `^1.5.4` / `^1.4.0` | Render and scan **invitation** codes only | Pinned in `package-lock.json`. Never used for credentials: the setup secret is not encoded in a QR or a URL. |

## 2. Reproducible Build Instructions

All Woolwire binaries are built without CGO to ensure deterministic compilation across Linux environments:

```bash
# Build the frontend first; the Go binary embeds web/dist
npm --prefix web ci && npm --prefix web run build

# Build standalone Woolwire binary (includes embedded frontend)
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=v1.0.0" -o bin/woolwire ./cmd/woolwire

# Build companion runner binary
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=v1.0.0" -o bin/woolwire-runner ./cmd/woolwire-runner
```

Only `cmd/woolwire` and `cmd/woolwire-runner` ship. Everything under `hack/`
(the M0 feasibility probe and the backup decryption helper) is built on demand
and is not part of a release.

Build flags explanation:
- `CGO_ENABLED=0`: Eliminates external libc and dynamic link dependencies.
- `-trimpath`: Strips absolute file system paths from compiler metadata.
- `-ldflags="-s -w"`: Strips symbol tables and debug information to ensure consistent binaries across build hosts.

## 3. Vulnerability Scanning and Advisory Procedures

1. **Go Dependency Audit:**
   ```bash
   govulncheck ./...
   ```
2. **Container Image Scanning:**
   ```bash
   trivy image woolwire:latest
   trivy image woolwire-runner:latest
   ```
3. **NPM Security Audit:**
   ```bash
   npm --prefix web audit
   ```

## 4. Cryptographic Primitives

- **Peer & Device Identity:** Ed25519 (32-byte public key, 64-byte signature)
- **Peer Authentication:** Mutual TLS 1.3. The peer API verifies the client
  certificate's Ed25519 key against an admitted roster row whose membership
  signature verifies under the pinned room authority. Bootstrap pins an
  authority-signed certificate instead, because the joiner has no roster yet.
  Session tickets are disabled: each peer request is a fresh connection.
- **Signed Payloads:** Length-prefixed, domain-separated encodings
  (`identity.SigningPayload`) carrying a `sig_version` field, so no combination
  of display names, model names, or message bodies can collide with a different
  record. Version 0 is the legacy colon-joined form, still verified but never
  produced.
- **Hashing & IDs:** SHA-256 (`evt-...` canonical IDs, model artifact verification)
- **Local Authentication:** Constant-time SHA-256 comparison of the one-time
  setup secret and the local API bearer token, via `crypto/subtle`. The setup
  secret is 128 bits, single-use, and rate limited per client address.
- **Backups:** NaCl secretbox (XSalsa20-Poly1305) under a key derived with
  Argon2id (time 3, memory 64 MiB, 4 threads, 16-byte salt).
- **Telemetry:** Strictly disabled (`telemetry_enabled: false`); no external phoning home or usage metrics.
