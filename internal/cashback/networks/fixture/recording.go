// The recording itself: the embedded response files, the shapes this
// network's answers actually have on the wire, and the one place in this
// package that knows the bytes came from a file rather than a socket. One
// file, because that is the whole of what makes this adapter a fixture -
// everything else about it is an adapter like any other.

package fixture

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sync"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// The sentinel a recording this package cannot read is refused with. Its rule
// is that a fixture whose own data is broken must fail at construction rather
// than halfway through somebody's window.
var (
	// ErrRecordingUnreadable reports embedded response files that are not the
	// recording this adapter expects: absent, not JSON, or holding a
	// different number of observations from the lifecycle [Stage] describes.
	//
	// It is raised by [New] rather than at the read that would have needed
	// the missing page, and that timing is the point. The recording is
	// compiled in, so a fault in it is a defect in this repository rather
	// than a network having a bad day; surfacing it at wiring is what stops a
	// conformance run reporting "the adapter yielded nothing at stage
	// reversed" when the truth is that a file was renamed.
	ErrRecordingUnreadable = errors.New("fixture: the recorded network responses could not be read")
)

// recordingFS holds the recorded responses. They are files rather than Go
// literals on purpose: contract rule 1 says the raw payload is what the
// network actually said, so the bytes a [networks.Reported] carries have to
// be bytes somebody recorded and can re-read, not a struct literal that was
// re-encoded on the way out and would quietly track every change to the
// struct. TestRecordedPayloadsAreVerbatimFileBytes reads the files off disk
// and finds each payload in them, which is only a test worth writing because
// the two can differ.
//
//go:embed testdata
var recordingFS embed.FS

const (
	// observationGlob matches the transaction observations in the order they
	// were observed. The order is the filename's, zero-padded so that lexical
	// order and observation order cannot come apart, and there is no manifest
	// beside the files: a second statement of which file comes when is a
	// second thing to get out of step with the first.
	observationGlob = "testdata/transactions/observation-*.json"
	// unmappedTransactionsPath is the page carrying a status word
	// [transactionStatuses] has no entry for. It sits outside the glob so
	// that it is served only when [WithUnmappableStatus] asks for it, and
	// never by accident.
	unmappedTransactionsPath = "testdata/transactions/unmapped.json"
	// cataloguePath is the recorded catalogue: a current state rather than a
	// period, which is why there is one of it and four transaction
	// observations.
	cataloguePath = "testdata/catalogue/catalogue.json"
	// unmappedMerchantsPath is the catalogue's equivalent of
	// unmappedTransactionsPath, because contract rule 2's totality covers a
	// route's status as well as a transaction's.
	unmappedMerchantsPath = "testdata/catalogue/unmapped.json"
)

// transactionPage is one response body the network returned for a
// transaction query: which page of how many, and the transactions on it.
//
// Transactions is a slice of raw messages rather than of decoded structs
// because contract rule 1 requires the VERBATIM fragment, and this is what
// keeps it verbatim: encoding/json fills a [json.RawMessage] with the exact
// bytes of the value as it appeared in the file, whitespace and field order
// and all, so the payload handed to a caller is the recording's own bytes
// rather than a re-encoding of what this package understood. Each element is
// decoded a second time, into [recordedTransaction], for the normalised
// columns - which is the same two-pass shape a real adapter needs, for the
// same reason.
type transactionPage struct {
	Page         int               `json:"page"`
	Pages        int               `json:"pages"`
	Transactions []json.RawMessage `json:"transactions"`
}

// merchantPage is one response body the network returned for a catalogue
// query, and keeps its entries raw for the reason above (FR-012).
type merchantPage struct {
	Page      int               `json:"page"`
	Pages     int               `json:"pages"`
	Merchants []json.RawMessage `json:"merchants"`
}

// recordedTransaction is the fixture network's own vocabulary for one
// transaction: its field names, its status words, its way of stating money.
// Nothing outside this package sees it (ADR-0003).
//
// ClickRef is the port's own [networks.ClickRef] because the network's
// encoding of attribution and the port's are the same two shapes - a string
// or a null - and going through the port's type is what keeps a recorded
// null from arriving as a blank string. Blank and absent are different facts
// the evidence table refuses to collapse, and a bespoke *string here would be
// one more place for them to collapse in.
type recordedTransaction struct {
	ExternalID   string            `json:"transaction_id"`
	ClickRef     networks.ClickRef `json:"clickref"`
	Status       string            `json:"status"`
	Sale         recordedAmount    `json:"sale"`
	Commission   recordedAmount    `json:"commission"`
	TransactedAt time.Time         `json:"transacted_at"`
}

// recordedMerchant is the fixture network's vocabulary for one catalogue
// entry. Country is a pointer because the recording states an unbound
// retailer as a JSON null, which is the same absence the merchant table
// spells as a null column and the port spells as the empty string.
type recordedMerchant struct {
	ExternalID string  `json:"merchant_id"`
	Name       string  `json:"display_name"`
	Country    *string `json:"country"`
	Status     string  `json:"status"`
}

// recordedAmount is how this network states money: integer minor units beside
// an explicit ISO-4217 code, never a decimal string and never a float (C-6).
// See the package comment for why a fixture is the wrong place to invent the
// decimal parser this repository deliberately does not have.
type recordedAmount struct {
	MinorUnits int64  `json:"minor_units"`
	Currency   string `json:"currency"`
}

// recording is every response this adapter can serve, decoded once. It is
// immutable after [loadRecording] and shared by every adapter in the process,
// which is safe only because nothing here is handed out by reference: a
// payload is cloned on its way to a caller (see reportFrom).
type recording struct {
	// observations are the transaction pages of each poll, indexed by
	// [Stage].
	observations [][]transactionPage
	// unmappedTransactions is the page [WithUnmappableStatus] appends.
	unmappedTransactions []transactionPage
	// catalogue is the recorded catalogue's pages.
	catalogue []merchantPage
	// unmappedMerchants is the catalogue page [WithUnmappableStatus]
	// appends.
	unmappedMerchants []merchantPage
}

// loadRecording decodes the embedded files once per process and hands every
// adapter the same immutable result. Decoding is not free and the answer
// cannot change, so a test that builds forty adapters pays for it once.
var loadRecording = sync.OnceValues(readRecording)

// readRecording decodes every embedded file, refusing anything it cannot read
// with an error wrapping [ErrRecordingUnreadable].
//
// It checks the number of observations against [stageCount] rather than
// trusting the directory, because the two are one fact stated twice - the
// lifecycle the constants describe and the files that play it - and a
// recording that gained a fifth observation without a fifth [Stage] would be
// a poll nothing could ever reach.
func readRecording() (*recording, error) {
	paths, err := fs.Glob(recordingFS, observationGlob)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrRecordingUnreadable, observationGlob, err)
	}
	slices.Sort(paths)
	if len(paths) != stageCount {
		return nil, fmt.Errorf("%w: %s matches %d files, and the lifecycle has %d stages",
			ErrRecordingUnreadable, observationGlob, len(paths), stageCount)
	}

	loaded := &recording{observations: make([][]transactionPage, 0, stageCount)}
	for _, path := range paths {
		pages, err := decodeRecorded[transactionPage](path)
		if err != nil {
			return nil, err
		}
		loaded.observations = append(loaded.observations, pages)
	}
	if loaded.unmappedTransactions, err = decodeRecorded[transactionPage](unmappedTransactionsPath); err != nil {
		return nil, err
	}
	if loaded.catalogue, err = decodeRecorded[merchantPage](cataloguePath); err != nil {
		return nil, err
	}
	if loaded.unmappedMerchants, err = decodeRecorded[merchantPage](unmappedMerchantsPath); err != nil {
		return nil, err
	}
	return loaded, nil
}

// decodeRecorded reads one recorded file, whose top level is the sequence of
// response bodies that poll received, in order. There is no envelope around
// them beyond the array: each element is a body the network would have sent,
// so what this package pages over is what a real adapter would page over.
func decodeRecorded[P transactionPage | merchantPage](path string) ([]P, error) {
	raw, err := recordingFS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrRecordingUnreadable, path, err)
	}
	var pages []P
	if err := json.Unmarshal(raw, &pages); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrRecordingUnreadable, path, err)
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("%w: %s records no response at all", ErrRecordingUnreadable, path)
	}
	return pages, nil
}

// transactionPages are the pages a read at stage answers from, with the
// unmapped page appended when the adapter was built with
// [WithUnmappableStatus].
//
// The result is always a fresh slice, never the recording's own. The
// recording is decoded once and shared by every adapter in the process, so a
// page list that shared its backing array would let one adapter's caller
// rewrite what another adapter is about to read - and the append that adds
// the unmapped page would write into the shared array's spare capacity
// whenever there happened to be some, which is a bug that appears and
// disappears with the size of a testdata file.
//
// The unmapped page goes LAST rather than first, so a caller sees the reports
// that mapped cleanly before the one that did not. That ordering is what
// makes the knob prove something: contract rule 2 is about an adapter that
// meets a word nobody mapped PART WAY THROUGH a window it was otherwise
// reading correctly, and a page that failed before anything was yielded would
// not tell a caller whether it can trust what it had already been handed.
func (r *recording) transactionPages(stage Stage, unmappable bool) []transactionPage {
	if !unmappable {
		return slices.Clone(r.observations[stage])
	}
	return slices.Concat(r.observations[stage], r.unmappedTransactions)
}

// merchantPages are the catalogue's pages, with the unmapped page appended on
// the same terms, freshly allocated for the same reason.
func (r *recording) merchantPages(unmappable bool) []merchantPage {
	if !unmappable {
		return slices.Clone(r.catalogue)
	}
	return slices.Concat(r.catalogue, r.unmappedMerchants)
}

// clonePayload copies a recorded fragment on its way to a caller.
//
// The recording is decoded once and shared by every adapter in the process,
// so handing out the backing array itself would let one caller's edit change
// what another caller is later handed as evidence. Evidence that can be
// rewritten after the fact is precisely what the whole ingestion path exists
// to prevent, and a fixture that allowed it here would be teaching the
// opposite lesson to every test written against it.
func clonePayload(raw json.RawMessage) json.RawMessage {
	return bytes.Clone(raw)
}
