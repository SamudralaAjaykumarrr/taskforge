// Command api runs TaskForge's HTTP API server. It is a stateless process
// — every request reads from or writes to PostgreSQL directly
// (docs/architecture.md).
//
// # Transport boundary (Phase 12, OD-7)
//
// This server speaks PLAINTEXT HTTP and terminates no TLS itself. That is
// a deliberate, documented architecture decision, not an omission: TLS
// termination is an external reverse-proxy/load-balancer responsibility,
// and running this process reachable from an untrusted network without
// such a proxy in front of it is a deployment error. TaskForge makes no
// transport-security claim about the leg between a client and that proxy;
// the only trust it places in the proxy is that the network segment
// BETWEEN the proxy and this process is private and operator-controlled --
// something this process cannot verify from the inside and therefore does
// not pretend to.
//
// Correspondingly, nothing in internal/api reads X-Forwarded-For,
// X-Forwarded-Proto, X-Real-IP, or any other proxy-injected header for any
// security decision. Authentication is entirely credential-based, and this
// phase adds no IP-based logic of any kind (rate limiting is Phase 13). If
// a future phase wants to trust a forwarded header, that trust decision
// must be made explicitly then -- including which proxy layer may set it
// and how a client-supplied copy of the same header is prevented from
// spoofing it. It is not granted implicitly by the proxy boundary above.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/api"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/buildinfo"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/config"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/governance"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/metrics"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/migrate"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/store"
)

func main() {
	versionFlag := flag.Bool("version", false, "print version information and exit")
	flag.Parse()
	if *versionFlag {
		fmt.Println("taskforge-api " + buildinfo.String())
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(logger); err != nil {
		logger.Error("api server exited with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	// Phase 12: the API server refuses to start without
	// TASKFORGE_API_KEY_PEPPER. There is no flag, env var, or build tag
	// that disables authentication (OD-6: hard cutover, no observe/enforce
	// dual mode), so the process either has what it needs to verify
	// credentials or it does not run.
	//
	// Connection guidance, not enforced here: this DSN should name the
	// least-privilege taskforge_api PostgreSQL role (G4), and for the
	// enterprise reference deployment it must carry
	// sslmode=verify-full (G5). Local/dev profiles are explicitly
	// exempt -- see docs/security-model.md and .env.example.
	cfg, err := config.FromEnvRequiringPepper()
	if err != nil {
		return err
	}

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	if err := migrate.Up(ctx, db); err != nil {
		return err
	}

	// Phase 8: m is shared between the Store (every metric it records --
	// see internal/store) and this process's /metrics endpoint below. A
	// fresh, private registry per process (never
	// prometheus.DefaultRegisterer) -- see internal/metrics.New's doc
	// comment.
	m := metrics.New()
	st := store.New(db, store.WithMetrics(m), store.WithLogger(logger))
	m.Registry.MustRegister(metrics.NewStateCollector(st, cfg.ActiveWorkerWindow, logger))

	// Phase 13: the queue-scoped gauges (taskforge_queue_depth,
	// taskforge_queue_running, taskforge_queue_concurrency_limit) --
	// registered as their own collector since they read both the Store
	// (jobs table) and the governance Store (queue_limits).
	gov := governance.New(db)
	m.Registry.MustRegister(metrics.NewQueueStateCollector(st, gov, logger))

	principals, err := principal.NewStore(db, cfg.APIKeyPepper)
	if err != nil {
		return err
	}

	// GET /metrics is handed to NewRouter rather than attached to the
	// returned handler afterwards, so it goes through the same mount
	// helper -- and therefore the same authentication middleware -- as
	// every other route, and requires the metrics scope (OD-5). Before
	// Phase 12 this endpoint was registered directly on the mux here,
	// unauthenticated, which is docs/security-model.md §5's "operational
	// volume disclosure" finding.
	handlers := api.NewHandlers(st, logger,
		api.WithAuthenticator(principals),
		api.WithMetrics(m),
		api.WithGovernance(gov),
		api.WithMaxInflightSubmissions(cfg.MaxInflightSubmissions),
	)
	router := api.NewRouter(handlers,
		api.WithMetricsEndpoint(promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})),
	)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api server listening", "addr", cfg.HTTPAddr, "version", buildinfo.Version, "commit", buildinfo.Commit)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-serveCtx.Done():
		logger.Info("shutting down api server")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		return srv.Shutdown(shutdownCtx)
	}
}
