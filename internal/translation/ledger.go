package translation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Queryer is the single-statement slice of database access this module
// needs, defined here per the boundary rules (the consumer names its
// dependency). Both *pgxpool.Pool and pgx.Tx satisfy it, so the same code
// serves a standalone call and a step inside somebody else's transaction.
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DB adds the ability to open a transaction, for the writes that have to
// land together. The composition root in cmd decides what is wired in.
type DB interface {
	Queryer
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Ledger reads and moves the monthly translation spend record.
//
// It is deliberately thin, because most of the ledger is not here: every
// successful translation moves the month row by trigger, in the same
// transaction as the insert (migration 0005), so a translation whose cost
// is not in the ledger cannot exist. This type carries the two things a
// trigger on translation cannot do - record spend that produced no
// translation row, and latch the month's halt - plus the read a caller
// needs before it decides to call a provider at all.
type Ledger struct {
	db Queryer
}

// NewLedger builds a Ledger on any query runner. Pass a transaction to make
// its reads and writes part of a larger atomic step.
func NewLedger(db Queryer) *Ledger { return &Ledger{db: db} }

// Month is the ledger's record of one calendar month.
type Month struct {
	// Month is the first day of the month the row keys.
	Month time.Time

	// SpentMicroUSD is what the month has cost so far. When
	// UnmeteredAttempts is non-zero it is a LOWER BOUND on the real total.
	SpentMicroUSD int64

	// UnmeteredAttempts is how many provider attempts this month were
	// accepted but never priced. Non-zero means the cap is nearer than
	// SpentMicroUSD says.
	UnmeteredAttempts int

	// HaltedAt is when the month crossed the cap and the pipeline stopped
	// translating; zero while it has not.
	HaltedAt time.Time
}

// Halted reports whether this month has already stopped translating.
func (m Month) Halted() bool { return !m.HaltedAt.IsZero() }

// ThisMonth reads the current month's ledger row. A month nothing has been
// spent in yet has no row, which reads as an empty, unhalted month rather
// than as an error - the ledger's absence of spend is a fact about the
// month, not a failure to answer.
//
// It is the pre-flight read: a caller checks it, and Caps, BEFORE calling a
// provider. Checking afterwards only tells you what you have already spent.
func (l *Ledger) ThisMonth(ctx context.Context) (Month, error) {
	var (
		month    pgtype.Date
		haltedAt pgtype.Timestamptz
		out      Month
	)
	err := l.db.QueryRow(ctx,
		`select m.month, coalesce(s.spent_microusd, 0), coalesce(s.unmetered_attempts, 0), s.halted_at
		   from (select date_trunc('month', now())::date as month) m
		   left join translation_spend s on s.month = m.month`).
		Scan(&month, &out.SpentMicroUSD, &out.UnmeteredAttempts, &haltedAt)
	if err != nil {
		return Month{}, fmt.Errorf("translation: reading the monthly spend ledger: %w", err)
	}
	out.Month = month.Time
	if haltedAt.Valid {
		out.HaltedAt = haltedAt.Time
	}
	return out, nil
}

// RecordUnbilledSpend adds spend that produced no translation row, and
// returns the month's total after the addition.
//
// A provider bills for the work it did, not for the work we could use: a
// call refused at the ceiling, an answer cut off mid-generation, a retry
// that failed after the tokens were generated. That money is gone, so the
// ledger has to know about it - the cap exists to stop the month
// overspending, and a total that omits the failures is not the month's
// spend.
//
// A consequence to state plainly, because a later reconciliation will
// otherwise read it as corruption: SUM(translation.cost_microusd) is
// LEGITIMATELY BELOW translation_spend.spent_microusd, by exactly the calls
// that were billed and produced nothing. The two numbers are not supposed
// to match. translation_spend carries no foreign key precisely so this
// spend has somewhere to go.
//
// The statement is the same upsert the 0005 trigger runs, and it is one
// statement for the same reason: it takes the month row's lock and returns
// the POST-increment total, so two workers crossing the month together
// each see a different number and neither loses the other's money.
func (l *Ledger) RecordUnbilledSpend(ctx context.Context, spend Spend) (int64, error) {
	if spend.CostMicroUSD < 0 {
		return 0, fmt.Errorf("translation: recording unbilled spend: cost is %d micro-USD, which is not an amount anyone was charged", spend.CostMicroUSD)
	}
	if spend.UnmeteredAttempts < 0 {
		return 0, fmt.Errorf("translation: recording unbilled spend: unpriced attempts is %d, which is not a count of anything", spend.UnmeteredAttempts)
	}
	var total int64
	err := l.db.QueryRow(ctx,
		`insert into translation_spend (month, spent_microusd, unmetered_attempts)
		 values (date_trunc('month', now())::date, $1, $2)
		 on conflict (month) do update
		    set spent_microusd = translation_spend.spent_microusd + excluded.spent_microusd,
		        unmetered_attempts = translation_spend.unmetered_attempts + excluded.unmetered_attempts
		 returning spent_microusd`,
		spend.CostMicroUSD, spend.UnmeteredAttempts).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("translation: recording unbilled spend: %w", err)
	}
	return total, nil
}

// Halt latches a month at the cap: the pipeline stops translating until the
// next one. It reports when the month halted and whether THIS call is what
// halted it.
//
// The condition lives in the statement, not around it. Only one transaction
// can match `halted_at is null` and win the row, so the database decides
// who crossed the cap first - and migration 0005 hangs the pipeline.halted
// domain event off that single winning update. A halt evaluated in Go and
// then written would append one event per tick to a stream that has no way
// to deduplicate them (I-3: the event stream is append-only).
//
// Calling it on a month that has not reached the cap, or has already
// halted, is not an error: it latches nothing and says so.
func (l *Ledger) Halt(ctx context.Context, month time.Time, monthlyCapMicroUSD int64) (haltedAt time.Time, latched bool, err error) {
	var at time.Time
	err = l.db.QueryRow(ctx,
		`update translation_spend
		    set halted_at = now()
		  where month = $1
		    and halted_at is null
		    and spent_microusd >= $2
		 returning halted_at`,
		pgtype.Date{Time: month, Valid: true}, monthlyCapMicroUSD).Scan(&at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("translation: halting the month at the cap: %w", err)
	}
	return at, true, nil
}
