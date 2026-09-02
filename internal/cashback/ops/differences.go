// Deriving a run's differences (T111, US6).
//
// A statement says what the network paid; the reports say what it said it
// would pay. Derive lays the two side by side and answers every place they
// disagree. It is pure - it reads nothing and writes nothing - so that every
// rule it applies can be stated as a table of inputs and outcomes, and so
// that detection is repeatable by construction: the same statement and the
// same reports derive the same differences every time.
//
// The comparison is Go rather than SQL, so that the statement's lines are
// read by exactly the code that read them at import (statement.go), and
// import and detection cannot disagree about what a line is.

package ops

import (
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// DifferenceKind is one of the three ways a statement and the reports can
// disagree, spelled as the schema spells it.
type DifferenceKind string

const (
	// ReportedNotPaid is a report the network confirmed, for a purchase in
	// the statement's period, that no line of the statement paid.
	ReportedNotPaid DifferenceKind = "reported_not_paid"
	// PaidNotReported is a line of the statement naming no report of this
	// publisher account - or naming one in another currency, which is the
	// same thing: money nothing expected.
	PaidNotReported DifferenceKind = "paid_not_reported"
	// AmountMismatch is a line that paid a report a different amount than
	// the report says is owed.
	AmountMismatch DifferenceKind = "amount_mismatch"
)

// Difference is one disagreement, as derived. It becomes a row of
// cashback.reconciliation_difference when recorded.
type Difference struct {
	Kind DifferenceKind
	// Report is the current report the difference is about; uuid.Nil for
	// PaidNotReported, which is about a line no report matches.
	Report uuid.UUID
	// TransactionID is the network's own id for the transaction: the line's
	// transaction_id, which for a matched line is also the report's
	// external_id. Set for every kind, so an operator reading the derivation
	// sees which transaction; stored only for PaidNotReported, where there
	// is no report to name it (0029).
	TransactionID string
	// Expected is what the report says is owed; nil for PaidNotReported.
	Expected *money.Amount
	// Actual is what the statement paid; nil for ReportedNotPaid.
	Actual *money.Amount
}

// Currency is the difference's currency: the payment's if there is one,
// otherwise the expectation's. Every kind has at least one.
func (d Difference) Currency() money.Currency {
	if d.Actual != nil {
		return d.Actual.Currency
	}
	return d.Expected.Currency
}

// Delta is the difference as money: what was paid less what was owed, in
// the difference's currency. Negative is money missing - a shorted or an
// unpaid report; positive is money nothing expected - an overpayment, a paid
// declined report, or a line matching no report. Checked arithmetic, because
// two figures in minor units can still overflow and C-6 does not round.
func (d Difference) Delta() (money.Amount, error) {
	expected := money.Amount{Currency: d.Currency()}
	if d.Expected != nil {
		expected = *d.Expected
	}
	actual := money.Amount{Currency: d.Currency()}
	if d.Actual != nil {
		actual = *d.Actual
	}
	return actual.Sub(expected)
}

// CurrentReport is a report as detection sees it: the tip of its
// transaction's chain, reduced to what the comparison needs.
type CurrentReport struct {
	ID         uuid.UUID
	ExternalID string
	Status     networks.Status
	// Commission is what the network reported it would pay, in the
	// report's own currency.
	Commission   money.Amount
	TransactedAt time.Time
}

// owed is what the network owes on this report as it stands: the commission
// while the network still intends to pay it, nothing once it has declined
// or reversed it. A pending report is owed in the sense that matters here -
// if the statement pays it, the amount must match - but it is not yet
// expected on a statement, which Derive handles separately.
func (r CurrentReport) owed() money.Amount {
	switch r.Status {
	case networks.StatusDeclined, networks.StatusReversed:
		return money.Amount{Minor: 0, Currency: r.Commission.Currency}
	default:
		return r.Commission
	}
}

// Derive lays the statement's lines beside the current reports and answers
// every disagreement, in transaction order. Pure: it reads nothing and
// writes nothing, which is what makes it testable against a table of cases
// and what keeps detection re-runnable.
//
// reports must be the CURRENT row of every transaction the lines name plus
// every current confirmed report for the period - CurrentReportsForStatement
// reads exactly that - and period is the statement's, half-open.
//
// The rules, in the order they are applied:
//
//   - A line naming no report is PaidNotReported: money nothing expected.
//   - A line naming a report in another currency is PaidNotReported too, and
//     the report is treated as unpaid: a payment in the wrong currency is not
//     a payment of this report, and a single row cannot carry two currencies.
//   - A line naming a report is compared to what the report is owed: the
//     commission while the network intends to pay, nothing once it declined
//     or reversed. A different figure is AmountMismatch; the same is silence.
//   - A confirmed report for a purchase in the period that no line paid is
//     ReportedNotPaid. A pending report is not yet expected on a statement;
//     a declined or reversed one is owed nothing, so its absence says nothing.
func Derive(lines []StatementLine, reports []CurrentReport, period Period) []Difference {
	byExternalID := make(map[string]CurrentReport, len(reports))
	for _, r := range reports {
		byExternalID[r.ExternalID] = r
	}
	paid := make(map[string]bool, len(lines))
	var found []Difference

	for _, line := range lines {
		report, named := byExternalID[line.TransactionID]
		actual := line.Paid
		switch {
		case !named, report.Commission.Currency != actual.Currency:
			found = append(found, Difference{Kind: PaidNotReported, TransactionID: line.TransactionID, Actual: &actual})
			continue
		}
		paid[line.TransactionID] = true
		expected := report.owed()
		if expected.Minor != actual.Minor {
			found = append(found, Difference{
				Kind: AmountMismatch, Report: report.ID, TransactionID: line.TransactionID,
				Expected: &expected, Actual: &actual,
			})
		}
	}

	for _, r := range reports {
		if paid[r.ExternalID] || r.Status != networks.StatusConfirmed || !period.contains(r.TransactedAt) {
			continue
		}
		expected := r.Commission
		found = append(found, Difference{Kind: ReportedNotPaid, Report: r.ID, TransactionID: r.ExternalID, Expected: &expected})
	}

	sort.SliceStable(found, func(i, j int) bool { return found[i].TransactionID < found[j].TransactionID })
	return found
}

// contains reports whether at is inside the half-open period.
func (p Period) contains(at time.Time) bool {
	return !at.Before(p.Start) && at.Before(p.End)
}
