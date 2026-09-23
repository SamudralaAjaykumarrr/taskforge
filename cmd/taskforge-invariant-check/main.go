// Command taskforge-invariant-check is Phase 15's operator-facing
// invariant-verification tool (docs/phase-15-plan.md §18/§27 OD-5,
// docs/disaster-recovery.md §3.2/§8): a thin wrapper around the existing
// internal/invariant.Checker.CheckAll -- which had no CLI entry point at
// all before this phase, only test-internal callers -- so an operator can
// run the mandatory post-restore/post-promotion verification step
// (docs/disaster-recovery.md §3.2) as a single command against a real
// database, rather than needing to write throwaway Go code.
//
// This is not a new production surface: it performs no writes, takes no
// action on a violation beyond reporting it, and is not started by
// cmd/api or cmd/worker. It exists solely so docs/disaster-recovery.md's
// own runbook can name a real, runnable command instead of an
// aspirational one -- OD-5 explicitly anticipated this exact, small,
// non-blocking addition if a "go run" invocation against the library
// package directly proved awkward, which it does (internal/invariant is
// a library package, not a main package, so nothing can "go run" it
// without a wrapper).
//
// Exit code 0: connected, ran every check, found zero violations.
// Exit code 1: connected, ran every check, found one or more violations
// (printed, one per line).
// Exit code 2: could not connect or a check itself failed to run (a
// distinct outcome from "ran cleanly and found violations" -- callers
// must not treat a connection failure as "no violations found").
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/buildinfo"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/invariant"
)

func main() {
	dbURL := flag.String("db-url", os.Getenv("TASKFORGE_DATABASE_URL"), "PostgreSQL connection string (default: TASKFORGE_DATABASE_URL)")
	timeout := flag.Duration("timeout", 60*time.Second, "overall timeout for connecting and running every check")
	versionFlag := flag.Bool("version", false, "print version information and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Println("taskforge-invariant-check " + buildinfo.String())
		return
	}

	os.Exit(run(*dbURL, *timeout))
}

func run(dbURL string, timeout time.Duration) int {
	if dbURL == "" {
		fmt.Fprintln(os.Stderr, "taskforge-invariant-check: -db-url or TASKFORGE_DATABASE_URL is required")
		return 2
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "taskforge-invariant-check: open database: %v\n", err)
		return 2
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "taskforge-invariant-check: ping database: %v\n", err)
		return 2
	}

	violations, err := invariant.New(db).CheckAll(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "taskforge-invariant-check: checker failed: %v\n", err)
		return 2
	}

	if len(violations) == 0 {
		fmt.Println("taskforge-invariant-check: OK -- zero violations across every TF-INV-* check")
		return 0
	}

	fmt.Printf("taskforge-invariant-check: FAILED -- %d violation(s) found:\n", len(violations))
	for _, v := range violations {
		fmt.Println(" " + v.String())
	}
	return 1
}
