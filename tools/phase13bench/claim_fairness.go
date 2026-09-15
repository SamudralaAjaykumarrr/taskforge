package main

import (
	"context"
	"database/sql"
)

// perQueueCandidate uses the same idx_bench_claim_or_reclaim index
// claim_concurrency.go's pickAndLockCandidateSQL relies on, restricted to
// a single queue via a correlated LATERAL reference (queueExpr) rather
// than a bind parameter, and deliberately non-locking (no FOR UPDATE
// here) -- this is step 1's "pick" read, not step 2's targeted claim, so
// it never locks a candidate it might not end up using. An earlier draft
// used a bare `state = 'QUEUED' OR state = 'RUNNING'` predicate spanning
// two separate single-state partial indexes here, which reproduces the
// same seq-scan-and-sort collapse claim_concurrency.go's own comment
// documents, for any queue whose own backlog is large -- exactly the
// flooded/hot queue every fairness experiment in this package
// deliberately creates.
func perQueueCandidate(queueExpr string) string {
	return `(
		SELECT id, state, priority, eligible_at FROM bench_jobs
		WHERE queue_name = ` + queueExpr + ` AND state IN ('QUEUED', 'RUNNING') AND ` + claimableWhere + `
		ORDER BY priority DESC, eligible_at ASC LIMIT 1
	)`
}

// claimProdBaseline runs prodClaimQuery -- TODAY's actual production
// claim query, verbatim, against prod_jobs's real index set -- and
// returns which harness-only "logical queue" label the claimed row
// carried, purely for this test's own measurement. Production itself
// never sees or uses that label.
func claimProdBaseline(ctx context.Context, db *sql.DB, owner string) (id int64, logicalQueue string, wasReclaim bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", false, err
	}
	defer tx.Rollback() //nolint:errcheck
	var oldState string
	err = tx.QueryRowContext(ctx, prodClaimQuery, owner).Scan(&id, &logicalQueue, &oldState)
	if err == sql.ErrNoRows {
		return 0, "", false, tx.Commit()
	}
	if err != nil {
		return 0, "", false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, "", false, err
	}
	return id, logicalQueue, oldState == "RUNNING", nil
}

// claimFairnessV2 implements both corrected fairness candidates (review
// finding H1/M2's fix) as a non-locking "pick the winning (queue, job)"
// read followed by a targeted, single-row locking claim -- never a
// cross-table ORDER BY or a windowed rank over the full backlog. Each
// per-queue candidate is fetched via a LATERAL subquery restricted to
// exactly that queue, which lets PostgreSQL use the existing
// (queue_name, priority DESC, eligible_at ASC) index once per queue,
// costing O(number of subscribed queues), not O(backlog size).
//
// integrated=true additionally makes this the "slot-table concurrency +
// corrected fairness" combined mechanism requirement 6 asks for: queue
// selection considers a queue eligible if it either has a free capacity
// slot (a fresh claim) or its own top candidate is itself a reclaimable
// RUNNING row (which needs no new slot -- a reclaimed job already holds
// the slot it was originally admitted under; reclaiming it is a
// continuation of the same concurrency unit, not a new admission, so
// slot accounting must not treat it as one). Step 2 branches on which
// case the winning candidate turned out to be, so a fresh claim atomically
// acquires a free slot (and the whole claim fails closed, not partially,
// if none remains by the time of the locking attempt) while a reclaim
// only re-marks the job RUNNING under a new attempt and never touches
// bench_queue_slots at all.
func claimFairnessV2(ctx context.Context, db *sql.DB, fair FairnessID, subscribed []string, owner string, integrated bool) (claimOutcome, string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return claimOutcome{}, "", err
	}
	defer tx.Rollback() //nolint:errcheck

	var winID int64
	var winQueue, winState string
	var pickErr error

	capacityFilter := ``
	if integrated {
		capacityFilter = ` AND (j.state = 'RUNNING' OR EXISTS (SELECT 1 FROM bench_queue_slots WHERE queue_name = j.qn AND held_by_job_id IS NULL))`
	}

	switch fair {
	case FairRoundRobinV2:
		q := `
			SELECT j.id, j.state, j.qn
			FROM (
				SELECT j.id, j.state, qn.queue_name AS qn, j.priority, j.eligible_at
				FROM unnest($1::text[]) AS qn(queue_name)
				CROSS JOIN LATERAL ` + perQueueCandidate("qn.queue_name") + ` j
			) j
			WHERE true` + capacityFilter + `
			ORDER BY j.priority DESC, j.eligible_at ASC LIMIT 1`
		pickErr = tx.QueryRowContext(ctx, q, subscribed).Scan(&winID, &winState, &winQueue)

	case FairLastClaimedV2:
		q := `
			SELECT j.id, j.state, j.qn
			FROM (
				SELECT j.id, j.state, q.queue_name AS qn, q.last_claimed_at
				FROM bench_queue_state q
				CROSS JOIN LATERAL ` + perQueueCandidate("q.queue_name") + ` j
				WHERE q.queue_name = ANY($1)
			) j
			WHERE true` + capacityFilter + `
			ORDER BY j.last_claimed_at ASC LIMIT 1`
		pickErr = tx.QueryRowContext(ctx, q, subscribed).Scan(&winID, &winState, &winQueue)
	}

	if pickErr == sql.ErrNoRows {
		return claimOutcome{attemptedTxns: 1}, "", tx.Commit()
	}
	if pickErr != nil {
		return claimOutcome{}, "", pickErr
	}

	var claimedID int64
	var oldState string

	if integrated && winState == "QUEUED" {
		// Fresh claim under capacity limiting: atomically bind a job AND a
		// free slot together (candidate x slot cross join yields zero rows,
		// so the whole claim fails closed, if no slot remains by now).
		var slotIndex int
		err = tx.QueryRowContext(ctx, `
			WITH candidate AS (
				SELECT id, state FROM bench_jobs WHERE id = $1 AND `+claimableWhere+` FOR UPDATE SKIP LOCKED
			), slot AS (
				SELECT slot_index FROM bench_queue_slots WHERE queue_name = $2 AND held_by_job_id IS NULL FOR UPDATE SKIP LOCKED LIMIT 1
			)
			UPDATE bench_jobs
			SET state='RUNNING', claimed_by=$3, claimed_at=now(), lease_expires_at=now()+interval '300 milliseconds', attempt_count=attempt_count+1
			FROM candidate, slot
			WHERE bench_jobs.id = candidate.id
			RETURNING bench_jobs.id, candidate.state, slot.slot_index`, winID, winQueue, owner).Scan(&claimedID, &oldState, &slotIndex)
		if err == nil {
			if _, serr := tx.ExecContext(ctx, `UPDATE bench_queue_slots SET held_by_job_id=$1 WHERE queue_name=$2 AND slot_index=$3`, claimedID, winQueue, slotIndex); serr != nil {
				return claimOutcome{}, "", serr
			}
		}
	} else {
		// Either the non-integrated candidates (no slot concept at all),
		// or an integrated reclaim (the job already owns its slot --
		// re-marking it RUNNING is a continuation, not a new admission,
		// so no bench_queue_slots write happens here at all).
		lease := `now()+interval '30 seconds'`
		if integrated {
			lease = `now()+interval '300 milliseconds'`
		}
		err = tx.QueryRowContext(ctx, `
			WITH c AS (
				SELECT id, state FROM bench_jobs WHERE id = $1 AND `+claimableWhere+` FOR UPDATE SKIP LOCKED
			)
			UPDATE bench_jobs
			SET state='RUNNING', claimed_by=$2, claimed_at=now(), lease_expires_at=`+lease+`, attempt_count=attempt_count+1
			FROM c WHERE bench_jobs.id = c.id
			RETURNING bench_jobs.id, c.state`, winID, owner).Scan(&claimedID, &oldState)
	}

	if err == sql.ErrNoRows {
		// The winning candidate was skip-locked away by a concurrent
		// claimer between the non-locking pick and this targeted attempt
		// -- a normal, expected race under contention, not an error.
		return claimOutcome{attemptedTxns: 1}, "", tx.Commit()
	}
	if err != nil {
		return claimOutcome{}, "", err
	}

	if fair == FairLastClaimedV2 {
		if _, err := tx.ExecContext(ctx, `UPDATE bench_queue_state SET last_claimed_at = now() WHERE queue_name = $1`, winQueue); err != nil {
			return claimOutcome{}, "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return claimOutcome{}, "", err
	}
	return claimOutcome{id: claimedID, ok: true, wasReclaim: oldState == "RUNNING", attemptedTxns: 1}, winQueue, nil
}
