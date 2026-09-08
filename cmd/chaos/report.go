package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
)

// report accumulates everything one campaign iteration observed, per
// docs/roadmap.md's Load Testing scope ("total jobs, completed outcomes,
// failure/dead-letter outcomes, elapsed duration, throughput ...
// invariant failures").
type report struct {
	seed      int64
	iteration int
	startedAt time.Time
	elapsed   time.Duration

	mu          sync.Mutex
	submitted   []uuid.UUID
	errors      []error
	succeeded   int64
	retried     int64
	deadLetters int64
	cancelled   int64
	timedOut    int64
	crashed     int64
	violations  []invariant.Violation
}

func newReport(seed int64, iteration int) *report {
	return &report{seed: seed, iteration: iteration, startedAt: time.Now()}
}

func (r *report) recordSubmitted(id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.submitted = append(r.submitted, id)
}

func (r *report) recordError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, err)
}

func (r *report) recordOutcome(o outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch o {
	case outcomeSucceeded:
		r.succeeded++
	case outcomeRetried:
		r.retried++
	case outcomeDeadLettered:
		r.deadLetters++
	case outcomeCancelled:
		r.cancelled++
	case outcomeTimedOut:
		r.timedOut++
	case outcomeCrashed:
		r.crashed++
	}
}

// checkAndReport runs the full durable invariant suite and folds any
// violations into report, printing each one immediately (per
// docs/roadmap.md: "print invariant ID, seed, relevant job/workflow IDs
// ... durable states needed for reproduction" -- Violation.String()
// already carries the invariant ID and subject; the seed is printed
// alongside here so a failure's reproduction command is never separated
// from the seed that produced it).
func checkAndReport(ctx context.Context, checker *invariant.Checker, r *report, phase string) []invariant.Violation {
	v, err := checker.CheckAll(ctx)
	if err != nil {
		r.recordError(fmt.Errorf("invariant check (%s) failed to run: %w", phase, err))
		return nil
	}
	if len(v) == 0 {
		return nil
	}
	r.mu.Lock()
	r.violations = append(r.violations, v...)
	r.mu.Unlock()
	fmt.Printf("!!! INVARIANT VIOLATION during %s phase (seed=%d, iteration=%d):\n", phase, r.seed, r.iteration)
	for _, vio := range v {
		fmt.Printf("    %s\n", vio.String())
	}
	return v
}

func (r *report) print() {
	r.mu.Lock()
	defer r.mu.Unlock()

	elapsed := r.elapsed
	if elapsed == 0 {
		elapsed = time.Since(r.startedAt)
	}
	throughput := float64(0)
	total := r.succeeded + r.deadLetters + r.cancelled
	if elapsed > 0 {
		throughput = float64(total) / elapsed.Seconds()
	}

	fmt.Printf("\n--- campaign report (seed=%d, iteration=%d) ---\n", r.seed, r.iteration)
	fmt.Printf("submitted:            %d\n", len(r.submitted))
	fmt.Printf("succeeded:            %d\n", r.succeeded)
	fmt.Printf("dead-lettered:        %d\n", r.deadLetters)
	fmt.Printf("cancelled:            %d\n", r.cancelled)
	fmt.Printf("retryable outcomes:   %d (intermediate -- not a final state)\n", r.retried)
	fmt.Printf("timed-out outcomes:   %d (intermediate/final via DLQ path)\n", r.timedOut)
	fmt.Printf("simulated crashes:    %d (abandoned; recovered by a later claim/reclaim)\n", r.crashed)
	fmt.Printf("terminal total:       %d\n", total)
	fmt.Printf("elapsed:              %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("throughput:           %.1f terminal outcomes/sec\n", throughput)
	fmt.Printf("harness errors:       %d\n", len(r.errors))
	for _, e := range r.errors {
		fmt.Printf("    %v\n", e)
	}
	fmt.Printf("invariant violations: %d\n", len(r.violations))
	if len(r.violations) > 0 {
		fmt.Println("REPRODUCE with the same -seed, -jobs, -workflows, -idem-groups, -workers flags printed at the top of this run.")
	}
}
