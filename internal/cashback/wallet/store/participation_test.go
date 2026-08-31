package store_test

// What the three participation statements do, and - much more importantly -
// what they answer NOTHING for (T080, FR-001..003).
//
// All three are :one, so "no row" arrives as pgx.ErrNoRows, and in two of
// them that is not a failure but the answer: an opt-in that returns nothing
// means the member is already in, and a leave that returns nothing means
// they were already out. The HTTP layer turns the first into 409 and the
// second into a repeat of the same 200, and neither is derivable from the
// row when there is one - so the emptiness is the contract these cases pin.

import (
	"context"
	"errors"
	"testing"

	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
)

// joiner seeds an account with no participation of its own. Separate from
// payout_test's member, which also seeds a destination: nothing here pays
// anybody, and a fixture that carried a payout destination would be
// asserting against a schema state these statements never see.
func joiner(ctx context.Context, t *testing.T, tx pgx.Tx) pgtype.UUID {
	t.Helper()
	var account pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Joining Member', 'reader') returning id`,
		"joining-"+tag(t)+"@example.test").Scan(&account); err != nil {
		t.Fatalf("seeding the member: %v", err)
	}
	return account
}

// optIn runs the upsert with one deployment's configured values.
func optIn(ctx context.Context, t *testing.T, tx pgx.Tx, account pgtype.UUID, brand, terms, currency string) (store.CashbackParticipation, error) {
	t.Helper()
	return store.New(tx).OptIntoParticipation(ctx, store.OptIntoParticipationParams{
		AccountID:       account,
		BrandID:         brand,
		TermsVersion:    terms,
		DefaultCurrency: currency,
	})
}

// joinedAt seeds a participation dated in the past.
//
// It exists because now() is TRANSACTION time, and this whole suite runs
// inside one transaction so that it can never leave a participation row
// behind. Every statement in a case therefore reads the same instant, and
// "the re-join moved the date" is not a claim a second call could ever
// demonstrate. Seeding the row directly, with a date the transaction's own
// clock is certain to be after, is what makes the claim decidable.
func joinedAt(ctx context.Context, t *testing.T, tx pgx.Tx, account pgtype.UUID, brand, terms, currency, ago string) time.Time {
	t.Helper()
	var opened time.Time
	if err := tx.QueryRow(ctx, `
		insert into cashback.participation
		    (account_id, brand_id, opted_in_at, terms_version, status, default_currency)
		values ($1, $2, now() - $3::interval, $4, 'active', $5)
		returning opted_in_at`,
		account, brand, ago, terms, currency).Scan(&opened); err != nil {
		t.Fatalf("seeding the participation: %v", err)
	}
	return opened
}

func TestOptingInRecordsTheAcceptance(t *testing.T) {
	t.Parallel()
	ctx, tx := schemaTx(t)
	account := joiner(ctx, t, tx)
	brand := "brand-" + tag(t)

	joined, err := optIn(ctx, t, tx, account, brand, "3.1.0", "SEK")
	if err != nil {
		t.Fatalf("OptIntoParticipation(): %v", err)
	}
	switch {
	case joined.Status != "active":
		t.Errorf("status = %q, want active", joined.Status)
	case joined.TermsVersion != "3.1.0":
		t.Errorf("terms_version = %q, want 3.1.0", joined.TermsVersion)
	case joined.DefaultCurrency != "SEK":
		t.Errorf("default_currency = %q, want SEK", joined.DefaultCurrency)
	case joined.BrandID != brand:
		t.Errorf("brand_id = %q, want %q", joined.BrandID, brand)
	case joined.LeftAt.Valid:
		t.Errorf("left_at is set on an active participation: %v", joined.LeftAt.Time)
	case !joined.OptedInAt.Valid:
		t.Error("opted_in_at is null; the acceptance has no date")
	}
}

// The 409, from the statement's point of view. A member who is already in
// must not have their acceptance quietly re-stated by a second POST: the
// terms they agreed to and the day they agreed are the whole of the FR-002
// record, and a re-statement would move both.
func TestOptingInTwiceChangesNothing(t *testing.T) {
	t.Parallel()
	ctx, tx := schemaTx(t)
	account := joiner(ctx, t, tx)
	brand := "brand-" + tag(t)
	opened := joinedAt(ctx, t, tx, account, brand, "3.1.0", "SEK", "30 days")

	if _, err := optIn(ctx, t, tx, account, brand, "4.0.0", "EUR"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the second opt-in returned %v, want pgx.ErrNoRows: an active member must not be re-stated", err)
	}

	held, err := store.New(tx).ParticipationFor(ctx, account)
	if err != nil {
		t.Fatalf("ParticipationFor(): %v", err)
	}
	if held.TermsVersion != "3.1.0" || held.DefaultCurrency != "SEK" {
		t.Errorf("the row now reads %q/%q; the second opt-in rewrote an acceptance it must not touch",
			held.TermsVersion, held.DefaultCurrency)
	}
	if !held.OptedInAt.Time.Equal(opened) {
		t.Errorf("opted_in_at moved from %v to %v", opened, held.OptedInAt.Time)
	}
}

func TestLeavingClosesTheParticipation(t *testing.T) {
	t.Parallel()
	ctx, tx := schemaTx(t)
	account := joiner(ctx, t, tx)

	joined, err := optIn(ctx, t, tx, account, "brand-"+tag(t), "3.1.0", "SEK")
	if err != nil {
		t.Fatalf("OptIntoParticipation(): %v", err)
	}
	left, err := store.New(tx).LeaveParticipation(ctx, account)
	if err != nil {
		t.Fatalf("LeaveParticipation(): %v", err)
	}
	switch {
	case left.Status != "left":
		t.Errorf("status = %q, want left", left.Status)
	case !left.LeftAt.Valid:
		t.Error("left_at is null on a left participation")
	case left.LeftAt.Time.Before(joined.OptedInAt.Time):
		t.Errorf("left_at %v is before opted_in_at %v", left.LeftAt.Time, joined.OptedInAt.Time)
	case left.TermsVersion != "3.1.0":
		t.Errorf("terms_version = %q; leaving must not disturb what was accepted", left.TermsVersion)
	}
}

// Leaving twice is one leaving. The empty answer is what tells the caller
// this request did not close anything - which is what keeps a retried
// DELETE from publishing a second cashback.participation.ended.
func TestLeavingTwiceClosesNothingFurther(t *testing.T) {
	t.Parallel()
	ctx, tx := schemaTx(t)
	account := joiner(ctx, t, tx)

	if _, err := optIn(ctx, t, tx, account, "brand-"+tag(t), "3.1.0", "SEK"); err != nil {
		t.Fatalf("OptIntoParticipation(): %v", err)
	}
	first, err := store.New(tx).LeaveParticipation(ctx, account)
	if err != nil {
		t.Fatalf("the first leave: %v", err)
	}
	if _, err := store.New(tx).LeaveParticipation(ctx, account); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the second leave returned %v, want pgx.ErrNoRows", err)
	}

	held, err := store.New(tx).ParticipationFor(ctx, account)
	if err != nil {
		t.Fatalf("ParticipationFor(): %v", err)
	}
	if !held.LeftAt.Time.Equal(first.LeftAt.Time) {
		t.Errorf("left_at moved from %v to %v; the second leave rewrote the date they left",
			first.LeftAt.Time, held.LeftAt.Time)
	}
}

// A member who never opted in has nothing to leave. Distinct from the case
// above at the HTTP layer - that one answers 200, this one 404 - and
// indistinguishable here, which is why the handler reads the row back.
func TestLeavingWithoutHavingJoinedFindsNothing(t *testing.T) {
	t.Parallel()
	ctx, tx := schemaTx(t)
	account := joiner(ctx, t, tx)

	if _, err := store.New(tx).LeaveParticipation(ctx, account); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("LeaveParticipation() on a stranger returned %v, want pgx.ErrNoRows", err)
	}
	if _, err := store.New(tx).ParticipationFor(ctx, account); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ParticipationFor() on a stranger returned %v, want pgx.ErrNoRows", err)
	}
}

// Re-joining is the one case where the accepted terms legitimately change,
// and 0017's guard permits it only as the left -> active transition. This
// is the statement that has to make that move exactly, so it is the case
// that would break first if the upsert were ever rewritten as a plain
// update.
func TestRejoiningRestatesTheAcceptance(t *testing.T) {
	t.Parallel()
	ctx, tx := schemaTx(t)
	account := joiner(ctx, t, tx)
	brand := "brand-" + tag(t)
	opened := joinedAt(ctx, t, tx, account, brand, "3.1.0", "SEK", "30 days")

	if _, err := store.New(tx).LeaveParticipation(ctx, account); err != nil {
		t.Fatalf("LeaveParticipation(): %v", err)
	}

	rejoined, err := optIn(ctx, t, tx, account, brand, "4.0.0", "EUR")
	if err != nil {
		t.Fatalf("the re-join: %v", err)
	}
	switch {
	case rejoined.Status != "active":
		t.Errorf("status = %q, want active", rejoined.Status)
	case rejoined.LeftAt.Valid:
		t.Errorf("left_at survived the re-join: %v", rejoined.LeftAt.Time)
	case rejoined.TermsVersion != "4.0.0":
		t.Errorf("terms_version = %q, want the terms in force now", rejoined.TermsVersion)
	case rejoined.DefaultCurrency != "EUR":
		t.Errorf("default_currency = %q, want EUR", rejoined.DefaultCurrency)
	case !rejoined.OptedInAt.Time.After(opened):
		t.Errorf("opted_in_at stayed at %v; coming back is a new acceptance with a new date", opened)
	}
}

// The brand a member joined under is theirs, not the running deployment's.
// 0017's guard raises on any change to brand_id, so a statement that
// re-stated it would turn a rebrand into an error every returning member
// hit - and a statement that changed it would move a financial record
// between tenants (ADR-0004). This proves the upsert does neither.
func TestRejoiningKeepsTheBrandItWasOpenedUnder(t *testing.T) {
	t.Parallel()
	ctx, tx := schemaTx(t)
	account := joiner(ctx, t, tx)
	opened := "brand-" + tag(t)
	joinedAt(ctx, t, tx, account, opened, "3.1.0", "SEK", "30 days")

	if _, err := store.New(tx).LeaveParticipation(ctx, account); err != nil {
		t.Fatalf("LeaveParticipation(): %v", err)
	}

	rejoined, err := optIn(ctx, t, tx, account, "brand-"+tag(t), "3.1.0", "SEK")
	if err != nil {
		t.Fatalf("re-joining under a renamed brand: %v", err)
	}
	if rejoined.BrandID != opened {
		t.Errorf("brand_id = %q, want the %q they joined under", rejoined.BrandID, opened)
	}
}
