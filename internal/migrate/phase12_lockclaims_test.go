// Phase 12 locking-claim guard.
//
// Two successive independent reviews found the same class of defect in this
// phase: documentation asserting a migration lock property that PostgreSQL
// does not deliver, backed by a test that appeared to prove it. The first
// round it was 0006/0007 (ADD COLUMN's ACCESS EXCLUSIVE held across the
// backfill). The second round it was 0009 -- documented as not blocking the
// worker claim query, measured against a 3M-row table as hitting lock_timeout
// (SQLSTATE 55P03) instead of claiming.
//
// Prose is not self-checking, so this file makes it checkable. It scans the
// authoritative Phase 12 documents and the migration files themselves for the
// specific false claims that were shipped twice, and fails if any of them
// reappears. TestPhase12Migrations_0009BlocksWritesAndTheClaimQuery asserts
// the real behaviour against real PostgreSQL; this asserts that the documents
// describing that behaviour still agree with it.
//
// It deliberately checks for the FALSE statements rather than requiring
// particular true ones: a whitelist of blessed sentences would ossify the
// prose, whereas these patterns are wrong in any phrasing.
package migrate_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/migrations"
)

// falseLockClaim is one retired claim, with the reason it is false so a
// future author who reintroduces it learns why from the failure alone.
type falseLockClaim struct {
	pattern *regexp.Regexp
	why     string
}

var falseLockClaims = []falseLockClaim{
	{
		// "no lock stronger than ROW EXCLUSIVE", in any phrasing.
		pattern: regexp.MustCompile("(?i)stronger than[\\s\u0060*\\\"']*ROW EXCLUSIVE"),
		why: "FALSE: migration 0009 holds SHARE UPDATE EXCLUSIVE and then SHARE -- both stronger " +
			"than ROW EXCLUSIVE -- while doing work proportional to table size (observed directly " +
			"in pg_locks). Describe the actual lock levels instead of asserting a ceiling",
	},
	{
		// 0009 (or "the index builds") claimed not to block claims/writes.
		pattern: regexp.MustCompile(`(?i)(0009|index build[s]?)[^.\n]{0,160}(do(es)? not|never|cannot|don't)[^.\n]{0,60}block[^.\n]{0,60}(claim|write)`),
		why: "FALSE: 0009's CREATE INDEX statements take SHARE, which conflicts with the ROW " +
			"EXCLUSIVE every write to jobs requires -- including the worker claim query, which is " +
			"an UPDATE (internal/store/claim.go), not the SELECT its candidate CTE resembles. " +
			"Measured: the real claim query hits lock_timeout (55P03) while 0009 runs",
	},
	{
		// The "reads and the claim query are unaffected" parenthetical.
		pattern: regexp.MustCompile(`(?i)(claim quer(y|ies)|claim path)[^.\n]{0,40}(are|is)\s+unaffected`),
		why: "FALSE for 0009: the claim query is a write and is blocked by it. Say which " +
			"migration, and say that 0009 blocks claiming",
	},
	{
		// The claim query described as a SELECT for locking purposes.
		pattern: regexp.MustCompile("(?i)claim quer(y|ies)'?s?[\\s`*]+SELECT[\\s.`*]*(\\.\\.\\.|…)[\\s.`*]*FOR UPDATE[^.\n]{0,80}(ROW SHARE|compatible)"),
		why: "MISLEADING: the claim query's CTE takes ROW SHARE, but the statement is an UPDATE " +
			"and takes ROW EXCLUSIVE. Reasoning about the CTE's lock is exactly what produced the " +
			"false 0009 claim. Reason about the statement",
	},
	{
		// Online / non-blocking / zero-downtime asserted rather than denied.
		pattern: regexp.MustCompile(`(?i)(these|the|phase 12)\s+migrations[^.\n]{0,60}\b(are|is)\s+(fully\s+)?(online|non-?blocking|zero[- ]downtime)`),
		why: "FALSE: the Phase 12 migration set is not online. 0006/0008/0010 take ACCESS " +
			"EXCLUSIVE and 0009 blocks all writes",
	},
}

// There is deliberately NO "but this sentence denies the claim" escape hatch.
//
// The first version of this file had one -- it exempted any match whose
// surrounding text contained a negation word. That made the whole guard
// vacuous, because every retired claim here IS phrased as a negation ("does
// not block the claim query", "no lock stronger than ROW EXCLUSIVE"), so the
// claim exempted itself. Re-injecting the exact false sentence into README.md
// did not fail the test.
//
// So the patterns below are instead written narrowly enough that only a
// genuinely false assertion matches, and the corrected prose is phrased so it
// does not collide. If a true statement ever trips one of these, the fix is
// to say the true thing more precisely -- not to widen an exemption.

func phase12LockDocuments(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}

	for _, rel := range []string{
		filepath.Join("..", "..", "README.md"),
		filepath.Join("..", "..", "docs", "data-model.md"),
		filepath.Join("..", "..", "docs", "phase-12-plan.md"),
		filepath.Join("..", "..", "docs", "security-model.md"),
	} {
		raw, err := os.ReadFile(rel)
		require.NoError(t, err, "authoritative Phase 12 document must exist: %s", rel)
		out[filepath.Base(rel)] = string(raw)
	}

	entries, err := migrations.Files.ReadDir(".")
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "00") || !strings.HasSuffix(name, ".sql") {
			continue
		}
		if v := name[:4]; v < "0005" {
			continue // pre-Phase-12 migrations are out of scope here
		}
		raw, err := migrations.Files.ReadFile(name)
		require.NoError(t, err)
		out[name] = string(raw)
	}
	return out
}

// TestPhase12Docs_ContainNoRetiredLockingClaim is the guard itself.
func TestPhase12Docs_ContainNoRetiredLockingClaim(t *testing.T) {
	docs := phase12LockDocuments(t)
	require.NotEmpty(t, docs)

	for name, body := range docs {
		// Join wrapped prose so a claim split across a line break is still
		// matched, but keep paragraph boundaries so patterns cannot span
		// unrelated statements.
		for _, para := range strings.Split(body, "\n\n") {
			flat := strings.Join(strings.Fields(para), " ")
			for _, c := range falseLockClaims {
				loc := c.pattern.FindStringIndex(flat)
				if loc == nil {
					continue
				}
				t.Errorf("%s reintroduces a retired Phase 12 locking claim:\n"+
					"  matched:  %q\n"+
					"  context:  %q\n"+
					"  why it is wrong: %s",
					name, flat[loc[0]:loc[1]], flat[max0(loc[0]-120):min(len(flat), loc[1]+120)], c.why)
			}
		}
	}
}

// TestPhase12Docs_StateThe0009BlockingBehaviour is the positive half: it is
// not enough that the false claim is absent -- an operator scheduling this
// migration has to be told, in the documents they actually read, that worker
// claiming stops while 0009 runs.
func TestPhase12Docs_StateThe0009BlockingBehaviour(t *testing.T) {
	// Tolerant of markdown emphasis/backticks and of line wrapping, strict
	// about the substance: "0009 blocks every write ... including the worker
	// claim query".
	blocksWrites := regexp.MustCompile(
		"(?i)[`*]*0009[`*]*[^.]{0,40}blocks every write to [`*]*jobs[`*]*,? including the worker claim")
	mustSay := map[string]*regexp.Regexp{
		"README.md":     blocksWrites,
		"data-model.md": blocksWrites,
		"0009_validate_principal_id_and_scope_idempotency.up.sql": regexp.MustCompile(
			`(?i)BLOCK ALL WRITES,\s*(--)?\s*INCLUDING THE WORKER CLAIM QUERY`),
	}

	docs := phase12LockDocuments(t)
	for name, re := range mustSay {
		body, ok := docs[name]
		require.True(t, ok, "expected document %s", name)
		flat := strings.Join(strings.Fields(body), " ")
		// require.True rather than require.Regexp: the latter dumps the whole
		// document into the failure message, which for README.md overruns
		// testify's scanner.
		require.True(t, re.MatchString(flat),
			"%s must state plainly that migration 0009 blocks writes and the worker claim query "+
				"(expected to match %s); an operator reads this to decide when to run it", name, re)
	}
}

// TestPhase12Docs_RequireBenchmarkingAndLockTimeout pins the deployment
// obligations, which are the only real mitigation this phase offers for
// 0009's blocking and for 0007's unbounded duration.
func TestPhase12Docs_RequireBenchmarkingAndLockTimeout(t *testing.T) {
	docs := phase12LockDocuments(t)
	for _, name := range []string{"README.md", "data-model.md"} {
		flat := strings.Join(strings.Fields(docs[name]), " ")
		require.True(t, regexp.MustCompile(`(?i)benchmark`).MatchString(flat),
			"%s must tell operators to benchmark 0007/0009 against realistic data -- duration is "+
				"data dependent and no test can establish it", name)
		require.True(t, regexp.MustCompile(`(?i)lock_timeout`).MatchString(flat),
			"%s must state the lock_timeout deployment obligation", name)
		require.True(t, regexp.MustCompile(`(?i)(quiet|maintenance) window`).MatchString(flat),
			"%s must state the quiet/maintenance-window deployment obligation", name)
	}
}

func max0(i int) int {
	if i < 0 {
		return 0
	}
	return i
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
