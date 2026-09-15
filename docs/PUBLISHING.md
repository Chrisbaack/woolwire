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
- [x] Select a project license and add its exact text and attribution. The
  project is [MIT](../LICENSE), copyright Chris Baack. Every Go module and npm
  package actually linked into a build is permissive (MIT, BSD-2/3-Clause, ISC,
  or Apache-2.0), so no copyleft obligation attaches; see
  [the dependency inventory](SBOM.md).
- [x] Publish the vulnerability reporting process in [SECURITY.md](../SECURITY.md).
  GitHub private advisory reporting is the documented channel. **Turning the
  setting on is a post-publication step:** the API rejects it on a private
  repository. See [after making the repository public](#after-making-the-repository-public).
- [x] Review the staged diff and scan the **entire history** for credentials and
  private data. Ignore rules do not remove anything already committed.
- [x] Review repository metadata, default branch, issue settings, and access.
- [x] Make the Go module path match the repository that will host it. The
  module is `github.com/Chrisbaack/woolwire`; a mismatch breaks `go install`
  and points readers at an unrelated account.
- [x] Community health files: issue forms, a pull request template, and a
  [CI workflow](../.github/workflows/ci.yml) running the contributor checks on
  every push and pull request.

What the history scan covered, so the claim above is checkable: every path ever
added across all refs (only `.env.example` files match the sensitive-name
patterns, and both contain empty or generated placeholder values); every added
line across the full patch history for OpenAI, GitHub, AWS, Slack, Google,
Hugging Face and Tailscale key formats, PEM private key blocks, and quoted
credential assignments (all hits are documentation snippets that generate a
random token, or test fixtures such as `runner-token`); long hex strings (all
synthetic digests in tests); and embedded PNG metadata in the screenshot (no
text or EXIF chunks). Author identities were rewritten before publication, and
the backup and `refs/original/` refs the rewrite left behind were deleted
afterwards, so there is nothing left to hunt for: `git for-each-ref` lists only
`main` and its remote counterparts. As general practice, a checkout that still
holds rewrite leftovers should not be pushed with `git push --all` or
`--mirror`, which would publish them; push only `main`.

## After making the repository public

These settings do not exist on a private repository, so they can only be turned
on once the repository is public. Do them in this order.

1. **Private vulnerability reporting.** Settings → Advanced Security → Private
   vulnerability reporting → Enable, or
   `gh api -X PUT repos/Chrisbaack/woolwire/private-vulnerability-reporting`.
   [SECURITY.md](../SECURITY.md) already sends reporters to the advisory form,
   so this should be enabled at the same time the repository becomes public,
   not later.
2. **Dependabot alerts** and **secret scanning with push protection.** Both are
   free on public repositories and neither runs Actions minutes.
3. Confirm the default branch. It is `main`, renamed from `master` through
   GitHub's own rename so existing clones and links redirect. The branch filter
   in [ci.yml](../.github/workflows/ci.yml) is the one place that names it, and
   the links in `.github/ISSUE_TEMPLATE` use `blob/HEAD` so they followed the
   rename on their own.

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
