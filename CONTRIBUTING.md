# Contributing to Woolwire

Start with the [feature guide](docs/FEATURES.md),
[validation status](docs/STATUS.md), and [architecture](docs/ARCHITECTURE.md).
For a bug, include the commit, operating system, deployment method, reproduction
steps, expected result, and sanitized error output. Share security-sensitive
findings through the process in [SECURITY.md](SECURITY.md).

The project license is still undecided. Resolve that with the maintainer before
submitting contributions that depend on specific licensing terms.

## Local setup

Use Go **1.27.1** as specified in `go.mod` and Node.js **22.6+** so the frontend's
`--experimental-strip-types` tests run. The app build is pure Go; race tests need
a supported platform and C toolchain. Python 3 runs the documentation checker.

```sh
npm --prefix web ci
npm --prefix web run build
CGO_ENABLED=0 go build -trimpath -o bin/woolwire ./cmd/woolwire
./bin/woolwire -state ./state
```

The database in `state/` is local development data, not a fixture. Use a different
state directory for an independent identity. Never run two processes against
the same live database or copy an identity into two simultaneously active nodes.

## Frontend development

The working application serves React assets from the Go binary. After editing
the UI:

```sh
npm --prefix web run build
CGO_ENABLED=0 go build -trimpath -o bin/woolwire ./cmd/woolwire
# Stop the running app, then restart this rebuilt binary with the same state.
./bin/woolwire -state ./state
```

`npm --prefix web run dev` runs Vite, but the current Vite config has no backend
proxy. Visiting its port alone is not an authenticated full-stack development
setup. Use the embedded build loop above unless you deliberately configure a
same-origin development proxy. Do not work around API errors by disabling Host,
CSRF, or authentication checks.

`web/dist` is tracked because Go embeds it. Include rebuilt assets with frontend
changes. Do not commit `web/node_modules`, local `.env` files, model weights,
backups, or setup output.

## Source map

| Path | Responsibility |
|---|---|
| `cmd/woolwire` | App process, flags, persistent identity and transport startup |
| `cmd/woolwire-runner` | Companion controller process |
| `internal/localapi` | Browser/owner API, client API, room operations, background sync |
| `internal/peerapi` | Peer and admission protocols |
| `internal/identity`, `internal/peerauth`, `internal/room` | Signing, membership, authentication |
| `internal/transport` | Transport abstraction and test transport |
| `internal/hosting`, `internal/modelpath` | External adapters, artifact discovery/downloads, runner client, path boundaries |
| `internal/inference` | Shared admission, queue, context checks, dispatch |
| `internal/runner` | Engine process lifecycle and runner HTTP API |
| `internal/store` | SQLite schema and persistence |
| `internal/catalog`, `internal/community`, `internal/contributions` | Advertisements and replicated room data |
| `internal/sse` | Streaming response utilities |
| `web/src` | React/TypeScript UI |
| `deploy` | Base and managed Compose configurations |
| `test/integration` | Separate-process, real Tailcat acceptance tests |
| `hack` | Feasibility probes and backup decryption helper |
| `docs` | User guides, OpenAPI, design and historical evidence |

## Checks before proposing a change

```sh
npm --prefix web run build
npm --prefix web test
go vet ./...
go test -race ./...
python3 scripts/check_docs.py
git diff --check
```

Run the checks relevant to your change and report exactly what you ran. Add
regression coverage for meaningful behavior changes, especially authentication,
replication, inference lifecycle, and persistence. Package tests use local test
servers and an in-memory transport; after dependencies are installed they do not
exercise public relays or require a real model engine.

Real-network acceptance is separate and takes minutes:

```sh
go test -tags integration -timeout 20m ./test/integration/
```

See [the integration guide](test/integration/README.md). Do not report a fake
engine test as proof of real CUDA, model loading, or cross-process inference.

## Keep interfaces and documentation together

When adding or changing a route, update its implementation and
[docs/openapi.yaml](docs/openapi.yaml), then regenerate the endpoint inventory:

```sh
python3 scripts/check_docs.py --write
python3 scripts/check_docs.py
```

The checker compares explicit Go route registrations with the OpenAPI operations,
checks the generated inventory, and validates local Markdown paths/headings.
It uses no third-party Python packages. It is not a full YAML/OpenAPI schema
validator and does not check external URLs or prove runtime behavior. For full
OpenAPI 3.1 validation, with `uv` installed (downloads validator dependencies):

```sh
uv run --no-project --with openapi-spec-validator python -c \
  "from openapi_spec_validator import validate; import yaml; validate(yaml.safe_load(open('docs/openapi.yaml')))"
```

Update startup/configuration guides when flags, environment variables, ports,
volumes, or defaults change. Treat roadmap/design documents as proposals and
keep the current feature guide honest. Use repository-relative links, synthetic
examples, and no local workstation paths or real credentials.

A useful change description explains the problem, resulting behavior, relevant
tradeoffs, and verification. Keep unrelated working-tree changes out of your
patch; do not replace someone else's in-progress work.
