package linkwise

// Every row of the recording translated, not only the one that falls inside a
// seven-day window (T247).
//
// The external suite drives FetchTransactions and therefore sees one
// transaction: the recording spans June to December 2024 and this adapter's
// widest window is a week. That leaves two recorded rows exercising nothing,
// and one of them is the interesting one - an insurance lead where the
// commission is larger than the amount, which is exactly the shape a
// validation written from retail assumptions would refuse.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// aClient builds a client with no transport: translate never reaches one.
func aClient(t *testing.T) *Client {
	t.Helper()
	account, err := networks.NewPublisherAccount(
		[16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, ID, "CD20")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}
	client, err := New(account, WithCredential("user", "pass"), WithReportCurrency("EUR"))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return client
}

func TestEveryRecordedRowTranslates(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "transactions.json"))
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}
	rows, err := decodeReport(raw)
	if err != nil {
		t.Fatalf("decodeReport(): %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("the recording carries %d rows, want the 3 it was captured with", len(rows))
	}

	client := aClient(t)
	want := []networks.Reported{
		{
			ExternalID: "420343717", StatusRaw: "Validated", Status: networks.StatusConfirmed,
			SaleAmount: money.Amount{Minor: 1951, Currency: "EUR"},
			Commission: money.Amount{Minor: 293, Currency: "EUR"},
			// 19:10:54+03:00
			TransactedAt: time.Date(2024, time.June, 7, 16, 10, 54, 0, time.UTC),
		},
		{
			ExternalID: "431535475", StatusRaw: "Validated", Status: networks.StatusConfirmed,
			SaleAmount: money.Amount{Minor: 16400, Currency: "EUR"},
			Commission: money.Amount{Minor: 377, Currency: "EUR"},
			// 16:33:58+03:00
			TransactedAt: time.Date(2024, time.October, 3, 13, 33, 58, 0, time.UTC),
		},
		{
			// The lead-generation shape: a commission of 18.50 on an amount
			// of 10.00. Nothing here judges it - this port records what the
			// network said - and a validation that assumed a commission is a
			// fraction of a sale would refuse a real transaction.
			ExternalID: "438545831", StatusRaw: "Validated", Status: networks.StatusConfirmed,
			SaleAmount: money.Amount{Minor: 1000, Currency: "EUR"},
			Commission: money.Amount{Minor: 1850, Currency: "EUR"},
			// 06:58:45+02:00, the winter offset - the recording spans a
			// daylight-saving change, so a fixed +03:00 would be an hour out
			// on this one row and only this one.
			TransactedAt: time.Date(2024, time.December, 16, 4, 58, 45, 0, time.UTC),
		},
	}

	for i, row := range rows {
		got, err := client.translate(row)
		if err != nil {
			t.Fatalf("translating row %d: %v", i+1, err)
		}
		w := want[i]
		switch {
		case got.ExternalID != w.ExternalID:
			t.Errorf("row %d: ExternalID = %q, want %q", i+1, got.ExternalID, w.ExternalID)
		case got.StatusRaw != w.StatusRaw:
			t.Errorf("row %d: StatusRaw = %q, want %q", i+1, got.StatusRaw, w.StatusRaw)
		case got.Status != w.Status:
			t.Errorf("row %d: Status = %q, want %q", i+1, got.Status, w.Status)
		case got.SaleAmount != w.SaleAmount:
			t.Errorf("row %d: SaleAmount = %s, want %s", i+1, got.SaleAmount, w.SaleAmount)
		case got.Commission != w.Commission:
			t.Errorf("row %d: Commission = %s, want %s", i+1, got.Commission, w.Commission)
		case !got.TransactedAt.Equal(w.TransactedAt):
			t.Errorf("row %d: TransactedAt = %s, want %s", i+1, got.TransactedAt, w.TransactedAt)
		}
		if _, present := got.ClickRef.Ref(); present {
			t.Errorf("row %d: a null subid1 became a present reference", i+1)
		}
		// Contract rule 1: the fragment, not a re-encoding of it.
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(got.RawPayload, &payload); err != nil {
			t.Errorf("row %d: the payload is not the row's own object: %v", i+1, err)
		}
		if _, ok := payload["payout_categories"]; !ok {
			t.Errorf("row %d: the payload lost the fields the adapter does not read", i+1)
		}
	}
}

// TestTheRecordingSpansADaylightSavingChange, which is why every timestamp is
// parsed with its own offset rather than shifted by a constant.
//
// The June and October rows carry +03:00 and the December row carries +02:00.
// An adapter that had hard-coded Athens' summer offset would be an hour out
// on winter transactions, and an hour is enough to move one across a window
// seam.
func TestTheRecordingSpansADaylightSavingChange(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "transactions.json"))
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}
	var rows []struct {
		Date string `json:"date"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("the recording will not parse: %v", err)
	}
	offsets := map[string]int{}
	for _, row := range rows {
		at, err := time.Parse(time.RFC3339, row.Date)
		if err != nil {
			t.Fatalf("%q is not an ISO 8601 timestamp: %v", row.Date, err)
		}
		_, offset := at.Zone()
		offsets[row.Date] = offset
	}
	seen := map[int]bool{}
	for _, offset := range offsets {
		seen[offset] = true
	}
	if len(seen) < 2 {
		t.Errorf("every recorded timestamp carries the same offset (%v), so this recording cannot hold the parser to reading each one's own", seen)
	}
}
