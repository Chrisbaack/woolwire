# hack

Nothing here is built into the Woolwire product. These are development and
evidence artifacts kept because they document how a decision was reached.

## `m0` and `m0probe`

The M0 transport feasibility probe: a minimal mutual-TLS-over-Tailcat
implementation with room-authority certificate pinning, plus the CLI that
drives it. It is the evidence behind
[docs/M0_FEASIBILITY.md](../docs/M0_FEASIBILITY.md) and the M0 acceptance gate.

The shipped code does not import it. The product's peer authentication lives in
`internal/peerauth`, which took M0's approach — a certificate pinned to the room
authority, mutual TLS 1.3, and a rejection of substituted peers — and adapted it
to the v1 seam: the peer API needs roster-based verification of ordinary HTTP
requests, not M0's fixed-port framed protocol.

The probe is kept rather than deleted because a Tailcat upgrade must rerun this
gate, and rebuilding it from scratch to do so would be wasteful. Its tests run
with the rest of the suite:

    go test ./hack/m0/

## `m0-compose`

The Podman/Docker Compose file that ran the probe as three isolated containers
for the M0 gate. It builds `m0probe`, not the Woolwire app.

    podman compose -f hack/m0-compose/compose.yaml up
