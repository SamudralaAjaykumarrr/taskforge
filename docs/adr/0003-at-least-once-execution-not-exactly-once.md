# ADR-0003: At-Least-Once Execution, Not Exactly-Once

Status: Accepted

## Context

Job engines are frequently marketed or assumed to provide "exactly-once"
execution. This is a well-known distributed-systems overreach: a system
that performs external side effects (HTTP calls, emails, third-party API
writes) outside its own transactional boundary cannot atomically couple
"the side effect happened" with "I durably recorded that it happened,"
because those are two separate operations against two separate systems
that cannot be made transactional with each other in general. See
[vision.md](../vision.md) core thesis, [failure-model.md](../failure-model.md)
F3, [retry-semantics.md](../retry-semantics.md) crash-timing table.

## Decision

TaskForge commits, explicitly and permanently, to **at-least-once
execution semantics**: every accepted job will be attempted at least once
and retried until it reaches a terminal state, but a retry may re-invoke a
handler whose previous attempt's side effect already took place. TaskForge
will never claim, market, or silently imply exactly-once execution of
arbitrary side effects anywhere in its documentation or code comments.

## Alternatives Considered

- **Attempt "exactly-once" via a two-phase-commit-style protocol between
  TaskForge and arbitrary downstream systems.** Rejected: this requires
  every downstream system (arbitrary HTTP APIs, email providers, etc.) to
  participate in a distributed transaction protocol, which is not
  realistic for arbitrary external systems and would make TaskForge
  useless for its actual target use cases (calling ordinary, non-XA-aware
  APIs).
- **Claim "effectively exactly-once" via extremely short lease windows and
  aggressive heartbeat requirements, minimizing the crash window.**
  Rejected as a *primary* strategy: this reduces the *probability* of
  duplicate side effects but does not eliminate the possibility, and
  presenting a probabilistic guarantee as if it were an absolute one would
  be dishonest — exactly the kind of overreach this ADR exists to avoid.
  Short leases remain a good practice (they bound recovery time,
  TF-INV-004) but are documented as reducing the *window*, not eliminating
  the *possibility*.
- **Do nothing about the distinction and let users assume whatever they
  want.** Rejected: ambiguity here is the single most damaging thing this
  project could do to its own credibility with the target audience of
  distributed-systems reviewers named in [vision.md](../vision.md), who
  will specifically probe for exactly this kind of unexamined claim.

## Consequences

- Positive: Every claim TaskForge makes is defensible under scrutiny. This
  is the foundation the project's credibility rests on.
- Positive: It clarifies the actual engineering problem for job authors —
  they know from the start that idempotency is *their* responsibility for
  side effects, and TaskForge gives them the tools to do it
  ([idempotency.md](../idempotency.md), see
  [ADR-0004](0004-idempotency-for-exactly-once-effects.md)).
- Negative: This is a less appealing marketing claim than "exactly-once,"
  and requires every piece of documentation, every README section, and
  every future comment in code to be worded carefully to avoid backsliding
  into the easier-sounding but false claim.

## Failure Implications

F3 in [failure-model.md](../failure-model.md) ("worker crashes after
external side effect but before acknowledgement") is the concrete failure
this ADR is about: TaskForge detects this failure (via lease expiry) and
recovers *the job's progress* (it will be retried), but it does not and
cannot detect or prevent the *duplicate side effect* that may result. This
is validated as an explicit, expected outcome by
[scenario-corpus.md](../scenario-corpus.md) SF-004, which is a test that
the duplication *can happen* (documenting the limitation), paired with a
companion test showing a handler using the idempotency patterns in
[ADR-0004](0004-idempotency-for-exactly-once-effects.md) avoids the
duplicate *logical* effect despite the duplicate *invocation*.
