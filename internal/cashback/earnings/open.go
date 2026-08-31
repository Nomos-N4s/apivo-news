// Opening an entry: the step that turns an attributed report and a computed
// share into money a member is owed (T073, FR-040, FR-042, C-2).
//
// No task named it, and the journey does not exist without it. T067 says
// which click a report belongs to, T068 says what the member's share of it
// is, and T069 moves an entry that already exists. This is the join: the
// first write of the credit itself, and the only one that decides which
// state a member's money starts in.
//
// The order inside is insert, post, record - the one place in this package
// where the insert comes first, and it is worth saying why. Exactly-once
// crediting is entry_one_per_report's to enforce, and enforcing it before
// the ledger is asked to move anything is the difference between refusing a
// duplicate poll and refusing it after a round trip that moved money. What
// makes it safe is the key: derived from the REPORT, so a retry after a
// failed commit re-posts to the same transfer instead of crediting a second
// time out of a second one.

package earnings

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/events"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

var (
	// ErrNoEvidence reports a credit citing no report. C-2 in Go, ahead of
	// the not-null column that says the same: a credit with no evidence is
	// unrepresentable, and saying so by name is kinder than a constraint
	// violation from inside an insert.
	ErrNoEvidence = errors.New("earnings: a credit needs the report it rests on")
	// ErrNoMember reports a credit owed to nobody.
	ErrNoMember = errors.New("earnings: a credit needs the member it is owed to")
	// ErrNoBrand reports a credit no brand owes (ADR-0004). An entry is money
	// owed to a member BY A BRAND, and neither is derivable from the other.
	ErrNoBrand = errors.New("earnings: a credit needs the brand that owes it")
	// ErrNotACredit reports an amount that is not money a member could be
	// owed. Zero is included: a credit of nothing is a row that would sit in
	// a member's wallet forever saying nothing, and a reversal of it would
	// post a transfer of nothing.
	ErrNotACredit = errors.New("earnings: a credit must be a positive amount")
	// ErrAlreadyCredited reports a report that has already earned an entry.
	// Ordinary rather than exceptional: a trailing poll re-reads a window it
	// has already read, and this is entry_one_per_report saying so - before
	// the ledger has been asked to move anything.
	ErrAlreadyCredited = errors.New("earnings: this report has already been credited")
	// ErrNotOpened reports a credit the database refused for any other
	// reason.
	ErrNotOpened = errors.New("earnings: the entry could not be opened")
)

// Credit is one member's share of one reported purchase, ready to be owed.
//
// The amount is the MEMBER'S share, already computed from the click-time
// snapshot (FR-013, T068), never the commission. What the network pays and
// what the member is owed are different numbers, and the difference is the
// business's - so an Open that took the commission would credit a member
// with revenue nobody meant them to have.
type Credit struct {
	// Member is who is owed, and Brand the tenant they are owed by.
	Member uuid.UUID
	Brand  string
	// Report is the evidence this credit rests on (C-2), and Click the click
	// it was attributed through. Click is the zero uuid only where an
	// operator attributed the purchase by hand from the unattributed queue,
	// because the network named no reference for a guard to read (FR-034).
	Report uuid.UUID
	Click  uuid.UUID
	// State is the state the credit opens in: held where a rule holds it back
	// (US7), pending otherwise. Confirmed is not among them and cannot be -
	// FR-043 wants a reconciled statement as well as the network's word, and
	// no report carries one.
	State State
	// HoldRule names the rule holding it when State is held, and is ignored
	// otherwise. The schema requires a held entry to name one and every other
	// entry to name none, in the same row.
	HoldRule string
	// Amount is the member's share.
	Amount money.Amount
	// Reason is free text recorded with the opening transition, and Actor the
	// operator where a human attributed the purchase rather than a poll.
	Reason string
	Actor  uuid.UUID
}

// Open writes the entry, posts the credit, and records the transition that
// joins them.
//
// Everything it can refuse, it refuses before the transaction: a state an
// entry cannot open in, a credit with no evidence, no member, no brand, or
// an amount that is not money. Those are the caller's mistakes and they read
// better as themselves than as a constraint name - and the database checks
// every one of them again anyway.
func (e *Entries) Open(ctx context.Context, db events.RowQuerier, credit Credit) (Entry, error) {
	if err := credit.validate(); err != nil {
		return Entry{}, err
	}
	where, err := postingsFor(credit.Member, "", credit.State, e.receivable)
	if err != nil {
		return Entry{}, err
	}

	created, err := e.store.CreateEntry(ctx, store.CreateEntryParams{
		AccountID:            pgUUID(credit.Member),
		BrandID:              credit.Brand,
		NetworkTransactionID: pgUUID(credit.Report),
		ClickID:              pgUUIDOrNull(credit.Click),
		State:                string(credit.State),
		AmountMinor:          credit.Amount.Minor,
		Currency:             string(credit.Amount.Currency),
		HoldRule:             pgTextOrNull(credit.holdRule()),
	})
	if err != nil {
		if alreadyCredited(err) {
			return Entry{}, fmt.Errorf("%w: report %s", ErrAlreadyCredited, credit.Report)
		}
		return Entry{}, fmt.Errorf("%w: report %s: %w", ErrNotOpened, credit.Report, err)
	}

	ref, err := e.post(ctx, where, credit.Amount, openingKey(credit.Report, credit.State))
	if err != nil {
		return Entry{}, err
	}
	recorded, err := e.record(ctx, uuid.UUID(created.ID.Bytes), "", credit.State, ref, credit.Reason, credit.Actor)
	if err != nil {
		return Entry{}, err
	}

	opened, err := entryFrom(created)
	if err != nil {
		return Entry{}, err
	}
	if err := e.announcer.Created(ctx, db, opened); err != nil {
		return Entry{}, err
	}
	if err := e.announcer.StateChanged(ctx, db, recorded); err != nil {
		return Entry{}, err
	}
	return opened, nil
}

// validate refuses everything about a credit that can be refused without
// touching the database.
func (c Credit) validate() error {
	switch {
	case !CanOpen(c.State):
		return ErrIllegalTransition{From: "", To: c.State}
	case c.Report == uuid.Nil:
		return fmt.Errorf("%w: member %s", ErrNoEvidence, c.Member)
	case c.Member == uuid.Nil:
		return fmt.Errorf("%w: report %s", ErrNoMember, c.Report)
	case c.Brand == "":
		return fmt.Errorf("%w: report %s", ErrNoBrand, c.Report)
	case !c.Amount.Currency.Valid() || c.Amount.Minor <= 0:
		return fmt.Errorf("%w: report %s is %d %s", ErrNotACredit, c.Report, c.Amount.Minor, c.Amount.Currency)
	}
	return nil
}

// holdRule answers what the opened row's hold_rule must be, for the reason
// [holdRuleFor] answers the same question about a move: the schema checks the
// rule and the state on the row as a whole.
func (c Credit) holdRule() string {
	if c.State == StateHeld {
		return c.HoldRule
	}
	return ""
}

// openingKey derives the posting key for a credit from its evidence (D8).
//
// The REPORT rather than the entry, and that is the whole safety of opening
// the entry first. A retry after a failed commit finds the entry it inserted
// rolled back, inserts a new one with a new id, and would post a second
// transfer under a key derived from it - crediting the member twice out of a
// receivable that was only ever owed once. Keyed on the report, the retry
// re-posts to the transfer the first attempt made, and the ledger records
// nothing new.
//
// entry_one_per_report is what makes the key unique: one report earns one
// entry, so one report earns one opening transfer.
func openingKey(report uuid.UUID, to State) string {
	return fmt.Sprintf("credit:report:%s:to:%s", report, to)
}

// alreadyCredited answers whether the database refused this insert because
// the report already has an entry, rather than for any other reason.
func alreadyCredited(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgerrcode.UniqueViolation &&
		pgErr.ConstraintName == "entry_one_per_report"
}
