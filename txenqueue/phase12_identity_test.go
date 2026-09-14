// Phase 12 test identity: txenqueue.EnqueueRequest.PrincipalID is now
// required (docs/phase-12-plan.md §6b), and internal/store's read methods
// take a principal.AccessContext.
//
// These pre-Phase-12 tests -- which prove transactional atomicity,
// rollback, and idempotency semantics, not identity -- supply the system
// principal and an admin access context so their subject is unchanged.
// The Phase 12 behaviour itself (PrincipalID required, zero value
// rejected before tx is touched, no default-to-system-principal path,
// per-principal idempotency scoping) is proved separately and
// adversarially in errors_test.go and txenqueue_test.go's own Phase 12
// tests.
package txenqueue_test

import (
	"github.com/SamudralaAjaykumarrr/taskforge/internal/principal"
)

var testPrincipalID = principal.SystemPrincipalID

var testAccess = principal.AccessContext{PrincipalID: testPrincipalID, IsAdmin: true}
