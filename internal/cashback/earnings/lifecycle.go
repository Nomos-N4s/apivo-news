// The crediting job: what the sweeps ingested, turned into what a member is
// owed, on a schedule (#435, FR-040, FR-043, SC-010).
//
// Every step of the lifecycle existed before this file, and nothing ran it.
// The matcher (T067), the share (T068), the state machine (T069), the
// confirmer (T070), the reversal (T071) and the hold rules (T118) were each
// proved against the schema by the journey tests - and the journey was their
// only caller. A report the sweeps stored sat as evidence for ever: no entry
// was opened outside a test, so no member was owed anything.
//
// This is the caller. One job, three passes in order: credit what is
// reported and not yet owed, confirm what the network approved and a
// statement covers, reverse what the network took back. Each item runs in
// its own transaction, so one report the rules cannot judge or one entry
// the ledger refuses is logged and left for the next run instead of rolling
// back everything beside it. The fleet-wide lock the scheduler takes under
// the job's name is what makes "read what is waiting, then act on each"
// safe: no second instance is reading the same rows.
//
// Nothing here decides money. The reads say what is waiting (lifecycle.sql),
// and every decision - whose click, what share, which state, whether a
// statement covers it, where the money comes back from - is made by the
// file that owned it before this one existed.

package earnings

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	clickoutstore "github.com/Nomos-N4s/apivo-news/internal/cashback/clickout/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

const (
	// LifecycleJobName identifies the job in the scheduler and names the
	// fleet-wide lock that keeps two instances from crediting the same
	// report at once.
	LifecycleJobName = "cashback-earnings-lifecycle"
	// LifecycleInterval is how long a report the sweeps stored waits before
	// it is money a member can see. The forward sweep runs every fifteen
	// minutes, so anything shorter than this buys nothing a member would
	// notice, and anything much longer makes "reported" and "credited" two
	// visibly different moments.
	LifecycleInterval = 5 * time.Minute
	// lifecycleTimeout bounds one run. Every item is one short transaction
	// against the local database plus, under the Blnk driver, one call to
	// the ledger, so a run that is still going after this is a run that is
	// stuck - and a stuck run must not hold the lock until the process dies.
	lifecycleTimeout = 10 * time.Minute
	// lifecycleBatch is how many items one pass takes. A page rather than
	// everything, so a backlog after an outage is worked down in bounded
	// runs; what a run does not reach, the next one reads first, because
	// every read orders by age.
	lifecycleBatch = 500
)

var (
	// ErrLifecycleNotConfigured reports a job built without one of its
	// parts. Refused at construction: a job that discovered it mid-run
	// would have read reports it cannot act on.
	ErrLifecycleNotConfigured = errors.New("earnings: the lifecycle job cannot run")
	// ErrLifecycleIncomplete reports a run that left work undone - a read
	// that failed, or items that could not be acted on. Every item's own
	// failure is logged where it happened, with its id; this is the one
	// line the scheduler logs saying the run as a whole was not clean.
	ErrLifecycleIncomplete = errors.New("earnings: the lifecycle run left work undone")
)

// Beginner opens the transaction each item runs in. Defined here per the
// boundary rules - the consumer names its dependency - and satisfied by
// *pgxpool.Pool, and by a pgx.Tx for a test that wants everything rolled
// back.
type Beginner interface {
	store.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Lifecycle runs the three passes on a schedule.
type Lifecycle struct {
	log        *slog.Logger
	db         Beginner
	ledger     wallet.Ledger
	receivable string
	rules      HoldRules
	now        func() time.Time
}

// NewLifecycle assembles the job, refusing one that could not run.
//
// The ledger is taken rather than built because the wallet, the withdrawal
// path and this job must post in THE SAME ONE - under the in-process driver
// a second ledger would be a second set of balances, credited here and
// invisible to the member. The rules may be the zero value, which runs no
// rule and opens every credit pending; a set that cannot run is refused
// here, with the field named, rather than on the first report.
func NewLifecycle(log *slog.Logger, db Beginner, ledger wallet.Ledger, receivable string, rules HoldRules) (*Lifecycle, error) {
	switch {
	case log == nil:
		return nil, fmt.Errorf("%w: no logger, and a credit nobody hears about failing is a member nobody pays", ErrLifecycleNotConfigured)
	case db == nil:
		return nil, fmt.Errorf("%w: no database to read reports from", ErrLifecycleNotConfigured)
	case ledger == nil:
		return nil, fmt.Errorf("%w: no ledger to post credits in", ErrLifecycleNotConfigured)
	case receivable == "":
		return nil, fmt.Errorf("%w: no house account to pay earnings out of", ErrLifecycleNotConfigured)
	}
	if err := rules.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLifecycleNotConfigured, err)
	}
	return &Lifecycle{log: log, db: db, ledger: ledger, receivable: receivable, rules: rules, now: time.Now}, nil
}

// Rules is the configured hold rules, for the start-up line that says which
// run.
func (l *Lifecycle) Rules() HoldRules { return l.rules }

// Register puts the job on the scheduler.
func (l *Lifecycle) Register(jobs *scheduler.Scheduler) error {
	return jobs.Register(scheduler.Job{
		Name:     LifecycleJobName,
		Interval: LifecycleInterval,
		Timeout:  lifecycleTimeout,
		Run: func(ctx context.Context) error {
			_, err := l.Run(ctx)
			return err
		},
	})
}

// Outcome is what one run did, counted the way the log line reports it.
type Outcome struct {
	// Credited entries opened pending, and Held entries opened held by a
	// rule. Queued reports matched no click and went to the unattributed
	// queue instead (FR-034).
	Credited, Held, Queued int
	// Confirmed entries moved to confirmed. Awaiting entries the network
	// approved that no statement covers yet - ordinary, and the half of
	// FR-043 an operator acts on by importing one.
	Confirmed, Awaiting int
	// Reversed credits the network took back and this run undid.
	Reversed int
	// Failed items this run could not act on. Each is logged with its id
	// where it failed, and each is read again next run.
	Failed int
}

// moved reports whether the run changed anything worth an Info line.
func (o Outcome) moved() bool {
	return o.Credited+o.Held+o.Queued+o.Confirmed+o.Reversed > 0
}

// Run makes the three passes and answers what they did.
//
// The order is credit, confirm, reverse, and it matters once: a report
// ingested already confirmed under a statement already imported becomes a
// pending entry in the first pass and a confirmed one in the second, in the
// same run, rather than sitting pending for an interval to say nothing. A
// read that fails ends the run, because a pass that cannot see what is
// waiting cannot act on it; an item that fails does not, because the ones
// beside it can.
func (l *Lifecycle) Run(ctx context.Context) (Outcome, error) {
	var out Outcome
	for _, pass := range []func(context.Context, *Outcome) error{l.credit, l.confirm, l.reverse} {
		if err := pass(ctx, &out); err != nil {
			return out, err
		}
	}
	attrs := []any{
		"job", LifecycleJobName,
		"credited", out.Credited, "held", out.Held, "queued", out.Queued,
		"confirmed", out.Confirmed, "awaiting_statement", out.Awaiting,
		"reversed", out.Reversed, "failed", out.Failed,
	}
	if out.moved() {
		l.log.InfoContext(ctx, "earnings lifecycle ran", attrs...)
	} else {
		l.log.DebugContext(ctx, "earnings lifecycle ran", attrs...)
	}
	if out.Failed > 0 {
		return out, fmt.Errorf("%w: %d items failed, each logged with its id", ErrLifecycleIncomplete, out.Failed)
	}
	return out, nil
}

// credit turns each report awaiting credit into an entry, or a queue row.
func (l *Lifecycle) credit(ctx context.Context, out *Outcome) error {
	waiting, err := store.New(l.db).ReportsAwaitingCredit(ctx, lifecycleBatch)
	if err != nil {
		return fmt.Errorf("%w: reading the reports awaiting credit: %w", ErrLifecycleIncomplete, err)
	}
	l.pageFull(ctx, "reports awaiting credit", len(waiting))
	for _, report := range waiting {
		if err := ctx.Err(); err != nil {
			return err
		}
		reportID := uuid.UUID(report.ID.Bytes)
		outcome, err := l.creditOne(ctx, report)
		switch {
		case err != nil:
			out.Failed++
			l.log.ErrorContext(ctx, "the report could not be credited",
				"report", reportID, "network", report.NetworkID, "error", err)
		case outcome.queued:
			out.Queued++
		case outcome.nothingOwed:
			// A commission this small at this share rounds to nothing even
			// in the member's favour. Not a failure and not a credit; said
			// once per run because the report is read again next run.
			l.log.InfoContext(ctx, "the report earns the member nothing at the promised share",
				"report", reportID, "network", report.NetworkID,
				"commission_minor", report.CommissionMinor, "currency", report.Currency)
		case outcome.entry.State == StateHeld:
			out.Held++
		default:
			out.Credited++
		}
	}
	return nil
}

// crediting is what one report became: an entry, a queue row, or nothing.
type crediting struct {
	entry       Entry
	queued      bool
	nothingOwed bool
}

// creditOne runs the credit path for one report in one transaction: match
// the reference to its click, find the brand that owes it, divide the
// commission at the click-time share, ask the hold rules, open the entry.
//
// Everything below the match is the production code path of the journey
// test, called in the journey's order. The matcher queues a reference that
// names no click and that is the whole of what happens to it (FR-034): the
// report leaves the awaiting set by being in the queue, and an operator
// decides the rest.
func (l *Lifecycle) creditOne(ctx context.Context, report store.ReportsAwaitingCreditRow) (crediting, error) {
	reportID := uuid.UUID(report.ID.Bytes)
	commission, err := money.New(report.CommissionMinor, money.Currency(report.Currency))
	if err != nil {
		return crediting{}, fmt.Errorf("the reported commission: %w", err)
	}
	sale, err := money.New(report.SaleAmountMinor, money.Currency(report.Currency))
	if err != nil {
		return crediting{}, fmt.Errorf("the reported sale: %w", err)
	}

	tx, err := l.db.Begin(ctx)
	if err != nil {
		return crediting{}, fmt.Errorf("opening the transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.New(tx)

	clicks, err := clickout.NewClicks(clickoutstore.New(tx))
	if err != nil {
		return crediting{}, err
	}
	matcher, err := NewMatcher(clicks, queries)
	if err != nil {
		return crediting{}, err
	}
	attributed, err := matcher.Match(ctx, tx, Report{ID: reportID, Ref: networks.NewClickRef(report.ClickRef.String)})
	if err != nil {
		return crediting{}, err
	}
	if !attributed.Matched {
		if err := tx.Commit(ctx); err != nil {
			return crediting{}, fmt.Errorf("committing the queue row: %w", err)
		}
		return crediting{queued: true}, nil
	}
	click := attributed.Click

	brand, err := queries.BrandOfOffer(ctx, pgUUID(click.OfferID))
	if err != nil {
		return crediting{}, fmt.Errorf("which brand publishes offer %s: %w", click.OfferID, err)
	}
	share, err := ShareOf(commission, click.Promised)
	if err != nil {
		return crediting{}, err
	}
	if share.Member.Minor <= 0 {
		return crediting{nothingOwed: true}, nil
	}

	hold, err := l.judge(ctx, queries, Candidate{Member: click.AccountID, Click: click, Sale: sale, At: l.now()})
	if err != nil {
		return crediting{}, err
	}
	entries, err := NewEntries(queries, l.ledger, l.receivable)
	if err != nil {
		return crediting{}, err
	}
	opened, err := entries.Open(ctx, tx, hold.Open(Credit{
		Member: click.AccountID,
		Brand:  brand,
		Report: reportID,
		Click:  click.ID,
		Amount: share.Member,
		Reason: "the network reported the purchase",
	}))
	if err != nil {
		return crediting{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return crediting{}, fmt.Errorf("committing entry %s: %w", opened.ID, err)
	}
	return crediting{entry: opened}, nil
}

// judge asks the configured rules about a credit, or nothing when none is
// configured. The evaluator is built over the item's own transaction, so
// what it counts is what this transaction sees.
func (l *Lifecycle) judge(ctx context.Context, queries HoldStore, candidate Candidate) (Hold, error) {
	if len(l.rules.Active()) == 0 {
		return Hold{}, nil
	}
	holds, err := NewHolds(l.rules, queries)
	if err != nil {
		return Hold{}, err
	}
	return holds.Evaluate(ctx, candidate)
}

// confirm moves each pending entry whose network has approved the commission
// to confirmed, where a statement covers it.
func (l *Lifecycle) confirm(ctx context.Context, out *Outcome) error {
	waiting, err := store.New(l.db).EntriesAwaitingConfirmation(ctx, lifecycleBatch)
	if err != nil {
		return fmt.Errorf("%w: reading the entries awaiting confirmation: %w", ErrLifecycleIncomplete, err)
	}
	l.pageFull(ctx, "entries awaiting confirmation", len(waiting))
	for _, row := range waiting {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry, err := entryFrom(row.CashbackEntry)
		if err == nil {
			err = l.confirmOne(ctx, entry, uuid.UUID(row.CurrentReport.Bytes))
		}
		switch {
		case errors.Is(err, ErrNotReconciled):
			// The network's word without the statement: FR-043's other
			// half is missing and nothing moves. Read again next run, and
			// the run after, until a statement covers it.
			out.Awaiting++
		case errors.Is(err, ErrEntryMoved):
			// Somebody moved it between the read and now - an operator, or
			// the reversal pass of a previous run that this read predates.
			// Whatever they decided stands; the next read sees the result.
		case err != nil:
			out.Failed++
			l.log.ErrorContext(ctx, "the entry could not be confirmed",
				"entry", uuid.UUID(row.CashbackEntry.ID.Bytes), "error", err)
		default:
			out.Confirmed++
		}
	}
	return nil
}

// confirmOne confirms one entry in one transaction, citing the current
// report as the cause. The status is the tip's, which the read guaranteed
// confirmed; whether the money arrived is the confirmer's own question.
func (l *Lifecycle) confirmOne(ctx context.Context, entry Entry, current uuid.UUID) error {
	tx, err := l.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("opening the transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := store.New(tx)

	entries, err := NewEntries(queries, l.ledger, l.receivable)
	if err != nil {
		return err
	}
	confirmations, err := NewConfirmations(entries, queries)
	if err != nil {
		return err
	}
	if _, err := confirmations.Confirm(ctx, tx, entry, networks.StatusConfirmed, current); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing the confirmation: %w", err)
	}
	return nil
}

// reverse undoes each credit whose network has since declined or reversed
// the commission.
func (l *Lifecycle) reverse(ctx context.Context, out *Outcome) error {
	waiting, err := store.New(l.db).EntriesAwaitingReversal(ctx, lifecycleBatch)
	if err != nil {
		return fmt.Errorf("%w: reading the entries awaiting reversal: %w", ErrLifecycleIncomplete, err)
	}
	l.pageFull(ctx, "entries awaiting reversal", len(waiting))
	for _, row := range waiting {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry, err := entryFrom(row.CashbackEntry)
		if err == nil {
			err = l.reverseOne(ctx, entry, uuid.UUID(row.CurrentReport.Bytes), networks.Status(row.CurrentStatus))
		}
		switch {
		case err != nil:
			out.Failed++
			l.log.ErrorContext(ctx, "the credit could not be reversed",
				"entry", uuid.UUID(row.CashbackEntry.ID.Bytes), "error", err)
		default:
			out.Reversed++
		}
	}
	return nil
}

// reverseOne writes the reversing entry for one credit in one transaction,
// citing the superseding report the network sent (C-3) and saying in the
// transition's own words what the network did.
func (l *Lifecycle) reverseOne(ctx context.Context, entry Entry, current uuid.UUID, status networks.Status) error {
	tx, err := l.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("opening the transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	entries, err := NewEntries(store.New(tx), l.ledger, l.receivable)
	if err != nil {
		return err
	}
	reversals, err := NewReversals(entries)
	if err != nil {
		return err
	}
	if _, err := reversals.Reverse(ctx, tx, Reversal{
		Original: entry,
		Report:   current,
		Reason:   fmt.Sprintf("the network %s the commission", status),
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing the reversal: %w", err)
	}
	return nil
}

// pageFull says when a pass took a whole page, because a backlog being
// worked down in bounded runs is a fact an operator wants to see and a
// full page every run is one that is not shrinking.
func (l *Lifecycle) pageFull(ctx context.Context, what string, read int) {
	if read >= lifecycleBatch {
		l.log.InfoContext(ctx, "the pass took a full page, and more are waiting for the next run",
			"job", LifecycleJobName, "read", what, "page", lifecycleBatch)
	}
}
