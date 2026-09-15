package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func percentile(sorted []float64, pct float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(pct/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// ---------------------------------------------------------------------
// 1. Concurrency-limit exactness (now including the reclaim branch)
// ---------------------------------------------------------------------

func runExactness(ctx context.Context, db *sql.DB, cand CandidateID, d runDurations) ExactnessResult {
	resetBenchSchema(ctx, db)
	const queue = "hot"
	const limit = 5
	const workers = 30
	seedQueue(ctx, db, queue, limit, limit, 3000)
	// Reclaim is exercised naturally, in-run, not by pre-seeding rows
	// already RUNNING (an earlier draft of this experiment did that and
	// created an invalid starting state -- more RUNNING rows than the
	// limit allows before any mechanism had a chance to enforce anything,
	// which is not a state a correctly-operating system ever reaches; see
	// docs/phase-13-concurrency-evidence.md's "corrected methodology"
	// section). Instead, a fraction of legitimately-claimed jobs below are
	// deterministically abandoned (their lease force-expired via
	// PostgreSQL's own clock, the same technique
	// internal/store/concurrency_stress_test.go already uses) rather than
	// completed, so later claim attempts genuinely reclaim them.

	claim := claimFor(cand)
	var totalClaims, totalReclaims, attemptedTxns, serFail, exhausted int64
	var maxRunning int32
	var violations int32
	var samples int64

	runCtx, cancel := context.WithTimeout(ctx, d.exactness)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(1 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				var running int
				if db.QueryRowContext(ctx, `SELECT count(*) FROM bench_jobs WHERE queue_name=$1 AND state='RUNNING'`, queue).Scan(&running) != nil {
					continue
				}
				atomic.AddInt64(&samples, 1)
				for {
					cur := atomic.LoadInt32(&maxRunning)
					if int32(running) <= cur || atomic.CompareAndSwapInt32(&maxRunning, cur, int32(running)) {
						break
					}
				}
				if running > limit {
					atomic.AddInt32(&violations, 1)
				}
			}
		}
	}()

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			owner := fmt.Sprintf("exactness-%s-%d", cand, id)
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				out, err := claim(runCtx, db, queue, owner)
				atomic.AddInt64(&attemptedTxns, out.attemptedTxns)
				atomic.AddInt64(&serFail, out.serFailures)
				if out.exhausted {
					atomic.AddInt64(&exhausted, 1)
				}
				if err != nil {
					if runCtx.Err() != nil {
						return
					}
					// Retry exhaustion under SERIALIZABLE is an expected,
					// already-counted outcome (RetryExhausted), not logged
					// per-occurrence to avoid drowning genuinely unexpected
					// errors in noise.
					continue
				}
				if !out.ok {
					time.Sleep(200 * time.Microsecond)
					continue
				}
				atomic.AddInt64(&totalClaims, 1)
				if out.wasReclaim {
					atomic.AddInt64(&totalReclaims, 1)
				}
				time.Sleep(15 * time.Millisecond)
				if out.id%6 == 0 {
					// Deterministic abandonment (~1/6 of claims): force the
					// lease into the past instead of completing, so a later
					// claim attempt reclaims this exact row. No sleep-based
					// timing dependency -- the row becomes reclaim-eligible
					// the instant this commits.
					if _, aerr := db.ExecContext(ctx, `UPDATE bench_jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, out.id); aerr != nil {
						log.Printf("exactness: unexpected abandon-write error for job %d: %v", out.id, aerr)
					}
				} else {
					if cerr := completeJob(ctx, db, cand, queue, out.id); cerr != nil {
						log.Printf("exactness: unexpected completion error for job %d: %v", out.id, cerr)
					}
				}
			}
		}(w)
	}
	wg.Wait()

	return ExactnessResult{
		Candidate: cand, Limit: limit, Workers: workers, DurationSec: d.exactness.Seconds(),
		TotalClaims: atomic.LoadInt64(&totalClaims), TotalReclaims: atomic.LoadInt64(&totalReclaims),
		MaxObservedRunning: int(atomic.LoadInt32(&maxRunning)), LimitViolations: int(atomic.LoadInt32(&violations)),
		AttemptedTxns: atomic.LoadInt64(&attemptedTxns), SerializationFailures: atomic.LoadInt64(&serFail),
		Retries: atomic.LoadInt64(&attemptedTxns) - atomic.LoadInt64(&totalClaims), RetryExhausted: atomic.LoadInt64(&exhausted),
		SamplesTaken: atomic.LoadInt64(&samples),
	}
}

// ---------------------------------------------------------------------
// 2. Throughput & latency vs worker count
// ---------------------------------------------------------------------

func runThroughput(ctx context.Context, db *sql.DB, cand CandidateID, workers int, d runDurations) ThroughputResult {
	resetBenchSchema(ctx, db)
	const queue = "hot"
	const limit = 20
	seedQueue(ctx, db, queue, limit, limit, 300000)

	claim := claimFor(cand)
	var totalClaims, attemptedTxns, serFail, exhausted int64
	var latMu sync.Mutex
	var latencies []float64

	runCtx, cancel := context.WithTimeout(ctx, d.throughput)
	defer cancel()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			owner := fmt.Sprintf("thr-%s-%d", cand, id)
			var local []float64
			for {
				select {
				case <-runCtx.Done():
					latMu.Lock()
					latencies = append(latencies, local...)
					latMu.Unlock()
					return
				default:
				}
				start := time.Now()
				out, err := claim(runCtx, db, queue, owner)
				elapsed := time.Since(start)
				atomic.AddInt64(&attemptedTxns, out.attemptedTxns)
				atomic.AddInt64(&serFail, out.serFailures)
				if out.exhausted {
					atomic.AddInt64(&exhausted, 1)
				}
				if err != nil {
					continue
				}
				if !out.ok {
					continue
				}
				local = append(local, float64(elapsed.Microseconds())/1000.0)
				atomic.AddInt64(&totalClaims, 1)
				time.Sleep(5 * time.Millisecond)
				_ = completeJob(ctx, db, cand, queue, out.id)
			}
		}(w)
	}
	wg.Wait()

	sort.Float64s(latencies)
	tc := atomic.LoadInt64(&totalClaims)

	return ThroughputResult{
		Candidate: cand, Workers: workers, DurationSec: d.throughput.Seconds(),
		TotalClaims: tc, ClaimsPerSec: float64(tc) / d.throughput.Seconds(),
		P50Ms: percentile(latencies, 50), P95Ms: percentile(latencies, 95), P99Ms: percentile(latencies, 99), MaxMs: percentile(latencies, 100),
		AttemptedTxns: atomic.LoadInt64(&attemptedTxns), SerializationFailures: atomic.LoadInt64(&serFail),
		Retries: atomic.LoadInt64(&attemptedTxns) - tc, RetryExhausted: atomic.LoadInt64(&exhausted),
	}
}

// ---------------------------------------------------------------------
// 3. Lock/contention characterization
// ---------------------------------------------------------------------

func runLockContention(ctx context.Context, db *sql.DB, cand CandidateID, workers int, d runDurations) LockContentionResult {
	resetBenchSchema(ctx, db)
	const queue = "hot"
	const limit = 10
	seedQueue(ctx, db, queue, limit, limit, 300000)

	claim := claimFor(cand)
	runCtx, cancel := context.WithTimeout(ctx, d.lockContention)
	defer cancel()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			owner := fmt.Sprintf("lock-%s-%d", cand, id)
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				out, err := claim(runCtx, db, queue, owner)
				if err != nil || !out.ok {
					continue
				}
				time.Sleep(5 * time.Millisecond)
				_ = completeJob(ctx, db, cand, queue, out.id)
			}
		}(w)
	}

	var samples int
	var sumWaiting float64
	var maxWaiting int
	var sumActive float64
	lockTypeCounts := map[string]int{}

	sampleTicker := time.NewTicker(50 * time.Millisecond)
	defer sampleTicker.Stop()
loop:
	for {
		select {
		case <-runCtx.Done():
			break loop
		case <-sampleTicker.C:
			rows, err := db.QueryContext(ctx, `SELECT locktype, count(*) FROM pg_locks WHERE NOT granted GROUP BY locktype`)
			if err != nil {
				continue
			}
			waiting := 0
			for rows.Next() {
				var lt string
				var cnt int
				if rows.Scan(&lt, &cnt) == nil {
					lockTypeCounts[lt] += cnt
					waiting += cnt
				}
			}
			rows.Close()
			var active int
			_ = db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state='active'`).Scan(&active)
			samples++
			sumWaiting += float64(waiting)
			sumActive += float64(active)
			if waiting > maxWaiting {
				maxWaiting = waiting
			}
		}
	}
	wg.Wait()

	avgWaiting, avgActive := 0.0, 0.0
	if samples > 0 {
		avgWaiting, avgActive = sumWaiting/float64(samples), sumActive/float64(samples)
	}
	return LockContentionResult{Candidate: cand, Workers: workers, Samples: samples, AvgWaitingLocks: avgWaiting, MaxWaitingLocks: maxWaiting, AvgActiveConns: avgActive, LockTypeCounts: lockTypeCounts}
}

// ---------------------------------------------------------------------
// 4. Production-baseline fidelity
// ---------------------------------------------------------------------

func runProdBaseline(ctx context.Context, db *sql.DB, d runDurations) ProdBaselineResult {
	resetProdSchema(ctx, db)
	const flood, trickle = "flood", "trickle"

	runCtx, cancel := context.WithTimeout(ctx, d.fairness)
	defer cancel()

	var floodProdWG sync.WaitGroup
	floodProdWG.Add(1)
	go func() {
		defer floodProdWG.Done()
		t := time.NewTicker(2 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				db.ExecContext(ctx, `INSERT INTO prod_jobs (bench_logical_queue, state) SELECT $1, 'QUEUED' FROM generate_series(1,5)`, flood)
			}
		}
	}()

	type ins struct {
		id int64
		at time.Time
	}
	trickleCh := make(chan ins, 2000)
	var trickleProdWG sync.WaitGroup
	trickleProdWG.Add(1)
	go func() {
		defer trickleProdWG.Done()
		t := time.NewTicker(300 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				var id int64
				t0 := time.Now()
				if db.QueryRowContext(ctx, `INSERT INTO prod_jobs (bench_logical_queue, state) VALUES ($1,'QUEUED') RETURNING id`, trickle).Scan(&id) == nil {
					trickleCh <- ins{id, t0}
				}
			}
		}
	}()

	var floodClaimed, trickleClaimed int64
	var claimTimesMu sync.Mutex
	claimTimes := map[int64]time.Time{}

	var wg sync.WaitGroup
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			owner := fmt.Sprintf("prodbaseline-%d", id)
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				jid, queue, _, err := claimProdBaseline(runCtx, db, owner)
				if err != nil || queue == "" {
					continue
				}
				now := time.Now()
				if queue == flood {
					atomic.AddInt64(&floodClaimed, 1)
				} else {
					atomic.AddInt64(&trickleClaimed, 1)
					claimTimesMu.Lock()
					claimTimes[jid] = now
					claimTimesMu.Unlock()
				}
				db.ExecContext(ctx, `UPDATE prod_jobs SET state='SUCCEEDED' WHERE id=$1`, jid)
			}
		}(w)
	}

	wg.Wait()
	floodProdWG.Wait()
	trickleProdWG.Wait()
	close(trickleCh)

	var waits []float64
	var total int64
	claimTimesMu.Lock()
	for tj := range trickleCh {
		total++
		if t, ok := claimTimes[tj.id]; ok {
			waits = append(waits, t.Sub(tj.at).Seconds()*1000)
		}
	}
	claimTimesMu.Unlock()
	sort.Float64s(waits)

	return ProdBaselineResult{
		DurationSec: d.fairness.Seconds(), FloodClaimed: atomic.LoadInt64(&floodClaimed),
		TrickleClaimed: atomic.LoadInt64(&trickleClaimed), TrickleTotal: total,
		P50WaitMs: percentile(waits, 50), P95WaitMs: percentile(waits, 95), MaxWaitMs: percentile(waits, 100),
	}
}

// ---------------------------------------------------------------------
// 5/6. Corrected fairness at multiple queue counts, plain and integrated
// (slot-table concurrency + fairness together, with reclaim active)
// ---------------------------------------------------------------------

func runFairnessMultiQueue(ctx context.Context, db *sql.DB, fair FairnessID, shape QueueShape, integrated bool, d runDurations) FairnessResult {
	resetBenchSchema(ctx, db)

	queueNames := make([]string, shape.NumQueues)
	hot := map[string]bool{}
	for i := range queueNames {
		queueNames[i] = fmt.Sprintf("q%03d", i)
		if i < shape.HotQueues {
			hot[queueNames[i]] = true
		}
	}
	limit, slots := 1_000_000, 0
	if integrated {
		limit, slots = 3, 3
	}
	for _, q := range queueNames {
		seedQueue(ctx, db, q, limit, slots, 0)
	}

	workers := 10
	if shape.NumQueues > 10 {
		workers = 30
	}

	runCtx, cancel := context.WithTimeout(ctx, d.fairness)
	defer cancel()

	// Producers.
	var prodWG sync.WaitGroup
	type insertRec struct {
		id int64
		at time.Time
	}
	sparseInsertsByQueue := make(map[string]chan insertRec, len(queueNames))
	for _, q := range queueNames {
		if !hot[q] {
			sparseInsertsByQueue[q] = make(chan insertRec, 2000)
		}
	}
	for _, qn := range queueNames {
		q := qn
		prodWG.Add(1)
		go func() {
			defer prodWG.Done()
			rate := shape.SparseRate
			if hot[q] {
				rate = shape.FloodRate
			}
			t := time.NewTicker(rate)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-t.C:
					if hot[q] {
						db.ExecContext(ctx, `INSERT INTO bench_jobs (queue_name, state) SELECT $1,'QUEUED' FROM generate_series(1,5)`, q)
					} else {
						var id int64
						t0 := time.Now()
						if db.QueryRowContext(ctx, `INSERT INTO bench_jobs (queue_name, state) VALUES ($1,'QUEUED') RETURNING id`, q).Scan(&id) == nil {
							sparseInsertsByQueue[q] <- insertRec{id, t0}
						}
					}
				}
			}
		}()
	}

	var totalClaims, totalReclaims int64
	var limitViolations int32
	var claimEventsMu sync.Mutex
	claimEventsByQueue := map[string][]time.Time{}
	lastClaimAt := map[string]time.Time{}
	seenInsertByQueue := map[string]bool{}
	runStart := time.Now()
	for _, q := range queueNames {
		if !hot[q] {
			lastClaimAt[q] = runStart
		}
	}

	// Drain sparseInsertsByQueue channels concurrently so seenInsertByQueue
	// stays current for the no-progress sampler (best-effort; only used to
	// avoid flagging a queue "stalled" before it has ever had any work).
	for q, ch := range sparseInsertsByQueue {
		q := q
		ch := ch
		go func() {
			for {
				select {
				case <-runCtx.Done():
					return
				case _, ok := <-ch:
					if !ok {
						return
					}
					claimEventsMu.Lock()
					seenInsertByQueue[q] = true
					claimEventsMu.Unlock()
				}
			}
		}()
	}

	// Workers.
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			owner := fmt.Sprintf("fair-%s-%s-%d", fair, shape.Name, id)
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				out, queue, err := claimFairnessV2(runCtx, db, fair, queueNames, owner, integrated)
				if err != nil {
					continue
				}
				if !out.ok {
					time.Sleep(200 * time.Microsecond)
					continue
				}
				now := time.Now()
				atomic.AddInt64(&totalClaims, 1)
				if out.wasReclaim {
					atomic.AddInt64(&totalReclaims, 1)
				}
				claimEventsMu.Lock()
				claimEventsByQueue[queue] = append(claimEventsByQueue[queue], now)
				lastClaimAt[queue] = now
				claimEventsMu.Unlock()

				if integrated {
					// Simulate real execution + a natural mix of normal
					// completion and abandonment (crash), so lease
					// expiry/reclaim is continuously, genuinely exercised
					// -- not simulated by force-editing timestamps.
					time.Sleep(3 * time.Millisecond)
					if (out.id % 10) != 0 { // ~90% complete normally
						completeJob(ctx, db, CandSlot, queue, out.id)
					} // else: abandoned: lease (300ms) expires, next winner reclaims it.
				} else {
					completeJob(ctx, db, CandSlot /* no slot table touched for non-integrated */, queue, out.id)
				}
			}
		}(w)
	}

	// Per-queue RUNNING-count sampler (integrated exactness proof at scale).
	if integrated {
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
						if c > limit {
							atomic.AddInt32(&limitViolations, 1)
						}
					}
					rows.Close()
				}
			}
		}()
	}

	// No-progress sampler (TF-INV-019 evidence): every 50ms, count how many
	// sparse queues that have already had at least one insert have gone
	// longer than 5x their own producer interval since their last
	// successful claim.
	var maxNoProgress int32
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		threshold := 5 * shape.SparseRate
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				now := time.Now()
				claimEventsMu.Lock()
				count := 0
				for q := range lastClaimAt {
					if !seenInsertByQueue[q] {
						continue
					}
					if now.Sub(lastClaimAt[q]) > threshold {
						count++
					}
				}
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
	for _, ch := range sparseInsertsByQueue {
		close(ch)
	}

	// Compute wait-time distribution across every sparse-queue job actually
	// claimed, read back authoritatively from the database rather than
	// reconciled from the per-queue producer channels above. bench_jobs
	// has no separate "inserted_at" column distinct from eligible_at, and
	// eligible_at defaults to now() at INSERT time for every row this
	// experiment creates, so eligible_at IS the insertion timestamp here.
	var waits []float64
	var sparseTotal int64
	rows2, err := db.QueryContext(ctx, `
		SELECT queue_name, eligible_at, claimed_at FROM bench_jobs
		WHERE claimed_at IS NOT NULL`)
	if err == nil {
		for rows2.Next() {
			var q string
			var eligibleAt, claimedAt time.Time
			if rows2.Scan(&q, &eligibleAt, &claimedAt) != nil {
				continue
			}
			if hot[q] {
				continue
			}
			sparseTotal++
			waits = append(waits, claimedAt.Sub(eligibleAt).Seconds()*1000)
		}
		rows2.Close()
	}
	sort.Float64s(waits)

	// Longest consecutive progress gap: the largest interval between two
	// successive claims from the same sparse queue (captures "went silent
	// for a long stretch," distinct from any single job's own wait time).
	longestGap := 0.0
	claimEventsMu.Lock()
	for q, times := range claimEventsByQueue {
		if hot[q] || len(times) < 2 {
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

	return FairnessResult{
		Fairness: fair, Shape: shape.Name, NumQueues: shape.NumQueues, Integrated: integrated,
		DurationSec: d.fairness.Seconds(), TotalClaims: atomic.LoadInt64(&totalClaims), TotalReclaims: atomic.LoadInt64(&totalReclaims),
		LimitViolations: int(atomic.LoadInt32(&limitViolations)), SparseClaimed: sparseTotal, SparseTotal: sparseTotal,
		P50WaitMs: percentile(waits, 50), P95WaitMs: percentile(waits, 95), P99WaitMs: percentile(waits, 99), MaxWaitMs: percentile(waits, 100),
		MaxNoProgressQueuesObserved: int(atomic.LoadInt32(&maxNoProgress)), LongestGapMs: longestGap,
	}
}

// ---------------------------------------------------------------------
// 7. Isolation, repeated
// ---------------------------------------------------------------------

func measureQueueThroughput(ctx context.Context, db *sql.DB, cand CandidateID, queue string, workers int, dur time.Duration) float64 {
	claim := claimFor(cand)
	runCtx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	var claims int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			owner := fmt.Sprintf("measure-%s-%d", cand, id)
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				out, err := claim(runCtx, db, queue, owner)
				if err != nil || !out.ok {
					continue
				}
				atomic.AddInt64(&claims, 1)
				time.Sleep(5 * time.Millisecond)
				_ = completeJob(ctx, db, cand, queue, out.id)
			}
		}(w)
	}
	wg.Wait()
	return float64(atomic.LoadInt64(&claims)) / dur.Seconds()
}

func runIsolation(ctx context.Context, db *sql.DB, cand CandidateID, d runDurations) IsolationResult {
	const lowQueue, hotQueue = "tenant-low", "tenant-hot"
	const lowLimit, hotLimit, workers = 5, 20, 15

	resetBenchSchema(ctx, db)
	seedQueue(ctx, db, lowQueue, lowLimit, lowLimit, 200000)
	alone := measureQueueThroughput(ctx, db, cand, lowQueue, workers, d.isolation)

	resetBenchSchema(ctx, db)
	seedQueue(ctx, db, lowQueue, lowLimit, lowLimit, 200000)
	seedQueue(ctx, db, hotQueue, hotLimit, hotLimit, 200000)
	runCtx, cancel := context.WithTimeout(ctx, d.isolation)
	defer cancel()
	claim := claimFor(cand)
	var hotWG sync.WaitGroup
	for w := 0; w < workers; w++ {
		hotWG.Add(1)
		go func(id int) {
			defer hotWG.Done()
			owner := fmt.Sprintf("iso-hot-%s-%d", cand, id)
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				out, err := claim(runCtx, db, hotQueue, owner)
				if err != nil || !out.ok {
					continue
				}
				time.Sleep(5 * time.Millisecond)
				_ = completeJob(ctx, db, cand, hotQueue, out.id)
			}
		}(w)
	}
	var lowClaims int64
	var lowWG sync.WaitGroup
	for w := 0; w < workers; w++ {
		lowWG.Add(1)
		go func(id int) {
			defer lowWG.Done()
			owner := fmt.Sprintf("iso-low-%s-%d", cand, id)
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				out, err := claim(runCtx, db, lowQueue, owner)
				if err != nil || !out.ok {
					continue
				}
				atomic.AddInt64(&lowClaims, 1)
				time.Sleep(5 * time.Millisecond)
				_ = completeJob(ctx, db, cand, lowQueue, out.id)
			}
		}(w)
	}
	lowWG.Wait()
	hotWG.Wait()
	underFlood := float64(atomic.LoadInt64(&lowClaims)) / d.isolation.Seconds()

	degradation := 0.0
	if alone > 0 {
		degradation = (alone - underFlood) / alone * 100
	}
	return IsolationResult{Candidate: cand, LowVolumeAloneThroughput: alone, LowVolumeUnderFloodThroughput: underFlood, DegradationPct: degradation}
}

// ---------------------------------------------------------------------
// 8. Correctness checks, re-verified against v2 schema
// ---------------------------------------------------------------------

func runCorrectnessChecks(ctx context.Context, db *sql.DB) []CorrectnessCheck {
	var checks []CorrectnessCheck

	resetBenchSchema(ctx, db)
	seedQueue(ctx, db, "qa", 100, 0, 0)
	seedQueue(ctx, db, "qb", 100, 0, 0)
	db.ExecContext(ctx, `INSERT INTO bench_jobs (queue_name, state) VALUES ('qb','QUEUED')`)
	{
		out, _, err := claimFairnessV2(ctx, db, FairRoundRobinV2, []string{"qa"}, "subscribed-a-only", false)
		pass := err == nil && !out.ok
		detail := "worker subscribed to {qa} correctly found nothing claimable while only qb had eligible work"
		if !pass {
			detail = fmt.Sprintf("unexpected claim (ok=%v err=%v)", out.ok, err)
		}
		checks = append(checks, CorrectnessCheck{"subscription_boundary_no_cross_queue_claim", pass, detail})
	}

	resetBenchSchema(ctx, db)
	seedQueue(ctx, db, "qa", 100, 0, 0)
	seedQueue(ctx, db, "qb", 100, 0, 0)
	db.ExecContext(ctx, `INSERT INTO bench_jobs (queue_name, state) VALUES ('qb','QUEUED')`)
	{
		out, queue, err := claimFairnessV2(ctx, db, FairRoundRobinV2, []string{"qa", "qb"}, "unset-subscription", false)
		pass := err == nil && out.ok && queue == "qb"
		detail := "worker with all-queues subscription claimed qb's job as expected"
		if !pass {
			detail = fmt.Sprintf("expected claim from qb, got ok=%v queue=%q err=%v", out.ok, queue, err)
		}
		checks = append(checks, CorrectnessCheck{"unset_subscription_claims_any_queue", pass, detail})
	}

	resetBenchSchema(ctx, db)
	var seededID int64
	err := db.QueryRowContext(ctx, `INSERT INTO bench_jobs (queue_name, state, lease_expires_at, attempt_count) VALUES ('qb','RUNNING', now()-interval '30 seconds', 1) RETURNING id`).Scan(&seededID)
	if err != nil {
		checks = append(checks, CorrectnessCheck{"reclaim_respects_subscription", false, "seed failed: " + err.Error()})
	} else {
		out, _, rerr := claimFairnessV2(ctx, db, FairRoundRobinV2, []string{"qa"}, "sub-a-only", false)
		pass := rerr == nil && !out.ok
		detail := "worker subscribed to {qa} correctly could not reclaim qb's expired lease"
		if !pass {
			detail = fmt.Sprintf("worker subscribed to {qa} reclaimed something (ok=%v err=%v) -- §6b violated", out.ok, rerr)
		}
		checks = append(checks, CorrectnessCheck{"reclaim_respects_subscription_boundary", pass, detail})

		out2, queue2, rerr2 := claimFairnessV2(ctx, db, FairRoundRobinV2, []string{"qa", "qb"}, "sub-both", false)
		pass2 := rerr2 == nil && out2.ok && queue2 == "qb" && out2.id == seededID && out2.wasReclaim
		detail2 := "worker subscribed to {qa,qb} correctly reclaimed qb's expired lease (TF-INV-004 liveness preserved)"
		if !pass2 {
			detail2 = fmt.Sprintf("expected reclaim of qb's job id=%d, got id=%d queue=%q wasReclaim=%v err=%v", seededID, out2.id, queue2, out2.wasReclaim, rerr2)
		}
		checks = append(checks, CorrectnessCheck{"reclaim_succeeds_for_subscribed_worker", pass2, detail2})
	}

	// New for v2: production-baseline query also respects the same
	// ordering-only, queue-blind semantics (no subscription concept exists
	// in production today, so this check instead confirms the reclaim
	// branch fires correctly against prod_jobs's own real schema).
	resetProdSchema(ctx, db)
	var prodSeededID int64
	perr := db.QueryRowContext(ctx, `INSERT INTO prod_jobs (bench_logical_queue, state, lease_expires_at, attempt_count) VALUES ('x','RUNNING', now()-interval '1 second', 1) RETURNING id`).Scan(&prodSeededID)
	if perr != nil {
		checks = append(checks, CorrectnessCheck{"prod_baseline_reclaim_branch_fires", false, "seed failed: " + perr.Error()})
	} else {
		id, _, wasReclaim, cerr := claimProdBaseline(ctx, db, "prod-reclaimer")
		pass := cerr == nil && id == prodSeededID && wasReclaim
		detail := "production-shape claimQuery correctly reclaimed the expired-lease row via its second WHERE branch"
		if !pass {
			detail = fmt.Sprintf("expected reclaim of id=%d, got id=%d wasReclaim=%v err=%v", prodSeededID, id, wasReclaim, cerr)
		}
		checks = append(checks, CorrectnessCheck{"prod_baseline_reclaim_branch_fires", pass, detail})
	}

	return checks
}
