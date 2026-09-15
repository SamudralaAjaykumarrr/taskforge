package main

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// This file is the TF-INV-019 bound sweep: queue-count sweep, capacity-
// configuration sweep, and adversarial stress tests against the
// recommended integrated mechanism (slot-table concurrency +
// last_claimed_at fairness). It is additive -- it does not modify
// runFairnessMultiQueue or any v2 experiment, so every v2 result in
// docs/phase-13-concurrency-evidence-v2.md remains exactly reproducible
// against the unmodified code path. This file's own experiments are
// reported in docs/phase-13-concurrency-evidence-v3.md.
//
// The one methodological refinement this file makes relative to v2's
// fairness experiments: a "sparse" queue here is kept continuously
// capacity-eligible (a replenisher goroutine ensures it always has at
// least one QUEUED row, polling every replenishInterval) rather than
// v2's fixed-interval producer, which left gaps where a sparse queue had
// nothing pending at all -- during those gaps it cannot be "starved"
// in TF-INV-019's own sense (starvation presupposes pending,
// capacity-eligible work), so v2's wait numbers, while still valid as a
// realistic-arrival-pattern measurement, understate how this experiment
// needs to isolate the bound itself. This file measures the bound
// directly: a queue that ALWAYS has eligible work, so any wait is
// entirely attributable to the fairness/concurrency mechanism, not to
// gaps in demand.

type SweepOptions struct {
	Label                string
	NumQueues            int
	HotQueues            int
	Capacity             int // per-queue concurrency_limit and bench_queue_slots row count
	Workers              int
	Duration             time.Duration
	Fair                 FairnessID
	ReplenishInterval    time.Duration // default 5ms if zero
	LateEligibleQueue    bool          // one sparse queue's first job arrives at Duration/2
	TempIneligibleWindow bool          // one sparse queue's slots are occupied from Duration/4 to 3*Duration/4
	DynamicWorkers       bool          // start with Workers/2, add the rest at Duration/2
}

type SweepResult struct {
	Label                                  string  `json:"label"`
	NumQueues                              int     `json:"num_queues"`
	HotQueues                              int     `json:"hot_queues"`
	Capacity                               int     `json:"capacity_per_queue"`
	Workers                                int     `json:"workers"`
	DurationSec                            float64 `json:"duration_sec"`
	OfferedFloodInserts                    int64   `json:"offered_flood_inserts"`
	OfferedSparseInserts                   int64   `json:"offered_sparse_inserts"`
	FloodClaims                            int64   `json:"flood_claims"`
	SparseClaims                           int64   `json:"sparse_claims"`
	TotalClaims                            int64   `json:"total_claims"`
	ThroughputPerSec                       float64 `json:"throughput_per_sec"`
	MaxWaitMs                              float64 `json:"max_wait_ms"`
	P50WaitMs                              float64 `json:"p50_wait_ms"`
	P95WaitMs                              float64 `json:"p95_wait_ms"`
	P99WaitMs                              float64 `json:"p99_wait_ms"`
	LongestGapMs                           float64 `json:"longest_progress_gap_ms"`
	ZeroProgressQueues                     int     `json:"zero_progress_queues"`
	MaxNoProgressQueuesObserved            int     `json:"max_no_progress_queues_observed"`
	LimitViolations                        int     `json:"limit_violations"`
	ReclaimCount                           int64   `json:"reclaim_count"`
	ReclaimCorrectnessFailures             int     `json:"reclaim_correctness_failures"`
	MaxOtherQueuesServedBetweenTurns       int     `json:"max_other_queues_served_between_turns"`
	MaxOtherQueuesServedBetweenTurnsSparse int     `json:"max_other_queues_served_between_turns_sparse"`
}

// runSweepPoint is the single core function behind every experiment in
// this file -- the queue-count sweep, the capacity-configuration sweep,
// and every adversarial variant are all this same function under
// different SweepOptions, not divergent copies.
func runSweepPoint(ctx context.Context, db *sql.DB, opt SweepOptions) SweepResult {
	resetBenchSchema(ctx, db)

	replenish := opt.ReplenishInterval
	if replenish == 0 {
		replenish = 5 * time.Millisecond
	}

	queueNames := make([]string, opt.NumQueues)
	hot := map[string]bool{}
	for i := range queueNames {
		queueNames[i] = fmt.Sprintf("s%03d", i)
		if i < opt.HotQueues {
			hot[queueNames[i]] = true
		}
	}
	for _, q := range queueNames {
		seedQueue(ctx, db, q, opt.Capacity, opt.Capacity, 0)
	}

	runCtx, cancel := context.WithTimeout(ctx, opt.Duration)
	defer cancel()

	var offeredFlood, offeredSparse int64

	// Flood producer(s): unconditionally insert a batch every 2ms per hot
	// queue -- always offering far more than any configured capacity can
	// admit, the adversarial "one overwhelmingly hot queue" load shape
	// requirement 5 and this pass's queue-count sweep both use throughout.
	var prodWG sync.WaitGroup
	for q := range hot {
		q := q
		prodWG.Add(1)
		go func() {
			defer prodWG.Done()
			t := time.NewTicker(2 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-t.C:
					if _, err := db.ExecContext(ctx, `INSERT INTO bench_jobs (queue_name, state) SELECT $1,'QUEUED' FROM generate_series(1,5)`, q); err == nil {
						atomic.AddInt64(&offeredFlood, 5)
					}
				}
			}
		}()
	}

	// Sparse-queue replenishers: keep each sparse queue continuously
	// capacity-eligible (always >=1 QUEUED row) rather than arriving on a
	// fixed schedule that leaves eligibility gaps -- see file doc comment.
	var sparseInsertedAtMu sync.Mutex
	sparseInsertedAt := map[int64]time.Time{} // job id -> insertion time
	seenEligible := map[string]bool{}         // queue -> has ever had a pending row

	replenishOne := func(q string) {
		var id int64
		t0 := time.Now()
		if db.QueryRowContext(ctx, `INSERT INTO bench_jobs (queue_name, state) VALUES ($1,'QUEUED') RETURNING id`, q).Scan(&id) == nil {
			atomic.AddInt64(&offeredSparse, 1)
			sparseInsertedAtMu.Lock()
			sparseInsertedAt[id] = t0
			seenEligible[q] = true
			sparseInsertedAtMu.Unlock()
		}
	}

	for i, qn := range queueNames {
		if hot[qn] {
			continue
		}
		q := qn
		startDelay := time.Duration(0)
		if opt.LateEligibleQueue && i == opt.HotQueues {
			// The first sparse queue (by index) becomes eligible only
			// halfway through the run -- "queue becoming eligible
			// mid-run" (requirement 5).
			startDelay = opt.Duration / 2
		}
		prodWG.Add(1)
		go func() {
			defer prodWG.Done()
			if startDelay > 0 {
				select {
				case <-runCtx.Done():
					return
				case <-time.After(startDelay):
				}
			}
			replenishOne(q)
			t := time.NewTicker(replenish)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-t.C:
					var cnt int
					if db.QueryRowContext(ctx, `SELECT count(*) FROM bench_jobs WHERE queue_name=$1 AND state='QUEUED'`, q).Scan(&cnt) == nil && cnt == 0 {
						replenishOne(q)
					}
				}
			}
		}()
	}

	// Temporary capacity-ineligibility window on one sparse queue:
	// occupy all its slots with a sentinel holder from Duration/4 to
	// 3*Duration/4, then release them -- "queue becoming temporarily
	// capacity-ineligible and later eligible again" (requirement 5).
	// This does not touch concurrency_limit; it mirrors the realistic
	// case of a queue whose existing admitted jobs simply haven't
	// finished yet, not a configuration change.
	if opt.TempIneligibleWindow && len(queueNames) > opt.HotQueues {
		targetQueue := queueNames[opt.HotQueues]
		prodWG.Add(1)
		go func() {
			defer prodWG.Done()
			select {
			case <-runCtx.Done():
				return
			case <-time.After(opt.Duration / 4):
			}
			db.ExecContext(ctx, `UPDATE bench_queue_slots SET held_by_job_id = -1 WHERE queue_name=$1 AND held_by_job_id IS NULL`, targetQueue)
			select {
			case <-runCtx.Done():
				db.ExecContext(ctx, `UPDATE bench_queue_slots SET held_by_job_id = NULL WHERE queue_name=$1 AND held_by_job_id = -1`, targetQueue)
				return
			case <-time.After(opt.Duration / 2):
			}
			db.ExecContext(ctx, `UPDATE bench_queue_slots SET held_by_job_id = NULL WHERE queue_name=$1 AND held_by_job_id = -1`, targetQueue)
		}()
	}

	var totalClaims, floodClaims, sparseClaims, totalReclaims int64
	var limitViolations int32
	var claimEventsMu sync.Mutex
	claimEventsByQueue := map[string][]time.Time{}
	lastClaimAt := map[string]time.Time{}
	claimHistoryByJob := map[int64][]bool{} // job id -> []wasReclaim, in order
	var servedSequence []string             // every successful claim's queue name, in order -- the direct empirical trace behind the service-opportunity bound (see below)

	runWorker := func(id int) {
		owner := fmt.Sprintf("sweep-%s-%d", opt.Label, id)
		for {
			select {
			case <-runCtx.Done():
				return
			default:
			}
			out, queue, err := claimFairnessV2(runCtx, db, opt.Fair, queueNames, owner, true /* integrated */)
			if err != nil {
				continue
			}
			if !out.ok {
				time.Sleep(200 * time.Microsecond)
				continue
			}
			now := time.Now()
			atomic.AddInt64(&totalClaims, 1)
			if hot[queue] {
				atomic.AddInt64(&floodClaims, 1)
			} else {
				atomic.AddInt64(&sparseClaims, 1)
			}
			if out.wasReclaim {
				atomic.AddInt64(&totalReclaims, 1)
			}
			claimEventsMu.Lock()
			claimEventsByQueue[queue] = append(claimEventsByQueue[queue], now)
			lastClaimAt[queue] = now
			claimHistoryByJob[out.id] = append(claimHistoryByJob[out.id], out.wasReclaim)
			servedSequence = append(servedSequence, queue)
			claimEventsMu.Unlock()

			time.Sleep(3 * time.Millisecond)
			if out.id%10 != 0 {
				completeJob(ctx, db, CandSlot, queue, out.id)
			} // else: abandoned -- lease (300ms) expires, a later claim reclaims it.
		}
	}

	var wg sync.WaitGroup
	initialWorkers := opt.Workers
	if opt.DynamicWorkers {
		initialWorkers = opt.Workers / 2
		if initialWorkers < 1 {
			initialWorkers = 1
		}
	}
	for w := 0; w < initialWorkers; w++ {
		wg.Add(1)
		go func(id int) { defer wg.Done(); runWorker(id) }(w)
	}
	if opt.DynamicWorkers {
		remaining := opt.Workers - initialWorkers
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-runCtx.Done():
				return
			case <-time.After(opt.Duration / 2):
			}
			for w := 0; w < remaining; w++ {
				wg.Add(1)
				go func(id int) { defer wg.Done(); runWorker(id) }(initialWorkers + w)
			}
		}()
	}

	// Per-queue RUNNING-count sampler (exactness proof at this
	// configuration's scale).
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(3 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				rows, err := db.QueryContext(ctx, `SELECT queue_name, count(*) FROM bench_jobs WHERE state='RUNNING' GROUP BY queue_name`)
				if err != nil {
					continue
				}
				for rows.Next() {
					var q string
					var c int
					rows.Scan(&q, &c)
					if c > opt.Capacity {
						atomic.AddInt32(&limitViolations, 1)
					}
				}
				rows.Close()
			}
		}
	}()

	// No-progress sampler.
	var maxNoProgress int32
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		threshold := 10 * replenish
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				now := time.Now()
				claimEventsMu.Lock()
				count := 0
				sparseInsertedAtMu.Lock()
				for q := range seenEligible {
					last, ok := lastClaimAt[q]
					if !ok {
						last = now.Add(-time.Hour) // never claimed but eligible: definitely stalled
					}
					if now.Sub(last) > threshold {
						count++
					}
				}
				sparseInsertedAtMu.Unlock()
				claimEventsMu.Unlock()
				for {
					cur := atomic.LoadInt32(&maxNoProgress)
					if int32(count) <= cur || atomic.CompareAndSwapInt32(&maxNoProgress, cur, int32(count)) {
						break
					}
				}
			}
		}
	}()

	wg.Wait()
	prodWG.Wait()

	// Wait-time distribution: authoritative from the DB, sparse queues
	// only, using eligible_at (== insertion time for every row this
	// experiment creates) rather than the in-memory map, since a
	// replenished-and-claimed-then-replenished-again job's later
	// insertion is a distinct row/id.
	var waits []float64
	var sparseClaimedTotal int64
	rows, err := db.QueryContext(ctx, `SELECT queue_name, eligible_at, claimed_at FROM bench_jobs WHERE claimed_at IS NOT NULL`)
	if err == nil {
		for rows.Next() {
			var q string
			var eligibleAt, claimedAt time.Time
			if rows.Scan(&q, &eligibleAt, &claimedAt) != nil {
				continue
			}
			if hot[q] {
				continue
			}
			sparseClaimedTotal++
			waits = append(waits, claimedAt.Sub(eligibleAt).Seconds()*1000)
		}
		rows.Close()
	}
	sort.Float64s(waits)

	longestGap := 0.0
	zeroProgress := 0
	claimEventsMu.Lock()
	for _, q := range queueNames {
		if hot[q] {
			continue
		}
		times := claimEventsByQueue[q]
		if len(times) == 0 {
			zeroProgress++
			continue
		}
		if len(times) < 2 {
			continue
		}
		sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
		for i := 1; i < len(times); i++ {
			gap := times[i].Sub(times[i-1]).Seconds() * 1000
			if gap > longestGap {
				longestGap = gap
			}
		}
	}
	claimEventsMu.Unlock()

	// Reclaim correctness: for every job claimed more than once, the
	// first claim must be a fresh claim (wasReclaim=false) and every
	// claim after it must be a genuine reclaim (wasReclaim=true) -- a
	// job claimed "fresh" twice would mean the same concurrency unit was
	// double-admitted, which TF-INV-002's own mechanism (unmodified by
	// this pass) should make structurally impossible; this checks it
	// held under this experiment's own load, not just in isolation.
	reclaimFailures := 0
	for _, hist := range claimHistoryByJob {
		if len(hist) < 2 {
			continue
		}
		if hist[0] {
			reclaimFailures++ // first-ever claim of a job flagged as a reclaim: impossible unless corrupted
			continue
		}
		for i := 1; i < len(hist); i++ {
			if !hist[i] {
				reclaimFailures++ // a second+ claim of the same job NOT flagged as a reclaim: double fresh admission
			}
		}
	}

	// Direct empirical trace of the service-opportunity bound: for every
	// queue, and every gap between its consecutive turns (including from
	// run start to its first turn), count how many DISTINCT other queues
	// were served in that gap. last_claimed_at's own ordering rule
	// predicts this can never exceed (number of simultaneously eligible
	// queues - 1): once every other eligible queue has been served once
	// since this queue's last turn, every one of them now has a newer
	// last_claimed_at than this queue's (still-unchanged, older) one, so
	// this queue must be selected next. This is the direct test of that
	// claim, not an inference from wall-clock wait times alone.
	maxOtherBetween, maxOtherBetweenSparse := 0, 0
	positionsByQueue := map[string][]int{}
	for i, q := range servedSequence {
		positionsByQueue[q] = append(positionsByQueue[q], i)
	}
	for q, positions := range positionsByQueue {
		prev := -1
		for _, pos := range positions {
			distinct := map[string]bool{}
			for j := prev + 1; j < pos; j++ {
				if servedSequence[j] != q {
					distinct[servedSequence[j]] = true
				}
			}
			if len(distinct) > maxOtherBetween {
				maxOtherBetween = len(distinct)
			}
			if !hot[q] && len(distinct) > maxOtherBetweenSparse {
				maxOtherBetweenSparse = len(distinct)
			}
			prev = pos
		}
	}

	tc := atomic.LoadInt64(&totalClaims)
	return SweepResult{
		Label: opt.Label, NumQueues: opt.NumQueues, HotQueues: opt.HotQueues, Capacity: opt.Capacity, Workers: opt.Workers,
		DurationSec:         opt.Duration.Seconds(),
		OfferedFloodInserts: atomic.LoadInt64(&offeredFlood), OfferedSparseInserts: atomic.LoadInt64(&offeredSparse),
		FloodClaims: atomic.LoadInt64(&floodClaims), SparseClaims: atomic.LoadInt64(&sparseClaims), TotalClaims: tc,
		ThroughputPerSec: float64(tc) / opt.Duration.Seconds(),
		MaxWaitMs:        percentile(waits, 100), P50WaitMs: percentile(waits, 50), P95WaitMs: percentile(waits, 95), P99WaitMs: percentile(waits, 99),
		LongestGapMs: longestGap, ZeroProgressQueues: zeroProgress, MaxNoProgressQueuesObserved: int(atomic.LoadInt32(&maxNoProgress)),
		LimitViolations: int(atomic.LoadInt32(&limitViolations)), ReclaimCount: atomic.LoadInt64(&totalReclaims), ReclaimCorrectnessFailures: reclaimFailures,
		MaxOtherQueuesServedBetweenTurns: maxOtherBetween, MaxOtherQueuesServedBetweenTurnsSparse: maxOtherBetweenSparse,
	}
}
