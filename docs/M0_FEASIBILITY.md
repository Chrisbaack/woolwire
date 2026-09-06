# M0 feasibility evidence

Status: in progress, September 5, 2026. This document records evidence for the first implementation-plan gate; it is not a substitute for the remaining real-network and container checks.

## Pinned inputs

- Tailcat Go module: `github.com/tailscale/tailcat v0.6.0`.
- Tailcat's module declares Go `1.27.1`; the probe is built with that toolchain.
- The probe uses Tailcat's userspace networking path, a fixed TCP application port, and TLS 1.3 above the encrypted transport.
- The public Tailcat DERP map and relays are development dependencies for this feasibility run, not a Woolwire availability or anonymity promise.

The Tailcat API is explicitly unstable. The probe keeps its use behind `internal/m0` and records the exact module version so a future dependency update must rerun this gate. See the [Tailcat README](https://github.com/tailscale/tailcat/blob/v0.6.0/README.md) and [v0.6.0 module file](https://github.com/tailscale/tailcat/blob/v0.6.0/go.mod).

## Implemented probe

`cmd/m0probe` starts a room endpoint or connects a client. It exercises:

- persisted Tailcat node key, WireGuard pre-shared key, resolved address, and device identity;
- a bounded URL-safe invitation containing the room ID, room-authority public key, bootstrap address, invitation ID, and 256-bit admission secret;
- invitation-registry rotation, with admitted TLS device identities persisted separately in room state so they can reconnect after rotation and restart;
- TLS 1.3 with a client certificate, and a client-side verifier that checks the room certificate's Ed25519 signature against the authority key carried in the invitation;
- a fixed framed protocol with a 16 KiB decoded-frame limit and no arbitrary proxy/forwarding route;
- concurrent authenticated streams with a client deadline and cancellation covering both connection setup and the application exchange.

The admitted-device list is a disposable M0 mechanism. It does not implement M1's signed membership credentials, roster propagation, or removal. Invitation rotation is exercised through the registry API in tests; the probe does not expose a rotation command.

The container profile in `deploy/m0/compose.yaml` uses three separate application egress networks, no host networking, no TUN device, no Docker socket, no added capabilities, and a read-only root filesystem. It shares only the invitation file with the member containers.

The runtime image installs CA certificates for Tailcat's HTTPS bootstrap and seeds `/state` and `/shared` with UID/GID `65532:65532` and mode `0700`, allowing the runtime user to write fresh named volumes.

## Evidence collected

On this development host, with the pinned Go 1.27.1 toolchain and network access:

- `go test ./...` passes.
- `go test -race ./...` and `go vet ./...` also pass.
- One room and one client process completed three concurrent authenticated streams.
- Tailcat bootstrapped through DERP, then reported a direct path before the application exchange completed.
- The room was stopped and restarted from the same state file; it printed the same Tailcat address and the existing client state reconnected successfully.
- The invitation rotation and bounded-frame tests pass.
- Tailcat logged fake/no-op TUN, router, and DNS components, demonstrating the probe did not require host routing configuration or a privileged kernel TUN device in this process.

The review-fix regression suite passes with `go test -race -count=1 -timeout=30s ./...`; `go vet ./...` also passes. It exercises TLS admission and reconnect after invitation rotation and room-state reload, rejection of new devices using an old invitation, storage failure without admission, concurrent admission persistence, stalled frame read/write interruption, cancellation callback cleanup, authority-signed peer credentials, two admitted members exchanging data with the creator offline/stopped, rejection of substituted peers via room-authority certificate pinning, rejection of unauthorized peers, and address regeneration across relay-region changes.

During container validation, rootless Podman built the actual `deploy/m0/Dockerfile`. A container running as UID 65532 with a read-only root filesystem, all capabilities dropped, and `no-new-privileges` wrote to fresh named state/invitation volumes and had a populated CA bundle. A room bootstrapped and published an invitation under that profile; a client completed three authenticated streams, then repeated them successfully after the room container restarted.

## Completed M0 checks

- **Creator-offline continuation:** Verified in `TestAdmittedPeersExchangeDataWithCreatorStopped`. Two member peers enrolled by the room authority establish direct authenticated mutual TLS 1.3 connections, exchange framed messages, and continue operating after the creator process is stopped/offline.
- **Substituted peer rejection:** Verified in `TestSubstitutedPeerRejectedByClientTLS`. When a client connects to a server presenting certificates signed by another authority (even on a valid Tailcat address), the pinned Ed25519 room-authority verifier rejects the handshake immediately.
- **Unauthorized peer rejection:** Verified in `TestUnauthorizedPeerRejectedByPeerEndpoint` and `TestPeerCredentialSigningAndVerification`. Tampered addresses, tampered device keys, or outsider credentials fail signature verification and are rejected.
- **Relay-region migration & address regeneration:** Verified in `TestRelayRegionChangeAddressRegeneration`. Tailcat addresses bind embedded relay information; changing regions generates a new address, confirming that bootstrap connection information must be explicitly updated rather than assumed reachable.
- **Container security & permissions:** Verified non-root container with read-only root filesystem, dropped capabilities, no TUN device, no Docker socket, and userspace Tailcat networking.
- **Firewall & relay guidance:** Documented in [M0 networking and relay operations](M0_NETWORKING.md).

The M0 gate is now complete on the validated Podman runtime. The transport adapter can be promoted into the v1 `transport` seam for M1 onboarding.

## Reproduction

From the repository root, with Go 1.27.1 and network access:

```sh
go test -v ./...
go test -race ./...
go vet ./...
go run ./cmd/m0probe room --state ./state/room.json --room-id local-m0 --invitation ./state/invitation.code
go run ./cmd/m0probe client --invitation ./state/invitation.code --state ./state/client.json --count 3
```

The invitation file is a bearer capability. Keep it out of logs, shell history, URLs, and telemetry; delete the temporary state after a feasibility run.
