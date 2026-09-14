// Phase 12 test identity (docs/phase-12-plan.md §5): every jobs and
// workflow_instances row now has a mandatory owning principal, and every
// read/cancel store method takes a principal.AccessContext.
//
// The tests in this package predate Phase 12 and are about state
// transitions, leases, retries, cancellation races, workflows, and
// invariants -- not about identity. They attribute their rows to the
// system principal (migration 0005's seed row, which always exists and
// needs no per-test setup) and pass an admin access context, so the
// ownership predicate added to every query matches unconditionally and
// these tests keep proving exactly what they proved before, unchanged.
//
// That is deliberate: Phase 12's own suite
// (internal/api/handlers_phase12_test.go, internal/store's
// principal-scoping tests, internal/principal, txenqueue) proves the
// authorization behaviour with real, distinct principals and adversarial
// cases. Re-deriving it here would add setup noise to a hundred unrelated
// tests without testing anything new -- and keeping this suite otherwise
// untouched is itself the regression evidence that Phase 12 added a
// boundary in front of the engine without changing the engine.
package invariant_test

import (
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
)

// testPrincipalID owns every job/workflow these tests create.
var testPrincipalID = principal.SystemPrincipalID

// testAccess is the access context these tests pass to principal-scoped
// store methods. IsAdmin is set so the ownership predicate is satisfied
// regardless of which principal a given row happens to carry -- these
// tests are not exercising isolation, and a test that silently started
// failing to match rows would be a confusing way to discover that.
var testAccess = principal.AccessContext{PrincipalID: testPrincipalID, IsAdmin: true}
