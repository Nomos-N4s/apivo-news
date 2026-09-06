package linkwise

// Linkwise's status vocabulary mapped into the domain's (T247).
//
// An internal test because the mapping is private and its two failure modes
// are both silent about money: a word matched in the wrong casing maps every
// real transaction to unknown, and a word mapped by guesswork moves a
// member's balance on a state nobody established.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// TestTheReportsOwnCasingMaps is the case that separates this table from one
// written out of the documentation.
//
// The usage text spells the filter values lower-case; the report answers
// capitalised. An adapter tested only against the documented spelling passes
// every test and maps every real transaction to unknown.
func TestTheReportsOwnCasingMaps(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		raw  string
		want networks.Status
	}{
		// As the report answers.
		{raw: "Pending", want: networks.StatusPending},
		{raw: "Validated", want: networks.StatusConfirmed},
		{raw: "Cancelled", want: networks.StatusDeclined},
		// As the filter spells it.
		{raw: "pending", want: networks.StatusPending},
		{raw: "validated", want: networks.StatusConfirmed},
		{raw: "cancelled", want: networks.StatusDeclined},
		// And with the whitespace a hand-edited fixture picks up.
		{raw: " Validated ", want: networks.StatusConfirmed},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()
			got, err := mapTransactionStatus("420343717", tt.raw)
			if err != nil {
				t.Fatalf("mapTransactionStatus(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("mapTransactionStatus(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestTheStatusInTheRecordingMaps closes the loop between the table and the
// evidence: whatever the live API actually answered has an entry.
func TestTheStatusInTheRecordingMaps(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "transactions.json"))
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}
	var rows []struct {
		ID     int64 `json:"id"`
		Status struct {
			Name string `json:"name"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("the recording will not parse: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the recording carries no transactions, so it holds this table to nothing")
	}
	for _, row := range rows {
		if _, err := mapTransactionStatus("420343717", row.Status.Name); err != nil {
			t.Errorf("the live API answered %q and this table does not map it: %v", row.Status.Name, err)
		}
	}
}

// TestAWordNobodyMappedIsRefused holds contract rule 2's totality. The
// refusal must also be USEFUL: an operator receiving it has to decide what a
// new word means before anybody's money moves, and "unknown status" alone
// sends them to documentation this network does not publish.
func TestAWordNobodyMappedIsRefused(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"",
		"approved",
		"void",
		"Παραγγελία",
		// The one that looks like it belongs. "pending_validated" is a
		// FILTER value meaning "pending OR validated" - two states asked for
		// together - not a state a transaction is in, so mapping it would
		// assign one domain status to a word that means two.
		"pending_validated",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			status, err := mapTransactionStatus("420343717", raw)
			if !errors.Is(err, networks.ErrUnmappableStatus) {
				t.Fatalf("mapTransactionStatus(%q) = %q, %v; want ErrUnmappableStatus", raw, status, err)
			}
			if status != "" {
				t.Errorf("a refused word still produced the status %q", status)
			}
			if !strings.Contains(err.Error(), "420343717") {
				t.Errorf("the refusal does not name the transaction: %v", err)
			}
			for _, known := range []string{"pending", "validated", "cancelled"} {
				if !strings.Contains(err.Error(), known) {
					t.Errorf("the refusal does not list %q among what this network is known to report: %v", known, err)
				}
			}
		})
	}
}

// TestReversedIsUnreachableOnThisNetwork pins the finding rather than the
// absence.
//
// Linkwise answers "Cancelled" whether a transaction was refused on day one
// or validated and taken back on day sixty; there is one status field and it
// carries one of three words. So no input maps to reversed, and a later
// reader who notices the gap should find this test rather than fill it in
// with a guess. The distinction is recoverable from the supersede chain - a
// declined report whose current stored row is confirmed - which is the
// ingestion path's knowledge and not an adapter's.
func TestReversedIsUnreachableOnThisNetwork(t *testing.T) {
	t.Parallel()

	for word, status := range transactionStatuses {
		if status == networks.StatusReversed {
			t.Errorf("%q maps to reversed; Linkwise publishes no word for a reversal, so this entry was invented rather than observed", word)
		}
	}
	// And the words somebody would reach for to fill the gap are refused
	// rather than quietly mapped.
	for _, guess := range []string{"reversed", "amended", "refunded", "chargeback"} {
		if _, err := mapTransactionStatus("420343717", guess); !errors.Is(err, networks.ErrUnmappableStatus) {
			t.Errorf("mapTransactionStatus(%q) was mapped; no evidence exists that Linkwise reports it", guess)
		}
	}
}

// TestEveryMappedStatusIsADomainStatus. A table entry that was not one of the
// four would be refused by Reported.Validate at the last moment instead of
// here.
func TestEveryMappedStatusIsADomainStatus(t *testing.T) {
	t.Parallel()

	for word, status := range transactionStatuses {
		if !status.Valid() {
			t.Errorf("%q maps to %q, which is not a domain status", word, status)
		}
	}
}
