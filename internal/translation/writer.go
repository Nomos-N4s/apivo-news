package translation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrAlreadyTranslated reports that this retrieved item already has a
// translation into this locale. It is a refusal, not a failure: the
// translation on record is the one the editorial queue reviews, and a
// second paid one would put two approvable origins in front of an editor
// for a single item and locale.
//
// The call that produced the refused result was still billed, so its spend
// is recorded before this is returned.
var ErrAlreadyTranslated = errors.New("translation: this item is already translated into this locale")

// ErrRetryable reports a transient database failure: nothing was written
// and NOTHING WAS BOOKED, so the caller must retry Record with the same
// Result - the retry is what settles the ledger, exactly once. A retry
// that succeeds books the spend through the 0005 trigger; a retry of a
// write that had actually committed is recognised by the 23505 row-match
// and books nothing. A caller that logs and moves on instead leaves a
// billed call off the ledger for good.
var ErrRetryable = errors.New("translation: transient database failure, nothing recorded; retry to settle the ledger")

// Writer records finished translations and the money they cost.
//
// It is the one place the budget meets the database. The ceiling is checked
// HERE, before the insert, because an over-ceiling result must not become a
// translation - but its cost must still reach the ledger, so the check
// cannot live in the schema (a CHECK would refuse the row and lose the
// money with it). The monthly cap is settled after the write, because by
// then the money is spent and the only question left is whether the month
// may keep going.
type Writer struct {
	db   DB
	caps Caps
}

// NewWriter builds a Writer. The caller supplies the budget; wiring it to
// configuration is a separate change.
func NewWriter(db DB, caps Caps) *Writer { return &Writer{db: db, caps: caps} }

// Record is one finished translation to persist: which retrieved item, into
// which locale, and everything the provider produced and charged.
type Record struct {
	// SourceItemID is the retrieved item this translates (I-5).
	SourceItemID uuid.UUID

	// TargetLocale is the primary language subtag translated into, e.g.
	// "de". It is a language, never a language-and-place tag.
	TargetLocale string

	// Result is the provider's output and its spend.
	Result
}

// Recorded is what one Record call left behind. It reports the ledger even
// when no translation was written, because the ledger moved either way:
// the money was spent before this module got a say.
type Recorded struct {
	// TranslationID is the stored translation row. It is the zero UUID
	// when nothing was written - which is exactly the cases that return
	// an error, so a caller never reads a success out of a refusal.
	TranslationID uuid.UUID

	// MonthToDateMicroUSD is the month's total after this call's spend
	// landed, read under the ledger row's lock.
	MonthToDateMicroUSD int64

	// HaltedAt is when this month stopped translating - set by this call
	// if it was the one that crossed the cap, or carried from an earlier
	// crossing. Zero while the month is still running.
	HaltedAt time.Time
}

// Halted reports whether the month has stopped translating. A caller that
// sees it must make no further provider calls until the next month.
func (r Recorded) Halted() bool { return !r.HaltedAt.IsZero() }

// Record stores one translation with its cost, and settles the month.
//
// Three outcomes, and all three move the ledger:
//
//   - over the per-article ceiling: nothing is written, ErrOverCeiling is
//     returned, and the spend is recorded anyway. The provider billed for
//     the call whether or not the price was acceptable, and a ledger that
//     omitted it would read low by exactly the amount that most needed to
//     be seen.
//   - refused by the database - already translated into this locale
//     (ErrAlreadyTranslated), an unknown locale, anything else the schema
//     definitively will not take: again the spend is recorded, in the SAME
//     transaction as the refusal (a savepoint around the insert keeps the
//     transaction alive through the failed statement). The call was billed
//     whatever the database made of its result.
//   - stored: the row lands, and migration 0005's trigger moves the ledger
//     in the same transaction. This method does NOT upsert the ledger on
//     that path; doing so as well would double every cost.
//
// Whichever outcome, if the month has reached its cap the halt is latched
// in the same transaction as the spend, and Recorded.Halted() tells the
// caller to stop calling the provider.
//
// THE RETRY CONTRACT. A transient failure - a dropped connection, a
// statement_timeout, a deadlock victim - books nothing, wraps
// ErrRetryable, and the caller MUST retry with the same Result: the retry
// is what settles the ledger, exactly once. That is safe in both
// directions. If the earlier attempt really failed, the retry's insert
// books the spend through the trigger. If the earlier attempt actually
// committed and only its acknowledgement was lost, the retry hits the
// unique index (23505) and finds its own exact result on record - model,
// prompt version, cost and texts all equal - which is recognised as our
// own landed write: Record returns success and books nothing. A caller
// that logs a transient error and moves on instead leaves a billed call
// off the ledger for good.
//
// Note what is deliberately NOT here: a refusal to record because the month
// has already halted. By the time this is called the money is spent, and
// discarding a paid translation to honour a limit it has already crossed
// would destroy the record without saving a cent. Stopping BEFORE the
// provider call is the caller's job, off Ledger.ThisMonth and Caps.
func (w *Writer) Record(ctx context.Context, rec Record) (Recorded, error) {
	if err := w.caps.Validate(); err != nil {
		return Recorded{}, fmt.Errorf("translation: recording a translation: %w", err)
	}
	if w.caps.OverCeiling(rec.CostMicroUSD) {
		out, err := w.recordRefusedSpend(ctx, rec.Spend)
		if err != nil {
			return out, err
		}
		return out, fmt.Errorf("%w: %d micro-USD against a ceiling of %d; the cost is on the ledger, the translation is not",
			ErrOverCeiling, rec.CostMicroUSD, w.caps.PerArticleMicroUSD)
	}

	tx, err := w.db.Begin(ctx)
	if err != nil {
		return Recorded{}, fmt.Errorf("translation: recording a translation: begin: %w", errors.Join(err, ErrRetryable))
	}
	// A no-op after commit; the guarantee that a failed write stores
	// nothing on any earlier return path.
	defer func() { _ = tx.Rollback(ctx) }()

	// The insert runs inside a savepoint (pgx nests transactions as
	// savepoints), so a refusal aborts only the savepoint's scope: rolling
	// back to it leaves the transaction alive for the refusal's spend.
	// Booking that spend in a second transaction instead would open a
	// crash window - a moment after the rollback and before the second
	// commit where a billed call exists nowhere - which is exactly the
	// application-discipline gap the 0005 trigger closes on the success
	// path.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Recorded{}, fmt.Errorf("translation: recording a translation: savepoint: %w", errors.Join(err, ErrRetryable))
	}
	var id string
	err = sp.QueryRow(ctx,
		`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd, unmetered_attempts)
		 values ($1, $2, $3, $4, $5, $6, $7, $8)
		 returning id`,
		rec.SourceItemID.String(), rec.TargetLocale, rec.Model, rec.PromptVersion,
		rec.Headline, rec.Extract, rec.CostMicroUSD, rec.UnmeteredAttempts,
	).Scan(&id)
	if err != nil {
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			return Recorded{}, fmt.Errorf("translation: rolling back to the insert savepoint: %w",
				errors.Join(err, rbErr, ErrRetryable))
		}
		return w.settleRefusedInsert(ctx, tx, rec, err)
	}
	if err := sp.Commit(ctx); err != nil {
		return Recorded{}, fmt.Errorf("translation: recording a translation: releasing the savepoint: %w", errors.Join(err, ErrRetryable))
	}

	// The ledger has already moved, by trigger, inside this transaction:
	// this reads the total back under the lock that upsert took.
	spent, haltedAt, err := w.settle(ctx, tx)
	if err != nil {
		return Recorded{}, err
	}

	translationID, err := uuid.Parse(id)
	if err != nil {
		return Recorded{}, fmt.Errorf("translation: database returned malformed id %q: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Recorded{}, fmt.Errorf("translation: recording a translation: commit: %w", errors.Join(err, ErrRetryable))
	}
	return Recorded{TranslationID: translationID, MonthToDateMicroUSD: spent, HaltedAt: haltedAt}, nil
}

// settleRefusedInsert decides what a failed translation insert means for
// the ledger, inside the still-live transaction the savepoint preserved.
//
// Three classes, three answers:
//
//   - our own write already on record (23505 where the winning row carries
//     this exact result): an earlier attempt committed and only its
//     acknowledgement was lost. Success, and NOTHING is booked - the
//     trigger booked this spend when that row landed, and booking it again
//     here is precisely the double count the classification exists to
//     prevent.
//   - a deterministic refusal (unique violation with someone else's row,
//     a constraint or validation failure): the provider billed for a call
//     whose result the database will never store, so the spend is booked
//     as unbilled ledger movement, atomically with this refusal.
//   - anything else is transient: nothing is booked and the error wraps
//     ErrRetryable, because the caller's retry will succeed and the
//     trigger would then book the same spend a second time on top of a
//     refusal booking made here.
func (w *Writer) settleRefusedInsert(ctx context.Context, tx pgx.Tx, rec Record, insertErr error) (Recorded, error) {
	refusal := fmt.Errorf("translation: inserting the translation: %w", insertErr)
	switch {
	case isAlreadyTranslated(insertErr):
		id, ours, err := w.ownWinningRow(ctx, tx, rec)
		if err != nil {
			return Recorded{}, fmt.Errorf("translation: reading the translation that won the unique index: %w",
				errors.Join(err, ErrRetryable))
		}
		if ours {
			spent, haltedAt, err := w.settle(ctx, tx)
			if err != nil {
				return Recorded{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return Recorded{}, fmt.Errorf("translation: recording a translation: commit: %w", errors.Join(err, ErrRetryable))
			}
			return Recorded{TranslationID: id, MonthToDateMicroUSD: spent, HaltedAt: haltedAt}, nil
		}
		refusal = fmt.Errorf("%w: item %s into %q", ErrAlreadyTranslated, rec.SourceItemID, rec.TargetLocale)
	case !isDeterministicRefusal(insertErr):
		return Recorded{}, errors.Join(refusal, ErrRetryable)
	}

	// A billed call whose result the database definitively refused: the
	// money is gone, so it goes on the ledger - in this same transaction,
	// so the refusal and its spend land or vanish together.
	if _, err := NewLedger(tx).RecordUnbilledSpend(ctx, rec.Spend); err != nil {
		// Both are reported: the refusal explains what was not written,
		// and the ledger failure warns that this call's cost is not yet
		// on the month - the caller retries, and the next attempt books
		// it through this same path.
		return Recorded{}, errors.Join(refusal, err, ErrRetryable)
	}
	spent, haltedAt, err := w.settle(ctx, tx)
	if err != nil {
		return Recorded{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Recorded{}, errors.Join(refusal, fmt.Errorf("translation: recording refused spend: commit: %w", err), ErrRetryable)
	}
	return Recorded{MonthToDateMicroUSD: spent, HaltedAt: haltedAt}, refusal
}

// ownWinningRow reports whether the translation that holds the unique
// index for this item and locale carries exactly this worker's result -
// same model, prompt version, texts, cost and unpriced-attempt count. If
// so, the 23505 is not a rival's translation but our own earlier write
// whose commit acknowledgement was lost, and the honest answer is success,
// not a second booking of money the trigger already counted.
func (w *Writer) ownWinningRow(ctx context.Context, tx pgx.Tx, rec Record) (uuid.UUID, bool, error) {
	var id string
	err := tx.QueryRow(ctx,
		`select id from translation
		  where source_item_id = $1 and target_locale = $2
		    and model = $3 and prompt_version = $4
		    and headline = $5 and extract = $6
		    and cost_microusd = $7 and unmetered_attempts = $8`,
		rec.SourceItemID.String(), rec.TargetLocale, rec.Model, rec.PromptVersion,
		rec.Headline, rec.Extract, rec.CostMicroUSD, rec.UnmeteredAttempts,
	).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return uuid.Nil, false, nil
	case err != nil:
		return uuid.Nil, false, err
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("translation: database returned malformed id %q: %w", id, err)
	}
	return parsed, true, nil
}

// recordRefusedSpend puts the cost of a call that produced no translation
// row onto the ledger and settles the month with it, atomically. The
// returned Recorded carries no translation id, because there is none.
//
// It serves only the refusals decided BEFORE any insert is attempted (the
// over-ceiling check), where this one transaction is the only one there
// is. A refusal decided BY the database happens mid-transaction and is
// settled in place by settleRefusedInsert - a second transaction there
// would open a crash window between the rollback and the booking.
func (w *Writer) recordRefusedSpend(ctx context.Context, spend Spend) (Recorded, error) {
	tx, err := w.db.Begin(ctx)
	if err != nil {
		return Recorded{}, fmt.Errorf("translation: recording refused spend: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Recorded even when it is zero: the upsert creates-or-locks the
	// month row and hands back the authoritative total, which the caller
	// needs whether or not this particular refusal cost anything.
	if _, err := NewLedger(tx).RecordUnbilledSpend(ctx, spend); err != nil {
		return Recorded{}, err
	}
	spent, haltedAt, err := w.settle(ctx, tx)
	if err != nil {
		return Recorded{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Recorded{}, fmt.Errorf("translation: recording refused spend: commit: %w", err)
	}
	return Recorded{MonthToDateMicroUSD: spent, HaltedAt: haltedAt}, nil
}

// settle reads the month the spend just landed in and, if the cap is now
// reached, latches the halt. It runs inside the caller's transaction, which
// already holds the month row's lock from the upsert - so the total it
// reads is this transaction's own, and no other worker can be latching the
// same month underneath it.
func (w *Writer) settle(ctx context.Context, q Queryer) (spent int64, haltedAt time.Time, err error) {
	ledger := NewLedger(q)
	month, err := ledger.ThisMonth(ctx)
	if err != nil {
		return 0, time.Time{}, err
	}
	if month.Halted() || !w.caps.Reached(month.SpentMicroUSD) {
		return month.SpentMicroUSD, month.HaltedAt, nil
	}
	at, latched, err := ledger.Halt(ctx, month.Month, w.caps.MonthlyMicroUSD)
	if err != nil {
		return 0, time.Time{}, err
	}
	if !latched {
		// Unreachable while the lock is held, but a halt that happened
		// is a halt: report the month as stopped rather than invent a
		// time for it.
		return month.SpentMicroUSD, month.HaltedAt, nil
	}
	return month.SpentMicroUSD, at, nil
}

// isAlreadyTranslated recognises the unique violation on (source_item_id,
// target_locale): this item already has a translation into this locale, so
// a re-run, a retry after a partial failure or a second worker has arrived
// at a translation that is already on record.
func isAlreadyTranslated(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgerrcode.UniqueViolation &&
		pgErr.ConstraintName == "translation_one_per_item_locale"
}

// isDeterministicRefusal recognises an insert the database definitively
// refused - a verdict on the statement, not a failure to execute it. Only
// these book the call's spend as a refusal; everything else is treated as
// transient and books nothing, because the caller's retry may succeed and
// the trigger would then count the same spend a second time.
//
// The classes are the SQLSTATE families in which the same statement fails
// the same way every time: 22 (data exception - a malformed value), 23
// (integrity constraint violation - unique, foreign key, check, not null)
// and 42 (the statement itself is invalid). A deadlock (40P01), a
// cancelled statement (57014), a dropped connection (class 08, or no
// server verdict at all) are the retryable shapes.
//
// An unrecognised class deliberately reads as transient, not as a
// refusal: misreading transient as deterministic double-books silently on
// retry, while misreading deterministic as transient keeps returning the
// same loud error to the caller until someone looks.
func isDeterministicRefusal(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return false
	}
	switch pgErr.Code[:2] {
	case "22", "23", "42":
		return true
	}
	return false
}
