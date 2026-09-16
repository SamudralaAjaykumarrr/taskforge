// Phase 13 (docs/phase-13-plan.md §6b/§8/§14, checkpoint 2): claim-query
// regression tests for the queue-subscription filter, run against a real
// PostgreSQL instance per this project's unbroken "no mocked database"
// discipline. Covers exactly the scenarios the implementation task names:
// no subscription config, one queue, multiple queues, jobs outside
// subscription, and the reclaim/subscription interaction (SF-046 through
// SF-049).
package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/job"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

func newJobParamsQueue(jobType, queueName string) job.NewParams {
	return job.NewParams{
		PrincipalID:             testPrincipalID,
		JobType:                 jobType,
		Payload:                 json.RawMessage(`{"k":"v"}`),
		MaxAttempts:             5,
		ExecutionTimeoutSeconds: 30,
		QueueName:               queueName,
	}
}

// TestClaim_QueueBlind_ClaimsFromEveryQueue is SF-046's first half: the
// compatibility entry point (Claim, no subscription) claims from every
// queue, including a queue that was never "configured" in any way (OD-8:
// queue_name is free-form, no registry).
func TestClaim_QueueBlind_ClaimsFromEveryQueue(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	for _, q := range []string{"default", "reports", "emails"} {
		_, err := s.Insert(ctx, newJobParamsQueue("test.p13.queueblind", q))
		require.NoError(t, err)
	}

	claimedQueues := map[string]bool{}
	for i := 0; i < 3; i++ {
		j, ok, err := s.Claim(ctx, "worker-blind")
		require.NoError(t, err)
		require.True(t, ok)
		claimedQueues[j.QueueName] = true
	}
	require.Equal(t, map[string]bool{"default": true, "reports": true, "emails": true}, claimedQueues)

	_, ok, err := s.Claim(ctx, "worker-blind")
	require.NoError(t, err)
	require.False(t, ok, "nothing left to claim")
}

// TestClaimFromQueues_OneQueue_OnlyClaimsFromThatQueue is the "one queue"
// scenario: a worker subscribed to exactly one queue_name never claims
// from any other, even though other queues have eligible work.
func TestClaimFromQueues_OneQueue_OnlyClaimsFromThatQueue(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsQueue("test.p13.one_queue", "reports"))
	require.NoError(t, err)
	_, err = s.Insert(ctx, newJobParamsQueue("test.p13.one_queue", "emails"))
	require.NoError(t, err)

	j, ok, err := s.ClaimFromQueues(ctx, "worker-reports", []string{"reports"})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "reports", j.QueueName)

	_, ok, err = s.ClaimFromQueues(ctx, "worker-reports", []string{"reports"})
	require.NoError(t, err)
	require.False(t, ok, "the 'emails' job must not be visible to a worker subscribed only to 'reports'")
}

// TestClaimFromQueues_MultipleQueues_ClaimsFromAnySubscribed is the
// "multiple queues" scenario: a worker subscribed to a SET of queues
// claims from any of them, but still never from one outside the set.
func TestClaimFromQueues_MultipleQueues_ClaimsFromAnySubscribed(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	for _, q := range []string{"reports", "emails", "billing"} {
		_, err := s.Insert(ctx, newJobParamsQueue("test.p13.multi_queue", q))
		require.NoError(t, err)
	}

	subscribed := []string{"reports", "emails"}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		j, ok, err := s.ClaimFromQueues(ctx, "worker-multi", subscribed)
		require.NoError(t, err)
		require.True(t, ok)
		seen[j.QueueName] = true
	}
	require.Equal(t, map[string]bool{"reports": true, "emails": true}, seen)

	_, ok, err := s.ClaimFromQueues(ctx, "worker-multi", subscribed)
	require.NoError(t, err)
	require.False(t, ok, "'billing' is outside this worker's subscription and must remain unclaimed by it")

	// The unsubscribed queue's job is still claimable by an unrestricted
	// (queue-blind) worker.
	j, ok, err := s.Claim(ctx, "worker-blind")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "billing", j.QueueName)
}

// TestClaimFromQueues_AdversarialContention_NeverClaimsOutsideSubscription
// is SF-047's adversarial direction: even under direct contention against
// an unrestricted worker racing for the same pool, a queue-restricted
// worker never claims a job outside its subscription.
func TestClaimFromQueues_AdversarialContention_NeverClaimsOutsideSubscription(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	const n = 20
	for i := 0; i < n; i++ {
		_, err := s.Insert(ctx, newJobParamsQueue("test.p13.adversarial", "off-limits"))
		require.NoError(t, err)
	}

	for i := 0; i < n; i++ {
		j, ok, err := s.ClaimFromQueues(ctx, fmt.Sprintf("restricted-%d", i), []string{"reports"})
		require.NoError(t, err)
		require.False(t, ok, "restricted worker must never claim from 'off-limits'")
		require.Nil(t, j)
	}

	// Every job is still there, unclaimed, for the unrestricted worker.
	for i := 0; i < n; i++ {
		j, ok, err := s.Claim(ctx, fmt.Sprintf("unrestricted-%d", i))
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "off-limits", j.QueueName)
	}
}

// TestClaimFromQueues_NeverReclaimsExpiredLeaseOutsideSubscription is
// SF-049's adversarial direction (§6b's settled rule): a worker restricted
// to a queue subset never reclaims an expired lease for a job outside that
// subset, even though the job is otherwise reclaim-eligible
// (lease_expires_at < now(), attempt budget remaining) -- the subscription
// filter is one authorization boundary applying identically to both claim
// branches, not a looser rule for reclaim.
func TestClaimFromQueues_NeverReclaimsExpiredLeaseOutsideSubscription(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsQueue("test.p13.reclaim_subscription", "off-limits"))
	require.NoError(t, err)

	claimed, ok, err := s.Claim(ctx, "crashed-worker")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, created.ID, claimed.ID)

	forceExpireLease(t, db, created.ID)

	// A worker restricted to a different queue must never reclaim it.
	for i := 0; i < 10; i++ {
		j, ok, err := s.ClaimFromQueues(ctx, fmt.Sprintf("restricted-%d", i), []string{"reports"})
		require.NoError(t, err)
		require.False(t, ok, "restricted worker must never reclaim a job outside its subscription")
		require.Nil(t, j)
	}

	// The job is still there, RUNNING, with its expired lease untouched.
	still, err := s.GetByID(ctx, created.ID, testAccess)
	require.NoError(t, err)
	require.Equal(t, int64(1), still.LeaseGeneration, "an unauthorized worker must not have advanced the lease generation")
}

// TestClaimFromQueues_ReclaimsExpiredLeaseWithinSubscription is SF-049's
// compatibility/positive direction: a worker subscribed to the job's own
// queue CAN reclaim its expired lease, exactly as an unrestricted worker
// could.
func TestClaimFromQueues_ReclaimsExpiredLeaseWithinSubscription(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsQueue("test.p13.reclaim_subscription_ok", "reports"))
	require.NoError(t, err)

	claimed, ok, err := s.Claim(ctx, "crashed-worker")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, created.ID, claimed.ID)

	forceExpireLease(t, db, created.ID)

	reclaimed, ok, err := s.ClaimFromQueues(ctx, "reports-worker", []string{"reports"})
	require.NoError(t, err)
	require.True(t, ok, "a worker subscribed to the job's own queue must be able to reclaim it")
	require.Equal(t, created.ID, reclaimed.ID)
	require.Equal(t, int64(2), reclaimed.LeaseGeneration)
}

// TestClaim_QueueBlindReclaimsAcrossAllQueues is SF-046's second half,
// extended to reclaim: an unrestricted (queue-blind) worker reclaims an
// expired lease regardless of which queue the job belongs to, identical
// to pre-Phase-13 behavior -- no queue partitioning narrows this unless an
// operator opts into TASKFORGE_WORKER_QUEUES.
func TestClaim_QueueBlindReclaimsAcrossAllQueues(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	created, err := s.Insert(ctx, newJobParamsQueue("test.p13.queueblind_reclaim", "some-other-queue"))
	require.NoError(t, err)

	claimed, ok, err := s.Claim(ctx, "crashed-worker")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, created.ID, claimed.ID)

	forceExpireLease(t, db, created.ID)

	reclaimed, ok, err := s.Claim(ctx, "blind-worker")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, created.ID, reclaimed.ID)
	require.Equal(t, int64(2), reclaimed.LeaseGeneration)
}

// TestClaimFromQueues_EmptySubscription_ClaimsNothing guards against a
// misconfiguration silently behaving like the queue-blind default:
// ClaimFromQueues with an empty slice is the EXPLICIT-subscription entry
// point (a Worker that wants queue-blind behavior must call Claim
// instead), so it must claim nothing rather than everything.
func TestClaimFromQueues_EmptySubscription_ClaimsNothing(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db)
	ctx := context.Background()

	_, err := s.Insert(ctx, newJobParamsQueue("test.p13.empty_subscription", "default"))
	require.NoError(t, err)

	j, ok, err := s.ClaimFromQueues(ctx, "worker", nil)
	require.NoError(t, err)
	require.False(t, ok)
	require.Nil(t, j)

	j, ok, err = s.ClaimFromQueues(ctx, "worker", []string{})
	require.NoError(t, err)
	require.False(t, ok)
	require.Nil(t, j)
}
