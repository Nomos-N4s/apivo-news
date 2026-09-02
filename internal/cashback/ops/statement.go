// What a network statement is, before it is imported (T110, US6, C-3).
//
// A statement is the counterparty's own account of what it paid for a
// period: the money, as opposed to the reports, which are the network's
// intention to pay. FR-043 turns on that difference - nothing confirms until
// a statement has accounted for it - so importing one is an accounting act
// with a named human behind it, and the row it lands in is immutable (0015).
//
// Immutability is why this file is mostly refusal. A run that cannot be
// corrected must not be written wrong, so a statement is read in full and
// every line checked BEFORE anything reaches the database: the differences
// T111 derives are only as good as what was stored.

package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

var (
	// ErrInvalidStatement reports a statement that cannot be read as one.
	// Refused before anything is written, because the row it would land in
	// cannot be corrected afterwards; the text after it says which part.
	ErrInvalidStatement = errors.New("ops: the statement cannot be read")
	// ErrNoSuchNetworkAccount reports a statement for a publisher account
	// that does not exist. Answered as such rather than as the foreign
	// key's constraint name.
	ErrNoSuchNetworkAccount = errors.New("ops: no such publisher account")
	// ErrStatementNotImported reports a statement that was not written for
	// a reason that is not the statement's: the database refused or could
	// not be reached. Nothing is recorded when it is returned.
	ErrStatementNotImported = errors.New("ops: the statement was not imported")
)

// Statement is one network statement as an operator imports it.
//
// Raw is the statement as the network supplied it and is stored verbatim,
// which is why it is a document and not a list of Go values: what T111
// derives differences from must be what the counterparty said, not what this
// module understood of it. The lines are READ FROM Raw by Lines, and Validate
// refuses a statement whose lines cannot be read. The shape read is
//
//	{"lines": [{"transaction_id": "<the network's own id>",
//	            "paid": {"minor": 250, "currency": "EUR"}}, ...]}
//
// Any other member of the document, at the top level or on a line, is kept
// and ignored: a network's statement carries totals and references this
// module has no opinion about, and stripping them would store less than was
// supplied. "lines" must be present - a statement that paid nothing says so
// with an empty list; a document without the key is not a statement this
// module can read - and every line names one transaction, once, and says
// what was paid for it in the shape C-6 mandates everywhere: minor units and
// a currency, never a decimal.
type Statement struct {
	// Account is the publisher account the statement is for
	// (cashback.network_account.id). A statement is scoped to one, and so
	// is every report it can disagree with.
	Account uuid.UUID
	// Period is what the statement covers. Reports are matched to it on
	// when the purchase happened, not on when Apivo polled for it.
	Period Period
	// Raw is the statement itself; see the type comment.
	Raw json.RawMessage
	// Operator is the named human importing it, recorded as imported_by
	// (US6, FR-061).
	Operator Operator
}

// Period is the half-open interval [Start, End) a statement covers.
type Period struct {
	Start time.Time
	End   time.Time
}

// normalised is the period as the database will hold it. Postgres keeps
// microseconds, so a caller's nanoseconds would make the stored period differ
// from the one echoed back - and from the one the same statement is looked up
// by on a retry.
func (p Period) normalised() Period {
	return Period{Start: p.Start.Truncate(time.Microsecond), End: p.End.Truncate(time.Microsecond)}
}

// StatementLine is one payment a statement names.
type StatementLine struct {
	// TransactionID is the network's own identifier for the transaction:
	// what network_transaction.external_id holds for the report it pays.
	// Surrounding whitespace is not part of an identifier and is removed.
	TransactionID string
	// Paid is what the statement says was paid for it. Negative is allowed:
	// a statement may carry a deduction against an earlier payment, and a
	// deduction is a fact about the money like any other.
	Paid money.Amount
}

// rawLine is a line as it sits in the document. Pointers, so that an absent
// member and a null one are both "not said" rather than a zero value that
// reads as a payment of nothing.
type rawLine struct {
	TransactionID *string       `json:"transaction_id"`
	Paid          *money.Amount `json:"paid"`
}

// readLine reads one line, so that a line that cannot be read is reported
// by its number rather than as a failure of the whole document.
func readLine(n int, raw json.RawMessage) (StatementLine, error) {
	var line rawLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return StatementLine{}, fmt.Errorf("%w: line %d cannot be read: %w", ErrInvalidStatement, n, err)
	}
	switch {
	case line.TransactionID == nil:
		return StatementLine{}, fmt.Errorf("%w: line %d names no transaction_id", ErrInvalidStatement, n)
	case line.Paid == nil:
		return StatementLine{}, fmt.Errorf("%w: line %d says nothing about what was paid", ErrInvalidStatement, n)
	}
	id := strings.TrimSpace(*line.TransactionID)
	if id == "" {
		return StatementLine{}, fmt.Errorf("%w: line %d has a blank transaction_id", ErrInvalidStatement, n)
	}
	if err := line.Paid.Validate(); err != nil {
		return StatementLine{}, fmt.Errorf("%w: line %d (%s): %w", ErrInvalidStatement, n, id, err)
	}
	return StatementLine{TransactionID: id, Paid: *line.Paid}, nil
}

// Validate refuses a statement that must not be written: one naming no
// account or no operator, one whose period is empty or inverted, or one whose
// lines cannot be read. Every refusal wraps ErrInvalidStatement and says
// which part.
func (s Statement) Validate() error {
	period := s.Period.normalised()
	switch {
	case s.Account == uuid.Nil:
		return fmt.Errorf("%w: it names no publisher account", ErrInvalidStatement)
	case s.Operator.ID == uuid.Nil:
		return fmt.Errorf("%w: nobody is importing it; an import records who (FR-061)", ErrInvalidStatement)
	case period.Start.IsZero() || period.End.IsZero():
		return fmt.Errorf("%w: the period needs a start and an end", ErrInvalidStatement)
	case !period.End.After(period.Start):
		return fmt.Errorf("%w: the period ends at %s, which is not after it starts at %s",
			ErrInvalidStatement, period.End.UTC().Format(time.RFC3339Nano), period.Start.UTC().Format(time.RFC3339Nano))
	}
	_, err := s.Lines()
	return err
}

// Lines reads the payments out of the raw statement, in document order.
//
// It is the one place the document's shape is interpreted, so that the
// import and the difference detection that later reads the stored statement
// agree on what a line is by construction rather than by coincidence.
func (s Statement) Lines() ([]StatementLine, error) {
	// The lines are kept raw here and read one at a time below, so a line
	// that cannot be read is reported by its number and not as a failure
	// of the whole document.
	var doc struct {
		Lines *[]json.RawMessage `json:"lines"`
	}
	dec := json.NewDecoder(bytes.NewReader(s.Raw))
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: it is not a JSON object with a \"lines\" array: %w", ErrInvalidStatement, err)
	}
	// Only a second Decode proves the document ended: More() would accept a
	// stray closing bracket after a valid object.
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: it must be a single JSON document", ErrInvalidStatement)
	}
	if doc.Lines == nil {
		return nil, fmt.Errorf("%w: it has no \"lines\"; a statement that paid nothing says so with an empty list", ErrInvalidStatement)
	}

	lines := make([]StatementLine, 0, len(*doc.Lines))
	seen := make(map[string]int, len(*doc.Lines))
	for i, raw := range *doc.Lines {
		n := i + 1
		line, err := readLine(n, raw)
		if err != nil {
			return nil, err
		}
		if first, dup := seen[line.TransactionID]; dup {
			return nil, fmt.Errorf("%w: line %d names transaction %s, which line %d already named; a statement pays a transaction once",
				ErrInvalidStatement, n, line.TransactionID, first)
		}
		seen[line.TransactionID] = n
		lines = append(lines, line)
	}
	return lines, nil
}

// ImportedStatement is the run a statement became, or already was.
type ImportedStatement struct {
	ID      uuid.UUID
	Account uuid.UUID
	Network networks.NetworkID
	Period  Period
	// Lines is how many payments the statement names.
	Lines int
	// Digest identifies the statement's content, computed by the database
	// from what it stored (0028).
	Digest     string
	ImportedBy uuid.UUID
	ImportedAt time.Time
	// AlreadyImported reports that this exact statement for this account and
	// period was here before this call, and that ID, ImportedBy and
	// ImportedAt are that earlier import's. Nothing was written and nothing
	// was announced: the first import already did both.
	AlreadyImported bool
}

// StatementImporter is what the import endpoint needs from a store, named
// here per the boundary rules. *PGStore satisfies it.
type StatementImporter interface {
	ImportStatement(ctx context.Context, s Statement) (ImportedStatement, error)
}
