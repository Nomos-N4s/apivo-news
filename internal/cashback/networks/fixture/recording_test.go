// The tests for recording.go: that the recording is complete, that what it
// hands out is the bytes on disk rather than a re-encoding of them (contract
// rule 1), and that nothing it hands out is the shared copy.

package fixture

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// fixtureTestRecording is the decoded recording, or a fatal failure. Every
// test here needs it and none of them may edit it.
func fixtureTestRecording(t *testing.T) *recording {
	t.Helper()
	recorded, err := loadRecording()
	if err != nil {
		t.Fatalf("loadRecording(): %v", err)
	}
	return recorded
}

func TestRecordingHoldsEveryPartOfTheScript(t *testing.T) {
	t.Parallel()

	recorded := fixtureTestRecording(t)
	if len(recorded.observations) != stageCount {
		t.Fatalf("the recording holds %d observations, want %d", len(recorded.observations), stageCount)
	}
	for stage := StageClick; stage <= StageReversed; stage++ {
		pages := recorded.observations[stage]
		if len(pages) == 0 {
			t.Errorf("observation %s records no response at all", stage)
		}
	}
	if len(recorded.unmappedTransactions) == 0 || len(recorded.unmappedMerchants) == 0 {
		t.Error("the unmappable pages are empty, so WithUnmappableStatus would prove nothing")
	}
	if len(recorded.catalogue) < 2 {
		t.Errorf("the catalogue records %d pages; a catalogue that never paged would not exercise a page boundary", len(recorded.catalogue))
	}
}

// TestRecordedPayloadsAreVerbatimFileBytes is contract rule 1 held at its
// root. The payload a report carries is what a normalisation fix is later
// re-derived from when the network will no longer serve the window it came
// from, so it has to be the bytes somebody recorded - not a re-encoding of
// what this package understood, which would quietly track every change to the
// struct it decoded into and silently lose any field nobody thought to add.
//
// It reads the files off disk rather than through the embedded filesystem, so
// that the thing being compared against is the artefact a human can open.
func TestRecordedPayloadsAreVerbatimFileBytes(t *testing.T) {
	t.Parallel()

	files := map[string][]json.RawMessage{}
	recorded := fixtureTestRecording(t)
	paths := fixtureTestObservationPaths(t)
	for stage := StageClick; stage <= StageReversed; stage++ {
		path := paths[stage]
		for _, page := range recorded.observations[stage] {
			files[path] = append(files[path], page.Transactions...)
		}
	}
	for _, page := range recorded.catalogue {
		files[cataloguePath] = append(files[cataloguePath], page.Merchants...)
	}

	fragments := 0
	for path, payloads := range files {
		onDisk, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, payload := range payloads {
			fragments++
			if !bytes.Contains(onDisk, payload) {
				t.Errorf("%s does not contain the payload the adapter would yield:\n%s", path, payload)
			}
		}
	}
	if fragments == 0 {
		t.Fatal("no payload was compared, so this rule judged nothing and passed vacuously")
	}
}

// fixtureTestObservationPaths is the recording's observation files in stage order, read
// back through the same glob the adapter loads them with so that a renamed
// file fails here as well as there.
func fixtureTestObservationPaths(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("testdata/transactions")
	if err != nil {
		t.Fatalf("reading testdata/transactions: %v", err)
	}
	var paths []string
	for _, entry := range entries {
		if name := entry.Name(); len(name) > len("observation-") && name[:len("observation-")] == "observation-" {
			paths = append(paths, "testdata/transactions/"+name)
		}
	}
	if len(paths) != stageCount {
		t.Fatalf("found %d observation files, want %d", len(paths), stageCount)
	}
	return paths
}

// TestRecordedTransactionCarriesMoreThanTheNormalisedColumns is what makes
// rule 1 worth anything. A payload that held only the fields [networks.Reported]
// already exposes could re-derive nothing, so the recording carries the
// network's own extra facts and this test refuses a recording that stops
// doing so.
func TestRecordedTransactionCarriesMoreThanTheNormalisedColumns(t *testing.T) {
	t.Parallel()

	recorded := fixtureTestRecording(t)
	payload := recorded.observations[StageClick][0].Transactions[0]
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("decoding the recorded fragment: %v", err)
	}
	for _, want := range []string{"advertiser_name", "site_name"} {
		if _, carried := fields[want]; !carried {
			t.Errorf("the recorded fragment carries no %q; a payload holding only the normalised columns could re-derive nothing", want)
		}
	}
}

// TestRecordedShapesDecodeTheNetworksVocabulary reads one fragment of each
// kind through the structs the adapter decodes with, so that a recording
// renamed on one side and not the other fails here rather than as an empty
// field somewhere downstream.
func TestRecordedShapesDecodeTheNetworksVocabulary(t *testing.T) {
	t.Parallel()

	recorded := fixtureTestRecording(t)

	var transaction recordedTransaction
	if err := json.Unmarshal(recorded.observations[StageClick][0].Transactions[0], &transaction); err != nil {
		t.Fatalf("decoding a recorded transaction: %v", err)
	}
	if transaction.ExternalID == "" || transaction.Status == "" || transaction.TransactedAt.IsZero() {
		t.Fatalf("a recorded transaction decoded to %+v", transaction)
	}
	if _, present := transaction.ClickRef.Ref(); present {
		t.Error("the first observation's transaction decoded as attributed; the recording's whole point is that attribution arrives later")
	}
	if got := (recordedAmount{MinorUnits: 4999, Currency: "EUR"}); transaction.Sale != got {
		t.Errorf("recorded sale = %+v, want %+v", transaction.Sale, got)
	}

	var merchant recordedMerchant
	if err := json.Unmarshal(recorded.catalogue[0].Merchants[1], &merchant); err != nil {
		t.Fatalf("decoding a recorded merchant: %v", err)
	}
	if merchant.Country != nil {
		t.Errorf("the second recorded merchant carries country %q; the recording needs one bound to no country at all", *merchant.Country)
	}
}

// TestTransactionPagesAppendsTheUnmappedPageLast holds the ordering
// WithUnmappableStatus depends on. Rule 2 is about an adapter meeting a word
// nobody mapped PART WAY THROUGH a window it was otherwise reading correctly;
// a page that failed before anything was yielded would not tell a caller
// whether it can trust what it had already been handed.
func TestTransactionPagesAppendsTheUnmappedPageLast(t *testing.T) {
	t.Parallel()

	recorded := fixtureTestRecording(t)
	plain := recorded.transactionPages(StageApproved, false)
	extended := recorded.transactionPages(StageApproved, true)

	if len(extended) != len(plain)+len(recorded.unmappedTransactions) {
		t.Fatalf("with the knob on there are %d pages, want %d", len(extended), len(plain)+len(recorded.unmappedTransactions))
	}
	for i := range plain {
		if extended[i].Page != plain[i].Page {
			t.Errorf("page %d changed when the knob was turned on", i)
		}
	}
}

func TestMerchantPagesAppendsTheUnmappedPageLast(t *testing.T) {
	t.Parallel()

	recorded := fixtureTestRecording(t)
	plain := recorded.merchantPages(false)
	extended := recorded.merchantPages(true)
	if len(extended) != len(plain)+len(recorded.unmappedMerchants) {
		t.Fatalf("with the knob on there are %d catalogue pages, want %d", len(extended), len(plain)+len(recorded.unmappedMerchants))
	}
	last := extended[len(extended)-1]
	if len(last.Merchants) == 0 {
		t.Fatal("the appended catalogue page carries no merchant")
	}
}

// TestPagesDoNotAliasTheSharedRecording refuses a page list that shares the
// recording's backing array. The recording is decoded once and shared by
// every adapter in the process, so an adapter handed the array itself could
// have its caller rewrite what another adapter is about to read - and the
// append that adds the unmapped page would write into the shared array's
// spare capacity whenever there happened to be some, which is a bug that
// appears and disappears with the size of a testdata file.
func TestPagesDoNotAliasTheSharedRecording(t *testing.T) {
	t.Parallel()

	recorded := fixtureTestRecording(t)
	for _, unmappable := range []bool{false, true} {
		pages := recorded.transactionPages(StageApproved, unmappable)
		pages[0] = transactionPage{Page: 999}
		if got := recorded.observations[StageApproved][0].Page; got == 999 {
			t.Errorf("unmappable=%t: editing the pages an adapter was handed edited the shared observation", unmappable)
		}

		catalogue := recorded.merchantPages(unmappable)
		catalogue[0] = merchantPage{Page: 999}
		if got := recorded.catalogue[0].Page; got == 999 {
			t.Errorf("unmappable=%t: editing the catalogue pages an adapter was handed edited the shared recording", unmappable)
		}
	}
}

// TestClonePayloadCopies is the same rule one level down: evidence a later
// caller can rewrite is not evidence.
func TestClonePayloadCopies(t *testing.T) {
	t.Parallel()

	original := json.RawMessage(`{"transaction_id":"FIX-1001"}`)
	clone := clonePayload(original)
	if !bytes.Equal(clone, original) {
		t.Fatalf("clonePayload returned %s, want %s", clone, original)
	}
	clone[2] = 'X'
	if bytes.Equal(clone, original) {
		t.Error("editing the clone edited the original; the payload was handed out by reference")
	}
}

func TestDecodeRecordedRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	if _, err := decodeRecorded[transactionPage]("testdata/transactions/no-such-file.json"); !errors.Is(err, ErrRecordingUnreadable) {
		t.Errorf("decoding a missing file: %v, want one wrapping ErrRecordingUnreadable", err)
	}
	if _, err := decodeRecorded[merchantPage]("testdata"); !errors.Is(err, ErrRecordingUnreadable) {
		t.Errorf("decoding a directory: %v, want one wrapping ErrRecordingUnreadable", err)
	}
}
