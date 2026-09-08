# Publication checklist

The repository can be prepared for public review before claiming a production
release. This checklist records the remaining decisions and verification; it
does not mean the repository has been pushed, made public, or tagged.

## Repository preparation

- [x] Public README with a base quick start and links to detailed guides.
- [x] Feature, configuration, startup, troubleshooting, and API documentation.
- [x] Explicit distinction between design goals and implemented interfaces.
- [x] Endpoint inventory checked against Go registrations and OpenAPI.
- [x] Contribution and security guidance without invented support promises.
- [x] Example environment files containing no real token.
- [x] Git and container-context exclusions for local environment files, keys,
  databases, model weights, backups, and logs.
- [ ] Select a project license and add its exact text and attribution.
- [ ] Configure a private vulnerability reporting channel, then update SECURITY.md.
- [ ] Review the staged diff and scan the **entire history** for credentials and
  private data. Ignore rules do not remove anything already committed.
- [ ] Review repository metadata, default branch, issue settings, and access.

Before committing:

```sh
python3 scripts/check_docs.py
git diff --check
git status --short --untracked-files=all
git diff --cached --stat
```

Review source changes and rebuilt frontend assets together. Include the actual
hashed assets referenced by `web/dist/index.html`; do not stage only the HTML.
The managed `.env` must remain local. `git check-ignore deploy/managed/.env`
should show it is ignored; that alone is not a secret scan.

If a live credential is found in history, rotate it and agree on history cleanup
before publishing. Do not publish a database or setup log as sample data. Use
synthetic data for screenshots and fixtures.

## Before an announced release

- [ ] Run the documented build, frontend tests, `go vet`, and Go race tests on
  the release commit.
- [ ] Validate OpenAPI 3.1, local documentation links, and startup commands.
- [ ] Complete the real transport integration gate and record relay/runtime details.
- [ ] Exercise real external and managed inference, cancellation, restart, and
  withdrawal; record model, engine revision, GPU/driver, and memory settings.
- [ ] Test the deployment variants you intend to advertise as supported.
- [ ] Perform an encrypted backup/restore drill, including creator recovery.
- [ ] Complete the pilot/security review or explicitly state the remaining limits.
- [ ] Pin or record container image digests and produce a machine-readable SBOM.
- [ ] Review dependency/model licenses and current vulnerability advisories.
- [ ] Choose a release version, build immutable artifacts, record checksums and
  provenance, and publish release notes describing tested scope and limitations.
- [ ] Push a `vX.Y.Z` tag to run [`.github/workflows/release.yml`](../.github/workflows/release.yml),
  which cross-builds both binaries for linux/darwin/windows on amd64/arm64,
  attaches them with a combined `SHA256SUMS.txt` to a **draft** GitHub Release
  for review before publishing, and builds and pushes both container images to
  `ghcr.io/<owner>/woolwire` and `ghcr.io/<owner>/woolwire-runner`. It only
  triggers on a version tag, so it costs nothing between releases; on a public
  repository it costs nothing at all (unlimited Actions minutes and Packages
  storage). On a private repository it draws from the free plan's 2,000
  minutes/month and 500MB Packages storage.

[STATUS.md](STATUS.md) is the source for current validation claims.
[SBOM.md](SBOM.md) is a dependency inventory, not a generated, complete SBOM or
proof of a reproducible release.
