// Command taskforge-admin is Phase 12's operator-facing key-management
// tool (docs/phase-12-plan.md §13). It creates principals, mints and
// revokes API keys, and lists what exists.
//
// It is deliberately a separate binary rather than an HTTP endpoint.
// Phase 12's explicit non-goal list (docs/phase-12-plan.md §3) rules out a
// self-service HTTP API for key creation/rotation/revocation, and this
// phase adds no new route anywhere: key lifecycle is an operator
// operation, run against the database with operator credentials, not a
// capability exposed to API callers. That keeps the new attack surface of
// this phase at zero new endpoints.
//
// It requires the same two secrets the API server does --
// TASKFORGE_DATABASE_URL and TASKFORGE_API_KEY_PEPPER -- because a key's
// secret_hash is only meaningful under the pepper the verifying server
// will use. Minting a key under a different pepper produces a credential
// that can never authenticate.
//
// Usage:
//
//	taskforge-admin create-principal -kind=caller -name="orders service"
//	taskforge-admin create-key -principal=<uuid> -scopes=jobs [-expires-in=720h]
//	taskforge-admin revoke-key -key-id=<key_id>
//	taskforge-admin revoke-principal -principal=<uuid>
//	taskforge-admin list-principals
//
// The raw secret is printed exactly once, by create-key, and is not
// recoverable afterwards: only its HMAC is stored. Rotation is "mint the
// new key, deploy it, revoke the old one" -- both are live at once during
// the overlap window, which is what makes rotation need no synchronised
// cutover.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/buildinfo"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/config"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/governance"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "taskforge-admin: "+err.Error())
		os.Exit(1)
	}
}

func usage() string {
	return strings.Join([]string{
		"usage: taskforge-admin <command> [flags]",
		"",
		"commands:",
		"  create-principal  -kind=caller|admin -name=<display name>",
		"  create-key        -principal=<uuid> -scopes=jobs[,metrics,admin] [-expires-in=<duration>]",
		"  revoke-key        -key-id=<key_id>",
		"  revoke-principal  -principal=<uuid>",
		"  list-principals",
		"  set-queue-limit   -queue=<name> [-concurrency=<N>|-unlimited]",
		"  set-rate-limit    -queue=<name> -rate=<per-sec> -burst=<N>",
		"  clear-rate-limit  -queue=<name>",
		"  show-queue-state",
		"  version",
	}, "\n")
}

// governanceCommands are handled entirely by internal/governance and need
// neither TASKFORGE_API_KEY_PEPPER nor internal/principal.Store -- an
// operator managing queue/rate-limit configuration should not be forced
// to have the credential pepper on hand at all (docs/phase-13-plan.md §8:
// this configuration is operator-tool-managed, not an HTTP surface, but
// it is also a DISTINCT concern from key lifecycle, and the two must not
// be coupled through a shared, unrelated startup requirement).
var governanceCommands = map[string]bool{
	"set-queue-limit":  true,
	"set-rate-limit":   true,
	"clear-rate-limit": true,
	"show-queue-state": true,
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no command given\n\n%s", usage())
	}
	cmd, rest := args[0], args[1:]
	if cmd == "version" {
		fmt.Println("taskforge-admin " + buildinfo.String())
		return nil
	}

	if governanceCommands[cmd] {
		dbCfg, err := config.FromEnv()
		if err != nil {
			return err
		}
		db, err := sql.Open("pgx", dbCfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer db.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			return err
		}

		g := governance.New(db)
		switch cmd {
		case "set-queue-limit":
			return setQueueLimit(ctx, g, rest)
		case "set-rate-limit":
			return setRateLimit(ctx, g, rest)
		case "clear-rate-limit":
			return clearRateLimit(ctx, g, rest)
		case "show-queue-state":
			return showQueueState(ctx, g, rest)
		}
	}

	// The pepper is mandatory for every command that touches api_keys,
	// and required unconditionally here so an operator cannot create a
	// principal now and discover the missing secret only at key-mint time.
	cfg, err := config.FromEnvRequiringPepper()
	if err != nil {
		return err
	}

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return err
	}

	store, err := principal.NewStore(db, cfg.APIKeyPepper)
	if err != nil {
		return err
	}

	switch cmd {
	case "create-principal":
		return createPrincipal(ctx, store, rest)
	case "create-key":
		return createKey(ctx, store, rest)
	case "revoke-key":
		return revokeKey(ctx, store, rest)
	case "revoke-principal":
		return revokePrincipal(ctx, store, rest)
	case "list-principals":
		return listPrincipals(ctx, db)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage())
	}
}

func createPrincipal(ctx context.Context, store *principal.Store, args []string) error {
	fs := flag.NewFlagSet("create-principal", flag.ContinueOnError)
	kind := fs.String("kind", string(principal.KindCaller), "principal kind: caller or admin")
	name := fs.String("name", "", "operator-assigned display name (never used for lookup)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	p, err := store.CreatePrincipal(ctx, principal.Kind(*kind), *name)
	if err != nil {
		return err
	}
	fmt.Printf("principal created\n  id:   %s\n  kind: %s\n  name: %s\n", p.ID, p.Kind, p.DisplayName)
	if p.Kind == principal.KindAdmin {
		fmt.Println("\nNOTE: an admin principal bypasses per-principal ownership scoping -- it can")
		fmt.Println("read and cancel ANY principal's jobs and workflows. That is the only")
		fmt.Println("documented exception to resource isolation; issue admin principals sparingly.")
	}
	return nil
}

func createKey(ctx context.Context, store *principal.Store, args []string) error {
	fs := flag.NewFlagSet("create-key", flag.ContinueOnError)
	principalID := fs.String("principal", "", "principal UUID this key belongs to")
	scopes := fs.String("scopes", principal.ScopeJobs, "comma-separated scopes: jobs, metrics, admin")
	expiresIn := fs.Duration("expires-in", 0, "optional lifetime, e.g. 720h; zero means no expiry")
	if err := fs.Parse(args); err != nil {
		return err
	}

	id, err := uuid.Parse(*principalID)
	if err != nil {
		return fmt.Errorf("-principal must be a valid UUID: %w", err)
	}

	var expiresAt *time.Time
	if *expiresIn > 0 {
		t := time.Now().Add(*expiresIn)
		expiresAt = &t
	}

	cred, err := store.CreateAPIKey(ctx, id, splitScopes(*scopes), expiresAt)
	if err != nil {
		return err
	}

	// The one and only time this secret is ever printed. It is not stored
	// and cannot be recovered or re-displayed -- only its HMAC under the
	// server's pepper exists in the database.
	fmt.Printf("api key created\n  key_id:    %s\n  principal: %s\n  scopes:    %s\n",
		cred.APIKey.KeyID, cred.APIKey.PrincipalID, strings.Join(cred.APIKey.Scopes, ","))
	if expiresAt != nil {
		fmt.Printf("  expires:   %s\n", expiresAt.UTC().Format(time.RFC3339))
	}
	fmt.Printf("\nSHOWN ONCE -- store it now, it cannot be retrieved again:\n\n  Authorization: Bearer %s\n\n", cred.Credential)
	return nil
}

func revokeKey(ctx context.Context, store *principal.Store, args []string) error {
	fs := flag.NewFlagSet("revoke-key", flag.ContinueOnError)
	keyID := fs.String("key-id", "", "the non-secret key_id to revoke")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := store.RevokeAPIKey(ctx, *keyID); err != nil {
		return err
	}
	// No redeploy, no restart, no cache invalidation: verification reads
	// revoked_at from the database on every single request.
	fmt.Printf("api key %s revoked; it stops authenticating on its very next use\n", *keyID)
	return nil
}

func revokePrincipal(ctx context.Context, store *principal.Store, args []string) error {
	fs := flag.NewFlagSet("revoke-principal", flag.ContinueOnError)
	principalID := fs.String("principal", "", "principal UUID to revoke")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := uuid.Parse(*principalID)
	if err != nil {
		return fmt.Errorf("-principal must be a valid UUID: %w", err)
	}
	if err := store.RevokePrincipal(ctx, id); err != nil {
		return err
	}
	fmt.Printf("principal %s revoked; every key it holds stops authenticating immediately\n", id)
	return nil
}

// listPrincipals reads directly rather than through internal/principal
// because a "list everything" query is operator convenience, not part of
// the identity model's own contract -- there is no application code path
// that enumerates principals.
func listPrincipals(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
		SELECT p.id, p.kind, p.display_name, p.revoked_at,
		       count(k.id) FILTER (WHERE k.revoked_at IS NULL) AS live_keys
		FROM principals p LEFT JOIN api_keys k ON k.principal_id = p.id
		GROUP BY p.id, p.kind, p.display_name, p.revoked_at
		ORDER BY p.created_at ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Printf("%-38s %-7s %-10s %-9s %s\n", "ID", "KIND", "LIVE KEYS", "REVOKED", "NAME")
	for rows.Next() {
		var (
			id        uuid.UUID
			kind      string
			name      string
			revokedAt sql.NullTime
			liveKeys  int
		)
		if err := rows.Scan(&id, &kind, &name, &revokedAt, &liveKeys); err != nil {
			return err
		}
		revoked := "no"
		if revokedAt.Valid {
			revoked = "yes"
		}
		fmt.Printf("%-38s %-7s %-10d %-9s %s\n", id, kind, liveKeys, revoked, name)
	}
	return rows.Err()
}

func splitScopes(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// setQueueLimit implements `set-queue-limit -queue=<name>
// [-concurrency=<N>|-unlimited]` (docs/phase-13-plan.md §8). Exactly one
// of -concurrency or -unlimited must be given: this tool distinguishes
// "set a limit" from "explicitly remove any limit" rather than trying to
// infer the operator's intent from a single ambiguous flag, and
// -concurrency=0 is rejected by internal/governance itself (SF-041 --
// distinct from -unlimited, never silently equivalent to it).
func setQueueLimit(ctx context.Context, g *governance.Store, args []string) error {
	fs := flag.NewFlagSet("set-queue-limit", flag.ContinueOnError)
	queue := fs.String("queue", "", "queue_name to configure (required)")
	concurrency := fs.Int("concurrency", -1, "concurrency limit (positive integer)")
	unlimited := fs.Bool("unlimited", false, "explicitly remove any concurrency limit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *queue == "" {
		return fmt.Errorf("-queue is required")
	}
	if *unlimited == (*concurrency >= 0) {
		return fmt.Errorf("specify exactly one of -concurrency=<N> or -unlimited")
	}

	var limit *int
	if !*unlimited {
		limit = concurrency
	}
	if err := g.SetConcurrencyLimit(ctx, *queue, limit); err != nil {
		return err
	}
	if limit == nil {
		fmt.Printf("queue %q: concurrency limit removed (unlimited)\n", *queue)
	} else {
		fmt.Printf("queue %q: concurrency limit set to %d\n", *queue, *limit)
	}
	return nil
}

// setRateLimit implements `set-rate-limit -queue=<name> -rate=<per-sec>
// -burst=<N>`.
func setRateLimit(ctx context.Context, g *governance.Store, args []string) error {
	fs := flag.NewFlagSet("set-rate-limit", flag.ContinueOnError)
	queue := fs.String("queue", "", "queue_name to configure (required)")
	rate := fs.Float64("rate", 0, "sustained submissions per second (required, positive)")
	burst := fs.Int("burst", 0, "token-bucket burst size (required, positive)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *queue == "" {
		return fmt.Errorf("-queue is required")
	}
	if err := g.SetRateLimit(ctx, *queue, *rate, *burst); err != nil {
		return err
	}
	fmt.Printf("queue %q: rate limit set to %.4g/s, burst %d\n", *queue, *rate, *burst)
	return nil
}

// clearRateLimit implements `clear-rate-limit -queue=<name>`.
func clearRateLimit(ctx context.Context, g *governance.Store, args []string) error {
	fs := flag.NewFlagSet("clear-rate-limit", flag.ContinueOnError)
	queue := fs.String("queue", "", "queue_name to clear (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *queue == "" {
		return fmt.Errorf("-queue is required")
	}
	if err := g.ClearRateLimit(ctx, *queue); err != nil {
		return err
	}
	fmt.Printf("queue %q: rate limit cleared\n", *queue)
	return nil
}

// showQueueState implements `show-queue-state`: current limits plus live
// concurrency/rate-bucket state, read-only operator visibility --
// docs/phase-13-plan.md §8's CLI-side complement to §12's metrics, for an
// operator who wants a point-in-time snapshot without a Prometheus query.
func showQueueState(ctx context.Context, g *governance.Store, args []string) error {
	fs := flag.NewFlagSet("show-queue-state", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	states, err := g.ListQueueState(ctx)
	if err != nil {
		return err
	}
	if len(states) == 0 {
		fmt.Println("no queues known (none configured, none ever submitted to)")
		return nil
	}

	fmt.Printf("%-20s %-10s %-8s %-14s %-16s %-10s %s\n",
		"QUEUE", "CONCURRENCY", "SLOTS", "RATE/BURST", "TOKENS", "LAST_CLAIMED", "")
	for _, st := range states {
		concurrency := "unlimited"
		if st.ConcurrencyLimit != nil {
			concurrency = fmt.Sprintf("%d", *st.ConcurrencyLimit)
		}
		slots := fmt.Sprintf("%d/%d", st.SlotsHeld, st.SlotsTotal)
		rate := "none"
		if st.RateLimitPerSec != nil && st.RateLimitBurst != nil {
			rate = fmt.Sprintf("%.4g/s,%d", *st.RateLimitPerSec, *st.RateLimitBurst)
		}
		tokens := "-"
		if st.RateTokens != nil {
			tokens = fmt.Sprintf("%.2f", *st.RateTokens)
		}
		lastClaimed := "never"
		if st.LastClaimedAt != nil {
			lastClaimed = st.LastClaimedAt.UTC().Format(time.RFC3339)
		}
		fmt.Printf("%-20s %-10s %-8s %-14s %-16s %-10s\n",
			st.QueueName, concurrency, slots, rate, tokens, lastClaimed)
	}
	return nil
}
