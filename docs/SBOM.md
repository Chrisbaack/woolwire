# Software Bill of Materials (SBOM) & Release Verification

## 1. Primary Components & Dependencies

| Component | Pinned Version / Revision | Purpose | Verification Mechanism |
|---|---|---|---|
| **Go Toolchain** | `1.27.1` | Backend compiler and runtime | SHA-256 binary hash, Go checksum database (`go.sum`) |
| **Tailcat** | `v0.6.0` (Tailscale) | Peer-to-peer transport & NAT traversal | Pinned in `go.mod`, isolated behind `transport` seam |
| **SQLite Driver** | `modernc.org/sqlite v1.58.0` | Embedded pure-Go SQLite engine | No CGO required; reproducible pure-Go build |
| **llama.cpp / Engine** | `ghcr.io/ggerganov/llama.cpp:server` | Isolated local model runner | Pinned container digest; restricted argument parsing |
| **React** | `^18.2.0` | Embedded Single-Page Application UI | Pinned in `package-lock.json`, built via Vite |
| **Vite** | `^5.4.21` | Frontend build pipeline & asset embedder | Built into `internal/localapi/dist` / `web/dist` |
| **TypeScript** | `^5.0.2` | Frontend type verification | Strict compiler checks (`tsc --noEmit`) |

## 2. Reproducible Build Instructions

All Woolwire binaries are built without CGO to ensure deterministic compilation across Linux environments:

```bash
# Build standalone Woolwire binary (includes embedded frontend)
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=v1.0.0" -o bin/woolwire ./cmd/woolwire

# Build companion runner binary
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=v1.0.0" -o bin/woolwire-runner ./cmd/woolwire-runner
```

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
- **Transport Security:** Mutual TLS 1.3 with pinned Ed25519 room-authority root
- **Hashing & IDs:** SHA-256 (`evt-...` canonical IDs, model artifact verification)
- **Local Authentication:** Constant-time SHA-256 setup secret comparison via `crypto/subtle`
- **Telemetry:** Strictly disabled (`telemetry_enabled: false`); no external phoning home or usage metrics.
