package wallet_test

// What a member's history shows, in what order, and what it refuses (T079,
// US3).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
)

// fakeEntries answers rows and records the parameters it was asked with -
// the page size above all, because reading one row more than the page is
// what lets the answer say whether there is a next one.
type fakeEntries struct {
	rows  []store.MemberEntriesRow
	err   error
	asked []store.MemberEntriesParams
}

func (f *fakeEntries) MemberEntries(_ context.Context, arg store.MemberEntriesParams) ([]store.MemberEntriesRow, error) {
	f.asked = append(f.asked, arg)
	if f.err != nil {
		return nil, f.err
	}
	if int(arg.PageSize) < len(f.rows) {
		return f.rows[:arg.PageSize], nil
	}
	return f.rows, nil
}

func history(t *testing.T, entries wallet.EntryReader) *wallet.History {
	t.Helper()
	h, err := wallet.NewHistory(entries)
	if err != nil {
		t.Fatalf("NewHistory(): %v", err)
	}
	return h
}

// aRow is one stored entry, newest-first ordering supplied by the caller.
func aRow(at time.Time, state string, minor int64) store.MemberEntriesRow {
	return store.MemberEntriesRow{
		ID:                 pgtype.UUID{Bytes: uuid.New(), Valid: true},
		State:              state,
		AmountMinor:        minor,
		Currency:           "EUR",
		CreatedAt:          pgtype.Timestamptz{Time: at, Valid: true},
		TransactedAt:       pgtype.Timestamptz{Time: at.Add(-24 * time.Hour), Valid: true},
		SaleAmountMinor:    minor * 20,
		SaleCurrency:       "EUR",
		SourceLanguageCode: pgtype.Text{String: "de", Valid: true},
	}
}

var listedAt = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

// TestAPageStopsAtItsSizeAndSaysThereIsMore. One row more than the page is
// read and not returned: it is what lets the answer carry a next cursor
// without a count, and a count over an unbounded history is work that grows
// with the member.
func TestAPageStopsAtItsSizeAndSaysThereIsMore(t *testing.T) {
	t.Parallel()

	entries := &fakeEntries{}
	for i := range 5 {
		entries.rows = append(entries.rows, aRow(listedAt.Add(-time.Duration(i)*time.Hour), "confirmed", 100))
	}

	page, err := history(t, entries).Page(t.Context(), wallet.PageRequest{Member: uuid.New(), Limit: 3})
	if err != nil {
		t.Fatalf("Page(): %v", err)
	}

	if len(page.Entries) != 3 {
		t.Fatalf("returned %d entries, want the 3 asked for", len(page.Entries))
	}
	if page.NextCursor == "" {
		t.Error("the page carries no cursor although more rows exist")
	}
	if entries.asked[0].PageSize != 4 {
		t.Errorf("read %d rows, want one more than the page", entries.asked[0].PageSize)
	}
}

// TestTheLastPageCarriesNoCursor, so a client stops by reading the answer
// rather than by making one more request to discover it.
func TestTheLastPageCarriesNoCursor(t *testing.T) {
	t.Parallel()

	entries := &fakeEntries{rows: []store.MemberEntriesRow{
		aRow(listedAt, "confirmed", 100),
		aRow(listedAt.Add(-time.Hour), "pending", 200),
	}}

	page, err := history(t, entries).Page(t.Context(), wallet.PageRequest{Member: uuid.New(), Limit: 5})
	if err != nil {
		t.Fatalf("Page(): %v", err)
	}

	if len(page.Entries) != 2 {
		t.Fatalf("returned %d entries, want 2", len(page.Entries))
	}
	if page.NextCursor != "" {
		t.Errorf("the last page carries cursor %q, want none", page.NextCursor)
	}
}

// TestTheCursorResumesAfterTheLastRowReturned, and is refused when it did
// not come from here.
func TestTheCursorResumesAfterTheLastRowReturned(t *testing.T) {
	t.Parallel()

	entries := &fakeEntries{}
	for i := range 3 {
		entries.rows = append(entries.rows, aRow(listedAt.Add(-time.Duration(i)*time.Hour), "confirmed", 100))
	}
	h := history(t, entries)

	first, err := h.Page(t.Context(), wallet.PageRequest{Member: uuid.New(), Limit: 2})
	if err != nil {
		t.Fatalf("the first page: %v", err)
	}
	if _, err := h.Page(t.Context(), wallet.PageRequest{Member: uuid.New(), Limit: 2, Cursor: first.NextCursor}); err != nil {
		t.Fatalf("the second page: %v", err)
	}

	// The position is the LAST ROW RETURNED, never the extra row read to
	// know there was more - resuming after the extra would skip it.
	last := first.Entries[len(first.Entries)-1]
	resumed := entries.asked[1]
	if !resumed.CursorAt.Valid || !resumed.CursorAt.Time.Equal(last.CreatedAt) {
		t.Errorf("resumed at %v, want the last row returned (%v)", resumed.CursorAt.Time, last.CreatedAt)
	}
	if uuid.UUID(resumed.CursorID.Bytes) != last.ID {
		t.Errorf("resumed at row %v, want %v", uuid.UUID(resumed.CursorID.Bytes), last.ID)
	}
}

// TestACursorFromSomewhereElseIsRefused. Answering the first page instead
// would loop a paging client forever.
func TestACursorFromSomewhereElseIsRefused(t *testing.T) {
	t.Parallel()

	for _, cursor := range []string{"not-base64!", "cGxhaW4", string(make([]byte, 300))} {
		_, err := history(t, &fakeEntries{}).Page(t.Context(),
			wallet.PageRequest{Member: uuid.New(), Cursor: cursor})
		if !errors.Is(err, wallet.ErrBadCursor) {
			t.Errorf("Page(%q) error = %v, want %v", cursor, err, wallet.ErrBadCursor)
		}
	}
}

// TestAnUnknownStateIsRefusedRatherThanAnsweredEmpty. An empty page reads as
// "you have earned nothing", and a member told that because of a typo would
// believe it.
func TestAnUnknownStateIsRefusedRatherThanAnsweredEmpty(t *testing.T) {
	t.Parallel()

	entries := &fakeEntries{}
	_, err := history(t, entries).Page(t.Context(), wallet.PageRequest{Member: uuid.New(), State: "confirmd"})

	if !errors.Is(err, wallet.ErrUnknownState) {
		t.Fatalf("Page() error = %v, want %v", err, wallet.ErrUnknownState)
	}
	if len(entries.asked) != 0 {
		t.Error("a misspelled state reached the database")
	}
}

// TestNoStateListsEveryState. The filter is optional, and its absence must
// reach the query as "every state" rather than as a state named "".
func TestNoStateListsEveryState(t *testing.T) {
	t.Parallel()

	entries := &fakeEntries{}
	if _, err := history(t, entries).Page(t.Context(), wallet.PageRequest{Member: uuid.New()}); err != nil {
		t.Fatalf("Page(): %v", err)
	}
	if entries.asked[0].State.Valid {
		t.Errorf("asked for state %q, want every state", entries.asked[0].State.String)
	}
}

// TestThePageSizeIsBounded. A member's history is unbounded and a page that
// could be too is a query nobody has budgeted for.
func TestThePageSizeIsBounded(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		asked int
		want  int32
	}{
		{"none asked for", 0, wallet.DefaultPageSize + 1},
		{"more than the maximum", wallet.MaxPageSize * 10, wallet.MaxPageSize + 1},
		{"a modest page", 7, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entries := &fakeEntries{}
			if _, err := history(t, entries).Page(t.Context(),
				wallet.PageRequest{Member: uuid.New(), Limit: tc.asked}); err != nil {
				t.Fatalf("Page(): %v", err)
			}
			if entries.asked[0].PageSize != tc.want {
				t.Errorf("read %d rows, want %d", entries.asked[0].PageSize, tc.want)
			}
		})
	}
}

// TestTheMerchantNameSaysWhichLanguageItIsIn. US5 scenario 2: a name in the
// merchant's own language is shown LABELLED, never passed off as the one the
// member reads - and a client cannot label what it was not told.
func TestTheMerchantNameSaysWhichLanguageItIsIn(t *testing.T) {
	t.Parallel()

	asked := aRow(listedAt, "confirmed", 100)
	asked.NameInLanguageAsked = pgtype.Text{String: "Kaufhaus", Valid: true}
	asked.NameInMerchantsLanguage = pgtype.Text{String: "Kaufhaus", Valid: true}

	fell := aRow(listedAt.Add(-time.Hour), "confirmed", 100)
	fell.NameInMerchantsLanguage = pgtype.Text{String: "Kaufhaus", Valid: true}

	// No click, so no route to a retailer: an operator attributed it by
	// hand because the network named no reference (FR-034).
	byHand := aRow(listedAt.Add(-2*time.Hour), "confirmed", 100)

	page, err := history(t, &fakeEntries{rows: []store.MemberEntriesRow{asked, fell, byHand}}).
		Page(t.Context(), wallet.PageRequest{Member: uuid.New(), Language: "el"})
	if err != nil {
		t.Fatalf("Page(): %v", err)
	}

	if got := page.Entries[0].Merchant; !got.Asked || got.Language != "el" {
		t.Errorf("the name in the language asked for came back as %+v, want el and not a fallback", got)
	}
	if got := page.Entries[1].Merchant; got.Asked || got.Language != "de" {
		t.Errorf("the fallback came back as %+v, want de and marked as a fallback", got)
	}
	if got := page.Entries[2].Merchant; got.Name != "" || got.Asked {
		t.Errorf("an entry with no click named merchant %+v, want none", got)
	}
}

// TestAHistoryIsRefusedWithoutItsParts.
func TestAHistoryIsRefusedWithoutItsParts(t *testing.T) {
	t.Parallel()

	if _, err := wallet.NewHistory(nil); !errors.Is(err, wallet.ErrNoEntryReader) {
		t.Errorf("NewHistory(nil) error = %v, want %v", err, wallet.ErrNoEntryReader)
	}
	if _, err := history(t, &fakeEntries{}).Page(t.Context(), wallet.PageRequest{}); !errors.Is(err, wallet.ErrNoMemberToList) {
		t.Errorf("Page(nobody) error = %v, want %v", err, wallet.ErrNoMemberToList)
	}
}

// TestAReadThatFailedIsNoPage, for the reason a partial wallet is no wallet.
func TestAReadThatFailedIsNoPage(t *testing.T) {
	t.Parallel()

	_, err := history(t, &fakeEntries{err: errors.New("connection reset")}).
		Page(t.Context(), wallet.PageRequest{Member: uuid.New()})

	if !errors.Is(err, wallet.ErrNotListed) {
		t.Fatalf("Page() error = %v, want one wrapping %v", err, wallet.ErrNotListed)
	}
}
