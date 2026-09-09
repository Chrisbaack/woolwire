# Implementation and validation status

This page separates what the checkout implements from what has been demonstrated
in deployment. Historical milestone headings saying “accepted” do not override
the limitations recorded here and at the top of the implementation plan.

## Current capability and evidence

| Area | Implemented | Evidence and remaining work |
|---|---|---|
| Transport | Persistent Tailcat identity, authenticated peer and bootstrap listeners | M0 real-network feasibility evidence exists. Full application multi-process acceptance over real relays remains outstanding. |
| Onboarding | Owner pairing, host/join, invitations, approval/removal | Package regressions and recorded single-node Podman pairing/replay/CSRF checks. Real multi-node admission/removal gate outstanding. |
| Chat and external models | Catalog, external adapters, shared fair queue, saved/no-save chats, SSE, compatible client API | Package and frontend tests; real cross-process host/requester gate outstanding. |
| Managed models | GGUF discovery/downloads, companion association, saved on-demand hosting, load/unload, runner controller | Tests use a fake engine. Real llama-server loading, GPU behavior, and a peer requesting managed inference need end-to-end evidence. |
| Community | Signed channels/events, sync, edits/deletion/moderation, retention | Package tests; real partition/rejoin and convergence gates outstanding. |
| Contributions | Joint receipts, scoring cap, self-service exclusion, opt-out | Package tests; real multi-node receipt convergence outstanding. |
| UI | Meadow theme, model filters, responsive navigation, expanded chat/community layouts | Build/frontend tests and Chromium checks with synthetic data at mobile/tablet/desktop sizes; not proof of deployed model behavior. |
| Operations | JSON metrics, encrypted export/decryption helper, container definitions | Backup and protocol tests; full deployment matrix, restore drill, pilot, and external security review remain release work. |

The recorded single-node Podman check used the managed profile's network shape:
the UI answered on loopback, a real Tailcat address survived restart, setup secret
replay was refused, and a cross-origin POST was refused. It did not establish
that every deployment variant or GPU works. See [review history](REVIEW_TASKS.md)
and [M0 evidence](M0_FEASIBILITY.md).

## Current interface limits

The shipped chat interface is text-only. Projector discovery does not mean the
UI accepts image attachments. Sampler controls, tool execution, embeddings,
multiple rooms per installation, and automatic administrator succession are
not current public interfaces. The host-limit record exposes queue and timeout
settings, not schedules or a full resource-policy engine.

Context and generation statistics include approximations. No-save mode prevents
transcript-body writes by Woolwire but does not hide a request from its selected
host or impose retention on external servers.

## Reproducing checks

Follow [Contributing](../CONTRIBUTING.md) for build, package/frontend tests, and
documentation checks. The real transport suite is opt-in:

```sh
go test -tags integration -timeout 20m ./test/integration/
```

A test result belongs to the exact commit, toolchain, runtime, and hardware used.
Record those along with failure details before updating this page. Do not treat
a successful build, a schema check, or a mock backend as an acceptance gate for
live inference.

[Publication checklist](PUBLISHING.md) · [Implementation plan](IMPLEMENTATION_PLAN.md)
