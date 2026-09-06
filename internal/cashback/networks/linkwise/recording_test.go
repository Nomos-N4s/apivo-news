package linkwise_test

// The recordings in testdata/, and what they are evidence FOR.
//
// They were captured from the live API against a real publisher account on
// 2026-09-04, with credentials that have since expired. Linkwise publishes no
// schema, so these files ARE the schema: every claim the adapter makes about
// the wire format is a claim about their STRUCTURE.
//
// Structure, and deliberately not bytes. The responses arrived with CRLF line
// endings and the repository normalises text to LF (.gitattributes, `* text=auto`),
// so what is committed is not byte-identical to what the server sent. That
// costs nothing here - JSON does not care, and every assertion below is about
// field names, nesting and types. It would cost something if a test ever
// wanted to prove RawPayload preserves a response verbatim: such a test needs
// bytes captured under `-text`, not these.
//
// The assertions below are deliberately about SHAPE rather than values. A
// test that pinned "commission is 2.93" would break the day somebody
// re-records and teach nothing; what has to hold is that the fields the
// adapter reads are the fields the network sends, under the names it sends
// them under.
//
// The one exception is subid1. That field is asserted by name and by
// position because its presence is the finding the whole Linkwise plan turned
// on: if the network did not echo a publisher-supplied reference back on a
// transaction, the click model could not attribute at all and this adapter
// would not exist.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readRecording(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}
	return raw
}

// TestTheTransactionRecordingCarriesASubID is the kill criterion, inverted
// into an assertion.
//
// K-L2 was "the reporting join key is Linkwise's own identifier and sub-ids
// are not echoed on transactions" - which would have made the current click
// model unable to attribute on this network at all. The recording settles it:
// subid1, subid2 and subid3 are present on every transaction the report
// returns, under those exact names.
//
// They are null in the recording, and that is expected rather than a
// weakness: this account's history predates any click this system issued.
// What matters is that the FIELD exists to be filled.
func TestTheTransactionRecordingCarriesASubID(t *testing.T) {
	t.Parallel()

	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(readRecording(t, "transactions.json"), &rows); err != nil {
		t.Fatalf("the recording is not a JSON array of objects: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the recording carries no transactions, so it is evidence of nothing")
	}

	for i, row := range rows {
		for _, field := range []string{"subid1", "subid2", "subid3"} {
			if _, ok := row[field]; !ok {
				t.Errorf("transaction %d does not carry %q; without it a transaction cannot be traced to the click that earned it", i, field)
			}
		}
	}
}

// TestTheTransactionRecordingCarriesTheMoneyAndTheLifecycle covers the rest
// of what an adapter must read to open an entry: the amounts, the three
// dates, and the network's own identifier for the transaction.
//
// The three dates matter separately. click -> transaction is what attribution
// joins on; transaction -> status is the approval, and it can arrive months
// later, which is why the report's based_on=status filter is the axis this
// network is polled on for late validations.
func TestTheTransactionRecordingCarriesTheMoneyAndTheLifecycle(t *testing.T) {
	t.Parallel()

	var rows []struct {
		ID     int64  `json:"id"`
		Type   string `json:"type"`
		Amount string `json:"amount"`
		Commis string `json:"commission"`
		Date   string `json:"date"`
		Status struct {
			Name string `json:"name"`
			Date string `json:"date"`
		} `json:"status"`
		Click struct {
			Date   string `json:"date"`
			RefURL any    `json:"ref_url"`
		} `json:"click"`
		Program struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"program"`
	}
	if err := json.Unmarshal(readRecording(t, "transactions.json"), &rows); err != nil {
		t.Fatalf("the recording does not match the shape the adapter reads: %v", err)
	}

	for i, r := range rows {
		switch {
		case r.ID == 0:
			t.Errorf("transaction %d has no id; it is the network's own key and the evidence row's identity", i)
		case r.Amount == "":
			t.Errorf("transaction %d has no amount", i)
		case r.Commis == "":
			t.Errorf("transaction %d has no commission; it is what a member is paid out of", i)
		case r.Status.Name == "":
			t.Errorf("transaction %d has no status name", i)
		case r.Status.Date == "":
			t.Errorf("transaction %d has no status date, so a late validation could never be found on the status axis", i)
		case r.Click.Date == "":
			t.Errorf("transaction %d has no click date", i)
		case r.Date == "":
			t.Errorf("transaction %d has no transaction date", i)
		case r.Program.ID == 0:
			t.Errorf("transaction %d names no programme, so it cannot be tied to a retailer", i)
		}
	}
}

// TestTheStatusVocabularyIsTheOneTheDocsName. The usage text lists pending,
// validated, cancelled and pending_validated as the values the status FILTER
// accepts. The recording shows what the report RETURNS, which is capitalised
// and nested under an object rather than flat - a difference worth pinning,
// because an adapter that compared against the filter's spelling would map
// every transaction to unknown.
func TestTheStatusVocabularyIsTheOneTheDocsName(t *testing.T) {
	t.Parallel()

	var rows []struct {
		Status struct {
			Name string `json:"name"`
		} `json:"status"`
	}
	if err := json.Unmarshal(readRecording(t, "transactions.json"), &rows); err != nil {
		t.Fatalf("reading the recording: %v", err)
	}

	// Every status seen in the recording. Not an exhaustive vocabulary -
	// this account's history happens to be all validated - so the assertion
	// is only that what comes back is a non-empty name in the report's own
	// casing, which is what the mapping has to be written against.
	for i, r := range rows {
		if r.Status.Name == "" {
			t.Errorf("transaction %d carries an empty status name", i)
		}
	}
}

// TestBothEnvelopeShapesWereRecorded. The API returns a bare JSON array by
// default and an object when rest_json_force_object=on. The adapter picks
// one; both are recorded so that switching is a decision made against
// evidence rather than a guess.
//
// It matters for more than parsing: RawPayload requires valid JSON opening
// with { or [, and a bare array satisfies that - so the evidence row can hold
// the response verbatim either way, which is what contract rule 1 is for.
func TestBothEnvelopeShapesWereRecorded(t *testing.T) {
	t.Parallel()

	var array []any
	if err := json.Unmarshal(readRecording(t, "transactions.json"), &array); err != nil {
		t.Errorf("the default recording is not a bare array: %v", err)
	}

	var object struct {
		Response []any `json:"response"`
	}
	if err := json.Unmarshal(readRecording(t, "transactions-object-envelope.json"), &object); err != nil {
		t.Errorf("the forced-object recording is not an object carrying response: %v", err)
	}
	if len(array) != len(object.Response) {
		t.Errorf("the two envelopes disagree on how many transactions there are: %d against %d",
			len(array), len(object.Response))
	}
}

// TestAnEmptyWindowIsAnEmptyArray, not null and not an error. The poller
// drains a window until it comes back empty, so "no rows" has to be
// distinguishable from "something went wrong" - and here it is simply [].
func TestAnEmptyWindowIsAnEmptyArray(t *testing.T) {
	t.Parallel()

	var rows []any
	if err := json.Unmarshal(readRecording(t, "transactions-empty.json"), &rows); err != nil {
		t.Fatalf("an empty window did not parse as an array: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("the empty recording carries %d rows", len(rows))
	}
}

// TestAnErrorIsAStructuredJSONObject, provided format=json was asked for.
//
// This was recorded expecting the opposite. Fetched WITHOUT format=json, a
// malformed request answers 400 with an HTML page - the usage text - and an
// adapter that assumed a JSON body would report a parse error while hiding
// what the network actually said.
//
// With format=json the same request answers 400 with a structured object
// carrying code, name and description. So the transport must send format=json
// on EVERY request, including ones it expects to fail: it is not only how the
// success body is chosen, it is how an error stays machine-readable.
func TestAnErrorIsAStructuredJSONObject(t *testing.T) {
	t.Parallel()

	var body struct {
		Error struct {
			Code        int    `json:"code"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.Unmarshal(readRecording(t, "error-400.json"), &body); err != nil {
		t.Fatalf("the recorded 400 is not a JSON error object: %v", err)
	}
	if body.Error.Code != 400 {
		t.Errorf("the error carries code %d, want 400", body.Error.Code)
	}
	if body.Error.Name == "" {
		t.Error("the error carries no name")
	}
	// The description is the usage text, and it is the only documentation
	// this API has. An adapter that logged the code and dropped this would
	// throw away the one thing that says what was wrong with the request.
	if len(body.Error.Description) < 100 {
		t.Errorf("the error description is %d characters; it should carry the usage text that says what the request was missing",
			len(body.Error.Description))
	}
}
