package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/identity"
)

// staticRoles is the kind of RoleLookup a consumer wires: here a canned
// answer, after migration 0002 a query against account.role.
type staticRoles struct {
	role string
	err  error
}

func (s staticRoles) Role(context.Context, uuid.UUID) (string, error) { return s.role, s.err }

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
