package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/identity"
)

// staticRoles is the kind of RoleLookup a consumer wires: here a canned
// answer, after migration 0002 a query against account.role.
type staticRoles struct {
	role string
	err  error
}

func (s staticRoles) Role(context.Context, uuid.UUID) (string, error) { return s.role, s.err }

// roleDB satisfies identity.Querier with a canned single-column row (or
// error); the real account.role read is covered by the integration test.
type roleDB struct {
	role string
	err  error
}

func (f roleDB) QueryRow(context.Context, string, ...any) pgx.Row { return roleRow(f) }

type roleRow roleDB

func (r roleRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*string)) = r.role
	return nil
}

func TestAccountRoles(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	id := uuid.New()
	errDB := errors.New("connection torn down")

	cases := []struct {
		name    string
		db      roleDB
		want    string
		wantErr error // sentinel matched with errors.Is; nil expects success
	}{
		{name: "editor role read", db: roleDB{role: identity.RoleEditor}, want: identity.RoleEditor},
		{name: "reader role read", db: roleDB{role: "reader"}, want: "reader"},
		{name: "missing account is unknown, not roleless", db: roleDB{err: pgx.ErrNoRows}, wantErr: identity.ErrUnknownAccount},
		{name: "database failure propagates", db: roleDB{err: errDB}, wantErr: errDB},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := identity.NewAccountRoles(tc.db).Role(ctx, id)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Role error = %v, want errors.Is(err, %v)", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Role: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Role = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRequireEditor(t *testing.T) {
	t.Parallel()

	id := identity.Identity{Subject: uuid.New(), Email: "editor@example.test", DisplayName: "Editor"}
	errLookup := errors.New("role column not migrated yet")

	cases := []struct {
		name    string
		roles   staticRoles
		wantErr error // sentinel matched with errors.Is; nil expects the gate to open
	}{
		{name: "editor passes", roles: staticRoles{role: identity.RoleEditor}},
		{name: "reader is refused", roles: staticRoles{role: "reader"}, wantErr: identity.ErrNotEditor},
		{name: "empty role is refused", roles: staticRoles{role: ""}, wantErr: identity.ErrNotEditor},
		{name: "lookup failure propagates", roles: staticRoles{err: errLookup}, wantErr: errLookup},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := identity.RequireEditor(t.Context(), id, tc.roles)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("RequireEditor: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("RequireEditor error = %v, want errors.Is(err, %v)", err, tc.wantErr)
			}
			if tc.roles.err != nil && errors.Is(err, identity.ErrNotEditor) {
				t.Error("a failed lookup must not read as a 403 verdict")
			}
		})
	}
}

func TestRequireOperator(t *testing.T) {
	t.Parallel()

	id := identity.Identity{Subject: uuid.New(), Email: "ops@example.test", DisplayName: "Operator"}
	errLookup := errors.New("role lookup timed out")

	cases := []struct {
		name    string
		roles   staticRoles
		wantErr error // sentinel matched with errors.Is; nil expects the gate to open
	}{
		{name: "operator passes", roles: staticRoles{role: identity.RoleOperator}},
		{name: "reader is refused", roles: staticRoles{role: "reader"}, wantErr: identity.ErrNotOperator},
		// The one refusal worth naming on its own: editorial authority is
		// not money-releasing authority, and an /ops/* route that let an
		// editor through would be a wider grant than any migration made.
		{name: "editor is refused", roles: staticRoles{role: identity.RoleEditor}, wantErr: identity.ErrNotOperator},
		{name: "empty role is refused", roles: staticRoles{role: ""}, wantErr: identity.ErrNotOperator},
		{name: "lookup failure propagates", roles: staticRoles{err: errLookup}, wantErr: errLookup},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := identity.RequireOperator(t.Context(), id, tc.roles)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("RequireOperator: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("RequireOperator error = %v, want errors.Is(err, %v)", err, tc.wantErr)
			}
			if tc.roles.err != nil && errors.Is(err, identity.ErrNotOperator) {
				t.Error("a failed lookup must not read as a 403 verdict")
			}
		})
	}
}

// TestTheTwoRoleGatesDoNotOverlap states the property the two tables above
// only imply one direction of: neither gate opens for the other's role. It
// is asserted rather than reasoned about because the pair of them is the
// whole separation between approving an article and releasing money.
func TestTheTwoRoleGatesDoNotOverlap(t *testing.T) {
	t.Parallel()

	id := identity.Identity{Subject: uuid.New()}
	if err := identity.RequireEditor(t.Context(), id, staticRoles{role: identity.RoleOperator}); !errors.Is(err, identity.ErrNotEditor) {
		t.Errorf("RequireEditor for an operator = %v, want errors.Is(err, %v)", err, identity.ErrNotEditor)
	}
	if err := identity.RequireOperator(t.Context(), id, staticRoles{role: identity.RoleEditor}); !errors.Is(err, identity.ErrNotOperator) {
		t.Errorf("RequireOperator for an editor = %v, want errors.Is(err, %v)", err, identity.ErrNotOperator)
	}
}
