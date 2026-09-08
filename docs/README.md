# Woolwire documentation

Start with [Getting started](GETTING_STARTED.md) to run the app, then the
[feature guide](FEATURES.md) for your first room and model.

## Using and operating Woolwire

| Document | Contents |
|---|---|
| [Getting started](GETTING_STARTED.md) | Docker, Podman, managed GPU/CPU options, native binaries, first login, LAN access, upgrades |
| [Features](FEATURES.md) | Rooms, chats, model hosting, community, contributions, privacy boundaries |
| [Configuration](CONFIGURATION.md) | CLI flags, environment variables, ports, volumes, queue defaults |
| [API guide](API.md) | Authentication, curl examples, SSE, endpoint inventory |
| [OpenAPI 3.1 reference](openapi.yaml) | Machine-readable HTTP contract for local, peer, bootstrap, and runner interfaces |
| [Troubleshooting](TROUBLESHOOTING.md) | Pairing, connection, container, model, and streaming problems |
| [Backup and restore](BACKUP_RESTORE.md) | Encrypted exports, database snapshots, recovery |
| [Security](../SECURITY.md) | Trust boundaries, limitations, responsible reporting |

## Developing and publishing

| Document | Contents |
|---|---|
| [Contributing](../CONTRIBUTING.md) | Source map, local development, tests, documentation maintenance |
| [Validation status](STATUS.md) | Implemented capabilities, recorded evidence, remaining acceptance work |
| [Publishing](PUBLISHING.md) | Repository preparation and release checklist |
| [Dependency inventory](SBOM.md) | Actual manifest sources, build provenance, advisory checks |

## Design and project history

These documents explain intent and prior work. They may describe planned
behavior; use the feature guide and API reference for the current interface.

- [Architecture and trust model](ARCHITECTURE.md)
- [Architecture diagram](architecture.svg) and [Mermaid source](architecture.mmd)
- [Implementation plan and milestone history](IMPLEMENTATION_PLAN.md)
- [Review fixes](REVIEW_TASKS.md)
- [Quality-of-life roadmap](QOL_ROADMAP.md)
- [M0 transport feasibility](M0_FEASIBILITY.md) and [networking notes](M0_NETWORKING.md)
- [Real transport integration tests](../test/integration/README.md)
- [Development helpers and M0 probe](../hack/README.md)
