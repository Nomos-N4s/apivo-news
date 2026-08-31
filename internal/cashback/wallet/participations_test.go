package wallet_test

// Joining, leaving and coming back, against the real schema (T080).
//
// Against a real database because every interesting answer here is one the
// DATABASE decides: "already in" is an upsert predicate that found an
// active row, "already out" is an update that matched nothing, and both are
// verdicts a fake store would have to be told rather than reach. What the
// fakes below stand in for is only the failure - a database that errors,
// which no real one does on demand.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
)

// theTerms are one deployment's configured brand, as the composition root
// would hand them over.
var theTerms = wallet.Terms{Brand: "a-brand", Version: "3.1.0", Currency: "SEK"}

// participations builds the service over the suite's transaction, so every
// case's writes roll back with it. pgx.Tx is a Beginner - a transaction
// begun inside one is a savepoint - which is what lets the service open its
// own transactions here at all.
func participations(t *testing.T, tx pgx.Tx, terms wallet.Terms) *wallet.Participations {
	t.Helper()
	made, err := wallet.NewParticipations(tx, store.New(tx), terms)
	if err != nil {
		t.Fatalf("NewParticipations(): %v", err)
	}
	return made
}

// aMember seeds an account with no participation.
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

// announced counts the events of one type about one member, which is how
// every case here checks that the stream and the table agree.
func announced(ctx context.Context, t *testing.T, tx pgx.Tx, eventType string, member uuid.UUID) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx,
		`select count(*) from domain_event where type = $1 and subject = $2`,
		eventType, member.String()).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", eventType, err)
	}
	return n
}

func TestJoiningRecordsAndAnnouncesTheAcceptance(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)

	joined, err := participations(t, tx, theTerms).Join(ctx, member, "3.1.0")
	if err != nil {
		t.Fatalf("Join(): %v", err)
	}
	switch {
	case !joined.Active():
		t.Errorf("status = %q, want active", joined.Status)
	case joined.TermsVersion != "3.1.0":
		t.Errorf("terms_version = %q, want 3.1.0", joined.TermsVersion)
	case joined.Brand != "a-brand":
		t.Errorf("brand = %q, want a-brand", joined.Brand)
	case string(joined.Currency) != "SEK":
		t.Errorf("currency = %q, want the brand's SEK", joined.Currency)
	}
	if n := announced(ctx, t, tx, wallet.TypeParticipationStarted, member); n != 1 {
		t.Errorf("the stream holds %d acceptances, want 1", n)
	}
}

// The version is the DEPLOYMENT's, never the member's. A client that sent
// the current version in the body and a service that trusted it would be a
// service where the member decides what they agreed to.
func TestJoiningRefusesTermsThatAreNotInForce(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)

	for _, version := range []string{"3.0.0", "", "  ", "4.0.0"} {
		if _, err := participations(t, tx, theTerms).Join(ctx, member, version); !errors.Is(err, wallet.ErrStaleTerms) {
			t.Errorf("Join(%q) returned %v, want wallet.ErrStaleTerms", version, err)
		}
	}
	if _, err := participations(t, tx, theTerms).Of(ctx, member); !errors.Is(err, wallet.ErrNotJoined) {
		t.Error("a refused acceptance left a participation behind")
	}
	if n := announced(ctx, t, tx, wallet.TypeParticipationStarted, member); n != 0 {
		t.Errorf("the stream holds %d acceptances after four refusals, want 0", n)
	}
}

// Surrounding whitespace is a client's formatting, not a different version.
func TestJoiningAcceptsTheVersionWithWhitespaceAroundIt(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)

	if _, err := participations(t, tx, theTerms).Join(ctx, member, "  3.1.0\n"); err != nil {
		t.Fatalf("Join(): %v", err)
	}
}

func TestJoiningTwiceIsRefusedAndAnnouncesNothingFurther(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)
	service := participations(t, tx, theTerms)

	if _, err := service.Join(ctx, member, "3.1.0"); err != nil {
		t.Fatalf("the first join: %v", err)
	}
	if _, err := service.Join(ctx, member, "3.1.0"); !errors.Is(err, wallet.ErrAlreadyJoined) {
		t.Fatalf("the second join returned %v, want wallet.ErrAlreadyJoined", err)
	}
	if n := announced(ctx, t, tx, wallet.TypeParticipationStarted, member); n != 1 {
		t.Errorf("the stream holds %d acceptances, want 1", n)
	}
}

// No brand file, no terms to accept. Reading and leaving still work, which
// is the whole reason this is refused on the join rather than at
// construction.
func TestJoiningWithoutABrandSaysSoAndReadingStillWorks(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)

	if _, err := participations(t, tx, wallet.Terms{}).Join(ctx, member, "3.1.0"); !errors.Is(err, wallet.ErrNoBrand) {
		t.Fatalf("Join() returned %v, want wallet.ErrNoBrand", err)
	}
	if _, err := participations(t, tx, wallet.Terms{}).Of(ctx, member); !errors.Is(err, wallet.ErrNotJoined) {
		t.Error("reading a participation needed a brand; it reads what the row says")
	}
	// Joined through a configured service, then left through an
	// unconfigured one: leaving touches nothing the brand supplies.
	if _, err := participations(t, tx, theTerms).Join(ctx, member, "3.1.0"); err != nil {
		t.Fatalf("Join(): %v", err)
	}
	if _, err := participations(t, tx, wallet.Terms{}).Leave(ctx, member); err != nil {
		t.Fatalf("Leave() without a brand: %v", err)
	}
}

// A half-filled Terms is a caller assembling one by hand rather than
// reading a brand file, and it must not reach the database: brand_id and
// terms_version are not blank by constraint, and default_currency is a
// currency by constraint, so the row would be refused - but with a
// constraint violation rather than an answer anyone can act on.
func TestJoiningRefusesAHalfConfiguredBrand(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)

	for name, terms := range map[string]wallet.Terms{
		"no brand":    {Version: "3.1.0", Currency: "SEK"},
		"no version":  {Brand: "a-brand", Currency: "SEK"},
		"no currency": {Brand: "a-brand", Version: "3.1.0"},
		// money.Currency checks the FORM of a code and deliberately not its
		// membership of the ISO register, so "ZZZ" is a currency as far as
		// this module is concerned and a lowercase code is not.
		"a currency in the wrong case": {Brand: "a-brand", Version: "3.1.0", Currency: "sek"},
	} {
		if _, err := participations(t, tx, terms).Join(ctx, member, "3.1.0"); !errors.Is(err, wallet.ErrNoBrand) {
			t.Errorf("Join() with %s returned %v, want wallet.ErrNoBrand", name, err)
		}
	}
}

func TestLeavingClosesTheRowAndAnnouncesTheDeparture(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)
	service := participations(t, tx, theTerms)

	if _, err := service.Join(ctx, member, "3.1.0"); err != nil {
		t.Fatalf("Join(): %v", err)
	}
	left, err := service.Leave(ctx, member)
	if err != nil {
		t.Fatalf("Leave(): %v", err)
	}
	switch {
	case left.Active():
		t.Errorf("status = %q, want left", left.Status)
	case left.LeftAt.IsZero():
		t.Error("left_at is the zero Time on a closed participation")
	case left.TermsVersion != "3.1.0":
		t.Errorf("terms_version = %q; leaving does not disturb what was accepted", left.TermsVersion)
	}
	if n := announced(ctx, t, tx, wallet.TypeParticipationEnded, member); n != 1 {
		t.Errorf("the stream holds %d departures, want 1", n)
	}
}

// DELETE is the method a client retries without thinking. The second call
// answers the same thing and must add nothing to the stream.
func TestLeavingTwiceAnswersTheSameAndAnnouncesOnce(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)
	service := participations(t, tx, theTerms)

	if _, err := service.Join(ctx, member, "3.1.0"); err != nil {
		t.Fatalf("Join(): %v", err)
	}
	first, err := service.Leave(ctx, member)
	if err != nil {
		t.Fatalf("the first leave: %v", err)
	}
	again, err := service.Leave(ctx, member)
	if err != nil {
		t.Fatalf("the second leave: %v", err)
	}
	if !again.LeftAt.Equal(first.LeftAt) {
		t.Errorf("left_at moved from %v to %v", first.LeftAt, again.LeftAt)
	}
	if n := announced(ctx, t, tx, wallet.TypeParticipationEnded, member); n != 1 {
		t.Errorf("the stream holds %d departures after two DELETEs, want 1", n)
	}
}

// Never joined is not the same as having left, and the difference is a 404
// against a 200: one member is shown the opt-in, the other their closed
// participation.
func TestLeavingWithoutHavingJoinedIsNotFound(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)

	if _, err := participations(t, tx, theTerms).Leave(ctx, member); !errors.Is(err, wallet.ErrNotJoined) {
		t.Fatalf("Leave() returned %v, want wallet.ErrNotJoined", err)
	}
	if n := announced(ctx, t, tx, wallet.TypeParticipationEnded, member); n != 0 {
		t.Errorf("the stream holds %d departures for a stranger, want 0", n)
	}
}

// Coming back is a second acceptance, of whatever is in force then - and
// the row keeps the brand it was opened under, which the schema freezes.
func TestRejoiningRecordsTheNewAcceptance(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)

	if _, err := participations(t, tx, theTerms).Join(ctx, member, "3.1.0"); err != nil {
		t.Fatalf("the first join: %v", err)
	}
	if _, err := participations(t, tx, theTerms).Leave(ctx, member); err != nil {
		t.Fatalf("Leave(): %v", err)
	}

	newer := wallet.Terms{Brand: "a-renamed-brand", Version: "4.0.0", Currency: "EUR"}
	rejoined, err := participations(t, tx, newer).Join(ctx, member, "4.0.0")
	if err != nil {
		t.Fatalf("the re-join: %v", err)
	}
	switch {
	case !rejoined.Active():
		t.Errorf("status = %q, want active", rejoined.Status)
	case rejoined.TermsVersion != "4.0.0":
		t.Errorf("terms_version = %q, want 4.0.0", rejoined.TermsVersion)
	case string(rejoined.Currency) != "EUR":
		t.Errorf("currency = %q, want EUR", rejoined.Currency)
	case rejoined.Brand != "a-brand":
		t.Errorf("brand = %q; the row keeps the brand it was opened under", rejoined.Brand)
	}
	if n := announced(ctx, t, tx, wallet.TypeParticipationStarted, member); n != 2 {
		t.Errorf("the stream holds %d acceptances, want 2", n)
	}
}

func TestOfReadsWhatTheMemberAccepted(t *testing.T) {
	t.Parallel()
	ctx, tx := outboxTx(t)
	member := aMember(ctx, t, tx)

	if _, err := participations(t, tx, theTerms).Join(ctx, member, "3.1.0"); err != nil {
		t.Fatalf("Join(): %v", err)
	}
	// Read through a service configured with DIFFERENT terms: what a member
	// accepted is in their row, not in today's configuration.
	newer := wallet.Terms{Brand: "a-renamed-brand", Version: "4.0.0", Currency: "EUR"}
	held, err := participations(t, tx, newer).Of(ctx, member)
	if err != nil {
		t.Fatalf("Of(): %v", err)
	}
	if held.TermsVersion != "3.1.0" || string(held.Currency) != "SEK" || held.Brand != "a-brand" {
		t.Errorf("Of() answered %q/%q/%q, want what they accepted: a-brand/3.1.0/SEK",
			held.Brand, held.TermsVersion, held.Currency)
	}
}

func TestAServiceNeedsADatabase(t *testing.T) {
	t.Parallel()
	if _, err := wallet.NewParticipations(nil, nil, theTerms); !errors.Is(err, wallet.ErrNoParticipations) {
		t.Fatalf("NewParticipations(nil, nil) returned %v, want wallet.ErrNoParticipations", err)
	}
}
