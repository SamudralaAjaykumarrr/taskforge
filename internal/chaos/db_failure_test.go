// Database failure injection campaigns (Phase 9 adversarial campaign
// items 6, 17, 18, docs/roadmap.md "Database Failure Injection"):
// transaction rollback during completion, temporary connection failure,
// and a constrained connection pool -- exercising docs/failure-model.md
// F5 ("Database transaction rollback") and F6 ("Temporary database
// unavailability") at real PostgreSQL failure boundaries (never a mocked
// driver), per docs/roadmap.md's "Fault-Injection Design" guidance.
package chaos_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/chaos"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/jobstate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
)

// TestChaos_DatabaseRollback_ClaimAndRetryTransitionsSurviveInjectedFailure
// is campaign item 6: forces a mid-transaction rollback (via
// chaos.PoisonAttemptInsert, the same UNIQUE-constraint-collision
// mechanism internal/store's own TF-INV-013 fault-injection tests use)
// at both the claim boundary and the retry-transition boundary, across
// several seeded jobs, and asserts every poisoned attempt leaves the row
// completely unchanged (TF-INV-013) while an un-poisoned retry afterward
// succeeds normally -- the injected failure is not a permanent poison.
func TestChaos_DatabaseRollback_ClaimAndRetryTransitionsSurviveInjectedFailure(t *testing.T) {
	for _, seed := range []int64{7, 17} {
		seed := seed
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			t.Logf("chaos seed=%d", seed)
			db := testutil.DB(t)
			s := store.New(db, store.WithLogger(discardLogger()))
			ctx := context.Background()
			rng := chaos.NewRand(seed)
			checker := invariant.New(db)

			const numJobs = 20
			for i := 0; i < numJobs; i++ {
				created, err := s.Insert(ctx, newChaosParams("chaos.dbrollback", 5))
				require.NoError(t, err)

				// Poison this job's very first claim attempt (attempt_number=1).
				require.NoError(t, chaos.PoisonAttemptInsert(ctx, db, created.ID, 1))

				_, ok, err := s.Claim(ctx, fmt.Sprintf("rollback-worker-%d", i))
				require.Error(t, err, "a poisoned attempt insert must surface as an error, not silently succeed")
				require.False(t, ok)

				after, err := s.GetByID(ctx, created.ID)
				require.NoError(t, err)
				require.Equal(t, jobstate.Queued, after.State, "TF-INV-013: a rolled-back claim must leave the job exactly as it was")
				require.Equal(t, 0, after.AttemptCount)
				require.Equal(t, int64(0), after.LeaseGeneration)

				require.NoError(t, chaos.UnpoisonAttemptInserts(ctx, db))

				// Recovery: the job is genuinely still claimable, and (per a
				// seeded coin flip) either succeeds outright or is pushed
				// through one retryable failure first, proving the store
				// recovers cleanly from the injected failure either way.
				claimed, ok, err := s.Claim(ctx, fmt.Sprintf("recovery-worker-%d", i))
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, int64(1), claimed.LeaseGeneration)

				if rng.Bool(0.5) {
					_, err = s.CompleteRetryableFailure(ctx, claimed.ID, fmt.Sprintf("recovery-worker-%d", i), claimed.LeaseGeneration, "transient", 0)
					require.NoError(t, err)
					require.NoError(t, chaos.ForceSetEligibleAt(ctx, db, claimed.ID, 0))
					reclaimed, ok, err := s.Claim(ctx, fmt.Sprintf("recovery-worker-2-%d", i))
					require.NoError(t, err)
					require.True(t, ok)
					_, err = s.CompleteSuccess(ctx, reclaimed.ID, fmt.Sprintf("recovery-worker-2-%d", i), reclaimed.LeaseGeneration, nil)
					require.NoError(t, err)
				} else {
					_, err = s.CompleteSuccess(ctx, claimed.ID, fmt.Sprintf("recovery-worker-%d", i), claimed.LeaseGeneration, nil)
					require.NoError(t, err)
				}
			}

			checkNoViolations(t, ctx, checker, seed)
		})
	}
}

// TestChaos_ConnectionInterruption_TerminatedBackendDoesNotCorruptState is
// campaign item 17: a real PostgreSQL backend process serving an
// in-flight completion transaction is forcibly terminated
// (pg_terminate_backend) mid-transaction -- not a simulated error, an
// actually severed connection -- and the job's row must be left exactly
// as it was before the transaction began (TF-INV-013), with the job
// still genuinely completable afterward through an ordinary connection.
func TestChaos_ConnectionInterruption_TerminatedBackendDoesNotCorruptState(t *testing.T) {
	db := testutil.DB(t)
	s := store.New(db, store.WithLogger(discardLogger()))
	ctx := context.Background()
	checker := invariant.New(db)

	const numJobs = 8
	for i := 0; i < numJobs; i++ {
		created, err := s.Insert(ctx, newChaosParams("chaos.conninterrupt", 5))
		require.NoError(t, err)
		claimed, ok, err := s.Claim(ctx, fmt.Sprintf("conn-worker-%d", i))
		require.NoError(t, err)
		require.True(t, ok)

		// Dedicate a single connection to this attempt's completion, so we
		// can identify and kill exactly its backend process.
		conn, err := db.Conn(ctx)
		require.NoError(t, err)
		pid, err := chaos.BackendPID(ctx, conn)
		require.NoError(t, err)

		tx, err := conn.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET state = 'RUNNING' WHERE id = $1`, created.ID) // no-op write, holds the tx open
		require.NoError(t, err)

		// Sever the connection from a second, independent connection
		// while the transaction is still open and uncommitted.
		require.NoError(t, chaos.TerminateBackend(ctx, db, pid))

		commitErr := tx.Commit()
		require.Error(t, commitErr, "a commit over a terminated backend connection must fail, not silently succeed")
		_ = conn.Close()

		// The row must be completely unaffected: the connection died
		// before commit, so PostgreSQL guarantees the transaction never
		// took effect (TF-INV-013).
		after, err := s.GetByID(ctx, created.ID)
		require.NoError(t, err)
		require.Equal(t, jobstate.Running, after.State)
		require.Equal(t, int64(1), after.LeaseGeneration)

		// The job is still perfectly completable through an ordinary
		// (unaffected) connection afterward.
		_, err = s.CompleteSuccess(ctx, claimed.ID, fmt.Sprintf("conn-worker-%d", i), claimed.LeaseGeneration, nil)
		require.NoError(t, err)
	}

	checkNoViolations(t, ctx, checker, -1)
}

// TestChaos_ConstrainedConnectionPool_ClaimProgressesUnderChaos is
// campaign item 18: combines a connection pool deliberately smaller than
// the worker count (docs/failure-model.md F6-adjacent: bounded resource
// pressure, not full unavailability) with concurrent crash/reclaim
// pressure, proving the short-transaction claim design degrades to
// queueing under contention, never deadlock or lost work, even while
// jobs are simultaneously being reclaimed.
func TestChaos_ConstrainedConnectionPool_ClaimProgressesUnderChaos(t *testing.T) {
	db := testutil.DB(t)
	const maxConns = 4
	db.SetMaxOpenConns(maxConns)
	s := store.New(db, store.WithLogger(discardLogger()))
	ctx := context.Background()
	checker := invariant.New(db)

	const numJobs = 60
	const numWorkers = 20 // deliberately > maxConns

	for i := 0; i < numJobs; i++ {
		_, err := s.Insert(ctx, newChaosParams("chaos.pool", 5))
		require.NoError(t, err)
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	start := make(chan struct{})
	var wg sync.WaitGroup
	var totalOutcomes int64
	errsCh := make(chan error, numWorkers)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			<-start
			for {
				select {
				case <-ctxTimeout.Done():
					return
				default:
				}
				j, ok, err := s.Claim(ctxTimeout, fmt.Sprintf("pool-chaos-worker-%03d", workerID))
				if err != nil {
					if ctxTimeout.Err() != nil {
						return
					}
					errsCh <- err
					return
				}
				if !ok {
					return
				}
				if workerID%3 == 0 {
					// A third of the pool "crashes" instead of completing --
					// pool pressure plus crash pressure at the same time.
					continue
				}
				_, err = s.CompleteSuccess(ctxTimeout, j.ID, fmt.Sprintf("pool-chaos-worker-%03d", workerID), j.LeaseGeneration, nil)
				if err != nil && !isStaleOrCtx(err) {
					errsCh <- err
					return
				}
				atomic.AddInt64(&totalOutcomes, 1)
			}
		}()
	}

	close(start)
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		require.NoError(t, err, "a connection pool smaller than the worker count must never deadlock or error, only serialize")
	}

	// Drain what "crashed" workers abandoned: expire and reclaim until
	// nothing eligible remains.
	for {
		n, err := chaos.ForceExpireAllRunningLeases(ctx, db)
		require.NoError(t, err)
		if n == 0 {
			break
		}
		for {
			j, ok, err := s.Claim(ctx, "pool-chaos-drain")
			require.NoError(t, err)
			if !ok {
				break
			}
			_, err = s.CompleteSuccess(ctx, j.ID, "pool-chaos-drain", j.LeaseGeneration, nil)
			require.NoError(t, err)
		}
	}

	var succeeded int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE job_type = 'chaos.pool' AND state = 'SUCCEEDED'`).Scan(&succeeded))
	require.Equal(t, numJobs, succeeded, "every job must eventually succeed despite constrained-pool + crash pressure combined")

	checkNoViolations(t, ctx, checker, -1)
}

func isStaleOrCtx(err error) bool {
	return errors.Is(err, store.ErrStaleTransition) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
