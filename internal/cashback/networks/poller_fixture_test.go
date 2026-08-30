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
