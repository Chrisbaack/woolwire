## What this changes

<!-- The problem, the resulting behavior, and any tradeoff you chose. -->

## How it was verified

<!--
Report exactly what you ran, not what you would normally run. Delete lines you
did not run rather than leaving them unchecked without explanation.
-->

- [ ] `npm --prefix web run build`
- [ ] `npm --prefix web test`
- [ ] `go vet ./...`
- [ ] `go test -race ./...`
- [ ] `python3 scripts/check_docs.py`
- [ ] `go test -tags integration -timeout 20m ./test/integration/` (real network; usually not needed)

A fake-engine test is not evidence of real CUDA, model loading, or cross-process
inference. Say so if a result is simulated.

## Checklist

- [ ] Regression coverage added for behavior changes to authentication,
      replication, inference lifecycle, or persistence.
- [ ] Route changes updated `docs/openapi.yaml` and regenerated the inventory
      with `python3 scripts/check_docs.py --write`.
- [ ] Flag, environment variable, port, volume, or default changes updated the
      startup and configuration guides.
- [ ] Frontend changes include the rebuilt `web/dist` assets that
      `web/dist/index.html` actually references.
- [ ] No credentials, invitations, database files, model weights, or local
      workstation paths in the diff.
- [ ] Unrelated working-tree changes kept out of this patch.

By submitting this pull request I agree that my contribution may be distributed
under the project's [MIT License](../LICENSE).
