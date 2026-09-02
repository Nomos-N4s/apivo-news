package ops_test

// What a statement must be before it is written (T110, C-3).
//
// The run a statement lands in is immutable, so every refusal here is a row
// that would otherwise have been wrong forever. The cases say what each
// refusal names, because an operator holding a network's file needs to know
// which line and which field, not that "the statement cannot be read".

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// twoLines is a statement that paid one transaction and deducted against
// another - a deduction is a fact about the money like any other.
const twoLines = `{"lines":[` +
	`{"transaction_id":"AWIN-1","paid":{"minor":250,"currency":"EUR"}},` +
	`{"transaction_id":"AWIN-2","paid":{"minor":-40,"currency":"EUR"}}]}`

// august is the period every statement below covers.
var august = ops.Period{
	Start: time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
	End:   time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
}

// aStatement is a valid statement carrying raw, for the cases to spoil.
func aStatement(raw string) ops.Statement {
	return ops.Statement{
		Account:  uuid.New(),
		Period:   august,
		Raw:      json.RawMessage(raw),
		Operator: ops.Operator{ID: uuid.New(), DisplayName: "Ops Person"},
	}
}

// line renders one statement line with the given transaction id and paid
// member, both verbatim, so a case can spell a wrong one.
func line(id, paid string) string {
	return `{"transaction_id":` + id + `,"paid":` + paid + `}`
}

func TestAStatementIsRefusedBeforeItIsWritten(t *testing.T) {
	t.Parallel()
	eur := func(minor string) string { return `{"minor":` + minor + `,"currency":"EUR"}` }
	lines := func(ls ...string) string { return `{"lines":[` + strings.Join(ls, ",") + `]}` }

	cases := []struct {
		name  string
		spoil func(s *ops.Statement)
		want  string
	}{
		{"no publisher account", func(s *ops.Statement) { s.Account = uuid.Nil }, "names no publisher account"},
		{"nobody importing it", func(s *ops.Statement) { s.Operator = ops.Operator{} }, "nobody is importing it"},
		{"no period", func(s *ops.Statement) { s.Period = ops.Period{} }, "needs a start and an end"},
		{"no end", func(s *ops.Statement) { s.Period.End = time.Time{} }, "needs a start and an end"},
		{"a period ending where it starts", func(s *ops.Statement) { s.Period.End = s.Period.Start }, "not after it starts"},
		{"an inverted period", func(s *ops.Statement) { s.Period.Start, s.Period.End = s.Period.End, s.Period.Start }, "not after it starts"},
		// Postgres keeps microseconds, so a period the database would store
		// as empty is refused here rather than by a check constraint.
		{"a period shorter than a microsecond", func(s *ops.Statement) { s.Period.End = s.Period.Start.Add(500 * time.Nanosecond) }, "not after it starts"},
		{"not JSON", func(s *ops.Statement) { s.Raw = json.RawMessage(`statement`) }, `not a JSON object with a "lines" array`},
		{"a JSON array", func(s *ops.Statement) { s.Raw = json.RawMessage(`[]`) }, `not a JSON object with a "lines" array`},
		{"a JSON string", func(s *ops.Statement) { s.Raw = json.RawMessage(`"lines"`) }, `not a JSON object with a "lines" array`},
		{"nothing at all", func(s *ops.Statement) { s.Raw = nil }, `not a JSON object with a "lines" array`},
		{"no lines", func(s *ops.Statement) { s.Raw = json.RawMessage(`{}`) }, `has no "lines"`},
		{"null lines", func(s *ops.Statement) { s.Raw = json.RawMessage(`{"lines":null}`) }, `has no "lines"`},
		{"lines that are an object", func(s *ops.Statement) { s.Raw = json.RawMessage(`{"lines":{}}`) }, `not a JSON object with a "lines" array`},
		{"two documents", func(s *ops.Statement) { s.Raw = json.RawMessage(`{"lines":[]} {}`) }, "single JSON document"},
		{"a stray closing bracket", func(s *ops.Statement) { s.Raw = json.RawMessage(`{"lines":[]}]`) }, "single JSON document"},
		{"a line that is a number", func(s *ops.Statement) { s.Raw = json.RawMessage(`{"lines":[1]}`) }, "line 1 cannot be read"},
		{"a line naming no transaction", func(s *ops.Statement) { s.Raw = json.RawMessage(lines(`{"paid":` + eur("1") + `}`)) }, "line 1 names no transaction_id"},
		{"a line with a null transaction", func(s *ops.Statement) { s.Raw = json.RawMessage(lines(line(`null`, eur("1")))) }, "line 1 names no transaction_id"},
		{"a line with a blank transaction", func(s *ops.Statement) { s.Raw = json.RawMessage(lines(line(`"  "`, eur("1")))) }, "line 1 has a blank transaction_id"},
		{"a line with a numeric transaction", func(s *ops.Statement) { s.Raw = json.RawMessage(lines(line(`12`, eur("1")))) }, "line 1 cannot be read"},
		{"a line saying nothing about payment", func(s *ops.Statement) { s.Raw = json.RawMessage(lines(`{"transaction_id":"AWIN-1"}`)) }, "line 1 says nothing about what was paid"},
		{"a line paid null", func(s *ops.Statement) { s.Raw = json.RawMessage(lines(line(`"AWIN-1"`, `null`))) }, "line 1 says nothing about what was paid"},
		// C-6: money is minor units and a currency, never a decimal.
		{"a line paid as a decimal", func(s *ops.Statement) { s.Raw = json.RawMessage(lines(line(`"AWIN-1"`, `2.5`))) }, "line 1 cannot be read"},
		{"a line paid a fractional minor", func(s *ops.Statement) { s.Raw = json.RawMessage(lines(line(`"AWIN-1"`, eur("2.5")))) }, "line 1 cannot be read"},
		{"a line paid a minor as a string", func(s *ops.Statement) { s.Raw = json.RawMessage(lines(line(`"AWIN-1"`, eur(`"250"`)))) }, "line 1 cannot be read"},
		{"a line paid in no currency", func(s *ops.Statement) { s.Raw = json.RawMessage(lines(line(`"AWIN-1"`, `{"minor":250}`))) }, "line 1"},
		{"a line paid in a currency that is not one", func(s *ops.Statement) {
			s.Raw = json.RawMessage(lines(line(`"AWIN-1"`, `{"minor":250,"currency":"euro"}`)))
		}, "line 1"},
		{"a second line paying the first's transaction", func(s *ops.Statement) {
			s.Raw = json.RawMessage(lines(line(`"AWIN-1"`, eur("250")), line(`"AWIN-1"`, eur("250"))))
		}, "line 2 names transaction AWIN-1, which line 1 already named"},
		{"the same transaction spelled with spaces", func(s *ops.Statement) {
			s.Raw = json.RawMessage(lines(line(`"AWIN-1"`, eur("250")), line(`" AWIN-1 "`, eur("250"))))
		}, "line 2 names transaction AWIN-1, which line 1 already named"},
		{"a bad line after good ones is named by its number", func(s *ops.Statement) {
			s.Raw = json.RawMessage(lines(line(`"AWIN-1"`, eur("1")), line(`"AWIN-2"`, eur("2")), line(`"AWIN-3"`, `null`)))
		}, "line 3 says nothing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := aStatement(twoLines)
			tc.spoil(&s)
			err := s.Validate()
			if !errors.Is(err, ops.ErrInvalidStatement) {
				t.Fatalf("Validate() = %v, want one wrapping ErrInvalidStatement", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate() = %q, want it to say %q", err, tc.want)
			}
		})
	}
}

func TestAStatementThatPaidNothingIsStillAStatement(t *testing.T) {
	t.Parallel()
	s := aStatement(`{"lines":[]}`)
	if err := s.Validate(); err != nil {
		t.Fatalf("an empty statement was refused: %v", err)
	}
	lines, err := s.Lines()
	if err != nil {
		t.Fatalf("Lines(): %v", err)
	}
	if lines == nil || len(lines) != 0 {
		t.Errorf("Lines() = %v, want an empty list - it paid nothing, and said so", lines)
	}
}

func TestLinesAreReadInOrderAndTheRestIsKept(t *testing.T) {
	t.Parallel()
	// A network's statement says more than this module reads: an id of its
	// own, a total, a status per line. All of it stays in the document;
	// none of it is interpreted.
	raw := `{"statement_id":"S-9","total":{"minor":210,"currency":"EUR"},"lines":[` +
		`{"transaction_id":" AWIN-1 ","paid":{"minor":250,"currency":"EUR"},"status":"approved"},` +
		`{"transaction_id":"AWIN-2","paid":{"minor":-40,"currency":"EUR"}}],"issued":"2026-09-01"}`
	s := aStatement(raw)
	if err := s.Validate(); err != nil {
		t.Fatalf("a statement with extras was refused: %v", err)
	}
	lines, err := s.Lines()
	if err != nil {
		t.Fatalf("Lines(): %v", err)
	}
	paid := func(minor int64) money.Amount {
		t.Helper()
		amount, err := money.New(minor, money.Currency("EUR"))
		if err != nil {
			t.Fatalf("money.New(%d): %v", minor, err)
		}
		return amount
	}
	want := []ops.StatementLine{
		{TransactionID: "AWIN-1", Paid: paid(250)},
		{TransactionID: "AWIN-2", Paid: paid(-40)},
	}
	if len(lines) != len(want) {
		t.Fatalf("Lines() read %d lines, want %d: %+v", len(lines), len(want), lines)
	}
	for i := range want {
		if lines[i].TransactionID != want[i].TransactionID || !lines[i].Paid.Equal(want[i].Paid) {
			t.Errorf("line %d = %+v, want %+v", i+1, lines[i], want[i])
		}
	}
	if !bytes.Equal(s.Raw, []byte(raw)) {
		t.Error("reading the lines changed the raw statement; what is stored must be what was supplied")
	}
}
