package earnings_test

// What opening a credit writes, in what order, and what it refuses (T073).
//
// The properties worth proving are about ORDER and about what does NOT
// happen: that a report already credited is refused before the ledger is
// asked to move anything, that the money comes out of the receivable and
// into the member's opening stage, and that the posting key is derived from
// the evidence rather than from the row that was just inserted.

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// aCredit is one member's share of one reported purchase, ready to open.
func aCredit(t *testing.T, state earnings.State) earnings.Credit {
	t.Helper()
	amount, err := money.New(1800, "EUR")
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	return earnings.Credit{
		Member: uuid.New(),
		Brand:  "apivo-de",
		Report: uuid.New(),
		Click:  uuid.New(),
		State:  state,
		Amount: amount,
	}
}

// TestACreditOpensOutOfTheReceivableAndIntoTheMembersStage is FR-040 at this
// layer. The commission the network reported is where the member's share
// comes from; taking it from anywhere else would credit money the business
// had not received.
func TestACreditOpensOutOfTheReceivableAndIntoTheMembersStage(t *testing.T) {
	t.Parallel()

	entries, ledger, out := &fakeEntries{}, &fakeLedger{}, &fakeOutbox{}
	credit := aCredit(t, earnings.StatePending)

	opened, err := machine(t, entries, ledger).Open(t.Context(), out, credit)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}

	if len(ledger.posted) != 1 {
		t.Fatalf("posted %d transfer(s), want one", len(ledger.posted))
	}
	wantFrom := wallet.HouseAccount(receivable)
	wantTo := wallet.MemberAccount(credit.Member, wallet.StagePending)
	if ledger.ensured[0] != wantFrom || ledger.ensured[1] != wantTo {
		t.Errorf("moved %s -> %s, want %s -> %s", ledger.ensured[0], ledger.ensured[1], wantFrom, wantTo)
	}
	if opened.State != earnings.StatePending || opened.Amount != credit.Amount {
		t.Errorf("opened %s holding %v, want pending holding %v", opened.State, opened.Amount, credit.Amount)
	}
	if opened.Report != credit.Report || opened.Click != credit.Click {
		t.Errorf("opened citing report %s through click %s, want %s through %s",
			opened.Report, opened.Click, credit.Report, credit.Click)
	}
	// The opening transition, from nothing, naming the transfer that carried
	// it - the same D7 rule every other move obeys.
	if len(entries.transitions) != 1 || len(entries.links) != 1 {
		t.Fatalf("recorded %d transition(s) and %d link(s), want one of each",
			len(entries.transitions), len(entries.links))
	}
	if entries.transitions[0].FromState.Valid {
		t.Errorf("the opening transition came from %q, want nothing", entries.transitions[0].FromState.String)
	}
	if entries.transitions[0].LedgerTransferRef == "" {
		t.Error("the opening transition names no transfer, which the schema refuses")
	}
}

// TestTheOpeningKeyIsDerivedFromTheEvidence is what makes opening the entry
// before posting safe. A retry after a failed commit inserts a NEW entry with
// a new id; a key derived from that id would post a second transfer and
// credit the member twice out of a receivable that was owed once.
func TestTheOpeningKeyIsDerivedFromTheEvidence(t *testing.T) {
	t.Parallel()

	credit := aCredit(t, earnings.StatePending)
	first, second := &fakeEntries{}, &fakeEntries{}

	for _, entries := range []*fakeEntries{first, second} {
		if _, err := machine(t, entries, &fakeLedger{}).Open(t.Context(), &fakeOutbox{}, credit); err != nil {
			t.Fatalf("Open(): %v", err)
		}
	}

	// Two attempts, two different entry ids, and the same key: the ledger
	// answers the second with the transfer the first made.
	keys := map[string]bool{}
	for _, entries := range []*fakeEntries{first, second} {
		keys[entries.transitions[0].LedgerTransferRef] = true
	}
	if len(keys) != 1 {
		t.Errorf("two attempts at one report posted %d different transfers, want one: %v", len(keys), keys)
	}
	if first.created.NetworkTransactionID != second.created.NetworkTransactionID {
		t.Fatal("the two attempts cited different reports, so this proves nothing")
	}
}

// TestAReportAlreadyCreditedIsRefusedBeforeAnyMoneyMoves. Exactly-once
// crediting is entry_one_per_report's to enforce, and a trailing poll
// re-reading a window it has already read is the ordinary way it fires.
func TestAReportAlreadyCreditedIsRefusedBeforeAnyMoneyMoves(t *testing.T) {
	t.Parallel()

	entries := &fakeEntries{createErr: &pgconn.PgError{
		Code:           pgerrcode.UniqueViolation,
		ConstraintName: "entry_one_per_report",
	}}
	ledger := &fakeLedger{}

	_, err := machine(t, entries, ledger).Open(t.Context(), &fakeOutbox{}, aCredit(t, earnings.StatePending))

	if !errors.Is(err, earnings.ErrAlreadyCredited) {
		t.Fatalf("Open() error = %v, want one wrapping %v", err, earnings.ErrAlreadyCredited)
	}
	if len(ledger.posted) != 0 {
		t.Errorf("a duplicate report posted %d transfer(s), want none", len(ledger.posted))
	}
}

// TestAHeldCreditNamesTheRuleHoldingIt, and one that is not held names none.
// The schema checks the rule and the state on the row as a whole, so a
// pending entry carrying a rule is a row it refuses.
func TestAHeldCreditNamesTheRuleHoldingIt(t *testing.T) {
	t.Parallel()

	held := aCredit(t, earnings.StateHeld)
	held.HoldRule = "new-member-first-purchase"
	entries := &fakeEntries{}
	if _, err := machine(t, entries, &fakeLedger{}).Open(t.Context(), &fakeOutbox{}, held); err != nil {
		t.Fatalf("Open(held): %v", err)
	}
	if entries.created.HoldRule.String != held.HoldRule {
		t.Errorf("the held entry names rule %q, want %q", entries.created.HoldRule.String, held.HoldRule)
	}

	pending := aCredit(t, earnings.StatePending)
	pending.HoldRule = "new-member-first-purchase"
	entries = &fakeEntries{}
	if _, err := machine(t, entries, &fakeLedger{}).Open(t.Context(), &fakeOutbox{}, pending); err != nil {
		t.Fatalf("Open(pending): %v", err)
	}
	if entries.created.HoldRule.Valid {
		t.Errorf("the pending entry names rule %q, want none", entries.created.HoldRule.String)
	}
}

// TestAnEntryCannotOpenConfirmed. FR-043 wants a reconciled statement as
// well as the network's word, and no report carries one - so confirmed is
// not a state anything can be born in.
func TestAnEntryCannotOpenConfirmed(t *testing.T) {
	t.Parallel()

	entries, ledger := &fakeEntries{}, &fakeLedger{}
	credit := aCredit(t, earnings.StateConfirmed)

	_, err := machine(t, entries, ledger).Open(t.Context(), &fakeOutbox{}, credit)

	var illegal earnings.ErrIllegalTransition
	if !errors.As(err, &illegal) {
		t.Fatalf("Open(confirmed) error = %v, want an ErrIllegalTransition", err)
	}
	if entries.creations != 0 || len(ledger.posted) != 0 {
		t.Errorf("inserted %d entry(ies) and posted %d transfer(s), want none of either",
			entries.creations, len(ledger.posted))
	}
}

// TestACreditIsRefusedWithoutItsParts covers the refusals that happen before
// the transaction, each named as itself rather than as a constraint.
func TestACreditIsRefusedWithoutItsParts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		spoil func(c *earnings.Credit)
		want  error
	}{
		{"no evidence", func(c *earnings.Credit) { c.Report = uuid.Nil }, earnings.ErrNoEvidence},
		{"no member", func(c *earnings.Credit) { c.Member = uuid.Nil }, earnings.ErrNoMember},
		{"no brand", func(c *earnings.Credit) { c.Brand = "" }, earnings.ErrNoBrand},
		{"nothing owed", func(c *earnings.Credit) { c.Amount = money.Amount{Minor: 0, Currency: "EUR"} }, earnings.ErrNotACredit},
		{"owed backwards", func(c *earnings.Credit) { c.Amount = money.Amount{Minor: -1, Currency: "EUR"} }, earnings.ErrNotACredit},
		{"no currency", func(c *earnings.Credit) { c.Amount = money.Amount{Minor: 100} }, earnings.ErrNotACredit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			entries, ledger := &fakeEntries{}, &fakeLedger{}
			credit := aCredit(t, earnings.StatePending)
			tc.spoil(&credit)

			_, err := machine(t, entries, ledger).Open(t.Context(), &fakeOutbox{}, credit)

			if !errors.Is(err, tc.want) {
				t.Fatalf("Open() error = %v, want one wrapping %v", err, tc.want)
			}
			if entries.creations != 0 || len(ledger.posted) != 0 {
				t.Errorf("inserted %d entry(ies) and posted %d transfer(s), want none of either",
					entries.creations, len(ledger.posted))
			}
		})
	}
}

// TestOpeningIsAnnouncedBesideItself: an entry that did not exist now does,
// and it moved into the state it was born in. Both facts, in the transaction
// that made them true.
func TestOpeningIsAnnouncedBesideItself(t *testing.T) {
	t.Parallel()

	entries, out := &fakeEntries{}, &fakeOutbox{}
	credit := aCredit(t, earnings.StatePending)

	opened, err := machine(t, entries, &fakeLedger{}).Open(t.Context(), out, credit)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}

	created := out.only(t, earnings.TypeEntryCreated)
	if created.Subject != opened.ID.String() {
		t.Errorf("the creation is about %q, want the entry %s", created.Subject, opened.ID)
	}
	if created.Payload["account_id"] != credit.Member.String() {
		t.Errorf("the creation names member %v, want %s", created.Payload["account_id"], credit.Member)
	}
	amount, shaped := created.Payload["amount"].(map[string]any)
	if !shaped || amount["minor"] != float64(1800) || amount["currency"] != "EUR" {
		t.Errorf("the creation carries amount %#v, want 1800 EUR", created.Payload["amount"])
	}

	moved := out.only(t, earnings.TypeEntryStateChanged)
	if moved.Payload["from"] != "" || moved.Payload["to"] != string(earnings.StatePending) {
		t.Errorf("the move says %v to %v, want nothing to pending", moved.Payload["from"], moved.Payload["to"])
	}
}
