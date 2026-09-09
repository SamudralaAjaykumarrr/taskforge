# Supply-Chain Security & Release Process

Status: **implemented** (docs/enterprise-roadmap.md Phase 10 — Supply-Chain
& Release Hardening). This document explains what Phase 10 built, exactly
what each control proves and does not prove, and how an operator or
reviewer independently verifies a release. It supersedes the "Supply-chain
security" rows in [docs/security-model.md](security-model.md) and the
supply-chain gaps listed in [docs/enterprise-readiness.md](enterprise-readiness.md)
as those applied *before* this phase — see each document's own updated
notes for the cross-reference.

Phase 10 is CI/release-process hardening only. It adds no runtime
behavior, no schema change, and touches no file under `internal/` or
`cmd/` except `internal/buildinfo` (release version metadata) and a
`-version` flag on `cmd/api`/`cmd/worker`. Everything else in this
document lives under `.github/`.

## 1. Dependency vulnerability scanning (govulncheck)

TaskForge uses Go's official vulnerability scanner,
[`govulncheck`](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck),
invoked identically in three places so they can never drift from each
other:

- **Locally**: `make vulncheck`
- **On every push/PR**: the `vulncheck` job in `.github/workflows/ci.yml`
- **Weekly, independent of PR activity**: `.github/workflows/scheduled-security.yml`

All three run `go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...`,
with `GOVULNCHECK_VERSION` pinned once in the `Makefile`. `govulncheck`
exits with status 3 when it finds a vulnerability reachable from
TaskForge's actual call graph (not merely present in `go.sum`); that
nonzero exit fails the `make` target and, in CI, fails the job — the
result is never piped through `|| true` or otherwise swallowed.

**Why the scheduled run exists, not just the PR-triggered one**: a CVE can
be disclosed in a dependency (including the Go standard library itself —
`govulncheck` scans stdlib symbols reachable from TaskForge's code, tied to
the toolchain version used to build it) *after* the last merged PR, with no
new PR opened for weeks. Dependabot's PR cadence is explicitly not treated
as a detection mechanism here (see §3) — the weekly scheduled scan is what
actually guarantees detection during a quiet period.

**Real evidence this gate works, not merely configured**: during Phase 10's
implementation, running `make vulncheck` against the repository's
then-current Go toolchain (1.26.5) surfaced six real, currently-disclosed
standard-library vulnerabilities (e.g. `GO-2026-5026`, `GO-2026-5972`),
fixed in `go1.26.6`. This is precisely the "CVE disclosed after the last
commit" scenario this control exists to catch, caught live rather than
staged. The fix — pinning `toolchain go1.26.8` in `go.mod` — is documented
inline in `go.mod` next to the `toolchain` directive; re-pin it whenever
`govulncheck` reports a new stdlib finding.

**What a passing `govulncheck` run does and does not prove**: a passing run
means no reachable known Go vulnerability was reported for the scanned
packages, at the scanned build/toolchain configuration, against the
vulnerability database (`vulndb`) as it stood at scan time. It is not a
claim that TaskForge's dependencies or toolchain are "free of CVEs" in any
absolute or exhaustive sense — a vulnerability not yet disclosed or not yet
in `vulndb`, one in a code path `govulncheck`'s reachability analysis does
not statically resolve (e.g. via reflection or a build configuration not
scanned), or one in a dependency scope `govulncheck` doesn't analyze, would
not be caught by this control alone. This is exactly why the scheduled run
(re-scanning the same, unchanged code against a *later* `vulndb` snapshot)
exists, and why this is one layer among several (§2, §4), not the only one.

## 2. Dependency-review (PR-time gate)

`.github/workflows/dependency-review.yml` runs GitHub's official
[`dependency-review-action`](https://github.com/actions/dependency-review-action)
on every pull request targeting `main`. It diffs the dependency graph
between the PR's base and head and fails the PR if a newly-introduced or
newly-changed dependency carries a known vulnerability, using explicit,
visible-in-the-workflow settings rather than silently-relied-upon defaults:
`fail-on-severity: low` and `fail-on-scopes: runtime`.

**This is a vulnerability gate only.** TaskForge has not established an
approved/disallowed open-source license policy, so this workflow sets
`license-check: false` explicitly and does **not** set `allow-licenses` or
`deny-licenses` — it does not block a PR on license grounds. GitHub's
dependency-review UI may still surface license
metadata for a PR's dependency diff (that is the action's own default
behavior when license data is available) — that is informational display
only and has no bearing on whether this check passes or fails. Do not
describe this control as "enforcing a license policy" or "blocking
disallowed licenses"; neither is true today.

This is a **complement** to `govulncheck`, not a replacement: dependency-review
looks at the dependency *graph diff* a PR introduces (catching a newly
added vulnerable package before merge); `govulncheck` looks at whether
TaskForge's code actually *reaches* a vulnerable symbol (catching a
disclosure in an already-merged dependency, including one no PR ever
touches). Both are needed; neither substitutes for the other.

**Platform limitation**: GitHub's dependency-review check can only be
enforced as a blocking, "required status check" via branch protection
rules, which are a repository *settings* change (Settings → Branches →
branch protection), not something expressible in a workflow file. This
implementation environment made the workflow itself correct and present;
it did not (and could not, without direct interactive access to the
repository's Settings UI/API with admin rights exercised outside this
session) toggle branch protection to mark it "required." This is recorded
honestly here rather than claimed as done — see §8 Limitations.

## 3. Dependabot

`.github/dependabot.yml` configures automated update PRs for:

- **`gomod`** (Go modules — `go.mod`/`go.sum`), weekly, grouped into one PR
  per week (`go-dependencies` group) rather than one PR per dependency, to
  keep review volume reasonable.
- **`github-actions`** (every `uses:` reference across `.github/workflows/`),
  weekly, similarly grouped. Dependabot understands SHA-pinned `uses:`
  references and opens a PR that updates both the SHA and its adjacent
  version comment together, keeping the pinning strategy in §5 intact
  across updates rather than requiring a manual re-pin every time.

**Why this is not the detection mechanism**: per docs/enterprise-roadmap.md
Phase 10's own stated failure scenario, "Dependabot happened to open a PR"
is explicitly rejected as a proof-of-detection criterion, because
Dependabot's PR cadence is not deterministic (it depends on upstream
release timing, GitHub's own scan latency, and this repository's update
schedule). The deterministic proof obligation is that the *configuration*
is present, valid, and scoped to both ecosystems TaskForge actually has —
verified by `.github/dependabot.yml` existing and validating (see §7) —
not that a PR was observed to open during implementation.

## 4. CodeQL (static analysis)

`.github/workflows/codeql.yml` runs GitHub CodeQL's Go analysis on every
push/PR to `main` and weekly. It closes the caveat
[docs/security-model.md](security-model.md) §3 names for the
parameterized-query finding: that TaskForge's freedom from SQL injection
had been verified by manual code inspection during the enterprise review,
not by an automated SAST gate — CodeQL is that gate, going forward.

CodeQL for a compiled language needs a build, not just a checkout — the
workflow runs `codeql-action/init` (language: `go`), `codeql-action/autobuild`,
then `codeql-action/analyze`. No Postgres service container is used in this
workflow: CodeQL performs static analysis over the compiled build, it does
not execute tests, so this deliberately does not duplicate `ci.yml`'s test
job (see docs/enterprise-roadmap.md's "no duplicate unnecessary compilation
pipelines" instruction).

## 5. Least-privilege permissions and action pinning

Every workflow file under `.github/workflows/` declares an explicit,
workflow-level `permissions:` block. `contents: read` is the default and
suffices for four of the five workflows in full. The two exceptions, both
scoped to a single job with an inline comment explaining the grant:

| Workflow / job | Extra permission | Why |
|---|---|---|
| `codeql.yml` / `analyze` | `security-events: write`, `actions: read` | Required by `codeql-action/analyze` to upload SARIF results to the repository's code scanning API (GitHub's own documented requirement for this action). |
| `release.yml` / `build` | `contents: write`, `id-token: write`, `attestations: write`, `artifact-metadata: write` | `contents: write` to create the GitHub Release and upload assets via `gh release create`; the other three are exactly what `actions/attest`'s own README documents as required to mint a Sigstore-signed attestation and persist it via GitHub's attestations API. |

No workflow uses `permissions: write-all`, and no job relies on an
implicit repository-wide default — every job's effective permission set is
traceable to a `permissions:` block in the same file.

**Checkout credentials**: every `actions/checkout` step across every
workflow file explicitly sets `persist-credentials: false`, overriding the
action's own default of `true`. None of TaskForge's workflows perform an
authenticated `git push` (or any other authenticated `git` operation) after
checking out the source — `release.yml`'s `build` job, the one job that
does write to the repository (a GitHub Release, via `gh release create`),
does so with an explicit `GH_TOKEN: ${{ github.token }}` passed to the `gh`
CLI, not via checkout-persisted git credentials. Leaving credential
persistence enabled would have left a working git credential in the
runner's local git config for the remainder of every job, with no job
actually needing it — an unnecessary, unused elevation removed by this
setting, present as a proof obligation reproducible with:

```sh
grep -rn "uses: actions/checkout" -A2 .github/workflows/*.yml
```

Every result must show a `with: persist-credentials: false` immediately
following the `checkout` step.

**Action pinning**: every third-party (and first-party GitHub) Action
reference across all five workflow files is pinned to a full, 40-character,
immutable commit SHA, with a `# vX.Y.Z`-style comment identifying the
upstream release the SHA corresponds to — never a mutable tag like `@v4`.
Each SHA below was resolved directly from the upstream repository's tag
references via the GitHub API at implementation time (not invented, not
copied from memory):

| Action | Pinned at | SHA |
|---|---|---|
| `actions/checkout` | v7.0.1 | `3d3c42e5aac5ba805825da76410c181273ba90b1` |
| `actions/setup-go` | v7.0.0 | `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e` |
| `actions/dependency-review-action` | v5.0.0 | `a1d282b36b6f3519aa1f3fc636f609c47dddb294` |
| `github/codeql-action/{init,autobuild,analyze}` | v4.37.9 | `cdf488f595d80d6e07e03d4674febd5ab45fa938` |
| `actions/attest` | v4.2.2 | `1e69f48acb82d1966a394da916b4c1698aa569d6` |

`actions/checkout@v4` and `actions/setup-go@v5` (the versions this
repository ran before Phase 10) were both already emitting a "Node.js 20 is
deprecated" warning at implementation time — both, and every other action
in this table, were re-resolved to their current, actively maintained
major version (v7 and v7 respectively) rather than re-pinned at the old,
soon-unsupported version.

A repository-wide audit confirming every `uses:` line matches this pattern
is reproducible with:

```sh
grep -rn "uses:" .github/workflows/*.yml
```

Every result must show a `@<40-hex-char-sha> # vX.Y.Z` reference; none may
show a bare tag (`@v4`), a branch, or `@main`.

## 6. SBOM (Software Bill of Materials)

TaskForge generates one [CycloneDX](https://cyclonedx.org/) 1.6 JSON SBOM
**per released binary** — eight in total (`{api,worker}` × `{linux,darwin}`
× `{amd64,arm64}`) — using
[`cyclonedx-gomod`](https://github.com/CycloneDX/cyclonedx-gomod)'s `app`
subcommand, the same Go-native tool and command for local use, CI, and
release:

```sh
make sbom
# ->
# dist/taskforge-api-linux-amd64.sbom.cdx.json
# dist/taskforge-api-linux-arm64.sbom.cdx.json
# dist/taskforge-api-darwin-amd64.sbom.cdx.json
# dist/taskforge-api-darwin-arm64.sbom.cdx.json
# dist/taskforge-worker-linux-amd64.sbom.cdx.json
# dist/taskforge-worker-linux-arm64.sbom.cdx.json
# dist/taskforge-worker-darwin-amd64.sbom.cdx.json
# dist/taskforge-worker-darwin-arm64.sbom.cdx.json
```

**Why `app` and not `mod`**: `cyclonedx-gomod mod` produces one aggregate
SBOM over every module required by *any* package anywhere in the module —
it does not evaluate Go build constraints, so it cannot distinguish what a
specific `GOOS`/`GOARCH`/`CGO_ENABLED` build of a specific main package
actually pulls in. `cyclonedx-gomod app` evaluates build constraints the
same way `go build` does, for one main package and one target platform at
a time — the tool's own documentation states this precisely: "because
build constraints influence Go's module selection, an SBOM should be
generated for each target in the build matrix," and "application
distributors typically use `app`." TaskForge distributes eight compiled
application binaries, so `make sbom` invokes `cyclonedx-gomod app` once per
`(binary, platform)` pair, each time with:

- `-main cmd/api` or `-main cmd/worker` (the application's actual main
  package, per the `MODULE_PATH`-relative `-main` flag `app` requires to
  target one binary within a module that has more than one `main`
  package), and
- `GOOS`/`GOARCH`/`CGO_ENABLED=0` environment variables matching exactly
  what `make release-build` uses to compile that same binary (both read
  from the same `RELEASE_PLATFORMS`/`RELEASE_BINS` lists in the
  `Makefile`, so the two can never drift against each other).

Each generated SBOM's `metadata.component` records the exact build
constraints it was generated under (as `cdx:gomod:build:env:GOOS`,
`GOARCH`, `CGO_ENABLED` properties and in its `purl`/`bom-ref`, which also
names the `-main` package via a `#cmd/api`-style fragment) — independently
inspectable evidence that a given SBOM file matches its filename's platform
and binary, not just an implementation claim.

**Why not hand-maintained**: every SBOM is generated directly from
`go.mod`/`go.sum` at release time (`.github/workflows/release.yml`'s
`build` job) — never committed to the repository as a static file, and
never edited by hand. A stale, hand-maintained SBOM would silently drift
from the actual dependency graph the moment a `go.sum` change landed
without a corresponding manual SBOM edit; generating it fresh at each
release closes that drift risk entirely rather than trusting discipline to
prevent it.

## 7. Artifact attestation / build provenance

Release binaries are attested using GitHub's official, unified
[`actions/attest`](https://github.com/actions/attest) action (the
successor to the now-deprecated `actions/attest-build-provenance` and
`actions/attest-sbom` wrapper actions — both now documented as thin
wrappers over `actions/attest` as of their v4 releases, so this
implementation uses the underlying action directly rather than a
soon-redundant wrapper). Nine attestations are created per release — one
build-provenance attestation covering all eight binaries, plus eight
per-binary SBOM attestations — all Sigstore-signed and uploaded to GitHub's
attestations API:

1. **Build provenance** (one `actions/attest` step, `subject-path` only): a
   SLSA build provenance predicate, auto-generated by the action, binding
   each released binary's digest to the workflow run, commit, and
   repository that built it. `subject-path` is an explicit,
   newline-delimited list of exactly the eight binary filenames — not a
   glob — so it can never accidentally widen to also match a SBOM file,
   `SHA256SUMS`, or `release-metadata.json`; a build-provenance attestation
   is only meaningful for the eight compiled binaries, not for the files
   describing them.
2. **SBOM attestation** (eight separate `actions/attest` steps, each with
   both `subject-path` and `sbom-path`): binds each released binary's own
   `.sbom.cdx.json` to that same binary's digest as a signed predicate.
   Each step names one exact binary/SBOM pair — never one aggregate SBOM
   attested against every binary:

   | Binary | SBOM |
   |---|---|
   | `taskforge-api-linux-amd64` | `taskforge-api-linux-amd64.sbom.cdx.json` |
   | `taskforge-api-linux-arm64` | `taskforge-api-linux-arm64.sbom.cdx.json` |
   | `taskforge-api-darwin-amd64` | `taskforge-api-darwin-amd64.sbom.cdx.json` |
   | `taskforge-api-darwin-arm64` | `taskforge-api-darwin-arm64.sbom.cdx.json` |
   | `taskforge-worker-linux-amd64` | `taskforge-worker-linux-amd64.sbom.cdx.json` |
   | `taskforge-worker-linux-arm64` | `taskforge-worker-linux-arm64.sbom.cdx.json` |
   | `taskforge-worker-darwin-amd64` | `taskforge-worker-darwin-amd64.sbom.cdx.json` |
   | `taskforge-worker-darwin-arm64` | `taskforge-worker-darwin-arm64.sbom.cdx.json` |

   These eight steps are written out explicitly in `release.yml` rather
   than generated by a matrix/loop, so each pairing stays directly
   readable and reviewable in the workflow file itself.

**What this proves**: that a binary bearing a given SHA-256 digest was
built by *this* repository's `release.yml` workflow, from a specific
commit, in a specific (recorded) workflow run — and, separately, that a
specific, matching SBOM's content is bound to that same digest. Both are
independently verifiable by anyone (see §9), without trusting TaskForge's
release process itself, only Sigstore's public-good transparency log and
GitHub's attestations API.

**What this does not prove** (stated explicitly, per
docs/enterprise-roadmap.md's instruction not to overclaim SLSA level):

- It does not prove the *source code* is free of vulnerabilities or
  backdoors — only that the binary traces back to a specific, named
  commit. Auditing that commit is a separate activity (CodeQL in §4 helps,
  but is not exhaustive).
- It does not prove hermetic, isolated, or reproducible builds — GitHub-hosted
  runners are not a hermetic build environment in the SLSA sense, and
  TaskForge makes no bit-for-bit reproducibility claim (see §9).
- It does not, by itself, prevent a compromised repository maintainer
  (or a compromised `GITHUB_TOKEN`/runner) from producing a validly-attested
  but malicious binary — attestation proves *provenance* (where a binary
  came from), not *trustworthiness* of that source.

**Availability**: GitHub Artifact Attestations are available on all
current plans for public repositories (this repository's case); private/internal
repositories require GitHub Enterprise Cloud. This is a GitHub platform
fact stated for completeness, not a limitation this implementation hit.

## 8. Secret scanning / push protection

GitHub secret scanning and push protection are repository/organization
**settings** (Settings → Code security), not workflow files — there is no
`.github/workflows/*.yml` mechanism to "enable" them, and no API call
available from this implementation environment that can toggle them
without direct, interactive administrative access to the repository's
Settings UI/API beyond what this session was authorized to exercise
autonomously.

**What this implementation did**: verified honestly that this control's
correct implementation surface is a settings toggle, and documents the
checklist an operator with admin access must complete:

- [ ] Settings → Code security → **Secret scanning**: Enable.
- [ ] Settings → Code security → **Push protection**: Enable (blocks a
      push that contains a recognizable secret pattern, rather than only
      flagging it after the fact).
- [ ] Confirm via Settings → Code security that both show as "Enabled" for
      this repository (public repositories get secret scanning free; push
      protection is also free for public repositories as of GitHub's
      current plan structure).

**What this implementation did not do**: fabricate a secret to test
scanning against, or claim the toggle is enabled without verifying it.
This is recorded as a platform-level limitation in §9, not silently
skipped.

**Existing synthetic test strings**: TaskForge's test suite (per
[docs/observability.md](observability.md)'s logging-redaction discipline)
uses deliberately fake credential-shaped strings to prove that real secret
values (e.g. `Idempotency-Key`) are never logged. These remain clearly
synthetic (e.g. obviously placeholder values, not real API keys or
tokens) — Phase 10 added no new test fixtures of this kind and did not
alter existing ones.

## 9. Release process

Triggered by a tag push matching the glob `v*.*.*` —
`.github/workflows/release.yml` has no `pull_request`/`pull_request_target`
trigger, so it can never run against untrusted fork PR code; pushing a tag
already requires write access to the repository. **The `v*.*.*` glob is
only a routing trigger, not SemVer validation** — it also matches invalid
tags such as `vfoo.bar.baz`, `v1.2`, `v01.2.3`, or `v1.2.3junk`. The
`verify` job's "Validate release tag is strict SemVer" step (below) is what
actually performs strict `vMAJOR.MINOR.PATCH` validation (nonnegative
numeric components, no leading zeroes except a lone `0`, no prerelease
support in this phase) and fails closed before any build/release work runs.

**Release trust boundary**: pushing a tag requires repository write access,
but write access alone does not require a merged PR — a collaborator could
otherwise tag an arbitrary, unmerged branch commit (one whose local tests
happen to pass) and obtain an official release. The `verify` job's "Verify
tagged commit is reachable from main" step closes this: it checks out full
history (`fetch-depth: 0`) and runs
`git merge-base --is-ancestor "$GITHUB_SHA" origin/main`, failing closed if
the tagged commit is not an ancestor of `origin/main`. This does **not**
require the tag to point at main's current tip — an older, genuinely-merged
commit may still be released — only that the commit's history traces back
into `main`.

```
tag push (vX.Y.Z, glob-matched)
   │
   ▼
verify job   — validate tag is strict SemVer (fails closed on a
   │            glob-matched but invalid tag, e.g. v1.2.3junk)
   │          — verify GITHUB_SHA is reachable from origin/main
   │            (fails closed on an unmerged/arbitrary branch commit)
   │          — gofmt, go vet, go build, go test, go test -race,
   │            go mod verify, govulncheck
   │            (repeats ci.yml's applicable Go/test/vulnerability gates,
   │            re-run against the tagged commit; dependency-review.yml is
   │            PR-diff-only and has no meaning against one commit, so it
   │            is not "rerun" here)
   ▼
build job    — rm -rf dist && mkdir -p dist   (clean output directory first)
   │          — set RELEASE_DATE once (GITHUB_ENV), shared by release-build
   │            and release-metadata below so both embed the same date
   │          — make release-build       (8 binaries: {api,worker} × 4 platforms,
   │            version/commit/date embedded via internal/buildinfo)
   │          — make sbom                (8 artifact-specific SBOMs, one per binary)
   │          — make release-metadata    (dist/release-metadata.json)
   │          — make checksums           (dist/SHA256SUMS, over the clean dist/)
   │          — actions/attest × 9       (1 build-provenance attestation covering
   │            all 8 binaries, then 8 explicit per-binary SBOM attestations)
   │          — gh release create        (publishes binaries + SBOMs +
   │            release-metadata.json + checksums)
   ▼
GitHub Release, tagged vX.Y.Z
```

**Version metadata mechanism** (docs/enterprise-roadmap.md: "do not spread
version constants across the codebase" / "prefer build metadata injected
at build time"): `internal/buildinfo` holds exactly three package-level
`var`s (`Version`, `Commit`, `Date`), defaulting to `"dev"`/`"none"`/`"unknown"`
for an ordinary local `go build`/`go run`. The release build (`make
release-build`, also what `release.yml` runs) injects real values via
`-ldflags -X`. Both `cmd/api -version` and `cmd/worker -version` print
`buildinfo.String()` and exit; both processes also log `version` and
`commit` fields on their normal startup log line. This is the single place
version metadata lives — no second copy exists anywhere else in the tree.

**Target platforms**: `linux/amd64`, `linux/arm64`, `darwin/amd64`,
`darwin/arm64` — the platform set a Go server/CLI project like TaskForge
is realistically deployed or developed on; no Windows target, no container
image (explicit non-scope per docs/enterprise-roadmap.md Phase 10), no
package-manager (Homebrew, apt, etc.) distribution.

**Release metadata manifest**: `dist/release-metadata.json` (`make
release-metadata`) is a small, deterministic, machine-readable summary of
the release as a whole — `version`, `commit`, `date`, the Go `go_version`
the binaries were actually compiled with, the `targets` (OS/arch) list, and
the `binaries` (`api`, `worker`) list. It supplements, and does not
replace, the version/commit/date already embedded per-binary via
`internal/buildinfo` and readable via `-version` — it exists for a reader
who wants one file describing the whole release rather than querying each
binary individually. It is generated at release time, included in
`SHA256SUMS`, and published as a release asset like every other file in
`dist/`. **It is not cryptographic proof of anything by itself** — like
step 4 below, it is a convenience/self-report, not a substitute for the
checksum and attestation checks that actually verify the release.

### Release verification procedure

Given a release `vX.Y.Z` and a downloaded binary (say
`taskforge-api-linux-amd64`), first download that release's
`SHA256SUMS`, `taskforge-api-linux-amd64.sbom.cdx.json` (its **matching**
SBOM — every binary's SBOM is named `<binary-filename>.sbom.cdx.json`,
never a shared/aggregate file), and optionally `release-metadata.json`.

1. **Checksum**:
   ```sh
   sha256sum -c SHA256SUMS --ignore-missing
   ```
   A mismatch means the binary (or its SBOM, or `release-metadata.json` —
   `SHA256SUMS` covers every file in the release) was corrupted or
   tampered with in transit/storage — do not run it.

2. **Build provenance attestation**:
   ```sh
   gh attestation verify taskforge-api-linux-amd64 \
     --repo SamudralaAjaykumarrr/taskforge \
     --signer-workflow SamudralaAjaykumarrr/taskforge/.github/workflows/release.yml
   ```
   Confirms the binary's digest matches a signed attestation produced by
   *this exact repository's* `release.yml` workflow, from a specific commit
   — `--repo` constrains verification to `SamudralaAjaykumarrr/taskforge`
   specifically (narrower than `--owner`, which would accept an attestation
   from *any* repository under that owner/account), and
   `--signer-workflow` additionally constrains the expected signer to this
   repository's `release.yml` workflow file specifically, rejecting an
   attestation minted by a different workflow in the same repository. `gh
   attestation verify` fails closed against a tampered binary (the digest
   simply won't match any attestation), a binary attested by a different
   repository, or one attested by a different workflow.

3. **SBOM attestation / SBOM content**: verify the *matching* SBOM file is
   the one actually bound to this binary's digest —
   ```sh
   gh attestation verify taskforge-api-linux-amd64 \
     --repo SamudralaAjaykumarrr/taskforge \
     --signer-workflow SamudralaAjaykumarrr/taskforge/.github/workflows/release.yml \
     --predicate-type https://cyclonedx.org/bom
   ```
   then inspect `taskforge-api-linux-amd64.sbom.cdx.json` (downloaded
   alongside the binary, above) directly for the dependency list and
   versions that specific binary actually shipped with. Do not substitute
   a different binary's SBOM file — each is build-constrained to its own
   `GOOS`/`GOARCH`/main package (§6) and is only accurate for the binary
   it is named after.

4. **Version/commit self-report** (defense in depth — this step alone is
   not cryptographic proof, unlike 2–3 above):
   ```sh
   ./taskforge-api-linux-amd64 -version
   ```
   Confirm the printed commit matches the release's tagged commit on
   GitHub (and, optionally, `release-metadata.json`'s `commit` field for
   the release as a whole).

Steps 2–3 are the only ones that cryptographically prove the artifact
came from this repository's CI, unmodified; step 1 proves the file wasn't
corrupted/tampered with *relative to the checksum manifest itself* (an
attacker able to replace both the binary and `SHA256SUMS` together
defeats step 1 alone, which is exactly why steps 2–3 exist and matter
more).

### "Traceable/repeatable," not "reproducible"

TaskForge's release builds are **not** claimed to be bit-for-bit
reproducible (no independent third party has verified that rebuilding
from the same commit/toolchain produces byte-identical binaries — Go
builds are reproducible in principle given an identical toolchain,
`GOFLAGS`, and `CGO_ENABLED=0`, which `make release-build` does hold
constant, but this has not actually been tested/proven here). What *is*
true, and is the accurate claim this document makes: the release
procedure is **traceable** (every artifact's provenance is attested back
to a specific commit and workflow run) and **repeatable** (`make
release-build VERSION=... COMMIT=... DATE=...` deterministically
reproduces the same build steps locally, for debugging or independent
verification of the *process*, even without a claim about byte-for-byte
binary identity).

## Local commands reference

| Command | What it does |
|---|---|
| `make vulncheck` | Run govulncheck against the module (same command CI/scheduled-security.yml use). |
| `make release-build` | Remove and recreate `dist/`, then cross-compile all 8 release binaries into it, with version metadata (`VERSION`/`COMMIT`/`DATE` overridable; defaults derived from `git describe`/`git rev-parse`/current UTC time). Always run first — it cleans `dist/`. |
| `make sbom` | Generate the 8 artifact-specific SBOMs into `dist/` (one per binary; see §6). |
| `make release-metadata` | Generate `dist/release-metadata.json` (see §9). |
| `make checksums` | Generate `dist/SHA256SUMS` over everything currently in `dist/` — run last, after every other `dist/`-writing target. |

Running all four in sequence (`release-build`, `sbom`, `release-metadata`,
`checksums`) exercises the entire release build locally (binaries, SBOMs,
release metadata, checksums) without publishing anything — `gh release
create` and the nine `actions/attest` steps only run inside
`.github/workflows/release.yml`, gated on an actual tag push.

## Limitations requiring GitHub/account-level action

Recorded honestly rather than claimed as done, per
docs/enterprise-roadmap.md Phase 10's own instruction:

1. **Dependency-review as a "required" branch-protection check** (§2): the
   workflow exists and is correct; marking it *required* (blocking merge
   until it passes) is a branch-protection *setting*, not a workflow
   change, and was not toggled from this implementation environment.
2. **Secret scanning / push protection enabled state** (§8): both are
   repository *settings*; this document provides the exact checklist, not
   a claim that the toggles were flipped.
3. **`gh attestation verify` / a real tagged release**: this phase could
   not push a git tag or trigger `release.yml` end-to-end from this
   implementation environment (per explicit instruction: no commit, push,
   or PR in this phase) — the release workflow, `make release-build`,
   `make sbom`, and `make checksums` were all exercised and verified
   *locally* (binaries build, run, print correct `-version` output; SBOM
   and checksum files generate correctly; see this phase's final report
   for the exact commands run), but no live GitHub attestation has
   actually been minted or verified against a real release yet. The first
   real tag push after this branch merges is what produces that evidence.
4. **CodeQL/dependency-review "required check" enforcement generally**: both
   depend on branch protection rules (a repository setting) to actually
   *block* a non-passing PR from merging, versus merely reporting a
   failing check. This document does not claim branch protection is
   configured — only that the workflows producing the checks are correct
   and present.
