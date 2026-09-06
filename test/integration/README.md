# Integration tests

These start real `cmd/woolwire` processes and drive them over their local
HTTP APIs. Unlike the package tests, they use the real Tailcat transport, so
they need outbound network access to reach DERP relays and `tailcat.dev`, and
they take minutes rather than milliseconds.

They are behind the `integration` build tag so a normal `go test ./...` stays
fast and offline:

    go test -tags integration -timeout 20m ./test/integration/

What they cover that the package tests cannot:

- Two nodes joining over real Tailcat, not the in-memory transport.
- A removal propagating between separate processes.
- Inference across a process boundary.
- Restart: the Tailcat address, the room, and the roster all survive, which
  is what proves the node key, the pre-shared key, and the pinned DERP region
  are actually persisted.
