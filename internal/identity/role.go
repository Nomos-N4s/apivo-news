package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
// This interface was the deliberate seam for migration 0002 while it was
// built in parallel; now that the account.role column exists, AccountRoles
// below is the real implementation and the composition root wires it with
// the platform pool. The seam stays: tests supply canned lookups, and a
// future role source (claims, a cache) slots in without touching callers.
type RoleLookup interface {
	// Role returns the role of the given account, e.g. "editor". An error
	// means the lookup failed, not that the account lacks a role.
	Role(ctx context.Context, accountID uuid.UUID) (string, error)
}

// AccountRoles is the database-backed RoleLookup, reading account.role
// (migration 0002) - the same column the article triggers enforce, so the
// early HTTP answer and the database verdict can never come from different
// sources of truth.
type AccountRoles struct {
	db Querier
}

// NewAccountRoles builds the RoleLookup the composition root wires; the
// platform pool satisfies Querier.
func NewAccountRoles(db Querier) AccountRoles { return AccountRoles{db: db} }

// Role returns account.role for the given account. A missing account
// reports ErrUnknownAccount - distinct from a lookup failure, which means
// the question went unanswered, not that the account lacks a role.
func (r AccountRoles) Role(ctx context.Context, accountID uuid.UUID) (string, error) {
	var role string
	err := r.db.QueryRow(ctx,
		`select role from account where id = $1`,
		accountID.String()).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: subject %s", ErrUnknownAccount, accountID)
	}
	if err != nil {
		return "", fmt.Errorf("identity: role lookup for %s: %w", accountID, err)
	}
	return role, nil
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
