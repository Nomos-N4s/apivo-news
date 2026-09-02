package ops_test

// The derivation, as a table (T111, US6). Derive is pure, so every rule it
// applies can be stated as inputs and the differences they must produce -
// and, as importantly, the silences: a matched payment, a pending report the
// statement does not mention, a declined report nobody paid.

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// eur is an amount in euros, for reading the tables below.
func eur(minor int64) money.Amount { return money.Amount{Minor: minor, Currency: "EUR"} }

// gbp is the other currency the tables need.
func gbp(minor int64) money.Amount { return money.Amount{Minor: minor, Currency: "GBP"} }

// inAugust and beforeAugust place a purchase inside and before the period.
var (
	inAugust     = time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	beforeAugust = time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
)

// current builds one current report.
func current(externalID string, status networks.Status, commission money.Amount, at time.Time) ops.CurrentReport {
	return ops.CurrentReport{ID: uuid.New(), ExternalID: externalID, Status: status, Commission: commission, TransactedAt: at}
}

// paidLine builds one statement line.
func paidLine(id string, paid money.Amount) ops.StatementLine {
	return ops.StatementLine{TransactionID: id, Paid: paid}
}

// expectation is a difference the way a case states it: by kind,
// transaction and figures, without the report id the case cannot know
// until it built the report.
type expectation struct {
	kind     ops.DifferenceKind
	txn      string
	expected *money.Amount
	actual   *money.Amount
	// namesReport says whether the difference must carry the report's id.
	namesReport bool
}

func want(kind ops.DifferenceKind, txn string, expected, actual *money.Amount) expectation {
	return expectation{kind: kind, txn: txn, expected: expected, actual: actual, namesReport: kind != ops.PaidNotReported}
}

func amount(a money.Amount) *money.Amount { return &a }

func TestDeriveStatesEveryDisagreementAndNothingElse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		lines   []ops.StatementLine
		reports []ops.CurrentReport
		want    []expectation
	}{
		{
			name:    "a matched payment is silence",
			lines:   []ops.StatementLine{paidLine("A", eur(499))},
			reports: []ops.CurrentReport{current("A", networks.StatusConfirmed, eur(499), inAugust)},
		},
		{
			name:    "a confirmed report nobody paid is reported-not-paid",
			reports: []ops.CurrentReport{current("A", networks.StatusConfirmed, eur(499), inAugust)},
			want:    []expectation{want(ops.ReportedNotPaid, "A", amount(eur(499)), nil)},
		},
		{
			name:    "a shorted payment is a mismatch with both figures",
			lines:   []ops.StatementLine{paidLine("A", eur(450))},
			reports: []ops.CurrentReport{current("A", networks.StatusConfirmed, eur(499), inAugust)},
			want:    []expectation{want(ops.AmountMismatch, "A", amount(eur(499)), amount(eur(450)))},
		},
		{
			name:    "an overpayment is a mismatch too",
			lines:   []ops.StatementLine{paidLine("A", eur(520))},
			reports: []ops.CurrentReport{current("A", networks.StatusConfirmed, eur(499), inAugust)},
			want:    []expectation{want(ops.AmountMismatch, "A", amount(eur(499)), amount(eur(520)))},
		},
		{
			name:  "money naming no report is paid-not-reported",
			lines: []ops.StatementLine{paidLine("X", eur(120))},
			want:  []expectation{want(ops.PaidNotReported, "X", nil, amount(eur(120)))},
		},
		{
			// The spec's independent test, in one table row: both flagged,
			// each with its delta.
			name: "an omitted and a shorted transaction are both flagged",
			lines: []ops.StatementLine{
				paidLine("B", eur(450)), paidLine("C", eur(300)), paidLine("X", eur(120)),
			},
			reports: []ops.CurrentReport{
				current("A", networks.StatusConfirmed, eur(499), inAugust),
				current("B", networks.StatusConfirmed, eur(499), inAugust),
				current("C", networks.StatusConfirmed, eur(300), inAugust),
			},
			want: []expectation{
				want(ops.ReportedNotPaid, "A", amount(eur(499)), nil),
				want(ops.AmountMismatch, "B", amount(eur(499)), amount(eur(450))),
				want(ops.PaidNotReported, "X", nil, amount(eur(120))),
			},
		},
		{
			name:    "a pending report is not yet expected on a statement",
			reports: []ops.CurrentReport{current("P", networks.StatusPending, eur(200), inAugust)},
		},
		{
			name:    "a pending report that was paid must still match",
			lines:   []ops.StatementLine{paidLine("P", eur(180))},
			reports: []ops.CurrentReport{current("P", networks.StatusPending, eur(200), inAugust)},
			want:    []expectation{want(ops.AmountMismatch, "P", amount(eur(200)), amount(eur(180)))},
		},
		{
			name:    "a declined report is owed nothing, so paying it is a mismatch",
			lines:   []ops.StatementLine{paidLine("D", eur(300))},
			reports: []ops.CurrentReport{current("D", networks.StatusDeclined, eur(300), inAugust)},
			want:    []expectation{want(ops.AmountMismatch, "D", amount(eur(0)), amount(eur(300)))},
		},
		{
			name:    "a declined report nobody paid is silence",
			reports: []ops.CurrentReport{current("D", networks.StatusDeclined, eur(300), inAugust)},
		},
		{
			name:    "a reversed report paid nothing is silence",
			lines:   []ops.StatementLine{paidLine("R", eur(0))},
			reports: []ops.CurrentReport{current("R", networks.StatusReversed, eur(300), inAugust)},
		},
		{
			name:    "a confirmed report from before the period is not expected on this statement",
			reports: []ops.CurrentReport{current("E", networks.StatusConfirmed, eur(499), beforeAugust)},
		},
		{
			// A late payment for an earlier purchase is still a payment for
			// it, and is still compared.
			name:    "a payment for an earlier purchase is compared all the same",
			lines:   []ops.StatementLine{paidLine("E", eur(450))},
			reports: []ops.CurrentReport{current("E", networks.StatusConfirmed, eur(499), beforeAugust)},
			want:    []expectation{want(ops.AmountMismatch, "E", amount(eur(499)), amount(eur(450)))},
		},
		{
			name:    "a payment in another currency is money nothing expected, and the report goes unpaid",
			lines:   []ops.StatementLine{paidLine("A", gbp(499))},
			reports: []ops.CurrentReport{current("A", networks.StatusConfirmed, eur(499), inAugust)},
			want: []expectation{
				want(ops.PaidNotReported, "A", nil, amount(gbp(499))),
				want(ops.ReportedNotPaid, "A", amount(eur(499)), nil),
			},
		},
		{
			name: "differences come back in transaction order",
			lines: []ops.StatementLine{
				paidLine("Z", eur(1)), paidLine("M", eur(1)),
			},
			reports: []ops.CurrentReport{current("A", networks.StatusConfirmed, eur(499), inAugust)},
			want: []expectation{
				want(ops.ReportedNotPaid, "A", amount(eur(499)), nil),
				want(ops.PaidNotReported, "M", nil, amount(eur(1))),
				want(ops.PaidNotReported, "Z", nil, amount(eur(1))),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ops.Derive(tc.lines, tc.reports, august)
			if len(got) != len(tc.want) {
				t.Fatalf("Derive() found %d difference(s), want %d:\n%s", len(got), len(tc.want), describe(got))
			}
			byExternal := map[string]uuid.UUID{}
			for _, r := range tc.reports {
				byExternal[r.ExternalID] = r.ID
			}
			for i, w := range tc.want {
				d := got[i]
				switch {
				case d.Kind != w.kind:
					t.Errorf("difference %d is %s, want %s", i, d.Kind, w.kind)
				case d.TransactionID != w.txn:
					t.Errorf("difference %d is about %s, want %s", i, d.TransactionID, w.txn)
				case !sameAmount(d.Expected, w.expected):
					t.Errorf("difference %d expected %v, want %v", i, d.Expected, w.expected)
				case !sameAmount(d.Actual, w.actual):
					t.Errorf("difference %d actual %v, want %v", i, d.Actual, w.actual)
				case w.namesReport && d.Report != byExternal[w.txn]:
					t.Errorf("difference %d names report %s, want %s's current report %s", i, d.Report, w.txn, byExternal[w.txn])
				case !w.namesReport && d.Report != uuid.Nil:
					t.Errorf("difference %d names report %s; money matching no report names none", i, d.Report)
				}
			}
		})
	}
}

func TestADifferenceHasTheCurrencyOfItsMoney(t *testing.T) {
	t.Parallel()
	paid := ops.Difference{Kind: ops.PaidNotReported, TransactionID: "X", Actual: amount(gbp(1))}
	if paid.Currency() != "GBP" {
		t.Errorf("a payment's currency = %s, want GBP", paid.Currency())
	}
	unpaid := ops.Difference{Kind: ops.ReportedNotPaid, TransactionID: "A", Expected: amount(eur(1))}
	if unpaid.Currency() != "EUR" {
		t.Errorf("an expectation's currency = %s, want EUR", unpaid.Currency())
	}
}

// sameAmount compares two optional amounts.
func sameAmount(got, want *money.Amount) bool {
	switch {
	case got == nil && want == nil:
		return true
	case got == nil || want == nil:
		return false
	}
	return got.Equal(*want)
}

// describe renders the differences for a failure message.
func describe(ds []ops.Difference) string {
	out := ""
	for _, d := range ds {
		out += "  " + string(d.Kind) + " " + d.TransactionID
		if d.Expected != nil {
			out += " expected " + d.Expected.String()
		}
		if d.Actual != nil {
			out += " actual " + d.Actual.String()
		}
		out += "\n"
	}
	return out
}

func TestTheDeltaIsWhatWasPaidLessWhatWasOwed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		d    ops.Difference
		want money.Amount
	}{
		{"an unpaid report is its whole amount missing", ops.Difference{Kind: ops.ReportedNotPaid, Expected: amount(eur(499))}, eur(-499)},
		{"a shorted report is the shortfall", ops.Difference{Kind: ops.AmountMismatch, Expected: amount(eur(499)), Actual: amount(eur(450))}, eur(-49)},
		{"an overpaid report is the excess", ops.Difference{Kind: ops.AmountMismatch, Expected: amount(eur(499)), Actual: amount(eur(520))}, eur(21)},
		{"a paid declined report is the whole payment", ops.Difference{Kind: ops.AmountMismatch, Expected: amount(eur(0)), Actual: amount(eur(300))}, eur(300)},
		{"money matching no report is the whole payment", ops.Difference{Kind: ops.PaidNotReported, Actual: amount(gbp(120))}, gbp(120)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.d.Delta()
			if err != nil {
				t.Fatalf("Delta(): %v", err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("Delta() = %s, want %s", got, tc.want)
			}
		})
	}
}
