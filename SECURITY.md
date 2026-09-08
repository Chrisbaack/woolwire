# Security

Woolwire is an early project intended for evaluation in private rooms with
people you trust. There is no externally audited release or published security
support window. See [validation status](docs/STATUS.md) for outstanding work.

## Reporting a vulnerability

Do not put an exploit containing real credentials, invitations, private chat
content, or databases in a public issue. A dedicated private reporting channel
has not yet been configured. If the repository offers **Security → Report a
vulnerability**, use it. Otherwise ask the maintainer for a private contact
channel with a minimal, non-sensitive description before sending details.

Include the affected commit, deployment mode, steps to reproduce using synthetic
data, impact, and any suggested fix. No response-time or bounty program is
currently promised. Enabling and publishing the private reporting route is a
[publication task](docs/PUBLISHING.md).

## Trust boundaries

- **Owner interface:** pairing a browser grants control of that installation,
  including access to saved chats and settings. The UI is loopback-published by
  default; allowing LAN access changes who can reach the pairing surface.
- **Room membership:** peer TLS identities are checked against signed admitted
  membership. Bootstrap pins the room authority from an invitation. The creator
  controls admission and removal, not private requester chat storage.
- **Inference host:** the selected host and its backend receive conversation
  content to generate a reply. Trust them with the content you submit. No-save
  mode controls Woolwire transcript persistence, not another server's logging.
- **Community:** messages and contribution receipts are replicated room data.
  Removal and deletion cannot erase copies already retained outside the app.
- **Runner:** the managed controller has a separate bearer credential, read-only
  model access, no app state mount, and an internal-only network. It shares the
  host kernel and GPU driver with the host; this is not a VM boundary.

## Operational limitations

Membership removals cannot propagate through a network partition. Existing
members can continue without a reachable creator; they cannot prove that no
newer unseen removal exists elsewhere.

SQLite contains private keys and credentials. It is opened with restricted file
permissions but is not encrypted at rest by the app. Protect the host/storage
and keep encrypted backups. Restored identities must not run simultaneously on
multiple nodes. The app exposes no individual-browser session revocation API.

The local app and native runner HTTP listeners do not terminate TLS. Protect LAN
or remote access at the network/proxy layer. Host and CSRF checks are separate
from authentication. Do not disable those checks to make a generic client work;
configure the required header instead.

External endpoint private-network opt-in permits destinations and private HTTP
that are otherwise refused; it is an explicit trust decision for that endpoint.
Downloads and inference contact external services as configured. Tailcat uses
rendezvous/relay infrastructure, whose operators can observe connection metadata.
`telemetry_enabled:false` means no product usage telemetry, not no network traffic.

Container base images currently use mutable tags. Dependency hashes, isolated
execution, and model file checksums are useful controls, not proof of an audited
supply chain. The [dependency inventory](docs/SBOM.md) records current sources
and release-provenance gaps.

## Before sharing diagnostics

Remove setup secrets, cookies, bearer tokens, backend API keys, invitation codes,
private prompts, database files, and local paths you do not intend to disclose.
The initial startup log contains a redeemable owner secret. Compose `config`
and container environment inspection can reveal the runner token; use quiet
validation for config checks and avoid posting expanded output.
