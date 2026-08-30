// The whole ingestion chain, end to end, with no publisher account anywhere
// (T055, T050).
//
// Every other poller test drives a fake adapter, because what those cases
// assert is the poller's own behaviour and a recording would decide half of
// it. This one is the opposite: a real adapter, serving a real recorded
// response, through the real port, into the real schema. What it proves is
// that the pieces fit - that a poll of a network nobody has credentials for
// stores evidence, and that the second sweep over the same ground finds the
// change the first could not have seen.
//
// It is also the demonstration the fixture adapter exists for. Until a
// network contract is signed, this is the only place the sentence "polling
// works" is backed by something other than an intention.

package networks_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/fixture"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
)

// The recording's transactions fall in the first days of August 2026. The
// backfill starts before them and the clock reads a few days after, so one
// forward window covers the lot.
var (
	fixturePollStart = time.Date(2026, time.July, 31, 0, 0, 0, 0, time.UTC)
	fixturePollNow   = time.Date(2026, time.August, 5, 0, 0, 0, 0, time.UTC)
)

// fixturePollAccount seeds the fixture network and an account at it. The
// network id is the adapter's own constant rather than a generated one: the
// port refuses an adapter polling an account held at another network, so
// this row has to be the network the adapter says it speaks for.
func fixturePollAccount(ctx context.Context, t *testing.T, tx pgx.Tx) networks.PublisherAccount {
	t.Helper()
	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_second, active)
		values ($1, 'Fixture Network', 'clickref', 31, 6, true)`, string(fixture.ID)); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	var accountID pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active, backfill_from)
		values ($1, 'publisher-1', 'config:networks.fixture.credential', true, $2)
		returning id`, string(fixture.ID), fixturePollStart).Scan(&accountID); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}
	account, err := networks.NewPublisherAccount(uuid.UUID(accountID.Bytes), fixture.ID, "publisher-1")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}
	return account
}

func TestPollingTheFixtureNetworkEndToEnd(t *testing.T) {
	t.Parallel()
	ctx, tx := pollerSchemaConnect(t)
	account := fixturePollAccount(ctx, t, tx)

	adapter, err := fixture.New(account)
	if err != nil {
		t.Fatalf("fixture.New(): %v", err)
	}
	// A lag of an hour rather than the default hundred days, so the sweep
	// has ground to re-read inside one test rather than one quarter.
	poller, err := networks.NewPoller(savepointBeginner{tx: tx},
		networks.WithPollerClock(func() time.Time { return fixturePollNow }),
		networks.WithTrailingLag(time.Hour))
	if err != nil {
		t.Fatalf("NewPoller(): %v", err)
	}

	// The first poll: the network has reported a sale and has not yet joined
	// it to the click that produced it. Unattributed is ordinary (FR-034),
	// and the row is stored without crediting anybody.
	forward, err := poller.PollForward(ctx, adapter)
	if err != nil {
		t.Fatalf("PollForward(): %v", err)
	}
	if forward.Outcome.FirstReports != 1 || forward.Outcome.Reports() != 1 {
		t.Fatalf("the first poll reported %s, want one first report", forward.Outcome)
	}
	root := currentReport(ctx, t, tx, "FIX-1001")
	if root.ClickRef.Valid {
		t.Errorf("the first report carries the click reference %q; at this observation the network had not joined the sale to a click",
			root.ClickRef.String)
	}

	// The trailing sweep walks the same ground once the network has had time
	// to change its mind about it - which here it has: the same transaction
	// now carries the reference the redirect passed, and a second one has
	// appeared beside it.
	trailing, err := poller.PollTrailing(ctx, adapter)
	if err != nil {
		t.Fatalf("PollTrailing(): %v", err)
	}
	if trailing.Outcome.Superseded != 1 || trailing.Outcome.FirstReports != 1 {
		t.Fatalf("the sweep reported %s, want one superseded and one first report", trailing.Outcome)
	}
	if rows := storedFor(ctx, t, tx, account); rows != 3 {
		t.Errorf("%d evidence row(s) after two polls, want 3", rows)
	}

	// The attribution the click-out path matches on is now on the current
	// row, and the row it replaced is exactly where it was: C-3 means the
	// unattributed report stays as readable as the attributed one.
	attributed := currentReport(ctx, t, tx, "FIX-1001")
	if !attributed.ClickRef.Valid || attributed.ClickRef.String != fixture.RecordedClickRef {
		t.Errorf("the current row carries click reference %q, want %q", attributed.ClickRef.String, fixture.RecordedClickRef)
	}
	if uuid.UUID(attributed.SupersedesID.Bytes) != uuid.UUID(root.ID.Bytes) {
		t.Errorf("the current row supersedes %v, want the first report %v", attributed.SupersedesID, root.ID)
	}
	superseded, err := store.New(tx).GetNetworkTransaction(ctx, root.ID)
	if err != nil {
		t.Fatalf("reading the superseded row: %v", err)
	}
	if superseded.ClickRef.Valid {
		t.Error("the superseded row gained a click reference; superseding must not edit")
	}

	// The two cursors moved independently, and the trailing one stayed the
	// documented distance behind.
	cursors := cursorsOf(ctx, t, tx, account)
	if !cursors.CursorAt.Time.Equal(forward.Window.To) {
		t.Errorf("the main cursor stands at %s, want %s", cursors.CursorAt.Time, forward.Window.To)
	}
	if !cursors.TrailingCursorAt.Time.Equal(trailing.Window.To) {
		t.Errorf("the trailing cursor stands at %s, want %s", cursors.TrailingCursorAt.Time, trailing.Window.To)
	}
	if !cursors.TrailingCursorAt.Time.Before(cursors.CursorAt.Time) {
		t.Error("the trailing cursor caught the main one; it re-reads ground already read, so it can never be level with it")
	}
}

// currentReport is what the network last said about one transaction.
func currentReport(ctx context.Context, t *testing.T, tx pgx.Tx, externalID string) store.CashbackNetworkTransaction {
	t.Helper()
	row, err := store.New(tx).GetCurrentNetworkTransaction(ctx, store.GetCurrentNetworkTransactionParams{
		NetworkID:  string(fixture.ID),
		ExternalID: externalID,
	})
	if err != nil {
		t.Fatalf("GetCurrentNetworkTransaction(%s): %v", externalID, err)
	}
	return row
}

// TestPollingTheFixtureRecordsTheUnattributedWork walks the whole recorded
// lifecycle and asserts what an operator is left holding (T058, FR-034).
//
// It is the case the design turns on, and the counts are exact rather than
// "at least one": under-recording is the failure that looks exactly like a
// quiet morning.
//
// The recording contains both halves of the problem. FIX-1001 arrives with
// no click reference and GAINS one at the next observation - so a queue that
// kept it would hand an operator a transaction the network has since
// attributed itself. FIX-1002 never gains one and is restated later - so a
// queue keyed to the first report would show money the network has since
// withdrawn.
func TestPollingTheFixtureRecordsTheUnattributedWork(t *testing.T) {
	t.Parallel()
	ctx, tx := pollerSchemaConnect(t)
	account := fixturePollAccount(ctx, t, tx)

	adapter, err := fixture.New(account)
	if err != nil {
		t.Fatalf("fixture.New(): %v", err)
	}
	// A window wide enough for the whole recording, and a clock that walks
	// forward a day at a time so each poll reads a window of its own and the
	// recording advances a stage per poll.
	now := fixturePollNow
	poller, err := networks.NewPoller(savepointBeginner{tx: tx},
		networks.WithPollerClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewPoller(): %v", err)
	}

	// Four polls, one per recorded observation.
	//
	// The cursor is put back between them, and that is the honest way to
	// drive this recording rather than a shortcut. The four observations
	// describe the SAME days seen four times, months apart - which in
	// production is the trailing sweep's job, at a hundred days' remove. A
	// forward cursor walks past those days after the first poll and never
	// returns to them, so without the reset the later observations would be
	// read over empty windows and the lifecycle would never play. What is
	// under test here is what the queue does across that lifecycle; the
	// cursor arithmetic has its own tests.
	unattributed := 0
	for stage := range 4 {
		if stage > 0 {
			if _, err := tx.Exec(ctx, `update cashback.network_account set cursor_at = null where id = $1`,
				pgtype.UUID{Bytes: account.ID(), Valid: true}); err != nil {
				t.Fatalf("putting the cursor back for observation %d: %v", stage, err)
			}
		}
		poll, err := poller.PollForward(ctx, adapter)
		if err != nil {
			t.Fatalf("poll %d: %v", stage, err)
		}
		unattributed += poll.Outcome.Unattributed
		now = now.Add(24 * time.Hour)
	}

	// Three observations recorded: FIX-1001 as first seen, FIX-1002 as first
	// seen, and FIX-1002 again when the network restated it as declined.
	// FIX-1001's later reports carry a reference and are never recorded.
	if unattributed != 3 {
		t.Errorf("the polls recorded %d observation(s), want 3", unattributed)
	}
	// Counted over this fixture's own network. The count is exact, and a
	// database shared with every other integration test in the package is
	// not a database with only these rows in it - scoping is what keeps
	// "exactly three" a statement about the recording rather than about
	// what else happened to run first.
	var recorded int
	if err := tx.QueryRow(ctx, `
		select count(*)
		  from cashback.unattributed_transaction u
		  join cashback.network_transaction nt on nt.id = u.network_transaction_id
		 where nt.network_id = $1`, string(fixture.ID)).Scan(&recorded); err != nil {
		t.Fatalf("counting the recorded observations: %v", err)
	}
	if recorded != 3 {
		t.Errorf("%d observation(s) are recorded, want 3", recorded)
	}

	// And exactly one of them is still work. Nothing was deleted and nothing
	// was resolved: the other two stopped being work because a later report
	// replaced the one they name.
	queue, err := networks.NewUnattributedQueue(store.New(tx))
	if err != nil {
		t.Fatalf("NewUnattributedQueue(): %v", err)
	}
	open, err := queue.Open(ctx, networks.After{}, 100)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	open = slices.DeleteFunc(open, func(row networks.OpenReport) bool {
		return row.Network != fixture.ID
	})
	if len(open) != 1 {
		t.Fatalf("%d line(s) of work, want 1: %+v", len(open), open)
	}
	if open[0].ExternalID != "FIX-1002" {
		t.Errorf("the work is %s, want FIX-1002 - the transaction the network never attributed", open[0].ExternalID)
	}
	// The restated figures, not the withdrawn ones: the open row IS the
	// current report.
	if open[0].Status != networks.StatusDeclined || open[0].Commission.Minor != 1125 {
		t.Errorf("the work reads %s / %s, want declined and the restated 1125",
			open[0].Status, open[0].Commission)
	}
	if !open[0].Attributable {
		t.Error("a report the network attached no reference to was offered as one an operator may only dismiss")
	}

	// FIX-1001's observation is recorded and closed, with nobody named as
	// having resolved it - which is the whole point of deriving the work
	// rather than editing the row.
	var resolved int
	if err := tx.QueryRow(ctx, `
		select count(*)
		  from cashback.unattributed_transaction u
		  join cashback.network_transaction nt on nt.id = u.network_transaction_id
		 where nt.network_id = $1 and u.resolved_at is not null`, string(fixture.ID)).Scan(&resolved); err != nil {
		t.Fatalf("counting resolutions: %v", err)
	}
	if resolved != 0 {
		t.Errorf("%d observation(s) were resolved, want 0; the network attributing a transaction is not an operator's decision", resolved)
	}
}
