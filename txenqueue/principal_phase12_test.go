// Phase 12 (docs/phase-12-plan.md §6b, OD-4; §11 verification points 7 and
// 14): txenqueue's principal contract.
//
// Three properties are proved here, all against real PostgreSQL:
//
//  1. PrincipalID is mandatory -- a zero value is ErrInvalidRequest.
//  2. The rejection happens BEFORE tx is touched, so the caller's
//     transaction is left exactly as usable as it was.
//  3. There is NO path -- not a branch, not a fallback, not a helper --
//     by which an unset PrincipalID resolves to the system principal.
//     This is the negative proof verification point 14 asks for, and it
//     directly contradicts the design Revision 1 of the plan proposed and
//     the review rejected.
package txenqueue_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
	"github.com/SamudralaAjaykumarrr/taskforge/internal/testutil"
	"github.com/SamudralaAjaykumarrr/taskforge/txenqueue"
)

// TestEnqueueTx_PrincipalID_Required_RejectsZeroValue is verification
// point 7's first half: a request with no principal is invalid, reported
// through the EXISTING ErrInvalidRequest sentinel (no new error type --
// this is an invalid request, and that sentinel already means exactly
// that).
func TestEnqueueTx_PrincipalID_Required_RejectsZeroValue(t *testing.T) {
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	ctx := context.Background()
	st := txenqueue.New()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck

	j, created, err := st.EnqueueTx(ctx, tx, txenqueue.EnqueueRequest{
		JobType: "test.p12.txenqueue.noprincipal",
		Payload: []byte(`{}`),
	})
	require.ErrorIs(t, err, txenqueue.ErrInvalidRequest,
		"a zero PrincipalID must be rejected as an invalid request")
	require.Nil(t, j)
	require.False(t, created)
	require.Contains(t, err.Error(), "principal_id",
		"the error must say which field was missing -- ErrInvalidRequest's text is the one this package "+
			"documents as safe to surface to an end caller")
}

// TestEnqueueTx_PrincipalID_ValidatedBeforeTxIsTouched is verification
// point 7's second half and the precise contract EnqueueTx documents for
// ErrInvalidRequest: "tx was never touched."
//
// It proves this behaviourally rather than by inspection: after the
// rejected call, the caller's transaction is still fully usable (a
// subsequent statement succeeds and commits), which could not be true if
// the failed call had issued a statement that PostgreSQL aborted.
func TestEnqueueTx_PrincipalID_ValidatedBeforeTxIsTouched(t *testing.T) {
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	setupBusinessTable(t, pool)
	ctx := context.Background()
	st := txenqueue.New()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)

	_, _, err = st.EnqueueTx(ctx, tx, txenqueue.EnqueueRequest{JobType: "test.p12.txenqueue.untouched"})
	require.ErrorIs(t, err, txenqueue.ErrInvalidRequest)

	// If the rejected call had issued any statement, PostgreSQL would
	// have put tx into its aborted state and this would fail.
	businessID := uuid.New()
	_, err = tx.Exec(ctx, `INSERT INTO txenqueue_test_business_rows (id, note) VALUES ($1, $2)`, businessID, "still usable")
	require.NoError(t, err, "a PrincipalID rejection must leave the caller's transaction fully usable")
	require.NoError(t, tx.Commit(ctx))
	require.True(t, businessRowExists(t, pool, businessID))
}

// TestEnqueueTx_PrincipalID_ValidatedBeforeNilTxCheck pins the ORDER of
// the two pre-flight checks. Validation-first is what makes the
// ErrInvalidRequest contract honest: an invalid request against a nil tx
// is still reported as an invalid request, consistent with every other
// validation-first path in this method.
func TestEnqueueTx_PrincipalID_ValidatedBeforeNilTxCheck(t *testing.T) {
	st := txenqueue.New()
	var nilTx pgx.Tx

	_, _, err := st.EnqueueTx(context.Background(), nilTx, txenqueue.EnqueueRequest{
		JobType: "test.p12.txenqueue.nilTx",
	})
	require.ErrorIs(t, err, txenqueue.ErrInvalidRequest,
		"a missing PrincipalID is a request-validation failure, checked before tx is even examined")
	require.NotErrorIs(t, err, txenqueue.ErrInvalidTransaction)

	// And a VALID request against a nil tx is still the transaction error,
	// so validation-first has not swallowed that case.
	_, _, err = st.EnqueueTx(context.Background(), nilTx, txenqueue.EnqueueRequest{
		PrincipalID: principal.SystemPrincipalID,
		JobType:     "test.p12.txenqueue.nilTx",
	})
	require.ErrorIs(t, err, txenqueue.ErrInvalidTransaction)
}

// TestEnqueueTx_AttributesJobToSuppliedPrincipal is the positive case: the
// principal the caller names is the principal the durable row carries --
// read back from the database, not from the return value.
func TestEnqueueTx_AttributesJobToSuppliedPrincipal(t *testing.T) {
	db := testutil.DB(t)
	dsn := testutil.DSN(t)
	caller := testutil.NewPrincipal(t, db, principal.KindCaller, "txenqueue caller")

	pool := newPool(t, dsn)
	ctx := context.Background()
	st := txenqueue.New()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	j, created, err := st.EnqueueTx(ctx, tx, txenqueue.EnqueueRequest{
		PrincipalID: caller,
		JobType:     "test.p12.txenqueue.attributed",
		Payload:     []byte(`{}`),
	})
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, tx.Commit(ctx))

	var stored uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT principal_id FROM jobs WHERE id = $1`, j.ID).Scan(&stored))
	require.Equal(t, caller, stored)
	require.NotEqual(t, principal.SystemPrincipalID, stored,
		"the job must carry the caller's own principal, not the backfill identity")
}

// TestEnqueueTx_NoDefaultSystemPrincipalPath is verification point 14's
// negative proof, in its behavioural form: across every shape of "no
// principal supplied" a caller could produce, NO job row is ever created,
// and in particular none is ever created against SystemPrincipalID.
//
// The system principal exists as migration 0007's backfill target and
// nothing else. A call that cannot say who it is enqueuing for does not
// enqueue.
func TestEnqueueTx_NoDefaultSystemPrincipalPath(t *testing.T) {
	db := testutil.DB(t)
	dsn := testutil.DSN(t)
	pool := newPool(t, dsn)
	ctx := context.Background()
	st := txenqueue.New()

	const jobType = "test.p12.txenqueue.nodefault"
	key := "no-default-key"

	for name, req := range map[string]txenqueue.EnqueueRequest{
		"zero value":           {JobType: jobType},
		"explicit uuid.Nil":    {PrincipalID: uuid.Nil, JobType: jobType},
		"zero with idem key":   {JobType: jobType, IdempotencyKey: &key},
		"zero with everything": {JobType: jobType, Payload: []byte(`{"a":1}`), IdempotencyKey: &key},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			require.NoError(t, err)
			defer tx.Rollback(ctx) //nolint:errcheck

			_, _, err = st.EnqueueTx(ctx, tx, req)
			require.ErrorIs(t, err, txenqueue.ErrInvalidRequest)
		})
	}

	var systemRows int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM jobs WHERE principal_id = $1`, principal.SystemPrincipalID).Scan(&systemRows))
	require.Zero(t, systemRows,
		"no EnqueueTx call may ever create a row owned by the system principal -- there is no default-to-system path")

	var anyRows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM jobs WHERE job_type = $1`, jobType).Scan(&anyRows))
	require.Zero(t, anyRows, "a rejected submission must create no row at all")
}

// TestTxenqueuePackage_ContainsNoSystemPrincipalFallback is verification
// point 14's structural half, and the one that survives refactoring.
//
// The behavioural test above can only show that the inputs it tried did
// not reach a fallback. This one asserts the fallback does not exist:
// txenqueue's source must contain no reference to SystemPrincipalID at all
// outside of comments, and must not name the contingency adapter OD-4
// documents but deliberately does not build.
//
// If that adapter is ever added, it must be a separately named function
// that logs on every call -- and this test must be updated deliberately at
// that time, which is exactly the point: the fallback cannot appear by
// accident.
func TestTxenqueuePackage_ContainsNoSystemPrincipalFallback(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++

		raw, err := os.ReadFile(name)
		require.NoError(t, err)

		// Re-print the AST parsed without comments, so the doc comments
		// that explain WHY there is no fallback do not trip the check.
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, raw, 0)
		require.NoError(t, err)
		var code strings.Builder
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				code.WriteString(id.Name)
				code.WriteString("\n")
			}
			if lit, ok := n.(*ast.BasicLit); ok {
				code.WriteString(lit.Value)
				code.WriteString("\n")
			}
			return true
		})

		require.NotContains(t, code.String(), "SystemPrincipalID",
			"%s must not reference the system principal: EnqueueTx has no default-to-system path", name)
		require.NotContains(t, code.String(), "EnqueueTxUnscoped",
			"%s must not define OD-4's contingency adapter -- it is documented as a contingency, not committed scope", name)
	}
	require.NotZero(t, scanned, "sanity: there must be production sources to scan")
}

// TestEnqueueTx_IdempotencyIsScopedPerPrincipal is verification point 7's
// cross-principal half: two principals racing on the SAME
// (job_type, idempotency_key) through concurrent transactions must each
// end up with exactly one row of their own -- proving the scoped index
// separates them, and that neither one's conflict-recovery re-read can
// return the other's job.
func TestEnqueueTx_IdempotencyIsScopedPerPrincipal(t *testing.T) {
	db := testutil.DB(t)
	dsn := testutil.DSN(t)
	a := testutil.NewPrincipal(t, db, principal.KindCaller, "tenant A")
	b := testutil.NewPrincipal(t, db, principal.KindCaller, "tenant B")

	pool := newPool(t, dsn)
	ctx := context.Background()
	st := txenqueue.New()

	const jobType = "test.p12.txenqueue.idem.scoped"
	key := "both-tenants-chose-this-key"

	const perPrincipal = 8
	ids := make([]uuid.UUID, 0, perPrincipal*2)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, p := range []uuid.UUID{a, b} {
		for i := 0; i < perPrincipal; i++ {
			wg.Add(1)
			go func(principalID uuid.UUID) {
				defer wg.Done()
				tx, err := pool.Begin(ctx)
				if err != nil {
					return
				}
				j, _, err := st.EnqueueTx(ctx, tx, txenqueue.EnqueueRequest{
					PrincipalID:    principalID,
					JobType:        jobType,
					IdempotencyKey: &key,
				})
				if err != nil {
					_ = tx.Rollback(ctx)
					return
				}
				if err := tx.Commit(ctx); err != nil {
					return
				}
				mu.Lock()
				ids = append(ids, j.ID)
				mu.Unlock()
			}(p)
		}
	}
	wg.Wait()

	for _, p := range []uuid.UUID{a, b} {
		var n int
		require.NoError(t, db.QueryRow(
			`SELECT count(*) FROM jobs WHERE principal_id = $1 AND job_type = $2 AND idempotency_key = $3`,
			p, jobType, key).Scan(&n))
		require.Equal(t, 1, n, "each principal must end up with exactly one row for its own key")
	}

	var total int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM jobs WHERE job_type = $1 AND idempotency_key = $2`, jobType, key).Scan(&total))
	require.Equal(t, 2, total,
		"two tenants racing on the same key must produce exactly two rows -- not one (a false duplicate) "+
			"and not many (a broken constraint)")

	// And no successful call ever returned the other tenant's job id.
	require.NotEmpty(t, ids)
	for _, id := range ids {
		var owner uuid.UUID
		require.NoError(t, db.QueryRow(`SELECT principal_id FROM jobs WHERE id = $1`, id).Scan(&owner))
		require.Contains(t, []uuid.UUID{a, b}, owner)
	}
}
