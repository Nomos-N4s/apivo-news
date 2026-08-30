// The poller: one window at a time, persisted whole, with a durable cursor
// that moves only afterwards (T055, FR-031).

package networks

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
)

var (
	// ErrNoPollerStore reports a poller built with nothing to write to.
	ErrNoPollerStore = errors.New("networks: a poller needs somewhere to persist what it reads")
	// ErrNoBackfillStart reports a poller built without a first window to
	// start from. An account that has never been polled has no cursor, and
	// only an operator knows how far back this publisher account's history
	// should be ingested: a poller that guessed would either skip history
	// nobody notices is missing, or ask a network for years of it.
	ErrNoBackfillStart = errors.New("networks: a poller needs a start for an account that has never been polled")
	// ErrAccountInactive reports a poll of an account nobody has switched
	// on. An account is born inactive (migration 0011) precisely so a
	// half-configured one cannot start fetching, and honouring that is the
	// poller's job rather than the scheduler's.
	ErrAccountInactive = errors.New("networks: the publisher account is not active")
	// ErrAccountMismatch reports an adapter whose publisher account is not
	// the row it names: the id belongs to an account held elsewhere, or
	// under another publisher identifier.
	//
	// It is a poll-time check rather than a wiring one because only the row
	// can answer it. [ValidateNetwork] holds an adapter to what it says
	// about ITSELF - that its account is at its own network - and both
	// halves of that can agree while naming a row that says something else.
	ErrAccountMismatch = errors.New("networks: the adapter's publisher account is not the row it names")
	// ErrCursorMoved reports a poll whose cursor was advanced by somebody
	// else between the read that opened it and the advance that ends it.
	// Nothing was written: this poll's transaction rolls back whole.
	//
	// A second POLLER cannot produce it. The cursor read takes FOR UPDATE,
	// so a concurrent poll of the same account waits, then reads the cursor
	// where this one left it and takes the NEXT window rather than losing a
	// race - which the store package's lock test is what proves. What is
	// left is a write that took no lock: an operator moving a cursor by
	// hand, or a caller that reads without one. For those the conditional
	// advance is the difference between refusing and carrying the cursor
	// past a window nobody persisted.
	ErrCursorMoved = errors.New("networks: the cursor moved while this poll was reading")
)

// Beginner starts the transaction one window is persisted in. A *pgxpool.Pool
// satisfies it.
//
// The transaction is the whole durability argument. Every report of a window
// and the cursor advance that follows it commit together, so a crash rolls
// back both and the window is simply read again - at most one window
// re-fetched, never one skipped (FR-031). The re-read is safe because an
// unchanged report is recognised as one it has already stored (T053), which
// is why that path exists.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// CursorStore is the conditional advance one poll ends with: move this
// account's cursor, and only from where the poll found it.
//
// It is named here rather than taken as the generated store, and the reason
// is worth stating plainly. The refusal it can return - no rows, because the
// cursor is no longer where the poll read it - is unreachable through the
// locked read above, so no test that drives a poll can reach the branch that
// turns it into [ErrCursorMoved]. A branch nothing reaches is a branch
// nothing would notice being deleted, and this one is what stops a cursor
// being carried past a window nobody persisted.
type CursorStore interface {
	AdvanceNetworkAccountCursor(ctx context.Context, arg store.AdvanceNetworkAccountCursorParams) (store.AdvanceNetworkAccountCursorRow, error)
	AdvanceNetworkAccountTrailingCursor(ctx context.Context, arg store.AdvanceNetworkAccountTrailingCursorParams) (store.AdvanceNetworkAccountTrailingCursorRow, error)
}

// Poller reads a publisher account's transactions one window at a time.
//
// It does not pace its own requests. The port puts that on the adapter -
// "the adapter holds itself to it, refusing a window wider than MaxWindow
// and pacing its requests to RequestsPerSecond" - and a poller that paced as
// well would hold every adapter to a rate its own network never documented.
type Poller struct {
	db           Beginner
	backfillFrom time.Time
	trailingLag  time.Duration
	now          func() time.Time
}

// DefaultTrailingLag is how far behind the main cursor the re-read sweep
// stays. Validation takes up to 90 days (ADR-0003), so a period is re-read
// once the network has had a hundred to make up its mind about it.
const DefaultTrailingLag = 100 * 24 * time.Hour

// PollerOption configures a [Poller].
type PollerOption func(*Poller)

// WithPollerClock replaces the clock the poller bounds its windows by, which
// is what lets a test drive a year of polling without waiting for one.
func WithPollerClock(now func() time.Time) PollerOption {
	return func(p *Poller) {
		if now != nil {
			p.now = now
		}
	}
}

// WithTrailingLag replaces how far behind the main cursor the re-read sweep
// stays. A shorter lag re-reads sooner and more often; a longer one waits
// past what a network is documented to need.
func WithTrailingLag(lag time.Duration) PollerOption {
	return func(p *Poller) {
		if lag > 0 {
			p.trailingLag = lag
		}
	}
}

// NewPoller builds a poller that starts an unpolled account at backfillFrom.
func NewPoller(db Beginner, backfillFrom time.Time, opts ...PollerOption) (*Poller, error) {
	if db == nil {
		return nil, ErrNoPollerStore
	}
	if backfillFrom.IsZero() {
		return nil, ErrNoBackfillStart
	}
	p := &Poller{
		db:           db,
		backfillFrom: backfillFrom,
		trailingLag:  DefaultTrailingLag,
		now:          time.Now,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	return p, nil
}

// PollForward reads the next period nobody has read and advances the main
// cursor over it.
func (p *Poller) PollForward(ctx context.Context, adapter Network) (Poll, error) {
	return p.poll(ctx, adapter, false)
}

// PollTrailing re-reads a period the main cursor passed long enough ago that
// the network has had time to change its mind about it, and advances the
// trailing cursor over it (ADR-0003).
//
// It writes through the same path as [Poller.PollForward] and differs only in
// which window it picks and which cursor it moves. A status change found here
// supersedes; a period whose transactions have not changed writes nothing.
func (p *Poller) PollTrailing(ctx context.Context, adapter Network) (Poll, error) {
	return p.poll(ctx, adapter, true)
}

func (p *Poller) poll(ctx context.Context, adapter Network, trailing bool) (Poll, error) {
	if adapter == nil {
		return Poll{}, fmt.Errorf("%w: no adapter to read through", ErrNoPollerStore)
	}
	// The whole wiring check, on every poll, rather than the account alone.
	// The composition root has already run it, and running it again costs
	// nothing beside a network round trip - while each thing it refuses is
	// otherwise found out after a window has been fetched. Unset limits are
	// the worst of the three: a MaxWindow of zero makes every window empty,
	// so a poll reports success, stores nothing, moves the cursor nowhere,
	// and is run again forever with no error anywhere to say why.
	if err := ValidateNetwork(adapter); err != nil {
		return Poll{}, err
	}
	account := adapter.Account()

	tx, err := p.db.Begin(ctx)
	if err != nil {
		return Poll{}, fmt.Errorf("networks: %s: beginning a poll: %w", account, err)
	}
	// Rolled back unless the commit below replaces it. Every path that
	// leaves without committing - nothing to read, a network that failed, a
	// cursor somebody else moved - leaves the account exactly as it was.
	defer func() { _ = tx.Rollback(ctx) }()

	queries := store.New(tx)
	cursors, err := queries.GetNetworkAccountCursors(ctx, pgtype.UUID{Bytes: account.ID(), Valid: true})
	if err != nil {
		return Poll{}, fmt.Errorf("networks: %s: reading the cursors: %w", account, err)
	}
	// The row is the authority on whose account this is, so it is read back
	// and not merely located. An adapter carrying one account's id under
	// another network's name writes evidence whose network_id and
	// network_account_id disagree - two NOT NULL columns on an IMMUTABLE
	// row, with no key between them to notice, because network_transaction
	// references the network and the account separately (migration 0012).
	// Checked before Active, because "that account is switched off" is a
	// misleading thing to say about a row that is not the account at all.
	if cursors.NetworkID != string(account.Network()) || cursors.ExternalPublisherID != account.ExternalID() {
		return Poll{}, fmt.Errorf("%w: the adapter polls %s, and that row holds %s at %s",
			ErrAccountMismatch, account,
			strconv.Quote(cursors.ExternalPublisherID), strconv.Quote(cursors.NetworkID))
	}
	if !cursors.Active {
		return Poll{}, fmt.Errorf("%w: %s", ErrAccountInactive, account)
	}

	// One reading of the clock for the whole poll, so the window's end and
	// the instant every row records as its retrieval agree. Two readings
	// would put rows outside the window they were read for.
	at := p.now().UTC()

	window, found := p.window(cursors, at, adapter.Limits().MaxWindow, trailing)
	if !found {
		return Poll{}, nil
	}

	outcome, err := persist(ctx, queries, adapter, Retrieval{Account: account, RetrievedAt: at, Window: window})
	if err != nil {
		return Poll{}, err
	}

	if err := p.advance(ctx, queries, account, cursors, window, trailing); err != nil {
		return Poll{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Poll{}, fmt.Errorf("networks: %s: committing %s: %w", account, window, err)
	}

	return Poll{Ran: true, Window: window, Outcome: outcome, CursorAdvancedTo: window.To}, nil
}

// window picks the period this poll reads, from the cursors it found and the
// clock it read. The arithmetic itself is pure and lives in poll.go; this is
// only the choice of which of the two sweeps is walking.
func (p *Poller) window(cursors store.GetNetworkAccountCursorsRow, at time.Time, maxWindow time.Duration, trailing bool) (QueryWindow, bool) {
	if trailing {
		return nextTrailingWindow(
			cursors.TrailingCursorAt.Time, cursors.TrailingCursorAt.Valid,
			p.backfillFrom, cursors.CursorAt.Time, cursors.CursorAt.Valid,
			p.trailingLag, maxWindow)
	}
	return nextForwardWindow(
		cursors.CursorAt.Time, cursors.CursorAt.Valid,
		p.backfillFrom, at, maxWindow)
}

// persist reads the window and records every report in it, in the caller's
// transaction. It stops at the first refusal: a window half read is a window
// whose cursor must not move, and continuing past an error would leave the
// count saying more was stored than was.
func persist(ctx context.Context, queries *store.Queries, adapter Network, retrieval Retrieval) (PollOutcome, error) {
	superseder, err := NewSuperseder(queries, queries)
	if err != nil {
		return PollOutcome{}, err
	}

	seq, err := adapter.FetchTransactions(ctx, retrieval.Window)
	if err != nil {
		return PollOutcome{}, fmt.Errorf("networks: %s: %w", retrieval, err)
	}

	var outcome PollOutcome
	for report, err := range seq {
		if err != nil {
			return PollOutcome{}, fmt.Errorf("networks: %s: %w", retrieval, err)
		}
		_, what, err := superseder.Record(ctx, retrieval, report)
		if err != nil {
			return PollOutcome{}, err
		}
		switch what {
		case OutcomeFirstReport:
			outcome.FirstReports++
		case OutcomeSuperseded:
			outcome.Superseded++
		case OutcomeUnchanged:
			outcome.Unchanged++
		}
	}
	return outcome, nil
}

// advance moves the cursor this poll was walking, and only from where the
// poll found it. No rows back means the cursor is no longer where it was
// read, which nothing holding the row's lock can have caused - see
// [ErrCursorMoved] for what remains, and why the condition is worth carrying
// anyway.
func (p *Poller) advance(ctx context.Context, cursors CursorStore, account PublisherAccount, found store.GetNetworkAccountCursorsRow, window QueryWindow, trailing bool) error {
	to := pgtype.Timestamptz{Time: window.To, Valid: true}
	id := pgtype.UUID{Bytes: account.ID(), Valid: true}

	var err error
	if trailing {
		_, err = cursors.AdvanceNetworkAccountTrailingCursor(ctx, store.AdvanceNetworkAccountTrailingCursorParams{
			ID: id, AdvanceTo: to, AdvanceFrom: found.TrailingCursorAt,
		})
	} else {
		_, err = cursors.AdvanceNetworkAccountCursor(ctx, store.AdvanceNetworkAccountCursorParams{
			ID: id, AdvanceTo: to, AdvanceFrom: found.CursorAt,
		})
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %s over %s", ErrCursorMoved, account, window)
	case err != nil:
		return fmt.Errorf("networks: %s: advancing over %s: %w", account, window, err)
	}
	return nil
}
