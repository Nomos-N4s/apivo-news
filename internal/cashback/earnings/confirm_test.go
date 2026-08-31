package earnings_test

// What confirming requires, and what it refuses (T070, FR-043).
//
// Confirmed is the state a member can turn into money leaving the business,
// so every case here is about not letting it happen too easily. The three
// refusals are separate tests rather than a table, because they mean three
// different things to whoever reads them.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// fakeReconciliation answers the receipt question, and records whether it was
// asked at all.
type fakeReconciliation struct {
	answer pgtype.Bool
	err    error
	asks   int
}

func (f *fakeReconciliation) ReportIsReconciled(_ context.Context, _ pgtype.UUID) (pgtype.Bool, error) {
	f.asks++
	if f.err != nil {
		return pgtype.Bool{}, f.err
	}
	return f.answer, nil
}

// arrived and outstanding are the two answers the statement can give.
func arrived() pgtype.Bool     { return pgtype.Bool{Bool: true, Valid: true} }
func outstanding() pgtype.Bool { return pgtype.Bool{Bool: false, Valid: true} }

// pendingEntry is an entry waiting to be confirmed.
func pendingEntry(t *testing.T) earnings.Entry {
	t.Helper()
	amount, err := money.New(3000, "EUR")
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	return earnings.Entry{
		ID:     uuid.New(),
		Member: uuid.New(),
		Report: uuid.New(),
		State:  earnings.StatePending,
		Amount: amount,
	}
}

// confirmer builds the confirmer over a working machine and the given answer.
func confirmer(t *testing.T, entries earnings.EntryStore, ledger *fakeLedger, reconciled earnings.Reconciliation) *earnings.Confirmations {
	t.Helper()
	c, err := earnings.NewConfirmations(machine(t, entries, ledger), reconciled)
	if err != nil {
		t.Fatalf("NewConfirmations(): %v", err)
	}
	return c
}

// TestBothHalvesTogetherConfirm is the path a member is eventually paid on.
func TestBothHalvesTogetherConfirm(t *testing.T) {
	t.Parallel()

	entry := pendingEntry(t)
	entries := &fakeEntries{row: anEntry(entry.Member, earnings.StatePending)}
	entries.row.ID = pgtype.UUID{Bytes: entry.ID, Valid: true}
	ledger := &fakeLedger{}
	statement := &fakeReconciliation{answer: arrived()}

	confirmed, err := confirmer(t, entries, ledger, statement).
		Confirm(t.Context(), &fakeOutbox{}, entry, networks.StatusConfirmed, uuid.New())
	if err != nil {
		t.Fatalf("Confirm(): %v", err)
	}
	if confirmed.State != earnings.StateConfirmed {
		t.Errorf("the entry is %s, want confirmed", confirmed.State)
	}
	if len(ledger.posted) != 1 {
		t.Errorf("posted %d transfer(s), want one", len(ledger.posted))
	}
}

// TestAnUnapprovedCommissionDoesNotConfirm covers the ordinary, temporary
// case: most reports sit pending for weeks while a return window runs.
//
// It also asserts the receipt question is never asked. Not for cost - the
// point is that an unapproved commission is not a reconciliation question at
// all, and asking would invite reading the answer as the reason.
func TestAnUnapprovedCommissionDoesNotConfirm(t *testing.T) {
	t.Parallel()

	entry := pendingEntry(t)
	entries, ledger := &fakeEntries{row: anEntry(entry.Member, earnings.StatePending)}, &fakeLedger{}
	statement := &fakeReconciliation{answer: arrived()}

	_, err := confirmer(t, entries, ledger, statement).
		Confirm(t.Context(), &fakeOutbox{}, entry, networks.StatusPending, uuid.New())

	if !errors.Is(err, earnings.ErrNotApproved) {
		t.Fatalf("Confirm() error = %v, want one wrapping %v", err, earnings.ErrNotApproved)
	}
	if statement.asks != 0 {
		t.Errorf("asked the statement %d time(s) about an unapproved commission, want none", statement.asks)
	}
	if len(ledger.posted) != 0 {
		t.Errorf("an unapproved commission posted %d transfer(s), want none", len(ledger.posted))
	}
}

// TestAnApprovedButUnreceivedCommissionDoesNotConfirm is the half FR-043
// exists for, and the one a network's word alone would let through: approval
// is an intention to pay, and money that was approved and then withdrawn
// would otherwise be withdrawable by the member.
func TestAnApprovedButUnreceivedCommissionDoesNotConfirm(t *testing.T) {
	t.Parallel()

	entry := pendingEntry(t)
	entries, ledger := &fakeEntries{row: anEntry(entry.Member, earnings.StatePending)}, &fakeLedger{}
	statement := &fakeReconciliation{answer: outstanding()}

	_, err := confirmer(t, entries, ledger, statement).
		Confirm(t.Context(), &fakeOutbox{}, entry, networks.StatusConfirmed, uuid.New())

	if !errors.Is(err, earnings.ErrNotReconciled) {
		t.Fatalf("Confirm() error = %v, want one wrapping %v", err, earnings.ErrNotReconciled)
	}
	if errors.Is(err, earnings.ErrNotApproved) {
		t.Error("an approved commission was reported as unapproved, which sends an operator to the wrong place")
	}
	if len(ledger.posted) != 0 {
		t.Errorf("an unreconciled commission posted %d transfer(s), want none", len(ledger.posted))
	}
}

// TestAFailedReadIsNotAnUnreceivedCommission. Refusing on an error is still
// deciding money on it, so the unreadable answer gets its own refusal - and
// an operator chasing a missing statement that was never missing is a day
// spent on nothing.
func TestAFailedReadIsNotAnUnreceivedCommission(t *testing.T) {
	t.Parallel()

	entry := pendingEntry(t)
	entries, ledger := &fakeEntries{row: anEntry(entry.Member, earnings.StatePending)}, &fakeLedger{}

	for _, statement := range []*fakeReconciliation{
		{err: errors.New("connection reset")},
		// `exists` cannot be null, so an invalid answer is the statement's
		// shape having changed rather than the database declining to say.
		{answer: pgtype.Bool{}},
	} {
		_, err := confirmer(t, entries, ledger, statement).
			Confirm(t.Context(), &fakeOutbox{}, entry, networks.StatusConfirmed, uuid.New())

		if !errors.Is(err, earnings.ErrReconciliationUnknown) {
			t.Fatalf("Confirm() error = %v, want one wrapping %v", err, earnings.ErrReconciliationUnknown)
		}
		if errors.Is(err, earnings.ErrNotReconciled) {
			t.Error("a read that failed reads as a statement that disagreed")
		}
	}
	if len(ledger.posted) != 0 {
		t.Errorf("an unreadable answer posted %d transfer(s), want none", len(ledger.posted))
	}
}

// TestAConfirmerIsRefusedWithoutItsParts covers the construction refusals.
func TestAConfirmerIsRefusedWithoutItsParts(t *testing.T) {
	t.Parallel()

	entries := machine(t, &fakeEntries{}, &fakeLedger{})
	if _, err := earnings.NewConfirmations(nil, &fakeReconciliation{}); !errors.Is(err, earnings.ErrNoEntries) {
		t.Errorf("NewConfirmations(nil, _) error = %v, want %v", err, earnings.ErrNoEntries)
	}
	if _, err := earnings.NewConfirmations(entries, nil); !errors.Is(err, earnings.ErrNoReconciliation) {
		t.Errorf("NewConfirmations(_, nil) error = %v, want %v", err, earnings.ErrNoReconciliation)
	}
}
