// The tests for destination.go, against the real schema. The rules here are
// check constraints and a unique key - the closed set of kinds, verification
// being all-or-none, ownership travelling with the id - and a fake store
// would agree with the code instead of with Postgres.

package payout_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// destinationTestTx migrates and opens a transaction the test throws away.
func destinationTestTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise payout destinations")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	return ctx, tx
}

// aMember seeds an account.
func aMember(ctx context.Context, t *testing.T, tx pgx.Tx) uuid.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'A Member', 'reader') returning id`,
		"member-"+uuid.NewString()+"@example.test").Scan(&id); err != nil {
		t.Fatalf("seeding the member: %v", err)
	}
	return uuid.UUID(id.Bytes)
}

// destinations builds the store over the test's transaction.
func destinations(t *testing.T, tx pgx.Tx) *payout.Destinations {
	t.Helper()
	d, err := payout.NewDestinations(tx)
	if err != nil {
		t.Fatalf("NewDestinations(): %v", err)
	}
	return d
}

func TestARecordedDestinationArrivesUnverified(t *testing.T) {
	ctx, tx := destinationTestTx(t)
	member := aMember(ctx, t, tx)
	d := destinations(t, tx)

	got, err := d.Record(ctx, payout.NewDestination{
		AccountID:  member,
		Kind:       payout.KindSEPA,
		DetailsRef: "vault:member/sepa/1",
	})
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}
	if got.ID == uuid.Nil {
		t.Error("the destination has no id")
	}
	if got.AccountID != member {
		t.Errorf("the destination belongs to %s, want %s", got.AccountID, member)
	}
	// Unverified, and that is the whole point: verification is a separate
	// flow, and a destination that arrived verified would be one nobody
	// proved belongs to the member (FR-051).
	if got.Verified() {
		t.Error("a newly recorded destination is already verified")
	}
	if got.VerifiedMethod != "" {
		t.Errorf("VerifiedMethod = %q on an unverified destination", got.VerifiedMethod)
	}
}

// TestAnotherMembersDestinationIsNotReadable is the ownership rule, and the
// reason every read takes the account. US4 scenario 6 and the contract both
// say 403; what this pins is that the store cannot answer any other way.
func TestAnotherMembersDestinationIsNotReadable(t *testing.T) {
	ctx, tx := destinationTestTx(t)
	owner := aMember(ctx, t, tx)
	stranger := aMember(ctx, t, tx)
	d := destinations(t, tx)

	theirs, err := d.Record(ctx, payout.NewDestination{
		AccountID:  owner,
		Kind:       payout.KindSEPA,
		DetailsRef: "vault:member/sepa/1",
	})
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}

	if _, err := d.Get(ctx, stranger, theirs.ID); !errors.Is(err, payout.ErrDestinationNotFound) {
		t.Fatalf("Get() as a stranger = %v, want one wrapping ErrDestinationNotFound", err)
	}
	// And a destination that does not exist answers the same, so nothing
	// tells a caller that another member's id is real.
	missing, err := d.Get(ctx, stranger, uuid.New())
	if !errors.Is(err, payout.ErrDestinationNotFound) {
		t.Fatalf("Get() for an id that names nothing = %v, want one wrapping ErrDestinationNotFound", err)
	}
	if missing.ID != uuid.Nil {
		t.Errorf("Get() returned %+v beside a refusal", missing)
	}
	// The owner still reads their own.
	back, err := d.Get(ctx, owner, theirs.ID)
	if err != nil {
		t.Fatalf("the owner cannot read their own destination: %v", err)
	}
	if back.ID != theirs.ID {
		t.Errorf("Get() = %s, want %s", back.ID, theirs.ID)
	}
}

// TestAListIsOnlyThisMembersDestinations: the same rule on the other read.
func TestAListIsOnlyThisMembersDestinations(t *testing.T) {
	ctx, tx := destinationTestTx(t)
	owner := aMember(ctx, t, tx)
	stranger := aMember(ctx, t, tx)
	d := destinations(t, tx)

	for _, ref := range []string{"vault:a", "vault:b"} {
		if _, err := d.Record(ctx, payout.NewDestination{
			AccountID: owner, Kind: payout.KindManual, DetailsRef: ref,
		}); err != nil {
			t.Fatalf("Record(): %v", err)
		}
	}
	if _, err := d.Record(ctx, payout.NewDestination{
		AccountID: stranger, Kind: payout.KindManual, DetailsRef: "vault:theirs",
	}); err != nil {
		t.Fatalf("Record(): %v", err)
	}

	mine, err := d.List(ctx, owner)
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("List() returned %d destinations, want 2", len(mine))
	}
	for _, got := range mine {
		if got.AccountID != owner {
			t.Errorf("List() for %s returned a destination belonging to %s", owner, got.AccountID)
		}
	}
	// Stable, which is the property the id tiebreaker exists for. Both of
	// these were recorded in one transaction, so created_at is identical to
	// the microsecond and ordering on it alone would leave Postgres free to
	// return them differently between two reads - a list a member cannot
	// pick from twice.
	again, err := d.List(ctx, owner)
	if err != nil {
		t.Fatalf("List() a second time: %v", err)
	}
	for n := range mine {
		if again[n].ID != mine[n].ID {
			t.Fatalf("two reads of the same list disagree at position %d: %s then %s",
				n, mine[n].ID, again[n].ID)
		}
	}
}

// TestAListIsOldestFirst is the other half of the order, and it seeds the
// rows directly because created_at cannot be arranged any other way: the
// column defaults to now(), which is TRANSACTION time, so every destination
// this test recorded through the store would share an instant to the
// microsecond - and the guard on the table refuses to move one afterwards
// (a destination is frozen after creation, FR-051). The insert is therefore
// the only moment the dates can differ, and what is under test is the
// query's ordering rather than the store's writing.
func TestAListIsOldestFirst(t *testing.T) {
	ctx, tx := destinationTestTx(t)
	member := aMember(ctx, t, tx)
	d := destinations(t, tx)

	// Seeded out of date order on purpose, so passing cannot be an accident
	// of insertion order.
	seeded := []struct {
		ref string
		at  time.Time
	}{
		{ref: "vault:newest", at: time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)},
		{ref: "vault:oldest", at: time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)},
		{ref: "vault:middle", at: time.Date(2026, time.August, 2, 12, 0, 0, 0, time.UTC)},
	}
	for _, row := range seeded {
		if _, err := tx.Exec(ctx, `
			insert into cashback.payout_destination (account_id, kind, details_ref, created_at)
			values ($1, 'manual', $2, $3)`, member, row.ref, row.at); err != nil {
			t.Fatalf("seeding %s: %v", row.ref, err)
		}
	}

	got, err := d.List(ctx, member)
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(got) != len(seeded) {
		t.Fatalf("List() returned %d destinations, want %d", len(got), len(seeded))
	}
	for n, want := range []string{"vault:oldest", "vault:middle", "vault:newest"} {
		if got[n].DetailsRef != want {
			t.Errorf("List()[%d] = %q, want %q", n, got[n].DetailsRef, want)
		}
	}
}

// TestAVerifiedDestinationReadsBackAsVerified pins the pair the column
// constraint makes inseparable: verification is when AND how, or neither.
func TestAVerifiedDestinationReadsBackAsVerified(t *testing.T) {
	ctx, tx := destinationTestTx(t)
	member := aMember(ctx, t, tx)
	d := destinations(t, tx)

	recorded, err := d.Record(ctx, payout.NewDestination{
		AccountID: member, Kind: payout.KindSEPA, DetailsRef: "vault:member/sepa/1",
	})
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}
	if _, err := tx.Exec(ctx, `
		update cashback.payout_destination
		   set verified_at = now(), verified_method = 'micro_deposit'
		 where id = $1`, recorded.ID); err != nil {
		t.Fatalf("verifying: %v", err)
	}

	got, err := d.Get(ctx, member, recorded.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if !got.Verified() {
		t.Error("a verified destination reads back unverified")
	}
	if got.VerifiedMethod != "micro_deposit" {
		t.Errorf("VerifiedMethod = %q, want the method that was recorded", got.VerifiedMethod)
	}
}

// TestTheColumnsClosedSetIsRefusedBeforePostgresDoes: a kind the check
// constraint would reject fails here, naming the kinds, rather than arriving
// as a constraint violation with an index name in it.
func TestTheColumnsClosedSetIsRefusedBeforePostgresDoes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    payout.NewDestination
	}{
		{
			name: "a kind nobody stated",
			n:    payout.NewDestination{AccountID: uuid.New(), DetailsRef: "vault:a"},
		},
		{
			name: "a kind no rail could pick up",
			n:    payout.NewDestination{AccountID: uuid.New(), Kind: "cheque", DetailsRef: "vault:a"},
		},
		{
			name: "no member owns it",
			n:    payout.NewDestination{Kind: payout.KindSEPA, DetailsRef: "vault:a"},
		},
		{
			name: "nothing says where the details live",
			n:    payout.NewDestination{AccountID: uuid.New(), Kind: payout.KindSEPA, DetailsRef: "  "},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.n.Validate(); !errors.Is(err, payout.ErrInvalidDestination) {
				t.Fatalf("Validate() = %v, want one wrapping ErrInvalidDestination", err)
			}
		})
	}
}

func TestParseKindTakesOnlyWhatTheColumnAccepts(t *testing.T) {
	t.Parallel()

	for _, want := range []payout.Kind{payout.KindSEPA, payout.KindManual, payout.KindStub} {
		got, err := payout.ParseKind(want.String())
		if err != nil {
			t.Errorf("ParseKind(%q): %v", want, err)
		}
		if got != want {
			t.Errorf("ParseKind(%q) = %q", want, got)
		}
	}
	for _, bad := range []string{"", "SEPA", "cheque", "paypal"} {
		if _, err := payout.ParseKind(bad); !errors.Is(err, payout.ErrInvalidDestination) {
			t.Errorf("ParseKind(%q) = %v, want one wrapping ErrInvalidDestination", bad, err)
		}
	}
}

func TestAStoreOverNoHandleIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := payout.NewDestinations(nil); !errors.Is(err, payout.ErrNoDestinationStore) {
		t.Fatalf("NewDestinations(nil) = %v, want one wrapping ErrNoDestinationStore", err)
	}
}
