package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// RoleEditor is the account role editorial endpoints require. It mirrors
// the value the editor-approver database trigger enforces on write
// (migration 0002): the HTTP 403 is the polite early answer, the trigger
// is the guarantee.
const RoleEditor = "editor"

// ErrNotEditor reports an authenticated account that lacks the editor
// role. Consumers map it to 403.
var ErrNotEditor = errors.New("identity: account is not an editor")

// RoleLookup resolves the role of an account.
//
// This interface is the deliberate seam for migration 0002, which is
// being built in parallel and adds the account.role column: this module
// compiles and tests against the 0001 schema and therefore must not read
// that column itself. Once 0002 lands, the composition root wires an
// implementation backed by `select role from account where id = $1`
// (satisfiable by the platform pool via a one-method adapter); until
// then, tests and callers supply their own. Nothing else in this module
// needs to change when the column arrives.
type RoleLookup interface {
	// Role returns the role of the given account, e.g. "editor". An error
	// means the lookup failed, not that the account lacks a role.
	Role(ctx context.Context, accountID uuid.UUID) (string, error)
}

// RequireEditor is the gate editorial endpoints apply after Authenticate:
// it returns nil when the identity's account holds the editor role,
// ErrNotEditor (403) when it holds any other role, and the wrapped lookup
// error when the role could not be determined. The database enforces the
// same rule again on write - this check exists to fail fast with a clear
// status code, not to be the last line of defence.
func RequireEditor(ctx context.Context, id Identity, roles RoleLookup) error {
	role, err := roles.Role(ctx, id.Subject)
	if err != nil {
		return fmt.Errorf("identity: role lookup for %s: %w", id.Subject, err)
	}
	if role != RoleEditor {
		return fmt.Errorf("%w: account %s holds role %q", ErrNotEditor, id.Subject, role)
	}
	return nil
}
