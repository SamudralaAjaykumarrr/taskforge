// Phase 12 trust-boundary guard for the worker process (OD-3, G3, G4).
//
// This file exists because of a specific documentation correction. An
// independent review found that docs/security-model.md §5 rated the
// "GET /metrics exposure" threat "Closed" while cmd/worker's own metrics
// listener (TASKFORGE_METRICS_ADDR, default :9090) served the same registry
// with no credential at all. The claim was narrowed to what the code
// actually delivers: only cmd/api's GET /metrics is credential-protected,
// and restricting the worker's listener to a private network is a stated
// deployment obligation.
//
// The reason the worker's listener is not credential-protected is a trust
// boundary, not an oversight: verifying an API key requires reading the
// principals/api_keys tables, and OD-3 plus deploy/postgres-roles.sql
// deliberately give taskforge_worker no access to them at all (asserted by
// internal/migrate's TestPostgresRoleScript_... "the worker role has no
// access to credentials at all"). The tempting WRONG fix -- reuse
// internal/api's middleware in the worker -- would silently require that
// access and quietly dissolve the boundary the whole phase rests on.
//
// The test below makes that wrong fix fail loudly at compile-test time. It
// does NOT forbid ever securing the worker's listener: a scrape token,
// mTLS at the listener, or a push gateway would all pass, because none of
// them needs the worker to read a credential table.
package main

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// forbiddenWorkerImports are packages the worker process must not depend
// on, each with the reason a reviewer needs to evaluate a change that adds
// one.
var forbiddenWorkerImports = map[string]string{
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal": "" +
		"the worker must not depend on the application/API identity model: its identity is its " +
		"PostgreSQL role, verified by PostgreSQL at connection time (OD-3). Importing this package " +
		"means the worker needs SELECT on principals/api_keys, which deploy/postgres-roles.sql " +
		"deliberately denies it. If you are trying to protect the worker's metrics listener, use a " +
		"mechanism that does not require credential-table access (scrape token, mTLS, push gateway)",
	"github.com/SamudralaAjaykumarrr/taskforge/internal/api": "" +
		"the worker serves no API surface and must not mount internal/api's router or middleware; " +
		"workers never call the HTTP API (docs/architecture.md's data-flow diagram)",
}

// TestWorkerDoesNotDependOnTheApplicationIdentityModel pins the boundary.
func TestWorkerDoesNotDependOnTheApplicationIdentityModel(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", name, err)
			}
			if reason, forbidden := forbiddenWorkerImports[path]; forbidden {
				t.Errorf("%s imports %s, which the Phase 12 trust boundary forbids: %s",
					name, path, reason)
			}
		}
	}

	if scanned == 0 {
		t.Fatal("sanity: cmd/worker must have production sources to scan")
	}
}

// TestWorkerMetricsListenerIsDocumentedAsUnauthenticated is the honesty
// half: the worker does serve a metrics endpoint, it is not authenticated,
// and the source must say so where an operator wiring the listener will see
// it -- rather than leaving them to infer it from a security document that
// previously said the opposite.
func TestWorkerMetricsListenerIsDocumentedAsUnauthenticated(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(raw)

	if !strings.Contains(src, "promhttp.HandlerFor") {
		t.Skip("cmd/worker no longer serves metrics directly; this guard no longer applies")
	}

	var findings []string
	for _, want := range []string{
		"unauthenticated",
		"TASKFORGE_METRICS_ADDR",
	} {
		if !strings.Contains(src, want) {
			findings = append(findings, want)
		}
	}
	if len(findings) > 0 {
		t.Errorf("cmd/worker/main.go serves a metrics endpoint but does not state its access-control "+
			"status near the wiring; missing mention of %v. docs/security-model.md §5 scopes the "+
			"\"metrics exposure closed\" claim to cmd/api precisely because this listener is open, "+
			"and that caveat must be visible here too", findings)
	}
}
