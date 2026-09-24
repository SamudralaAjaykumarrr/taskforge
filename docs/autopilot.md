# TaskForge Autopilot

Status: **V1, repository/developer tooling.** Autopilot is not part of the
TaskForge job-processing runtime (`cmd/api`, `cmd/worker`, `internal/store`,
...). It does not appear in [enterprise-roadmap.md](enterprise-roadmap.md)
as a numbered phase — it is tooling that carries a future roadmap phase
*through* this repository's existing, real planning → implementation →
review → CI → merge workflow, not a phase of product work itself.

## Purpose

TaskForge's own history (Phases 12 through 16, visible in `git log`) has
already established a repeatable, disciplined pattern for landing a
roadmap phase:

1. A **planning** pass produces `docs/phase-N-plan.md` on a
   `phase-N-planning` branch, reviewed once, merged via PR.
2. An **implementation** pass does the work on a `phase-N-<slug>` branch,
   reviewed once, pushed, watched through CI, merged via PR.
3. **Post-merge**, `main` is fast-forwarded and the *real, gh-observed*
   CI/CodeQL runs for the actual merged commit are confirmed green before
   the phase is considered done — not the PR's own pre-merge checks.

Carrying a phase through that pattern by hand is a lot of repetitive
copy/paste: generating prompts, invoking Claude, running the same quality
gates, writing PR bodies, watching checks, merging. Autopilot automates the
mechanical parts of that pattern while leaving every decision this
project's own engineering discipline reserves for a human exactly where it
already was.

Autopilot exists to carry Phase 16 → Phase 17 → Phase 18 → the final
Enterprise v1.0.0 gates through this same pattern with less manual
overhead — not to change the pattern, and not to remove the human from it.

## Safety model

Autopilot follows a **bounded state machine** (`internal/autopilot/stage.go`).
It is built to never:

- bypass protected `main`, or push/commit to it directly
- use `gh --admin`, or any other branch-protection bypass
- force-push (`git push --force`/`-f`/`--force-with-lease`) — refused at
  the argument-inspection level in `Git.run`, not merely "no method exists
  for it," as defense in depth
- run `git reset --hard` or `git clean` — refused the same way
- merge with a failing required check (`doMerge` re-verifies check state
  immediately before merging, even if the merge gate was approved a while
  ago)
- weaken, remove, or skip a test/proof obligation to obtain green CI — this
  is stated as a hard instruction in every generated prompt, and a review
  finding of this kind (`BLOCKER_TYPE=proof_weakening`) always pauses for a
  human rather than being auto-"fixed"
- invent a commit, PR, test result, or CI result — every one of those
  comes from a real `git`/`gh` call or a real local gate run; there is no
  code path that fabricates one
- assume a failing test is unrelated without evidence — CI repair only
  proceeds once real, captured failed-check logs exist (`doCIRepair`
  refuses to guess when it cannot collect any)
- perform a destructive/non-additive migration automatically, or make an
  architectural/security/invariant decision automatically — see "Human
  Gates" below
- continue indefinitely through repeated unexplained failures — every
  bounded counter (review, recheck, CI repair) is enforced in
  `internal/autopilot/state.go` and pauses rather than looping past its
  bound
- mark a phase complete because its PR merged — merging and "the phase is
  actually done" are different facts; see "Post-Merge Verification" below
- silently skip a phase-specific proof obligation an approved plan actually
  requires — see "Phase Proof Manifest" below

Autopilot is **not a daemon**. Every command is one bounded invocation that
performs some work and exits; `taskforge-autopilot resume` is how a stopped
or interrupted run continues.

## Prerequisites

Checked explicitly by `taskforge-autopilot doctor`, never assumed:

- `git` and `gh` on `PATH`, `gh` authenticated (`gh auth status`)
- a `claude` CLI on `PATH` that actually supports the non-interactive
  invocation contract `Claude.Invoke` (`internal/autopilot/claudeops.go`)
  depends on — not merely a binary that happens to be named `claude`. This
  is verified by `Claude.CheckCapabilities`: a bounded `claude --version`
  (to report the installed version) followed by a bounded `claude --help`
  (never a real model prompt), checked for every flag `Invoke` passes
  (`--print`, `--output-format`, `--permission-mode`,
  `--no-session-persistence`, kept in sync with `Invoke` via the shared
  `requiredClaudeFlags` list). An installed-but-incompatible CLI is a hard
  **FAIL**, not a WARN — the alternative is every subsequent
  draft/review/fix invocation failing mid-run instead of being caught up
  front.
- a resolvable repository root, an `origin` remote, and a resolvable
  default branch
- the repository's `.github/workflows/*.yml` files are discoverable, and
  `docs/enterprise-roadmap.md` exists

`doctor` performs **zero mutation** — every check is a read-only
inspection (a `LookPath`, a `git rev-parse`/`status`, a `gh repo view`, a
`claude --version`/`--help` capability probe, a file read).

## Commands

```
taskforge-autopilot status
taskforge-autopilot start --phase N --stage planning|implementation [--force]
taskforge-autopilot resume
taskforge-autopilot approve
taskforge-autopilot pause [--reason TEXT]
taskforge-autopilot doctor
taskforge-autopilot dry-run --phase N --stage planning|implementation
```

Exit codes: `0` success/healthy, `1` error, `2` usage error, `3` doctor
found a hard failure, `4` a run is awaiting human approval (useful for
scripting: "did this need me?").

## State machine

Both `planning` and `implementation` stages walk the **same** step graph
(`internal/autopilot/stage.go`'s `nextSteps`) — what "Draft" and "Review"
mean differs (a plan doc vs. an implementation), but the shape mirrors this
repository's own real history exactly:

```
branch -> draft -> review -+-> local_gates -> commit -> push -> pr_create
                            |                                       |
                            +-> fix_blocker -> recheck --------------+
                                                                      v
                                                                  ci_watch
                                                                   |    |
                                                       (pass)      |    | (fail)
                                                          v        |    v
                                                  merge_approval <-+  ci_repair
                                                          |             |
                                                          v             |
                                                        merge <---------+
                                                          |        (loops back
                                                          v         to local_gates,
                                                     post_merge      bounded)
                                                          |
                                                          v
                                                      complete
```

`ValidateTransition` is the single choke point every state change goes
through — there is no code path in `internal/autopilot` that mutates
`State.Step` without it. Notably: `review` can only be reached once per
run (there is no edge back into it), and `pr_create` is idempotent (a
CI-repair round trip pushes new commits to the *same* PR rather than
opening a second one). `merge -> post_merge -> complete` is likewise never
collapsed into one step: reaching `merge` only proves the PR merged, not
that the phase is done — see "Post-Merge Verification" below for what
`post_merge` actually re-verifies before `complete` is reachable.

## Human gates

Autopilot pauses (`Status=awaiting_approval`, `PausedReason` set) instead
of proceeding automatically for exactly these nine cases:

| # | `PauseReason` | Trigger |
|---|---|---|
| 1 | `architectural_decision` | Review/implementer classified a blocker `BLOCKER_TYPE=architecture` |
| 2 | `destructive_migration` | `BLOCKER_TYPE=migration` |
| 3 | `security_trust_boundary_change` | `BLOCKER_TYPE=security` |
| 4 | `invariant_change` | `BLOCKER_TYPE=invariant` |
| 5 | `test_or_proof_obligation_weakening` | `BLOCKER_TYPE=proof_weakening` |
| 6 | `force_push_or_main_protection_bypass` | Never reachable in code (no method/path exists) — listed for completeness; would only ever be a manual, out-of-band operator action |
| 7 | `ci_repair_attempts_exhausted` | `CIRepairCount` would exceed `Config.MaxCIRepairAttempts` (default 3) |
| 8 | `release_candidate_or_v1_approval` | Reserved for Phase 18 (External Human Validation & Release Candidate) — not reachable by Phases ≤17's ordinary workflow |
| 9 | `external_human_validation_required` | A required **human** proof obligation from this phase's proof manifest (see "Phase Proof Manifest" below) is not yet approved |
| — | `merge_confirmation_required` | **Always**, immediately before merge, even with every required check green (V1 default — see below) |

Only `BLOCKER_TYPE=ordinary` (a routine implementation defect: a missing
test, a bug, a scope slip) gets Autopilot's one bounded, focused fix
attempt followed by one focused re-check. Everything else pauses
immediately — Autopilot never guesses at which category a finding belongs
to beyond what the reviewer itself reports, and a reviewer is instructed to
classify conservatively (never default to "ordinary" to avoid escalating).

`Config.AutoMergeAfterGreen` exists as a documented option for a more
trusted future run, but **V1's default is `false`**, and nothing in this
package flips it silently — merge always pauses for an explicit human
`approve` first.

## Resume behavior

`taskforge-autopilot resume` never blindly trusts `state.json`.
`internal/autopilot/reconcile.go`'s `Reconcile` re-checks reality first:

- if the saved local branch no longer exists, that's surfaced as a warning
  (not silently papered over)
- if the saved PR is already `MERGED` on GitHub (e.g. a human merged it by
  hand while Autopilot wasn't running), the state is advanced to
  `post_merge` to match reality
- if the saved PR was `CLOSED` without merging, the run is marked `Failed`
  — resume refuses to continue automatically over state it can no longer
  trust

State survives terminal closure, editor closure, or a Claude session ending,
because none of those processes hold the only copy of it: it's a plain
JSON file at `.taskforge-autopilot/state.json`, written atomically
(temp file + rename) after every single step transition.

## Claude integration

Autopilot never hardcodes a phase's scope into Go source. It reads
`docs/phase-N-plan.md` (once it exists) and scans it for `docs/adr/*.md`
references (`internal/autopilot/phase.go`'s `DiscoverPhase`), then builds a
compact prompt (`internal/autopilot/prompts.go`) that tells Claude to treat
those documents — plus the fixed set (roadmap, testing-strategy,
invariants, compatibility-policy, security-model, observability) — as
authoritative, and to never stage/commit/push/open a PR/merge itself (the
orchestrator controls every one of those steps explicitly).

Two roles, both invoked via `claude -p ... --output-format text
--permission-mode acceptEdits --no-session-persistence`:

- **implementer/planning-drafter** (`RoleImplementer`) — produces the plan
  or the implementation.
- **independent reviewer** (`RoleReviewer`) — always a **fresh session**
  (no `--resume`/`--continue` is ever passed), so it has no access to the
  implementer's conversation context. This is what "independent" means
  here, not just a different prompt in the same session.

If a review finds exactly one `BLOCKER_TYPE=ordinary` blocker, one focused
correction prompt limited to that specific finding
(`GenerateFixPrompt`) is generated — never a request to redo the whole
plan or implementation.

Every implementer/reviewer prompt also lists this phase's checked-in proof
manifest's required obligations, when one loads successfully
(`manifestSummary`, "Phase Proof Manifest" below) — purely informational;
`local_gates`, not prompt generation, is what actually enforces it.

## Result contract

Autopilot does not parse arbitrary prose. Every Claude invocation is
required to end its output with a machine-readable block:

```
AUTOPILOT_RESULT_BEGIN
VERDICT=APPROVE|BLOCKED
READY=true|false
BLOCKER_COUNT=<integer>
BLOCKER_TYPE=ordinary|architecture|migration|security|invariant|proof_weakening
BLOCKERS=<one-line summaries>
AUTOPILOT_RESULT_END
```

(for a reviewer), or

```
AUTOPILOT_RESULT_BEGIN
VALIDATION=PASS|FAIL
READY_FOR_REVIEW=true|false
BLOCKER_COUNT=<integer>
BLOCKERS=<one-line summaries>
AUTOPILOT_RESULT_END
```

(for an implementer/fixer). `internal/autopilot/resultcontract.go` parses
this **and fails closed**: a missing block, an unterminated block, a
malformed `KEY=VALUE` line, or an internally-inconsistent combination
(e.g. `VERDICT=APPROVE` with `BLOCKER_COUNT=2`) is always a hard error —
Autopilot never guesses at intent from prose.

## GitHub integration

- **PR bodies always go through a temporary file** (`gh pr create
  --body-file <file>` / `gh pr edit --body-file <file>`), never an inline
  `--body` string or a heredoc — this repository has hit real
  shell/heredoc PR-body corruption before, and this is the fix.
- Required checks are **discovered**, not assumed forever:
  `WorkflowFileNames`/`WorkflowDisplayName` read `.github/workflows/*.yml`
  directly (a light, dependency-free scrape of each file's top-level
  `name:` field — no YAML library, no new dependency), and `gh pr checks`
  is used for the live, per-PR view. The known core expectations (`CI/test`,
  `CI/vulncheck`, `CodeQL/analyze`, `Dependency Review`) are documented,
  not hardcoded as the only possible names.
- Duplicate push/pull_request check runs of the same name are **not**
  treated as an error — `gh pr checks --json` already consolidates them;
  Autopilot does not reimplement that logic.
- `MergePR` calls `gh pr merge --merge` — the repository's normal merge
  workflow. There is no `--admin` code path anywhere in this package.

## CI repair loop

On a failed check, `doCIRepair`:

1. Refuses to proceed past `Config.MaxCIRepairAttempts` (default 3) —
   pauses with `ci_repair_attempts_exhausted` instead.
2. Collects the **real** failed-check log via `gh run view --log-failed`,
   saved under `.taskforge-autopilot/logs/ci/`. If no evidence can be
   collected at all, it pauses rather than guessing.
3. Generates one evidence-based repair prompt (`GenerateCIRepairPrompt`),
   quoting the actual captured log text and explicitly forbidding weakening
   a test to make CI pass.
4. Reruns local gates, commits, pushes to the **same** branch/PR (never a
   new one), and re-enters `ci_watch`.

## Post-merge verification

Merging a PR proves the PR merged. It does not prove the phase is done —
main could still be red. `doPostMerge` (`internal/autopilot/workflow.go`)
treats these as separate facts:

1. **Resolve the real merge commit.** `GitHub.MergeCommitSHA` calls
   `gh pr view <n> --json state,mergeCommit` and returns the actual
   `mergeCommit.oid` GitHub reports — never the PR's own pre-merge head SHA
   (which, for a squash or rebase merge, is not the commit that lands on
   the base branch), and never invented. Persisted to `State.MergeCommitSHA`
   immediately, before anything else.
2. **Verify main actually contains it.** After `git checkout main` +
   `git pull --ff-only` and a clean-tree check, `Git.IsAncestor` runs
   `git merge-base --is-ancestor <merge-commit> main` — main may have
   advanced further via other merges since; this only requires the merge
   commit to be reachable, not to be `main`'s current tip.
3. **Discover the real post-merge runs.** `GitHub.RunsForCommit` calls
   `gh run list --commit <merge-commit> --json ...` — the *only* sanctioned
   source of post-merge evidence. The PR's own pre-merge `gh pr checks` is a
   different, earlier set of runs against the PR's head commit and is never
   consulted here, no matter how green it was.
4. **Required workflows**, discovered from `.github/workflows/*.yml` the
   same way `doctor` does (`RequiredPostMergeWorkflows`) — at minimum, per
   this repository, CI and CodeQL.
5. `EvaluatePostMergeRuns` matches each required workflow's most recent run
   against the merge commit. **A required workflow whose run hasn't
   appeared yet is "pending," never "success"** — absence is never
   interpreted as passing.
6. **Bounded polling, like `ci_watch`.** Up to `Config.CIWatchMaxPolls`
   checks per invocation (the same knob `ci_watch` uses); still pending
   after that budget leaves the run at `post_merge`, resumable, watching
   the exact same `MergeCommitSHA` on the next `resume` — never re-merging,
   never re-resolving the commit.
7. **A failed required run is `Status=Failed`, with evidence.** The failed
   run's real log is fetched (`gh run view --log-failed`) and saved under
   `.taskforge-autopilot/logs/post-merge/`; the run ID and conclusion are
   recorded in `State.PostMergeRuns`. The phase is never marked complete.
8. Only once every required workflow shows a real, `completed`/`success`
   run against `MergeCommitSHA` does `post_merge` advance to `complete`.

## Failure handling

`RunLocalGates` stops at the first failing gate (never runs a later gate
against a tree already known broken by an earlier one) and captures every
gate's full output under `.taskforge-autopilot/logs/gates/`. A missing or
malformed Claude result contract is a hard `Status=failed`, never a
best-effort continuation. `Workflow.run`'s own loop detects "no progress
was made this invocation" (step and status both unchanged — e.g. `ci_watch`
exhausted its bounded poll budget without a conclusive answer) and stops
cleanly rather than busy-looping; Autopilot is not a daemon, so a later
`resume` is what continues.

## Quality gates

The fixed local-gate sequence (`internal/autopilot/gates.go`) mirrors this
repository's own `Makefile`/CI exactly: `gofmt -l .`, `go vet ./...`,
`go build ./...`, `go test -p 1 ./...`, `go test -race -p 1 ./...`,
`go mod verify`, `git diff --check`. For an **implementation**-stage run,
`local_gates` additionally enforces this phase's checked-in proof manifest
— see below. (A **planning**-stage run does not: a manifest binds to a
*ratified* plan's exact digest, which cannot meaningfully exist while that
plan is still being drafted/reviewed.)

## Phase proof manifest

A roadmap phase's approved plan can require proof beyond the fixed core
gates (e.g. Phase 16's plan requires a `go test -bench` overhead comparison
against a real OTLP collector). `RunLocalGates` always accepted an `extra
[]gateSpec` parameter for exactly this, but nothing populated it — so a
plan's own extra obligations could be silently dropped. `manifest.go`
closes that gap with a **checked-in, machine-readable, digest-bound**
registry: `autopilot/phases/<phase>.json` (e.g.
[autopilot/phases/16.json](../autopilot/phases/16.json)) — deliberately
*not* under `.taskforge-autopilot/` (that directory is gitignored,
run-local state; a manifest is reviewed, committed content).

```json
{
  "phase": 16,
  "plan_path": "docs/phase-16-plan.md",
  "plan_sha256": "<sha-256 of that exact file>",
  "proofs": [
    {
      "name": "tracing-overhead-benchmark",
      "type": "human",
      "required": true,
      "description": "..."
    }
  ]
}
```

Each `proofs` entry is either:

- **`type: "command"`** — automatable. `command` (a real argv) is appended
  to the local-gate sequence (`PhaseManifest.RequiredCommandProofs`) and
  must pass like any other gate.
- **`type: "human"`** — cannot be safely automated (e.g. it needs a real,
  externally-run OTLP collector, or the artifact it proves doesn't exist
  until implementation happens). `local_gates` **pauses**
  (`external_human_validation_required`, `State.PendingHumanProof` set to
  the proof's name) before running *any* gate, and only an explicit
  `taskforge-autopilot approve` — recorded durably in
  `State.ApprovedHumanProofs` — satisfies it. Nothing else does.

`LoadManifest` **fails closed** on every one of: the manifest file missing;
malformed JSON; `phase`/`plan_path` not matching what's expected;
`plan_sha256` no longer matching the plan file's actual current digest (the
plan changed since the manifest was last reconciled); a proof entry missing
its `name`, an empty `command` on a `command` proof, or an empty
`description` on a `human` proof; or a duplicate proof `name`. A
`local_gates` step that hits any of these becomes `Status=Failed` — it
never silently proceeds as if there were nothing to prove. An **empty**
`proofs` list is deliberately treated the same way *unless* the manifest
also sets `"no_extra_proofs_reviewed": true` — nil/absent proofs is never
silently read as "nothing to prove"; it has to be a reviewed, explicit
statement.

Wired into: `start`/`resume` (via `local_gates`, the actual enforcement
point), `dry-run` (shows every registered proof and whether the manifest is
currently valid, executing nothing), `status` (`pending_proof`/
`approved_proofs` fields), and prompt generation (`manifestSummary` tells
the implementer/reviewer up front what `local_gates` will require, purely
informational — an unloadable manifest never blocks prompt generation
itself, only `local_gates`).

## Examples

Check the environment is ready (no mutation):

```
$ taskforge-autopilot doctor
[PASS] git available                            /usr/bin/git
[PASS] gh available                              /usr/bin/gh
[PASS] claude available                          /home/you/.local/bin/claude
[PASS] claude CLI capability                     2.1.0 (Claude Code) (/home/you/.local/bin/claude) supports --print, --output-format, --permission-mode, --no-session-persistence
[PASS] repository root found                      /home/you/projects/taskforge
[PASS] origin remote exists
[PASS] main resolvable                            main
[PASS] working tree status                        clean
[PASS] gh authenticated
[PASS] GitHub repository resolvable                you/taskforge
[PASS] required workflow files discoverable        ci.yml (CI), codeql.yml (CodeQL), ...
[PASS] authoritative roadmap exists                docs/enterprise-roadmap.md
```

An installed-but-incompatible `claude` CLI fails closed:

```
[PASS] claude available                         /usr/local/bin/claude
[FAIL] claude CLI capability                     0.9.0 at /usr/local/bin/claude does not support required flag(s): --no-session-persistence
```

See exactly what a future run would do, with zero mutation:

```
$ taskforge-autopilot dry-run --phase 16 --stage implementation
[DRY-RUN] Phase 16, stage=implementation
[DRY-RUN] branch: phase-16-implementation (base: main)
[DRY-RUN] plan doc: docs/phase-16-plan.md (found)
[DRY-RUN] discovered ADR: docs/adr/0012-distributed-tracing-and-durable-trace-context.md
[DRY-RUN] authoritative doc: docs/enterprise-roadmap.md
...
[DRY-RUN] phase proof manifest: autopilot/phases/16.json (plan_sha256 verified against docs/phase-16-plan.md)
[DRY-RUN]   proof (required, human gate): tracing-overhead-benchmark -- docs/phase-16-plan.md §19/§22: ...
[DRY-RUN] intended workflow (nothing below is executed):
   1. [branch] create branch "phase-16-implementation" from "main" ...
   ...
   6. [local_gates] run gofmt, go vet, ... PLUS this phase's checked-in proof manifest's required command proofs ...
   ...
  12. [merge_approval] PAUSE: require explicit human confirmation before merge ...
  14. [post_merge] resolve the REAL gh-reported merge commit SHA ... discover and bounded-poll the real GitHub Actions runs for CI+CodeQL against that exact commit ...
  ...
[DRY-RUN] zero repository/GitHub/Claude mutations performed.
```

Start a real implementation run, then continue it later:

```
$ taskforge-autopilot start --phase 16 --stage implementation
...
$ taskforge-autopilot resume       # after closing the terminal and coming back
...
$ taskforge-autopilot approve      # at the merge-confirmation gate
```

## Limitations

- **Phase-specific proof obligations require a human to author the
  manifest.** `LoadManifest` enforces whatever `autopilot/phases/<N>.json`
  says, but nothing parses a plan's free-form English proof-obligation
  prose into that JSON automatically — a human reads the ratified plan and
  writes the manifest (as this repository's own
  [autopilot/phases/16.json](../autopilot/phases/16.json) was, from
  [docs/phase-16-plan.md](phase-16-plan.md) §19/§22). Parsing that
  reliably from prose was judged out of scope, and higher-risk, for V1.
- **`ci_watch` and `post_merge` both poll a bounded number of times per
  invocation, then stop**
  (not an unbounded/background wait) — a long-running CI suite may need
  more than one `resume` call before checks conclude. This is deliberate
  (no daemon), not an oversight.
- **`force_push_or_main_protection_bypass` has no reachable code path** —
  by design, there is nothing in this package that could trigger it. It is
  documented because it's one of the nine gates the design brief named, not
  because Autopilot can attempt and then be stopped from it.
- **No automatic retry classification of "definitely unrelated" CI
  failures** — Autopilot always treats a failure as worth investigating
  with real evidence; it never assumes a failure is flaky/unrelated without
  a human saying so.
- **V1 supports exactly the two stages this repository's real history
  uses** (`planning`, `implementation`) — not a configurable pipeline of
  arbitrary stages.

## Why bounded, not fully autonomous

TaskForge's own engineering discipline (invariants, ADRs, proof
obligations, the `mount()`-style single-choke-point conventions) exists
precisely because letting *any* actor — human or automated — make
architectural, security, or correctness-weakening decisions silently is
how an enterprise-readiness effort quietly rots. Autopilot's job is to
remove the copy/paste tedium of carrying a phase through a workflow this
project already trusts, not to make decisions that workflow was designed
to route to a human. Every bound in this document (one review, one
recheck, a fixed CI-repair budget, a default-required merge confirmation)
exists so that trust, not to work around it.
