package clickout_test

// Recording a click and finding it again.
//
// The store is faked here because what this file is about is what the
// package refuses and how it maps a row: the schema's own rules have their
// tests beside the statements. The refusals matter on their own, though -
// each of them is a click that would otherwise reach a member as a successful
// redirect and come back attributable to nobody.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// fakeStore answers with a canned row and records what it was asked to
// write.
type fakeStore struct {
	row       store.CashbackClick
	insertErr error
	getErr    error
	// echo makes an insert answer with the row it was asked to write,
	// stamped as the database would stamp it. Callers that care what came
	// back rather than what went in set it, so a mapping that dropped a
	// field on the way out is visible.
	echo bool

	inserted store.InsertClickParams
	askedFor string
	inserts  int
	reads    int
}

func (f *fakeStore) InsertClick(_ context.Context, arg store.InsertClickParams) (store.CashbackClick, error) {
	f.inserted, f.inserts = arg, f.inserts+1
	if f.insertErr != nil {
		return store.CashbackClick{}, f.insertErr
	}
	if f.echo {
		return store.CashbackClick{
			ID:                     pgtype.UUID{Bytes: uuid.New(), Valid: true},
			ClickRef:               arg.ClickRef,
			AccountID:              arg.AccountID,
			OfferID:                arg.OfferID,
			ClickedAt:              pgtype.Timestamptz{Time: echoedClickedAt, Valid: true},
			RateSnapshot:           arg.RateSnapshot,
			MemberShareBpsSnapshot: arg.MemberShareBpsSnapshot,
			ContextDigest:          arg.ContextDigest,
		}, nil
	}
	return f.row, nil
}

// echoedClickedAt is the instant an echoing store stamps, standing in for
// the column's own default.
var echoedClickedAt = time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)

func (f *fakeStore) GetClickByRef(_ context.Context, ref string) (store.CashbackClick, error) {
	f.askedFor, f.reads = ref, f.reads+1
	if f.getErr != nil {
		return store.CashbackClick{}, f.getErr
	}
	return f.row, nil
}

// aRef is a reference of exactly the shape the minter produces.
func aRef(t *testing.T) networks.IssuedClickRef {
	t.Helper()
	ref, err := clickout.NewMinter().Mint()
	if err != nil {
		t.Fatalf("minting a reference: %v", err)
	}
	return ref
}

// aPromise is a well-formed published band and share.
func aPromise() clickout.Promise {
	return clickout.Promise{
		Rate:        catalogue.RateBand{Kind: catalogue.RatePercent, Percent: 400},
		MemberShare: 5000,
	}
}

// storedRow is the row the database would return for the given click.
func storedRow(t *testing.T, ref networks.IssuedClickRef, account, offer uuid.UUID, digest string) store.CashbackClick {
	t.Helper()
	snapshot, err := json.Marshal(aPromise().Rate)
	if err != nil {
		t.Fatalf("marshalling the band: %v", err)
	}
	return store.CashbackClick{
		ID:                     pgtype.UUID{Bytes: uuid.New(), Valid: true},
		ClickRef:               ref.Ref(),
		AccountID:              pgtype.UUID{Bytes: account, Valid: true},
		OfferID:                pgtype.UUID{Bytes: offer, Valid: true},
		ClickedAt:              pgtype.Timestamptz{Time: time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC), Valid: true},
		RateSnapshot:           snapshot,
		MemberShareBpsSnapshot: 5000,
		ContextDigest:          pgtype.Text{String: digest, Valid: digest != ""},
	}
}

// recorder builds a Clicks over the given store.
func recorder(t *testing.T, s clickout.ClickStore) *clickout.Clicks {
	t.Helper()
	clicks, err := clickout.NewClicks(s)
	if err != nil {
		t.Fatalf("NewClicks(): %v", err)
	}
	return clicks
}

func TestARecordedClickCarriesWhatTheMemberWasPromised(t *testing.T) {
	t.Parallel()

	ref, account, offer := aRef(t), uuid.New(), uuid.New()
	digest := clickout.NewContextDigest("ua/1.0", "1.2.3.4")
	fake := &fakeStore{row: storedRow(t, ref, account, offer, digest.String())}

	click, err := recorder(t, fake).Record(t.Context(), clickout.NewClick{
		Ref: ref, AccountID: account, OfferID: offer,
		Promised: aPromise(), Context: digest,
	})
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}

	// What was written.
	if fake.inserted.ClickRef != ref.Ref() {
		t.Errorf("wrote reference %q, want %q", fake.inserted.ClickRef, ref.Ref())
	}
	if uuid.UUID(fake.inserted.AccountID.Bytes) != account {
		t.Errorf("wrote account %v, want %v", uuid.UUID(fake.inserted.AccountID.Bytes), account)
	}
	if fake.inserted.MemberShareBpsSnapshot != 5000 {
		t.Errorf("wrote a share of %d, want 5000", fake.inserted.MemberShareBpsSnapshot)
	}
	// The band goes to the column as the band's own encoder writes it -
	// which is the shape a snapshot read back years later must still be
	// readable in (FR-013, C-6).
	var written catalogue.RateBand
	if err := json.Unmarshal(fake.inserted.RateSnapshot, &written); err != nil {
		t.Fatalf("the written snapshot is not a band: %v", err)
	}
	if written != aPromise().Rate {
		t.Errorf("wrote band %+v, want %+v", written, aPromise().Rate)
	}
	// The digest, never the raw context it came from.
	if fake.inserted.ContextDigest.String != digest.String() {
		t.Errorf("wrote context %q, want the digest %q", fake.inserted.ContextDigest.String, digest.String())
	}

	// What was read back.
	if click.Ref != ref || click.AccountID != account || click.OfferID != offer {
		t.Errorf("Record() = %+v, want the click that was recorded", click)
	}
	if click.Promised != aPromise() {
		t.Errorf("the promise reads %+v, want %+v", click.Promised, aPromise())
	}
	// The instant is the row's, not one this process chose.
	if !click.ClickedAt.Equal(fake.row.ClickedAt.Time) {
		t.Errorf("clicked_at = %s, want the row's %s", click.ClickedAt, fake.row.ClickedAt.Time)
	}
}

// TestAClickWithNoContextRecordsNone keeps "nobody digested a context" and
// "the context digested to nothing" apart on the way to a nullable column.
func TestAClickWithNoContextRecordsNone(t *testing.T) {
	t.Parallel()

	ref, account, offer := aRef(t), uuid.New(), uuid.New()
	fake := &fakeStore{row: storedRow(t, ref, account, offer, "")}

	click, err := recorder(t, fake).Record(t.Context(), clickout.NewClick{
		Ref: ref, AccountID: account, OfferID: offer, Promised: aPromise(),
	})
	if err != nil {
		t.Fatalf("Record(): %v", err)
	}
	if fake.inserted.ContextDigest.Valid {
		t.Errorf("wrote context %q, want null", fake.inserted.ContextDigest.String)
	}
	if click.Context.Recorded() {
		t.Errorf("the click reports a context digest of %q, want none", click.Context)
	}
}

func TestAClickThatCouldNotBeCreditedIsNeverRecorded(t *testing.T) {
	t.Parallel()

	ref, account, offer := aRef(t), uuid.New(), uuid.New()

	cases := []struct {
		name  string
		click clickout.NewClick
		want  error
	}{
		{
			// FR-020's ordering, enforced: a redirect built before its
			// reference was minted would send the member out with nothing
			// to match their purchase back on.
			name:  "no reference was minted",
			click: clickout.NewClick{AccountID: account, OfferID: offer, Promised: aPromise()},
			want:  clickout.ErrNotRecorded,
		},
		{
			// FR-023: an anonymous click can never later be credited to an
			// account, and the cheapest guarantee is that it never exists.
			name:  "the click names no member",
			click: clickout.NewClick{Ref: ref, OfferID: offer, Promised: aPromise()},
			want:  clickout.ErrAnonymousClick,
		},
		{
			name:  "the click names no offer",
			click: clickout.NewClick{Ref: ref, AccountID: account, Promised: aPromise()},
			want:  clickout.ErrUnofferedClick,
		},
		{
			name: "the promised share is not a share",
			click: clickout.NewClick{Ref: ref, AccountID: account, OfferID: offer,
				Promised: clickout.Promise{Rate: aPromise().Rate, MemberShare: money.BasisPointsScale + 1}},
			want: clickout.ErrNotRecorded,
		},
		{
			// A band that does not hold to its own invariant does not
			// encode, and a snapshot silently written with the wrong rate
			// in it is a credit nobody can reconstruct.
			name: "the promised band is not one",
			click: clickout.NewClick{Ref: ref, AccountID: account, OfferID: offer,
				Promised: clickout.Promise{Rate: catalogue.RateBand{Kind: "sideways"}, MemberShare: 5000}},
			want: clickout.ErrNotRecorded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeStore{}
			_, err := recorder(t, fake).Record(t.Context(), tc.click)

			if !errors.Is(err, tc.want) {
				t.Fatalf("Record() error = %v, want one wrapping %v", err, tc.want)
			}
			if fake.inserts != 0 {
				t.Errorf("the store was written to %d time(s); a refused click is never recorded", fake.inserts)
			}
		})
	}
}

// TestATakenReferenceIsNamedAsItself keeps the one collision that matters
// distinguishable from any other write failure. At 128 bits it means the
// entropy source is broken or a caller is re-using a reference, and a caller
// that saw only "could not record" might retry into the same defect.
func TestATakenReferenceIsNamedAsItself(t *testing.T) {
	t.Parallel()

	ref, account, offer := aRef(t), uuid.New(), uuid.New()

	cases := []struct {
		name string
		err  error
		want error
	}{
		{
			name: "the reference is already issued",
			err:  &pgconn.PgError{Code: pgerrcode.UniqueViolation, ConstraintName: "click_ref_unique"},
			want: clickout.ErrReferenceTaken,
		},
		{
			// The same SQLSTATE on this table, from a different rule. A
			// check on the code alone would call this a taken reference and
			// send a caller looking at its entropy source.
			name: "a different unique rule refused it",
			err:  &pgconn.PgError{Code: pgerrcode.UniqueViolation, ConstraintName: "click_id_account_unique"},
			want: clickout.ErrNotRecorded,
		},
		{
			name: "the write failed for some other reason",
			err:  errors.New("connection reset"),
			want: clickout.ErrNotRecorded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeStore{insertErr: tc.err}
			_, err := recorder(t, fake).Record(t.Context(), clickout.NewClick{
				Ref: ref, AccountID: account, OfferID: offer, Promised: aPromise(),
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Record() error = %v, want one wrapping %v", err, tc.want)
			}
		})
	}
}

func TestAReportedReferenceFindsItsClickOrNothing(t *testing.T) {
	t.Parallel()

	ref, account, offer := aRef(t), uuid.New(), uuid.New()

	t.Run("a reference that names a click", func(t *testing.T) {
		t.Parallel()
		fake := &fakeStore{row: storedRow(t, ref, account, offer, "")}
		click, err := recorder(t, fake).ByRef(t.Context(), networks.NewClickRef(ref.Ref()))
		if err != nil {
			t.Fatalf("ByRef(): %v", err)
		}
		if click.Ref != ref {
			t.Errorf("ByRef() = %q, want %q", click.Ref, ref)
		}
		// Asked for verbatim: the lookup is exact, and anything this layer
		// trimmed or folded would widen the match before the query could.
		if fake.askedFor != ref.Ref() {
			t.Errorf("asked the store for %q, want %q", fake.askedFor, ref.Ref())
		}
	})

	t.Run("a reference that names no click", func(t *testing.T) {
		t.Parallel()
		fake := &fakeStore{getErr: pgx.ErrNoRows}
		_, err := recorder(t, fake).ByRef(t.Context(), networks.NewClickRef("SomeOtherPublishersRef"))
		if !errors.Is(err, clickout.ErrNoSuchClick) {
			t.Fatalf("ByRef() error = %v, want one wrapping %v", err, clickout.ErrNoSuchClick)
		}
	})

	// A report that carries no reference never claimed one, so there is
	// nothing to look up and looking anyway would be a query for the empty
	// string - which the click table cannot hold, but which a future schema
	// change could make match something.
	t.Run("a report carrying no reference at all", func(t *testing.T) {
		t.Parallel()
		fake := &fakeStore{}
		_, err := recorder(t, fake).ByRef(t.Context(), networks.ClickRef{})
		if !errors.Is(err, clickout.ErrNoSuchClick) {
			t.Fatalf("ByRef() error = %v, want one wrapping %v", err, clickout.ErrNoSuchClick)
		}
		if fake.reads != 0 {
			t.Errorf("the store was read %d time(s) for a report that named no reference", fake.reads)
		}
	})

	t.Run("the read failed", func(t *testing.T) {
		t.Parallel()
		fake := &fakeStore{getErr: errors.New("connection reset")}
		_, err := recorder(t, fake).ByRef(t.Context(), networks.NewClickRef(ref.Ref()))
		if errors.Is(err, clickout.ErrNoSuchClick) {
			t.Fatal("a failed read reads as 'no such click', which would queue a matched purchase as unattributed")
		}
		if err == nil {
			t.Fatal("ByRef() returned no error for a failed read")
		}
	})
}

// TestARowThatCannotBeATrustedClickIsRefused covers the mapping's own
// checks. A row is only ever as good as what is in it, and a value written
// before a constraint existed - or by something other than this code - must
// not be handed on as a reference a redirect could be rebuilt from.
func TestARowThatCannotBeATrustedClickIsRefused(t *testing.T) {
	t.Parallel()

	ref, account, offer := aRef(t), uuid.New(), uuid.New()

	cases := []struct {
		name  string
		spoil func(row *store.CashbackClick)
	}{
		{name: "a reference no redirect could use", spoil: func(r *store.CashbackClick) { r.ClickRef = "short" }},
		{name: "a snapshot that is not a band", spoil: func(r *store.CashbackClick) { r.RateSnapshot = []byte(`{"kind":"sideways"}`) }},
		{name: "a snapshot that is not JSON", spoil: func(r *store.CashbackClick) { r.RateSnapshot = []byte(`not json`) }},
		{name: "a share outside the possible range", spoil: func(r *store.CashbackClick) { r.MemberShareBpsSnapshot = int32(money.BasisPointsScale) + 1 }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := storedRow(t, ref, account, offer, "")
			tc.spoil(&row)
			if _, err := recorder(t, &fakeStore{row: row}).ByRef(t.Context(), networks.NewClickRef(ref.Ref())); err == nil {
				t.Fatal("ByRef() returned a click built from a row that cannot be one")
			}
		})
	}
}

func TestARecorderNeedsAStore(t *testing.T) {
	t.Parallel()

	if _, err := clickout.NewClicks(nil); !errors.Is(err, clickout.ErrNoClickStore) {
		t.Fatalf("NewClicks(nil) error = %v, want one wrapping %v", err, clickout.ErrNoClickStore)
	}
}

func TestAContextDigestNeverCarriesWhatItDigested(t *testing.T) {
	t.Parallel()

	const address, agent = "203.0.113.7", "Mozilla/5.0 (a device someone owns)"
	digest := clickout.NewContextDigest(agent, address)

	if !digest.Recorded() {
		t.Fatal("a digest of two real parts reports nothing recorded")
	}
	// FR-022: enough to tell two clicks apart, and nothing that puts a
	// person's address in a table nobody thought held one.
	for _, raw := range []string{address, agent} {
		if strings.Contains(digest.String(), raw) {
			t.Errorf("the digest %q carries %q verbatim", digest, raw)
		}
	}
	if digest.String() != clickout.NewContextDigest(agent, address).String() {
		t.Error("the same context digested twice gave two answers; an abuse rule cannot use that")
	}
	if digest.String() == clickout.NewContextDigest(address, agent).String() {
		t.Error("two different contexts digested to the same value")
	}
}

// TestTwoContextsCannotBeJoinedIntoOne pins the separator's job: without
// one, ("ab","c") and ("a","bc") are the same bytes and so the same digest -
// two different devices an abuse rule would count as one.
func TestTwoContextsCannotBeJoinedIntoOne(t *testing.T) {
	t.Parallel()

	if clickout.NewContextDigest("ab", "c").String() == clickout.NewContextDigest("a", "bc").String() {
		t.Error("two different splittings digested to the same value")
	}
}

func TestADigestOfNothingIsNotADigest(t *testing.T) {
	t.Parallel()

	for _, parts := range [][]string{nil, {}, {""}, {"  "}, {"", "\t\n"}} {
		digest := clickout.NewContextDigest(parts...)
		if digest.Recorded() || digest.String() != "" {
			t.Errorf("NewContextDigest(%q) = %q, want the zero value - a click with nothing to digest records nothing", parts, digest)
		}
	}
	// A blank part beside a real one is dropped rather than digested, so
	// the same context reaches the same digest however the caller padded it.
	if clickout.NewContextDigest("ua/1.0", "").String() != clickout.NewContextDigest("ua/1.0").String() {
		t.Error("a blank part changed the digest")
	}
}
