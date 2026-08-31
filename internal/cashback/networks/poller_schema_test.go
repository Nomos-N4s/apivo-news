// The poller against the database that arbitrates it (T055).
//
// Everything asserted here is a property of the transaction rather than of
// the Go: that a window half read leaves the cursor and the evidence exactly
// as they were, that a re-read of ground already covered writes nothing, and
// that the two cursors move independently. None of it can be shown against a
// fake, because the fake would be agreeing with whatever this file believed
// the schema did.
//
// Every case runs inside a savepoint of the test's own transaction, and the
// poller's transactions are savepoints inside that - which is exactly what
// makes a rolled-back poll observable: the case can look at the account
// afterwards and see it untouched. Nothing here commits, so the suite leaves
// the database as it found it.
//
// What is deliberately NOT here is two pollers running at once. A savepoint
// cannot contend with itself, and the property that makes concurrency safe -
// the cursor read taking FOR UPDATE, so the second poller waits and then
// reads the cursor where the first left it - is proved with two real
// connections in the store package, next to the query that states it.

package networks_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// The instants every case below is measured from. The backfill start is what
// an operator would have configured; the clock is far enough past it that a
// first window is a whole one rather than a stub.
var (
	pollerSchemaStart = time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	pollerSchemaNow   = time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
)

const pollerSchemaMaxWindow = 31 * 24 * time.Hour

// savepointBeginner is the poller's [networks.Beginner] over a transaction
// the test owns. pgx spells a nested Begin as a savepoint, so a poll commits
// for real as far as the poller and the schema are concerned, and the test
// still throws everything away at the end.
type savepointBeginner struct{ tx pgx.Tx }

func (b savepointBeginner) Begin(ctx context.Context) (pgx.Tx, error) { return b.tx.Begin(ctx) }

// pollerSchemaConnect migrates and opens the pool, or skips.
func pollerSchemaConnect(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the poller")
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

// pollerSchemaAccount seeds a network and an active publisher account at it.
// Each call makes its own network, so two cases can report the same external
// transaction id without meeting.
func pollerSchemaAccount(ctx context.Context, t *testing.T, tx pgx.Tx) networks.PublisherAccount {
	t.Helper()

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	// network.id is constrained to ^[a-z][a-z0-9_]*$, so the suffix is
	// lower-case hex behind a letter.
	networkID := "fixture_" + hex.EncodeToString(suffix)

	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ($1, 'Poller Network', 'clickref', 31, 360, true)`, networkID); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	var accountID pgtype.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active, backfill_from)
		values ($1, 'publisher-1', 'config:networks.fixture.credential', true, $2)
		returning id`, networkID, pollerSchemaStart).Scan(&accountID); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}

	account, err := networks.NewPublisherAccount(uuid.UUID(accountID.Bytes), networks.NetworkID(networkID), "publisher-1")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}
	return account
}

// eachPoll runs one case inside a savepoint of its own, so a case that
// provokes a refusal leaves the outer transaction usable for the next.
func eachPoll(ctx context.Context, t *testing.T, tx pgx.Tx, name string, scenario func(t *testing.T, tx pgx.Tx)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		sub, err := tx.Begin(ctx)
		if err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		defer func() { _ = sub.Rollback(ctx) }()
		scenario(t, sub)
	})
}

// pollerSchemaPoller builds a poller over the case's transaction, at the
// fixed clock every case reads from.
func pollerSchemaPoller(t *testing.T, tx pgx.Tx, opts ...networks.PollerOption) *networks.Poller {
	t.Helper()
	opts = append([]networks.PollerOption{networks.WithPollerClock(func() time.Time { return pollerSchemaNow })}, opts...)
	poller, err := networks.NewPoller(savepointBeginner{tx: tx}, opts...)
	if err != nil {
		t.Fatalf("NewPoller(): %v", err)
	}
	return poller
}

// cursorsOf reads where an account has got to.
func cursorsOf(ctx context.Context, t *testing.T, tx pgx.Tx, account networks.PublisherAccount) store.GetNetworkAccountCursorsRow {
	t.Helper()
	row, err := store.New(tx).GetNetworkAccountCursors(ctx, pgtype.UUID{Bytes: account.ID(), Valid: true})
	if err != nil {
		t.Fatalf("reading the cursors: %v", err)
	}
	return row
}

// storedFor counts the evidence rows one account's polls have produced.
func storedFor(ctx context.Context, t *testing.T, tx pgx.Tx, account networks.PublisherAccount) int {
	t.Helper()
	var rows int
	if err := tx.QueryRow(ctx,
		`select count(*) from cashback.network_transaction where network_account_id = $1`,
		pgtype.UUID{Bytes: account.ID(), Valid: true}).Scan(&rows); err != nil {
		t.Fatalf("counting the evidence: %v", err)
	}
	return rows
}

// pollerTestReports is the script for a case whose answer does not change:
// the same reports for every window, and nothing ever fails.
func pollerTestReports(reports ...networks.Reported) func(int, networks.QueryWindow) ([]networks.Reported, error) {
	return func(int, networks.QueryWindow) ([]networks.Reported, error) { return reports, nil }
}

// pollerTestReport builds one report of the shape an adapter yields, already
// through Reported.Validate. No click reference: an unattributed transaction
// is an ordinary report (FR-034), and the poller stores it without crediting
// anybody.
func pollerTestReport(t *testing.T, externalID string, status networks.Status, raw string, at time.Time, commissionMinor int64) networks.Reported {
	t.Helper()
	sale, err := money.New(4999, money.Currency("EUR"))
	if err != nil {
		t.Fatalf("sale amount: %v", err)
	}
	commission, err := money.New(commissionMinor, money.Currency("EUR"))
	if err != nil {
		t.Fatalf("commission: %v", err)
	}
	return networks.Reported{
		ExternalID:   externalID,
		StatusRaw:    raw,
		Status:       status,
		SaleAmount:   sale,
		Commission:   commission,
		TransactedAt: at,
		RawPayload:   json.RawMessage(fmt.Sprintf(`{"transaction_id":%q,"status":%q}`, externalID, raw)),
	}
}

// The two transactions every case reports, both inside the first window a
// poll of these accounts asks for.
func pollerSchemaPair(t *testing.T) (networks.Reported, networks.Reported) {
	t.Helper()
	return pollerTestReport(t, "FIX-1001", networks.StatusPending, "pending", pollerSchemaStart.Add(96*time.Hour), 499),
		pollerTestReport(t, "FIX-1002", networks.StatusPending, "pending", pollerSchemaStart.Add(120*time.Hour), 900)
}

func TestPollForwardAgainstTheRealSchema(t *testing.T) {
	t.Parallel()
	ctx, tx := pollerSchemaConnect(t)
	first, second := pollerSchemaPair(t)

	eachPoll(ctx, t, tx, "a first poll starts where the operator said to", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		adapter := pollerTestNetwork(account, pollerTestReports(first, second))

		poll, err := pollerSchemaPoller(t, tx).PollForward(ctx, adapter)
		if err != nil {
			t.Fatalf("PollForward(): %v", err)
		}
		if !poll.Ran {
			t.Fatal("a never-polled account had nothing to read; its whole history is unread")
		}
		if !poll.Window.From.Equal(pollerSchemaStart) {
			t.Errorf("the first window starts at %s, want the backfill start %s", poll.Window.From, pollerSchemaStart)
		}
		if got := poll.Window.Duration(); got != pollerSchemaMaxWindow {
			t.Errorf("the first window spans %s, want the network's maximum %s", got, pollerSchemaMaxWindow)
		}
		if poll.Outcome.FirstReports != 2 || poll.Outcome.Reports() != 2 {
			t.Errorf("the poll reported %s, want 2 first reports", poll.Outcome)
		}
		if rows := storedFor(ctx, t, tx, account); rows != 2 {
			t.Errorf("%d evidence row(s) were stored, want 2", rows)
		}

		cursors := cursorsOf(ctx, t, tx, account)
		if !cursors.CursorAt.Time.Equal(poll.Window.To) || !poll.CursorAdvancedTo.Equal(poll.Window.To) {
			t.Errorf("the cursor stands at %s and the poll says %s, want the window's end %s",
				cursors.CursorAt.Time, poll.CursorAdvancedTo, poll.Window.To)
		}
		if cursors.TrailingCursorAt.Valid {
			t.Errorf("a forward poll moved the trailing cursor to %s", cursors.TrailingCursorAt.Time)
		}
	})

	eachPoll(ctx, t, tx, "the next poll starts where the last one stopped", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		adapter := pollerTestNetwork(account, pollerTestReports(first, second))
		poller := pollerSchemaPoller(t, tx)

		one, err := poller.PollForward(ctx, adapter)
		if err != nil {
			t.Fatalf("the first poll: %v", err)
		}
		two, err := poller.PollForward(ctx, adapter)
		if err != nil {
			t.Fatalf("the second poll: %v", err)
		}
		if !two.Window.From.Equal(one.Window.To) {
			t.Errorf("the second window starts at %s and the first ended at %s; a gap is a period nobody will ever read again, an overlap is a period paid for twice",
				two.Window.From, one.Window.To)
		}
		if len(adapter.windows) != 2 {
			t.Fatalf("the network was asked for %d window(s), want 2", len(adapter.windows))
		}
		// The reports are outside the second window, so the network
		// answering with them again writes nothing: the same facts under
		// the same transaction id are a re-report, whatever window they
		// arrived in.
		if two.Outcome.FirstReports != 0 || two.Outcome.Unchanged != 2 {
			t.Errorf("the second poll reported %s, want 2 unchanged", two.Outcome)
		}
		if rows := storedFor(ctx, t, tx, account); rows != 2 {
			t.Errorf("%d evidence row(s) after two polls, want 2", rows)
		}
		if got := cursorsOf(ctx, t, tx, account).CursorAt.Time; !got.Equal(two.Window.To) {
			t.Errorf("the cursor stands at %s, want %s", got, two.Window.To)
		}
	})

	eachPoll(ctx, t, tx, "a poll never asks for more than the network allows", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		adapter := pollerTestNetwork(account, pollerTestReports())
		// A network that answers a day at a time. The adapter refuses a
		// wider window itself (contract rule 3), so a poller that split the
		// backfill wrongly fails here rather than silently reading less
		// than it thought.
		adapter.limits = networks.Limits{MaxWindow: 24 * time.Hour, RequestsPerMinute: 360}

		poll, err := pollerSchemaPoller(t, tx).PollForward(ctx, adapter)
		if err != nil {
			t.Fatalf("PollForward(): %v", err)
		}
		if got := poll.Window.Duration(); got != 24*time.Hour {
			t.Errorf("the window spans %s, want the day this network allows", got)
		}
	})

	eachPoll(ctx, t, tx, "a cursor that has caught up with the present reads nothing", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		adapter := pollerTestNetwork(account, pollerTestReports(first, second))
		// The clock reads the backfill start: there is no elapsed period to
		// ask about yet.
		poller := pollerSchemaPoller(t, tx, networks.WithPollerClock(func() time.Time { return pollerSchemaStart }))

		poll, err := poller.PollForward(ctx, adapter)
		if err != nil {
			t.Fatalf("a poll with nothing to read reported a failure: %v", err)
		}
		if poll.Ran || !poll.CursorAdvancedTo.IsZero() {
			t.Errorf("the poll reported %+v, want a poll that did not run", poll)
		}
		if len(adapter.windows) != 0 {
			t.Errorf("the network was asked for %v; a caught-up cursor should cost no request", adapter.windows)
		}
		if cursorsOf(ctx, t, tx, account).CursorAt.Valid {
			t.Error("a poll that read nothing moved the cursor")
		}
	})

	eachPoll(ctx, t, tx, "an account nobody has switched on is not polled", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		if _, err := tx.Exec(ctx, `update cashback.network_account set active = false where id = $1`,
			pgtype.UUID{Bytes: account.ID(), Valid: true}); err != nil {
			t.Fatalf("switching the account off: %v", err)
		}
		adapter := pollerTestNetwork(account, pollerTestReports(first, second))

		_, err := pollerSchemaPoller(t, tx).PollForward(ctx, adapter)
		if !errors.Is(err, networks.ErrAccountInactive) {
			t.Fatalf("PollForward() = %v, want one wrapping ErrAccountInactive", err)
		}
		if len(adapter.windows) != 0 {
			t.Errorf("the network was asked for %v; an account nobody switched on is one nobody may fetch for", adapter.windows)
		}
		if cursorsOf(ctx, t, tx, account).CursorAt.Valid {
			t.Error("an inactive account's cursor moved")
		}
	})

	eachPoll(ctx, t, tx, "an account that does not say how far back to read is refused", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		if _, err := tx.Exec(ctx, `update cashback.network_account set backfill_from = null where id = $1`,
			pgtype.UUID{Bytes: account.ID(), Valid: true}); err != nil {
			t.Fatalf("clearing the backfill start: %v", err)
		}
		adapter := pollerTestNetwork(account, pollerTestReports(first, second))

		_, err := pollerSchemaPoller(t, tx).PollForward(ctx, adapter)
		if !errors.Is(err, networks.ErrNoBackfillStart) {
			t.Fatalf("PollForward() = %v, want one wrapping ErrNoBackfillStart", err)
		}
		if len(adapter.windows) != 0 {
			t.Errorf("the network was asked for %v before anybody had said where to start", adapter.windows)
		}
	})

	eachPoll(ctx, t, tx, "an account whose start has not happened yet is refused, not left idle", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		adapter := pollerTestNetwork(account, pollerTestReports(first, second))
		// A mistyped year. The forward sweep never ends a window in the
		// future, so without this refusal the account would report "nothing
		// to read" on this tick and every tick after it - silence that
		// looks exactly like an account with no new transactions.
		before := pollerSchemaPoller(t, tx,
			networks.WithPollerClock(func() time.Time { return pollerSchemaStart.Add(-time.Hour) }))

		_, err := before.PollForward(ctx, adapter)
		if !errors.Is(err, networks.ErrBackfillStartInFuture) {
			t.Fatalf("PollForward() = %v, want one wrapping ErrBackfillStartInFuture", err)
		}
		if errors.Is(err, networks.ErrNoBackfillStart) {
			t.Error("a start in the future reads as a missing one; they are different mistakes with different fixes")
		}
		if len(adapter.windows) != 0 {
			t.Errorf("the network was asked for %v", adapter.windows)
		}
	})

	eachPoll(ctx, t, tx, "an adapter that is not the account it names is refused", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)

		// The right row, the wrong publisher identifier at it. Nothing at
		// wiring can catch this: the adapter's own story is consistent.
		misnamed, err := networks.NewPublisherAccount(account.ID(), account.Network(), "publisher-2")
		if err != nil {
			t.Fatalf("NewPublisherAccount(): %v", err)
		}
		adapter := pollerTestNetwork(misnamed, pollerTestReports(first, second))
		if _, err := pollerSchemaPoller(t, tx).PollForward(ctx, adapter); !errors.Is(err, networks.ErrAccountMismatch) {
			t.Fatalf("PollForward() = %v, want one wrapping ErrAccountMismatch", err)
		}

		// The right row, filed under another network. This is the one that
		// would write evidence whose network_id and network_account_id
		// disagree, on an immutable row, with no key between them.
		elsewhere, err := networks.NewPublisherAccount(account.ID(), networks.NetworkID("another"), "publisher-1")
		if err != nil {
			t.Fatalf("NewPublisherAccount(): %v", err)
		}
		crossed := pollerTestNetwork(elsewhere, pollerTestReports(first, second))
		if _, err := pollerSchemaPoller(t, tx).PollForward(ctx, crossed); !errors.Is(err, networks.ErrAccountMismatch) {
			t.Fatalf("PollForward() = %v, want one wrapping ErrAccountMismatch", err)
		}

		if len(adapter.windows)+len(crossed.windows) != 0 {
			t.Error("a mis-wired adapter was asked for a window")
		}
		if storedFor(ctx, t, tx, account) != 0 || cursorsOf(ctx, t, tx, account).CursorAt.Valid {
			t.Error("a mis-wired adapter wrote evidence or moved a cursor")
		}
	})

	eachPoll(ctx, t, tx, "a window half read leaves the cursor and the evidence exactly as they were", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		// The first read yields one report and then reports the network
		// gone, which is what an adapter does when it stops before the
		// answer is whole (contract rule 8). The second answers properly.
		broken := errors.New("the network stopped answering")
		adapter := pollerTestNetwork(account, func(call int, _ networks.QueryWindow) ([]networks.Reported, error) {
			if call == 0 {
				return []networks.Reported{first}, broken
			}
			return []networks.Reported{first, second}, nil
		})
		poller := pollerSchemaPoller(t, tx)

		if _, err := poller.PollForward(ctx, adapter); !errors.Is(err, broken) {
			t.Fatalf("PollForward() = %v, want one wrapping the network's failure", err)
		}
		// The report that WAS persisted before the failure is gone with the
		// transaction. This is the whole of FR-031's guarantee: a poll is
		// all or nothing, so the window is read again rather than resumed
		// from a point no cursor can name.
		if rows := storedFor(ctx, t, tx, account); rows != 0 {
			t.Errorf("%d evidence row(s) survived a half-read window, want 0", rows)
		}
		if cursorsOf(ctx, t, tx, account).CursorAt.Valid {
			t.Fatal("the cursor moved over a window that was never fully read; the rest of that window would never be read again")
		}

		poll, err := poller.PollForward(ctx, adapter)
		if err != nil {
			t.Fatalf("the poll after the failure: %v", err)
		}
		if len(adapter.windows) != 2 || !adapter.windows[1].From.Equal(adapter.windows[0].From) || !adapter.windows[1].To.Equal(adapter.windows[0].To) {
			t.Fatalf("the network was asked for %v, want the same window twice", adapter.windows)
		}
		if poll.Outcome.FirstReports != 2 {
			t.Errorf("the poll after the failure reported %s, want both transactions as first reports", poll.Outcome)
		}
		if rows := storedFor(ctx, t, tx, account); rows != 2 {
			t.Errorf("%d evidence row(s) after the re-read, want 2", rows)
		}
	})
}

func TestPollTrailingAgainstTheRealSchema(t *testing.T) {
	t.Parallel()
	ctx, tx := pollerSchemaConnect(t)
	first, second := pollerSchemaPair(t)

	// Short enough that the first forward window has already aged past it,
	// so a case can watch the sweep work without waiting a hundred days.
	const lag = 24 * time.Hour

	eachPoll(ctx, t, tx, "there is nothing to re-read before the account has been polled forward", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		adapter := pollerTestNetwork(account, pollerTestReports(first, second))

		poll, err := pollerSchemaPoller(t, tx, networks.WithTrailingLag(lag)).PollTrailing(ctx, adapter)
		if err != nil {
			t.Fatalf("PollTrailing(): %v", err)
		}
		if poll.Ran {
			t.Errorf("the trailing sweep read %s of history nobody has read forward yet", poll.Window)
		}
		if len(adapter.windows) != 0 {
			t.Errorf("the network was asked for %v", adapter.windows)
		}
	})

	eachPoll(ctx, t, tx, "a polled account that lost its start is refused rather than re-read from nothing", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		adapter := pollerTestNetwork(account, pollerTestReports(first, second))
		poller := pollerSchemaPoller(t, tx, networks.WithTrailingLag(lag))
		if _, err := poller.PollForward(ctx, adapter); err != nil {
			t.Fatalf("the forward poll: %v", err)
		}

		// The state a row added before 0023 would be in: polled forward,
		// with no start. The trailing sweep walks FROM the start until its
		// own cursor exists, so without this refusal it would re-read from
		// the zero instant - two thousand years of empty windows, four a
		// day, reporting success.
		if _, err := tx.Exec(ctx, `update cashback.network_account set backfill_from = null where id = $1`,
			pgtype.UUID{Bytes: account.ID(), Valid: true}); err != nil {
			t.Fatalf("clearing the backfill start: %v", err)
		}
		if _, err := poller.PollTrailing(ctx, adapter); !errors.Is(err, networks.ErrNoBackfillStart) {
			t.Fatalf("PollTrailing() = %v, want one wrapping ErrNoBackfillStart", err)
		}
	})

	eachPoll(ctx, t, tx, "the sweep re-reads aged ground and moves only its own cursor", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		adapter := pollerTestNetwork(account, pollerTestReports(first, second))
		poller := pollerSchemaPoller(t, tx, networks.WithTrailingLag(lag))

		forward, err := poller.PollForward(ctx, adapter)
		if err != nil {
			t.Fatalf("the forward poll: %v", err)
		}

		poll, err := poller.PollTrailing(ctx, adapter)
		if err != nil {
			t.Fatalf("PollTrailing(): %v", err)
		}
		if !poll.Ran {
			t.Fatal("the sweep found nothing to re-read, and the forward cursor had passed a month of ground")
		}
		if !poll.Window.From.Equal(pollerSchemaStart) {
			t.Errorf("the sweep starts at %s, want the backfill start %s", poll.Window.From, pollerSchemaStart)
		}
		if horizon := forward.Window.To.Add(-lag); poll.Window.To.After(horizon) {
			t.Errorf("the sweep read up to %s, which is inside the %s the network is given to change its mind", poll.Window.To, lag)
		}
		// The whole point of the re-read: the same facts again cost nothing.
		if poll.Outcome.Unchanged != 2 || poll.Outcome.Changed() {
			t.Errorf("re-reading the same reports produced %s, want 2 unchanged", poll.Outcome)
		}
		if rows := storedFor(ctx, t, tx, account); rows != 2 {
			t.Errorf("%d evidence row(s) after the re-read, want the 2 the forward poll stored", rows)
		}

		cursors := cursorsOf(ctx, t, tx, account)
		if !cursors.CursorAt.Time.Equal(forward.Window.To) {
			t.Errorf("the trailing sweep moved the main cursor to %s, want it left at %s", cursors.CursorAt.Time, forward.Window.To)
		}
		if !cursors.TrailingCursorAt.Time.Equal(poll.Window.To) {
			t.Errorf("the trailing cursor stands at %s, want the window's end %s", cursors.TrailingCursorAt.Time, poll.Window.To)
		}

		// And it does not spin: the ground it has covered is behind it.
		again, err := poller.PollTrailing(ctx, adapter)
		if err != nil {
			t.Fatalf("the second sweep: %v", err)
		}
		if again.Ran {
			t.Errorf("the sweep re-read %s immediately, and nothing there has aged since", again.Window)
		}
	})

	eachPoll(ctx, t, tx, "a change the sweep finds supersedes what was stored", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		// The transaction the advertiser validated between the two reads,
		// which is why the trailing sweep exists at all.
		confirmed := first
		confirmed.StatusRaw = "validated"
		confirmed.Status = networks.StatusConfirmed
		adapter := pollerTestNetwork(account, func(call int, _ networks.QueryWindow) ([]networks.Reported, error) {
			if call == 0 {
				return []networks.Reported{first, second}, nil
			}
			return []networks.Reported{confirmed, second}, nil
		})
		poller := pollerSchemaPoller(t, tx, networks.WithTrailingLag(lag))

		if _, err := poller.PollForward(ctx, adapter); err != nil {
			t.Fatalf("the forward poll: %v", err)
		}
		poll, err := poller.PollTrailing(ctx, adapter)
		if err != nil {
			t.Fatalf("PollTrailing(): %v", err)
		}
		if poll.Outcome.Superseded != 1 || poll.Outcome.Unchanged != 1 || poll.Outcome.FirstReports != 0 {
			t.Errorf("the sweep reported %s, want one superseded and one unchanged", poll.Outcome)
		}
		if !poll.Outcome.Changed() {
			t.Error("a status change reported that nothing changed; the work that follows a poll would not run")
		}
		if rows := storedFor(ctx, t, tx, account); rows != 3 {
			t.Errorf("%d evidence row(s) after the change, want 3 - the two first reports and the new row that supersedes one of them", rows)
		}

		current, err := store.New(tx).GetCurrentNetworkTransaction(ctx, store.GetCurrentNetworkTransactionParams{
			NetworkID:  string(account.Network()),
			ExternalID: first.ExternalID,
		})
		if err != nil {
			t.Fatalf("GetCurrentNetworkTransaction(): %v", err)
		}
		if current.Status != string(networks.StatusConfirmed) || current.StatusRaw != "validated" {
			t.Errorf("the current row reads %s/%s, want the confirmation the sweep found", current.Status, current.StatusRaw)
		}
	})
}
