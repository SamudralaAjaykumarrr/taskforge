package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/chaos"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
)

// outcome is what happened to one claimed job, for report accounting.
type outcome int

const (
	outcomeSucceeded outcome = iota
	outcomeRetried
	outcomeDeadLettered
	outcomeCancelled
	outcomeTimedOut
	outcomeCrashed
)

// diamondSpec returns a small, fixed diamond DAG (A -> B, A -> C, B+C ->
// D) under prefix-namespaced job_types -- the same shape
// internal/chaos's own test suite uses (docs/scenario-corpus.md SF-022),
// duplicated here (rather than imported, since it lives in a _test.go
// file) so this command can submit real workflow chaos load without
// depending on test-only code.
func diamondSpec(prefix string) workflow.GraphSpec {
	return workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		{NodeKey: "A", JobType: prefix + ".a", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 10},
		{NodeKey: "B", JobType: prefix + ".b", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 10, DependsOn: []string{"A"}},
		{NodeKey: "C", JobType: prefix + ".c", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 10, DependsOn: []string{"A"}},
		{NodeKey: "D", JobType: prefix + ".d", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 10, DependsOn: []string{"B", "C"}},
	}}
}

// runWorkerLoop repeatedly claims and resolves jobs from the shared pool
// until ctx is done or nothing is eligible, mirroring
// internal/chaos.resolveCombinedClaim's weighted-outcome shape (success,
// retryable failure, permanent failure, simulated crash, cancellation,
// timeout) at manually configurable load.
func runWorkerLoop(ctx context.Context, s *store.Store, rng *chaos.Rand, workerID int, r *report) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		j, ok, err := s.Claim(ctx, fmt.Sprintf("chaos-worker-%03d", workerID))
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return
			}
			r.recordError(fmt.Errorf("claim: %w", err))
			return
		}
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
			continue
		}

		resolveClaim(ctx, s, rng, j, r)
	}
}

// resolveClaim resolves one claimed job per a weighted seeded decision.
// Weights favor eventual convergence (mostly success, some retry/failure,
// a modest crash rate) so a bounded-duration run makes real progress
// while still exercising every fault class.
func resolveClaim(ctx context.Context, s *store.Store, rng *chaos.Rand, j *job.Job, r *report) {
	owner := *j.LeaseOwner
	decision := rng.Pick([]float64{60, 15, 8, 8, 5, 4}) // success, retry, permanent, crash, cancel, timeout
	var err error
	switch decision {
	case 0:
		_, err = s.CompleteSuccess(ctx, j.ID, owner, j.LeaseGeneration, nil)
		r.recordOutcome(outcomeSucceeded)
	case 1:
		_, err = s.CompleteRetryableFailure(ctx, j.ID, owner, j.LeaseGeneration, "chaos: stress transient", 0)
		r.recordOutcome(outcomeRetried)
	case 2:
		_, err = s.CompleteFailure(ctx, j.ID, owner, j.LeaseGeneration, "chaos: stress permanent", job.ErrorClassPermanent)
		r.recordOutcome(outcomeDeadLettered)
	case 3:
		r.recordOutcome(outcomeCrashed) // abandon: leave RUNNING, recovered once its lease expires
		return
	case 4:
		if _, rerr := s.RequestCancellation(ctx, j.ID); rerr != nil {
			return // lost the race to a concurrent terminal transition -- not an error
		}
		_, err = s.CompleteCancelled(ctx, j.ID, owner, j.LeaseGeneration)
		r.recordOutcome(outcomeCancelled)
	case 5:
		_, err = s.CompleteTimeout(ctx, j.ID, owner, j.LeaseGeneration, 0)
		r.recordOutcome(outcomeTimedOut)
	}
	if err != nil && !errors.Is(err, store.ErrStaleTransition) {
		r.recordError(fmt.Errorf("resolve job %s: %w", j.ID, err))
	}
}
