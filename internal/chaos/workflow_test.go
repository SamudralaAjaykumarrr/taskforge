// Workflow chaos campaigns (Phase 9 adversarial campaign items 14, 15,
// 16, docs/roadmap.md "Workflow Chaos"): concurrent predecessor
// completion racing a shared fan-in node, a predecessor's retry not
// prematurely unblocking a dependent, and a stale (post-reclaim)
// workflow-node completion never unblocking or cancelling dependents --
// combining Phase 7's dependency-gating guarantee (TF-INV-012) with
// Phase 2/3/5's fencing and retry machinery under chaos, across
// deterministic diamond/fan-in DAGs.
package chaos_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/chaos"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/workflow"
)

func diamondSpec(prefix string) workflow.GraphSpec {
	return workflow.GraphSpec{Nodes: []workflow.NodeSpec{
		{NodeKey: "A", JobType: prefix + ".a", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30},
		{NodeKey: "B", JobType: prefix + ".b", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30, DependsOn: []string{"A"}},
		{NodeKey: "C", JobType: prefix + ".c", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30, DependsOn: []string{"A"}},
		{NodeKey: "D", JobType: prefix + ".d", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30, DependsOn: []string{"B", "C"}},
	}}
}

func nodeByKey(inst *workflow.Instance, key string) workflow.Node {
	for _, n := range inst.Nodes {
		if n.NodeKey == key {
			return n
		}
	}
	panic("node not found: " + key)
}

// TestChaos_WorkflowFanIn_ConcurrentPredecessorCompletion_Seeded is
// campaign item 14: across many seeded diamond DAGs, B and C (the two
// predecessors of the shared fan-in node D) complete at, as close as
// synchronization allows, the exact same instant, released from one
// start barrier by two real goroutines with real pooled PostgreSQL
// connections. D must become eligible exactly once -- never zero times
// (a lost-update race) and never claimed twice.
func TestChaos_WorkflowFanIn_ConcurrentPredecessorCompletion_Seeded(t *testing.T) {
	for _, seed := range []int64{71, 72} {
		seed := seed
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			t.Logf("chaos seed=%d", seed)
			db := testutil.DB(t)
			s := store.New(db, store.WithLogger(discardLogger()))
			ctx := context.Background()
			rng := chaos.NewRand(seed)
			checker := invariant.New(db)

			const numDiamonds = 15
			for i := 0; i < numDiamonds; i++ {
				prefix := fmt.Sprintf("chaos.wf.fanin.%d.%d", seed, i)
				inst, err := s.CreateWorkflow(ctx, diamondSpec(prefix))
				require.NoError(t, err)

				a := nodeByKey(inst, "A")
				claimedA, ok, err := claimUntilResolved(t, ctx, s, "wf-fanin-a")
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, a.JobID, claimedA.ID)
				_, err = s.CompleteSuccess(ctx, claimedA.ID, "wf-fanin-a", claimedA.LeaseGeneration, nil)
				require.NoError(t, err)

				b := nodeByKey(inst, "B")
				c := nodeByKey(inst, "C")
				// B and C become eligible at exactly the same instant (A's
				// completion transaction activates both), so the claim
				// query may legitimately return either one first -- claim
				// twice and identify each result by matching its job ID
				// back to b.JobID/c.JobID, rather than assuming a
				// caller-chosen worker name determines which node is
				// returned.
				firstClaim, ok, err := claimUntilResolved(t, ctx, s, "wf-fanin-1")
				require.NoError(t, err)
				require.True(t, ok)
				secondClaim, ok, err := claimUntilResolved(t, ctx, s, "wf-fanin-2")
				require.NoError(t, err)
				require.True(t, ok)

				var claimedB, claimedC *job.Job
				for _, claim := range []*job.Job{firstClaim, secondClaim} {
					switch claim.ID {
					case b.JobID:
						claimedB = claim
					case c.JobID:
						claimedC = claim
					default:
						t.Fatalf("workflow %s: claimed job %s is neither B (%s) nor C (%s)", inst.ID, claim.ID, b.JobID, c.JobID)
					}
				}
				require.NotNil(t, claimedB)
				require.NotNil(t, claimedC)

				// A seeded coin flip decides which of B/C "wins" the race
				// to complete first in program order below -- both must
				// still resolve D's eligibility correctly regardless. Each
				// node's actual lease_owner (whichever worker name its
				// claim call landed under) is used, not an assumed one.
				first, second := claimedB, claimedC
				if rng.Bool(0.5) {
					first, second = claimedC, claimedB
				}
				firstOwner, secondOwner := *first.LeaseOwner, *second.LeaseOwner

				start := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					_, err := s.CompleteSuccess(ctx, first.ID, firstOwner, first.LeaseGeneration, nil)
					require.NoError(t, err)
				}()
				go func() {
					defer wg.Done()
					<-start
					_, err := s.CompleteSuccess(ctx, second.ID, secondOwner, second.LeaseGeneration, nil)
					require.NoError(t, err)
				}()
				close(start)
				wg.Wait()

				d := nodeByKey(inst, "D")
				claimedD, ok, err := claimUntilResolved(t, ctx, s, "wf-fanin-d")
				require.NoError(t, err)
				require.True(t, ok, "workflow %s: D must become eligible exactly once B and C have both succeeded, however the race resolved", inst.ID)
				require.Equal(t, d.JobID, claimedD.ID)

				// D must never be claimable a second time.
				second2, ok, err := s.Claim(ctx, "wf-fanin-d-2")
				require.NoError(t, err)
				require.False(t, ok, "workflow %s: D must not be claimable twice", inst.ID)
				require.Nil(t, second2)

				_, err = s.CompleteSuccess(ctx, claimedD.ID, "wf-fanin-d", claimedD.LeaseGeneration, nil)
				require.NoError(t, err)

				final, err := s.GetWorkflow(ctx, inst.ID)
				require.NoError(t, err)
				require.Equal(t, workflow.Succeeded, final.State)
			}

			checkNoViolations(t, ctx, checker, seed)
		})
	}
}

// TestChaos_WorkflowPredecessorRetry_DoesNotUnblockDependent is campaign
// item 15: across many linear A->B workflows, A fails retryably a
// seeded-random number of times (staying RETRY_WAIT, never DEAD_LETTERED
// or CANCELLED) before eventually succeeding -- B must remain durably
// blocked throughout every one of A's retry cycles, becoming eligible
// only once A's retry actually reaches SUCCEEDED.
func TestChaos_WorkflowPredecessorRetry_DoesNotUnblockDependent(t *testing.T) {
	for _, seed := range []int64{81, 182} {
		seed := seed
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			t.Logf("chaos seed=%d", seed)
			db := testutil.DB(t)
			s := store.New(db, store.WithLogger(discardLogger()))
			ctx := context.Background()
			rng := chaos.NewRand(seed)
			checker := invariant.New(db)

			const numWorkflows = 20
			for i := 0; i < numWorkflows; i++ {
				prefix := fmt.Sprintf("chaos.wf.predretry.%d", i)
				spec := workflow.GraphSpec{Nodes: []workflow.NodeSpec{
					{NodeKey: "A", JobType: prefix + ".a", Payload: []byte(`{}`), MaxAttempts: 6, ExecutionTimeoutSeconds: 30},
					{NodeKey: "B", JobType: prefix + ".b", Payload: []byte(`{}`), MaxAttempts: 3, ExecutionTimeoutSeconds: 30, DependsOn: []string{"A"}},
				}}
				inst, err := s.CreateWorkflow(ctx, spec)
				require.NoError(t, err)
				a := nodeByKey(inst, "A")
				b := nodeByKey(inst, "B")

				failures := rng.Intn(4) // 0..3 retryable failures before success, always < max_attempts=6
				for f := 0; f < failures; f++ {
					claimed, ok, err := claimUntilResolved(t, ctx, s, "wf-predretry-a")
					require.NoError(t, err)
					require.True(t, ok)
					require.Equal(t, a.JobID, claimed.ID, "only A should ever be eligible while B remains dependency-blocked")
					_, err = s.CompleteRetryableFailure(ctx, claimed.ID, "wf-predretry-a", claimed.LeaseGeneration, "chaos: transient", 0)
					require.NoError(t, err)

					// B must remain durably blocked during EVERY
					// intermediate RETRY_WAIT cycle, not just at the very
					// end -- checked directly against B's own durable row
					// (never claimed, still QUEUED) rather than via a
					// generic Claim() call, which -- since
					// CompleteRetryableFailure's delay=0 makes A itself
					// immediately re-eligible -- would otherwise just
					// re-claim A and prove nothing about B.
					gotB, err := s.GetByID(ctx, b.JobID)
					require.NoError(t, err)
					require.Equal(t, jobstate.Queued, gotB.State, "workflow %s: B must remain QUEUED (dependency-blocked) while A is still RETRY_WAIT", inst.ID)
					require.Equal(t, 0, gotB.AttemptCount, "workflow %s: B must never have been claimed while A is still RETRY_WAIT", inst.ID)
				}

				claimed, ok, err := claimUntilResolved(t, ctx, s, "wf-predretry-a-final")
				require.NoError(t, err)
				require.True(t, ok)
				_, err = s.CompleteSuccess(ctx, claimed.ID, "wf-predretry-a-final", claimed.LeaseGeneration, nil)
				require.NoError(t, err)

				claimedB, ok, err := claimUntilResolved(t, ctx, s, "wf-predretry-b")
				require.NoError(t, err)
				require.True(t, ok, "workflow %s: B must become eligible once A's retry actually succeeds", inst.ID)
				require.Equal(t, b.JobID, claimedB.ID)
				_, err = s.CompleteSuccess(ctx, claimedB.ID, "wf-predretry-b", claimedB.LeaseGeneration, nil)
				require.NoError(t, err)
			}

			checkNoViolations(t, ctx, checker, seed)
		})
	}
}

// TestChaos_StaleWorkflowNodeCompletion_NeverUnblocksOrCancelsDependents
// is campaign item 16: SF-028's exact sequence (a workflow node's owner
// loses its lease, a second worker reclaims and resolves it, and the
// FIRST worker's stale, late report arrives afterward), replayed across
// many seeded diamond DAGs with both a stale-success and a stale-failure
// variant. The stale call must be rejected outright and must never reach
// the point of propagating anything to dependents -- D's eligibility
// must reflect only the genuine, reclaiming generation's outcome.
func TestChaos_StaleWorkflowNodeCompletion_NeverUnblocksOrCancelsDependents(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db, store.WithLogger(discardLogger()))
	ctx := context.Background()
	checker := invariant.New(db)

	const numDiamonds = 16
	for i := 0; i < numDiamonds; i++ {
		prefix := fmt.Sprintf("chaos.wf.stale.%d", i)
		inst, err := s.CreateWorkflow(ctx, diamondSpec(prefix))
		require.NoError(t, err)

		a := nodeByKey(inst, "A")
		claimedA, ok, err := claimUntilResolved(t, ctx, s, "wf-stale-a1")
		require.NoError(t, err)
		require.True(t, ok)

		staleOwner, staleGen := "wf-stale-a1", claimedA.LeaseGeneration
		require.NoError(t, chaos.ForceExpireLease(ctx, db, a.JobID))

		reclaimedA, ok, err := claimUntilResolved(t, ctx, s, "wf-stale-a2")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, staleGen+1, reclaimedA.LeaseGeneration)

		// Half the diamonds: the genuine reclaiming generation succeeds.
		// Half: it dead-letters permanently (max_attempts=3, so this
		// exhausts on generation 2's own failure only if we push it to
		// the limit -- here we use CompleteFailure to force an immediate
		// permanent dead-letter instead, isolating the propagation
		// question from retry-budget timing).
		genuineSucceeds := i%2 == 0
		if genuineSucceeds {
			_, err = s.CompleteSuccess(ctx, reclaimedA.ID, "wf-stale-a2", reclaimedA.LeaseGeneration, nil)
			require.NoError(t, err)
		} else {
			_, err = s.CompleteFailure(ctx, reclaimedA.ID, "wf-stale-a2", reclaimedA.LeaseGeneration, "chaos: permanent", "PERMANENT")
			require.NoError(t, err)
		}

		// The stale generation's late report -- both a stale success and
		// a stale failure attempt -- must be rejected outright.
		_, err = s.CompleteSuccess(ctx, a.JobID, staleOwner, staleGen, nil)
		require.ErrorIs(t, err, store.ErrStaleTransition)
		_, err = s.CompleteFailure(ctx, a.JobID, staleOwner, staleGen, "stale", "PERMANENT")
		require.ErrorIs(t, err, store.ErrStaleTransition)

		b := nodeByKey(inst, "B")
		c := nodeByKey(inst, "C")
		if genuineSucceeds {
			// B and C become eligible at exactly the same instant -- claim
			// twice and identify each result by job ID, exactly as
			// TestChaos_WorkflowFanIn_ConcurrentPredecessorCompletion_Seeded
			// does, rather than assuming claim order.
			firstClaim, ok, err := claimUntilResolved(t, ctx, s, "wf-stale-1")
			require.NoError(t, err)
			require.True(t, ok, "workflow %s: B and C must both be eligible after A's genuine success", inst.ID)
			secondClaim, ok, err := claimUntilResolved(t, ctx, s, "wf-stale-2")
			require.NoError(t, err)
			require.True(t, ok)
			require.ElementsMatch(t, []uuid.UUID{b.JobID, c.JobID}, []uuid.UUID{firstClaim.ID, secondClaim.ID})
		} else {
			got, err := s.GetByID(ctx, b.JobID)
			require.NoError(t, err)
			require.Equal(t, "CANCELLED", string(got.State), "workflow %s: B must be cancelled by A's genuine permanent failure, never by the rejected stale one", inst.ID)
			got, err = s.GetByID(ctx, c.JobID)
			require.NoError(t, err)
			require.Equal(t, "CANCELLED", string(got.State))
		}
	}

	checkNoViolations(t, ctx, checker, -1)
}
