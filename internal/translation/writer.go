package translation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
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
//     will not take: again the spend is recorded. The call was billed
//     whatever the database made of its result.
//   - stored: the row lands, and migration 0005's trigger moves the ledger
//     in the same transaction. This method does NOT upsert the ledger on
//     that path; doing so as well would double every cost.
//
// Whichever outcome, if the month has reached its cap the halt is latched
// in the same transaction as the spend, and Recorded.Halted() tells the
// caller to stop calling the provider.
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
		return Recorded{}, fmt.Errorf("translation: recording a translation: begin: %w", err)
	}
	// A no-op after commit; the guarantee that a failed write stores
	// nothing on any earlier return path.
	defer func() { _ = tx.Rollback(ctx) }()

	var id string
	err = tx.QueryRow(ctx,
		`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd, unmetered_attempts)
		 values ($1, $2, $3, $4, $5, $6, $7, $8)
		 returning id`,
		rec.SourceItemID.String(), rec.TargetLocale, rec.Model, rec.PromptVersion,
		rec.Headline, rec.Extract, rec.CostMicroUSD, rec.UnmeteredAttempts,
	).Scan(&id)
	if err != nil {
		refusal := fmt.Errorf("translation: inserting the translation: %w", err)
		if isAlreadyTranslated(err) {
			refusal = fmt.Errorf("%w: item %s into %q", ErrAlreadyTranslated, rec.SourceItemID, rec.TargetLocale)
		}
		// The failed statement aborted this transaction, so the spend
		// cannot be recorded in it. Roll back and record separately: the
		// call was billed whatever the database made of its result, and
		// the ledger is the one place that must not lose track of that.
		_ = tx.Rollback(ctx)
		out, spendErr := w.recordRefusedSpend(ctx, rec.Spend)
		if spendErr != nil {
			// Both are reported: the refusal explains what was not
			// written, and the ledger failure warns that this call's
			// cost is missing from the month.
			return Recorded{}, errors.Join(refusal, spendErr)
		}
		return out, refusal
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
		return Recorded{}, fmt.Errorf("translation: recording a translation: commit: %w", err)
	}
	return Recorded{TranslationID: translationID, MonthToDateMicroUSD: spent, HaltedAt: haltedAt}, nil
}

// recordRefusedSpend puts the cost of a call that produced no translation
// row onto the ledger and settles the month with it, atomically. The
// returned Recorded carries no translation id, because there is none.
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
