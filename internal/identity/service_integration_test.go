package identity_test

// Exercises the subject-to-account mapping against the real, migrated
// schema: the uuid in the token's sub claim is looked up as account.id
// (the account table comment in 0001 pins that alignment).

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lestrrat-go/jwx/v3/jwa"

	"github.com/Nomos-N4s/apivo-news/internal/identity"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// The module's Querier is satisfied by the platform pool at wiring time;
// this is where that claim is checked at compile time.
var _ identity.Querier = (*pgxpool.Pool)(nil)

func TestAuthenticateAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the account mapping")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer pool.Close()

	// The account row is seeded inside a transaction and rolled back;
	// pgx.Tx satisfies the same narrow Querier interface as the pool.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	subject := uuid.New()
	suffix := subject.String()[:8]
	email := "editor-" + suffix + "@example.test"
	name := "Test Editor " + suffix
	if _, err := tx.Exec(ctx,
		`insert into account (id, email, display_name) values ($1, $2, $3)`,
		subject.String(), email, name); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	rsaRaw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating rsa key: %v", err)
	}
	key := signingKey(t, rsaRaw, jwa.RS256(), "it-rsa")
	srv := jwksServer(t, key)
	verifier, err := identity.NewVerifier(ctx, identity.VerifierConfig{
		JWKSURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	t.Cleanup(func() { _ = verifier.Close(context.Background()) })

	svc := identity.New(verifier, tx)
	now := time.Now()

	got, err := svc.Authenticate(ctx, mintToken(t, key, jwa.RS256(), tokenSpec{
		sub: subject.String(), iat: now.Add(-time.Minute), exp: now.Add(time.Hour),
	}))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	want := identity.Identity{Subject: subject, Email: email, DisplayName: name}
	if got != want {
		t.Errorf("Authenticate = %+v, want %+v", got, want)
	}

	// A token email claim takes precedence over the account row's email.
	claimed := "claimed-" + suffix + "@example.test"
	got, err = svc.Authenticate(ctx, mintToken(t, key, jwa.RS256(), tokenSpec{
		sub: subject.String(), iat: now.Add(-time.Minute), exp: now.Add(time.Hour), email: claimed,
	}))
	if err != nil {
		t.Fatalf("Authenticate with email claim: %v", err)
	}
	if got.Email != claimed {
		t.Errorf("Email = %q, want the token claim %q", got.Email, claimed)
	}

	// A valid token whose subject has no account row is refused.
	_, err = svc.Authenticate(ctx, mintToken(t, key, jwa.RS256(), tokenSpec{
		sub: uuid.NewString(), iat: now.Add(-time.Minute), exp: now.Add(time.Hour),
	}))
	if !errors.Is(err, identity.ErrUnknownAccount) {
		t.Errorf("Authenticate error = %v, want errors.Is(err, ErrUnknownAccount)", err)
	}
}

// TestAccountRolesAgainstSchema exercises the database-backed RoleLookup
// against the real account.role column (migration 0002) and its interplay
// with RequireEditor - the gate editorial endpoints apply.
func TestAccountRolesAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the role lookup")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	seed := func(role string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := tx.Exec(ctx,
			`insert into account (id, email, display_name, role) values ($1, $2, $3, $4)`,
			id.String(), "role-"+id.String()[:8]+"@example.test", "Role Test "+id.String()[:8], role); err != nil {
			t.Fatalf("seed %s account: %v", role, err)
		}
		return id
	}
	editorID := seed("editor")
	readerID := seed("reader")

	roles := identity.NewAccountRoles(tx)

	if got, err := roles.Role(ctx, editorID); err != nil || got != identity.RoleEditor {
		t.Errorf("Role(editor) = %q, %v; want %q, nil", got, err, identity.RoleEditor)
	}
	if got, err := roles.Role(ctx, readerID); err != nil || got != "reader" {
		t.Errorf("Role(reader) = %q, %v; want %q, nil", got, err, "reader")
	}
	if _, err := roles.Role(ctx, uuid.New()); !errors.Is(err, identity.ErrUnknownAccount) {
		t.Errorf("Role(unknown) error = %v, want errors.Is(err, ErrUnknownAccount)", err)
	}

	// The pieces compose: the gate opens for the editor and refuses the reader.
	if err := identity.RequireEditor(ctx, identity.Identity{Subject: editorID}, roles); err != nil {
		t.Errorf("RequireEditor(editor): %v", err)
	}
	if err := identity.RequireEditor(ctx, identity.Identity{Subject: readerID}, roles); !errors.Is(err, identity.ErrNotEditor) {
		t.Errorf("RequireEditor(reader) error = %v, want errors.Is(err, ErrNotEditor)", err)
	}
}
