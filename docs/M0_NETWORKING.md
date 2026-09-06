# M0 networking and relay operations

Reviewed September 5, 2026, against Tailcat `v0.6.0` and the Tailscale revision pinned in `go.mod`.

## Container runtime

Podman is an accepted substitute for Docker for this project. M0 uses non-root containers with a read-only root filesystem, all capabilities dropped, `no-new-privileges`, protected state volumes, and userspace Tailcat networking. A Docker Engine or Docker Desktop rerun is not an M0 requirement.

Separate Podman networks exercise separate application network namespaces on one host. They do not establish a support claim for every physical WAN, NAT, or desktop configuration. The pilot must still exercise those environments.

## Application egress

| Destination | Purpose | Requirement |
|---|---|---|
| Configured DNS resolver | Resolve bootstrap and relay hostnames | Required when connection information relies on hostnames. |
| `tailcat.dev:443/TCP` | Fetch the default DERP map | Required for default first startup; a fully embedded custom region avoids this dependency. |
| Selected relay nodes, normally `443/TCP` | DERP bootstrap and fallback traffic | Required even when peers can subsequently establish a direct path. A custom region may specify another port. |
| Selected relay nodes, normally `3478/UDP` | STUN/NAT discovery | Supports direct-path discovery; relay-only operation remains possible without direct UDP. |
| Peer endpoints over UDP | Direct WireGuard transport | Allow outbound UDP and stateful replies for direct connectivity. |

Tailcat's pinned `createEngine` uses `ListenPort: 0`; do not assume that this probe uses the regular Tailscale daemon's fixed default UDP port. Port `4242` is the application listener **inside** the Tailcat tunnel and needs no published container or host port. The app does not need a Tailscale account or its coordination service.

The underlying transport uses HTTPS for relay connections and UDP for direct paths and STUN. See the [Tailscale firewall guidance](https://tailscale.com/docs/reference/faq/firewall-ports), with the Tailcat-specific differences above confirmed in the [pinned Tailcat source](https://github.com/tailscale/tailcat/blob/v0.6.0/tailcat.go). TLS interception must preserve trusted certificates and the DERP connection upgrade; a generic HTTP reverse proxy is not automatically compatible.

## Relay choice and capacity

The default for development and the initial private pilot is Tailcat's public DERP map. This is a best-effort choice: upstream explicitly provides no uptime SLA or throughput target and may revoke access. No numeric public-relay capacity is assumed or certified by M0, and the test harness must not load-test public relays. [Tailcat v0.6.0 stability and relay documentation](https://github.com/tailscale/tailcat/blob/v0.6.0/README.md#stability)

If the pilot requires assured availability or more fallback capacity, use an operator-controlled DERP region before making that promise. Tailcat supports embedded custom relay details and custom DERP maps. The operator owns bandwidth, monitoring, TLS renewal, and limits; the app must retain application authentication regardless of relay choice. [Bring your own relay](https://github.com/tailscale/tailcat/blob/v0.6.0/README.md#bring-your-own-derp-relay)

## Recovery expectations

Persist the transport key, pre-shared key, resolved region/address, room authority, and admitted identities together. Restoring the same reachable region preserves the bootstrap address. Changing embedded relay information changes the connection information clients need; retaining the application identity alone cannot make an obsolete relay reachable.

A relay migration must preserve identity, explicitly regenerate the address, and distribute updated bootstrap information through a trusted path. Existing members may use a reachable authenticated peer to learn updates in a later milestone; M0 must demonstrate explicit recovery, not claim automatic discovery from dead rendezvous information. Keep the old relay available during a planned overlap where possible. Never log full Tailcat addresses or invitation codes: they carry capabilities.
