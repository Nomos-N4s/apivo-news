package wallet_test

// What an export includes, and what it refuses to do rather than include
// less (T081, FR-003).
//
// Every case here is about completeness. A page that stops short is a page;
// an export that stops short is a document a member may rely on as their
// whole record, so the walk has to reach the end and the failure to reach it
// has to be an error rather than a shorter file.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/store"
)

// pagingEntries is a reader that honours the keyset cursor, which
// fakeEntries deliberately does not: that one answers one page and the
// export walks many, so a fake that ignored the cursor would hand the same
// rows back for ever and the walk would never end.
//
// The comparison is the query's own - strictly before (created_at, id),
// newest first - so this fake pages exactly as the statement does, and a
// walk that is wrong here is wrong there.
type pagingEntries struct {
	rows  []store.MemberEntriesRow
	err   error
	calls int
	asked []store.MemberEntriesParams
}

func (p *pagingEntries) MemberEntries(_ context.Context, arg store.MemberEntriesParams) ([]store.MemberEntriesRow, error) {
	p.calls++
	p.asked = append(p.asked, arg)
	if p.err != nil {
		return nil, p.err
	}
	var after []store.MemberEntriesRow
	for _, row := range p.rows {
		if !arg.CursorAt.Valid {
			after = append(after, row)
			continue
		}
		at, cursorAt := row.CreatedAt.Time, arg.CursorAt.Time
		switch {
		case at.Before(cursorAt):
			after = append(after, row)
		case at.Equal(cursorAt) && string(row.ID.Bytes[:]) < string(arg.CursorID.Bytes[:]):
			after = append(after, row)
		}
	}
	if int(arg.PageSize) < len(after) {
		return after[:arg.PageSize], nil
	}
	return after, nil
}

// aHistory is n entries, newest first, one hour apart.
func aHistory(n int) *pagingEntries {
	entries := &pagingEntries{}
	for i := range n {
		entries.rows = append(entries.rows, aRow(listedAt.Add(-time.Duration(i)*time.Hour), "confirmed", 100))
	}
	return entries
}

func exporter(t *testing.T, entries wallet.EntryReader) *wallet.Exports {
	t.Helper()
	made, err := wallet.NewExports(history(t, entries))
	if err != nil {
		t.Fatalf("NewExports(): %v", err)
	}
	return made
}

// A history shorter than one page is one read, and the answer is all of it.
func TestAShortHistoryExportsWhole(t *testing.T) {
	t.Parallel()
	entries := aHistory(3)

	all, err := exporter(t, entries).All(t.Context(), wallet.ExportRequest{Member: uuid.New()})
	if err != nil {
		t.Fatalf("All(): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("exported %d entries, want 3", len(all))
	}
	if entries.calls != 1 {
		t.Errorf("read %d pages for 3 entries, want 1", entries.calls)
	}
}

// The case the whole file exists for: a history longer than a page must
// come back COMPLETE, not one page of it. This is what would break if the
// walk ever stopped following the cursor.
func TestALongHistoryExportsEveryPage(t *testing.T) {
	t.Parallel()
	// Two and a bit pages, so the walk has to continue past a full page
	// AND stop on a partial one.
	const total = wallet.MaxPageSize*2 + 7
	entries := aHistory(total)

	all, err := exporter(t, entries).All(t.Context(), wallet.ExportRequest{Member: uuid.New()})
	if err != nil {
		t.Fatalf("All(): %v", err)
	}
	if len(all) != total {
		t.Fatalf("exported %d entries, want all %d", len(all), total)
	}
	if entries.calls != 3 {
		t.Errorf("read %d pages, want 3", entries.calls)
	}

	// Newest first and strictly descending, with no row repeated across the
	// page boundary - which is the mistake an offset walk makes.
	seen := make(map[uuid.UUID]bool, len(all))
	for i, entry := range all {
		if seen[entry.ID] {
			t.Fatalf("entry %s appears twice; the walk repeated a row", entry.ID)
		}
		seen[entry.ID] = true
		if i > 0 && !entry.CreatedAt.Before(all[i-1].CreatedAt) {
			t.Fatalf("entry %d is not older than the one before it: %v then %v",
				i, all[i-1].CreatedAt, entry.CreatedAt)
		}
	}
}

// A history exactly one page long must not read a second page to discover
// it is finished: History.Page reads one row more than the page, so the
// cursor is empty and the walk stops.
func TestAHistoryOfExactlyOnePageStopsThere(t *testing.T) {
	t.Parallel()
	entries := aHistory(wallet.MaxPageSize)

	all, err := exporter(t, entries).All(t.Context(), wallet.ExportRequest{Member: uuid.New()})
	if err != nil {
		t.Fatalf("All(): %v", err)
	}
	if len(all) != wallet.MaxPageSize {
		t.Fatalf("exported %d entries, want %d", len(all), wallet.MaxPageSize)
	}
	if entries.calls != 1 {
		t.Errorf("read %d pages for exactly one page of history, want 1", entries.calls)
	}
}

// A member who has earned nothing exports an empty history rather than an
// error: they have a record, and it is empty.
func TestAnEmptyHistoryExportsEmpty(t *testing.T) {
	t.Parallel()

	all, err := exporter(t, &pagingEntries{}).All(t.Context(), wallet.ExportRequest{Member: uuid.New()})
	if err != nil {
		t.Fatalf("All(): %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("exported %d entries for a member who has earned nothing", len(all))
	}
}

// A read that fails part-way through stops the export. Returning what had
// been collected would be a shorter document that looks complete, which is
// the one outcome this file exists to prevent.
func TestAFailedPageStopsTheExport(t *testing.T) {
	t.Parallel()
	broken := errors.New("the database is not answering")

	_, err := exporter(t, &pagingEntries{err: broken}).All(t.Context(), wallet.ExportRequest{Member: uuid.New()})
	if err == nil {
		t.Fatal("a failed read produced an export")
	}
	if !errors.Is(err, wallet.ErrNotListed) {
		t.Errorf("All() returned %v, want wallet.ErrNotListed", err)
	}
}

func TestAnExportNeedsTheMemberItIsAbout(t *testing.T) {
	t.Parallel()

	_, err := exporter(t, &pagingEntries{}).All(t.Context(), wallet.ExportRequest{})
	if !errors.Is(err, wallet.ErrNoMemberToList) {
		t.Fatalf("All() with no member returned %v, want wallet.ErrNoMemberToList", err)
	}
}

// endlessEntries answers a full page for ever, each one older than the last,
// which is what a history with no end looks like from the walk's side.
//
// No real history is endless. What this stands in for is the walk's own
// safety: without the bound, a query that kept answering - a fault, a clock
// that went backwards, a cursor that stopped advancing - would loop until
// the process ran out of memory, with a member waiting on a download. The
// bound is what turns that into an error somebody can read.
type endlessEntries struct{ calls int }

func (e *endlessEntries) MemberEntries(_ context.Context, arg store.MemberEntriesParams) ([]store.MemberEntriesRow, error) {
	e.calls++
	rows := make([]store.MemberEntriesRow, 0, arg.PageSize)
	for i := range int(arg.PageSize) {
		rows = append(rows, aRow(listedAt.Add(-time.Duration(e.calls*1000+i)*time.Hour), "confirmed", 100))
	}
	return rows, nil
}

// A history above the bound is REFUSED, not shortened. It is the one case
// where the alternative is a file a member would believe: a CSV cut short
// has no closing bracket to be missing and no length to disagree with, so
// nothing about it says it is incomplete.
func TestAHistoryTooLargeToExportIsRefused(t *testing.T) {
	t.Parallel()
	entries := &endlessEntries{}

	_, err := exporter(t, entries).All(t.Context(), wallet.ExportRequest{Member: uuid.New()})
	if !errors.Is(err, wallet.ErrExportTooLarge) {
		t.Fatalf("All() returned %v, want wallet.ErrExportTooLarge", err)
	}
	// And it stopped near the bound rather than wandering past it: the
	// walk reads whole pages, so it may overshoot by at most one.
	if want := wallet.MaxExportEntries/wallet.MaxPageSize + 1; entries.calls > want {
		t.Errorf("read %d pages before refusing, want at most %d", entries.calls, want)
	}
}

func TestAnExporterNeedsAHistory(t *testing.T) {
	t.Parallel()

	if _, err := wallet.NewExports(nil); !errors.Is(err, wallet.ErrNoHistoryToExport) {
		t.Fatalf("NewExports(nil) returned %v, want wallet.ErrNoHistoryToExport", err)
	}
}

// unreadable is a row whose currency is not one, which the schema refuses
// and a replica lagging a migration could still hand over.
func unreadable() store.MemberEntriesRow {
	row := aRow(listedAt, "confirmed", 100)
	row.Currency = "not-a-currency"
	return row
}

// A row this module cannot read stops the export, for the reason a failed
// read does. The currency check is the last place a figure can be caught
// before a member is handed a spreadsheet of it (C-6).
func TestARowThatCannotBeReadStopsTheExport(t *testing.T) {
	t.Parallel()

	_, err := exporter(t, &pagingEntries{rows: []store.MemberEntriesRow{unreadable()}}).
		All(t.Context(), wallet.ExportRequest{Member: uuid.New()})
	if err == nil {
		t.Fatal("a row with an unreadable currency was exported")
	}
}

// The language reaches EVERY page. An export whose later pages fell back to
// another language would be one document in two languages, and the member
// would have no way to tell which rows were which.
func TestEveryPageOfTheExportAsksForTheLanguage(t *testing.T) {
	t.Parallel()
	entries := aHistory(wallet.MaxPageSize*2 + 1)

	if _, err := exporter(t, entries).All(t.Context(), wallet.ExportRequest{
		Member: uuid.New(), Language: "el",
	}); err != nil {
		t.Fatalf("All(): %v", err)
	}
	if len(entries.asked) < 3 {
		t.Fatalf("read %d pages, want at least 3", len(entries.asked))
	}
	for i, arg := range entries.asked {
		if arg.Language != "el" {
			t.Errorf("page %d asked for language %q, want el", i, arg.Language)
		}
	}
}
