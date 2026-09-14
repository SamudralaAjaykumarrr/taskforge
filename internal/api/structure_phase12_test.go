// Phase 12's absence-of-behaviour proofs (docs/phase-12-plan.md §11,
// verification points 5 and 15). Both assert properties about what the
// production code does NOT do, which no request-level test can establish:
// a runtime test can only show that the paths it happened to exercise
// behave correctly, whereas these show that the offending code does not
// exist anywhere in the package.
//
// They are the same category of proof as TF-INV-016's "schema test
// asserting the constraint exists" -- mechanical, and loud on a future
// refactor that reintroduces the thing being ruled out.
package api_test

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// productionSources returns every non-test .go file in internal/api.
func productionSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		require.NoError(t, err)
		out[name] = string(raw)
	}
	require.NotEmpty(t, out, "internal/api must have production sources to scan")
	return out
}

// TestRouter_AllHandlersMountedThroughAuth is verification point 5's
// structural half: authentication must be deny-by-default because of how
// routes are registered, not because each handler remembers to check.
//
// It parses every production file in this package and asserts that the
// only call to mux.Handle/mux.HandleFunc anywhere is the one inside the
// mount helper -- which applies requireAuth unconditionally and demands a
// required scope in its signature. A future route registered any other
// way fails this test rather than silently shipping unauthenticated.
func TestRouter_AllHandlersMountedThroughAuth(t *testing.T) {
	fset := token.NewFileSet()
	var offenders []string
	mountCalls := 0

	for name, src := range productionSources(t) {
		file, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err)

		// Find the byte range of func mount, so calls inside it can be
		// excluded from the offender list.
		var mountStart, mountEnd token.Pos
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if ok && fn.Name.Name == "mount" && fn.Recv == nil {
				mountStart, mountEnd = fn.Pos(), fn.End()
			}
			return true
		})

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc" {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "mux" {
				return true
			}
			if mountStart.IsValid() && call.Pos() >= mountStart && call.End() <= mountEnd {
				mountCalls++
				return true
			}
			offenders = append(offenders, fset.Position(call.Pos()).String())
			return true
		})
	}

	require.Equal(t, 1, mountCalls,
		"mount must contain exactly one mux registration -- the single choke point every route passes through")
	require.Empty(t, offenders,
		"every route must be registered through mount (which applies requireAuth and requires a scope); "+
			"these registrations bypass it and would ship unauthenticated: %v", offenders)
}

// TestAPIPackage_ReadsNoForwardedOrIdentityHeaders is verification point
// 15's absence-of-behaviour half.
//
// OD-7 puts TLS termination at an external proxy, so every request reaches
// this package over plaintext HTTP carrying whatever headers that proxy (or
// a client pretending to be behind one) chose to set. Phase 12 places no
// trust in any of them: authentication is entirely credential-based and
// this phase adds no IP-based logic at all (rate limiting is Phase 13).
//
// This test asserts the production code never even READS such a header, so
// the property cannot be eroded by a later change that reads one "just for
// logging" and then grows a security decision on top of it. The only
// header this package may read for a security decision is Authorization.
func TestAPIPackage_ReadsNoForwardedOrIdentityHeaders(t *testing.T) {
	forbidden := []string{
		"X-Forwarded-For",
		"X-Forwarded-Proto",
		"X-Forwarded-Host",
		"X-Forwarded-User",
		"X-Real-IP",
		"X-Authenticated-User",
		"X-Principal-Id",
		"X-Principal-ID",
		"X-Taskforge-Principal",
		"X-API-Key",
		"X-Api-Key",
		"X-Admin",
		"X-Scopes",
		"Forwarded",
	}

	for name, src := range productionSources(t) {
		code := stripComments(t, name, src)
		for _, header := range forbidden {
			require.NotContains(t, code, `"`+header+`"`,
				"%s must not read or reference the %s header: Phase 12 trusts no proxy-injected "+
					"or client-supplied identity header (docs/phase-12-plan.md §10)", name, header)
		}
		// Nor may it reach for the connection's peer address, which is
		// the other shape an IP-based decision would take.
		require.NotContains(t, code, "RemoteAddr",
			"%s must not use r.RemoteAddr for any decision: Phase 12 adds no IP-based logic", name)
	}
}

// TestAPIPackage_ReadsOnlyAuthorizationHeaderForIdentity is the positive
// complement: identity comes from exactly one header, read in exactly one
// place.
func TestAPIPackage_ReadsOnlyAuthorizationHeaderForIdentity(t *testing.T) {
	total := 0
	for name, src := range productionSources(t) {
		code := stripComments(t, name, src)
		n := strings.Count(code, `Header.Get("Authorization")`)
		if n > 0 {
			require.Equal(t, "auth.go", name,
				"the Authorization header must be read only in the authentication middleware")
		}
		total += n
	}
	require.Equal(t, 1, total, "the Authorization header must be read exactly once, in one place")
}

// stripComments returns name's source with comments removed, so an
// assertion about what the CODE does is not tripped by a doc comment that
// merely names the thing being ruled out (this package's comments discuss
// X-Forwarded-For at length precisely because it is not trusted).
//
// It works by re-printing the AST parsed WITHOUT ParseComments, which
// drops every comment while preserving the code verbatim.
func stripComments(t *testing.T, name, src string) string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, 0)
	require.NoError(t, err)

	var b strings.Builder
	require.NoError(t, printer.Fprint(&b, fset, file))
	return b.String()
}
