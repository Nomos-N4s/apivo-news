// The adapter conformance suite (T051).
//
// [networks.Network] is a contract, not an interface shape. Its doc comments
// and contracts/ports.md state ten rules every adapter must keep, and the
// point of a port is that a caller need not know which adapter it was handed.
// A suite written per adapter cannot show that: each one is written by
// somebody reading the same prose and reaching their own reading of it, and
// the readings only have to differ by a little for the poller built on top to
// be right over one network and wrong over another - where "wrong" means a
// member's money.
//
// So this file asserts the contract once and runs it against every adapter.
// A scenario here is written against the PORT and never against an
// implementation: it may not name a network's vocabulary, know its pagination
// scheme, or assume which transactions its recording holds. Everything an
// adapter alone can know reaches a scenario through the table below, so that
// adding the first real network means adding one entry rather than editing a
// single assertion.
//
// This is an external test package, which is what lets it import an adapter:
// adapters import [networks], and nothing imports networks_test.
//
// # What a scenario may assume, and what it may not
//
// It may assume the port: the sentinels, the iterator shape, the two error
// channels, the values' own Validate. It may not assume a period has
// transactions in it, that a status word exists, or that a network can be
// made unwell - those are the four capabilities on [conformAdapter], and an
// adapter that cannot offer one skips the scenarios that need it rather than
// having them softened for everybody.
//
// A skip is reported per adapter and per scenario, never for the file, so a
// run that exercised one capability cannot read as a run that exercised them
// all.
//
// # Why the fixture is enough to be worth running
//
// It is an adapter rather than a mock: it keeps all ten rules, its bytes come
// from recorded responses, and it can be made to page, to fail, to be
// cancelled and to meet a word nobody mapped (T050). A suite that passes
// against it is therefore evidence about the port. What it cannot be is
// evidence that a REAL network's adapter keeps the contract - only running
// this suite against that adapter is - which is exactly why the table is a
// table.
package networks_test

import (
	"errors"
	"iter"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/fixture"
)

// conformAdapter is one adapter under the shared suite: a name for the
// subtest, a way to get a fresh one, and the handful of things only an
// adapter can know.
//
// Every capability beyond open is optional and nil means "cannot". That is
// deliberate: a real network's adapter may have no way to be made unwell in a
// test, and the alternative to skipping - weakening the scenario until every
// adapter passes it - would leave the suite asserting the intersection of
// what everyone happened to implement rather than the contract.
type conformAdapter struct {
	name string
	// open answers an adapter no other scenario is using, built the way its
	// composition root would build it.
	open func(t *testing.T) networks.Network
	// window answers a period this adapter's limits accept AND its network
	// has transactions in. The suite cannot derive the second half: a window
	// inside the limits is easy, a window with data in it is knowledge only
	// the adapter has.
	window func(t *testing.T, n networks.Network) networks.QueryWindow
	// deeplink answers a redirect target this adapter can build from, in the
	// shape an offer row would hold.
	deeplink func(t *testing.T, n networks.Network) networks.DeeplinkTarget
	// openUnmappable answers an adapter whose next read meets a status word
	// nobody mapped, or nil when this adapter cannot be made to meet one.
	openUnmappable func(t *testing.T) networks.Network
	// openReporting answers an adapter whose next network-touching calls
	// report the given failure - one of the port's own classification
	// sentinels - or nil when this adapter cannot be made unwell.
	openReporting func(t *testing.T, report error) networks.Network
}

// conformAdapters is the table. One entry per adapter, and the only place in
// this file that may name one.
func conformAdapters() []conformAdapter {
	return []conformAdapter{conformFixture()}
}

// conformFixture is the fixture adapter's entry. Every type assertion in this
// file lives here, which is what keeps the scenarios themselves free of them.
func conformFixture() conformAdapter {
	open := func(t *testing.T) *fixture.Network {
		t.Helper()
		account, err := networks.NewPublisherAccount(uuid.New(), fixture.ID, "conformance-publisher")
		if err != nil {
			t.Fatalf("building the fixture's publisher account: %v", err)
		}
		adapter, err := fixture.New(account)
		if err != nil {
			t.Fatalf("fixture.New(): %v", err)
		}
		return adapter
	}
	return conformAdapter{
		name: string(fixture.ID),
		open: func(t *testing.T) networks.Network { return open(t) },
		window: func(t *testing.T, _ networks.Network) networks.QueryWindow {
			t.Helper()
			return networks.QueryWindow{
				From: time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
				To:   time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC),
			}
		},
		deeplink: func(t *testing.T, n networks.Network) networks.DeeplinkTarget {
			t.Helper()
			return n.(*fixture.Network).DeeplinkTarget(uuid.New())
		},
		openUnmappable: func(t *testing.T) networks.Network {
			t.Helper()
			account, err := networks.NewPublisherAccount(uuid.New(), fixture.ID, "conformance-publisher")
			if err != nil {
				t.Fatalf("building the fixture's publisher account: %v", err)
			}
			adapter, err := fixture.New(account, fixture.WithUnmappableStatus())
			if err != nil {
				t.Fatalf("fixture.New(WithUnmappableStatus()): %v", err)
			}
			return adapter
		},
		openReporting: func(t *testing.T, report error) networks.Network {
			t.Helper()
			adapter := open(t)
			adapter.SetFailure(conformFixtureFailure(t, report), fixture.FailureAlways)
			return adapter
		},
	}
}

// conformFixtureFailure translates one of the port's classification sentinels
// into the fixture's own knob. The mapping is here rather than in a scenario
// because a scenario asks for "a network that is rate limited" and must not
// know how any particular adapter is told to be one.
func conformFixtureFailure(t *testing.T, report error) fixture.Failure {
	t.Helper()
	// errors.Is rather than a switch: a scenario is free to ask for a wrapped
	// sentinel, and a bare comparison would send it to the default branch and
	// fail the test for the wrong reason.
	switch {
	case errors.Is(report, networks.ErrNetworkUnavailable):
		return fixture.FailureUnavailable
	case errors.Is(report, networks.ErrNetworkRateLimited):
		return fixture.FailureRateLimited
	case errors.Is(report, networks.ErrNetworkRefused):
		return fixture.FailureRefused
	default:
		t.Fatalf("the suite asked the fixture to report %v, which is not a classification sentinel", report)
		return fixture.FailureNone
	}
}

// eachAdapter runs one scenario against every adapter in the table, as a
// subtest named for the adapter, so a failure says which one broke the
// contract before it says how.
func eachAdapter(t *testing.T, scenario func(t *testing.T, a conformAdapter)) {
	t.Helper()
	for _, a := range conformAdapters() {
		t.Run(a.name, func(t *testing.T) {
			t.Parallel()
			scenario(t, a)
		})
	}
}

// TestConformanceAnAdapterIsWhatItSaysItIs holds the declarations everything
// else in the port is written on top of (contract rules 3 and 10).
//
// An id or an account that changed between calls would not be caught by any
// other scenario here, and would strand evidence rather than fail: the id is
// how a stored row is traced back to the code that wrote it, and the account
// owns both durable cursors, so an adapter that answered differently on
// Tuesday would resume one account's window from another's watermark with
// nothing logged. They are cheap to check and impossible to notice.
func TestConformanceAnAdapterIsWhatItSaysItIs(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		adapter := a.open(t)

		// What the composition root refuses at wiring, restated: an adapter
		// that reaches a scenario at all has already been through this, and
		// a suite that did not check it would be proving the contract about
		// a value the port would never have accepted.
		if err := networks.ValidateNetwork(adapter); err != nil {
			t.Fatalf("ValidateNetwork(): %v", err)
		}

		if id := adapter.ID(); id != adapter.ID() {
			t.Errorf("ID() answered %s and then something else", id)
		}
		if account := adapter.Account(); account.ID() != adapter.Account().ID() {
			t.Errorf("Account() answered %s and then something else", account)
		}
		if got, want := adapter.Account().Network(), adapter.ID(); got != want {
			t.Errorf("the adapter polls an account held at %s, and speaks to %s; an adapter that files one network's transactions under another's id is the wiring mistake this pairing exists to prevent", got, want)
		}

		limits := adapter.Limits()
		if err := limits.Validate(); err != nil {
			t.Fatalf("Limits().Validate(): %v", err)
		}
		if limits != adapter.Limits() {
			t.Errorf("Limits() answered %+v and then something else; they are read once at wiring, so a network that revises its documented limit is a restart rather than a release", limits)
		}
		if limits.MaxWindow <= 0 {
			t.Errorf("Limits().MaxWindow is %s; a non-positive maximum makes every window too wide and stops ingestion dead", limits.MaxWindow)
		}
		if limits.RequestsPerSecond <= 0 {
			t.Errorf("Limits().RequestsPerSecond is %d; a non-positive rate makes every request unpermitted", limits.RequestsPerSecond)
		}
	})
}

// conformCollect drains a sequence, returning what it yielded before any
// error alongside that error. What arrived BEFORE a failure is half of what
// these scenarios are about: an adapter that hands back reports and then
// fails mid-window is the situation a cursor gets wrong.
func conformCollect[V any](seq iter.Seq2[V, error]) ([]V, error) {
	var values []V
	for value, err := range seq {
		if err != nil {
			return values, err
		}
		values = append(values, value)
	}
	return values, nil
}

// conformTransactions reads one window whole, failing the test on the
// immediate error - which is about the window rather than about the network,
// so a scenario that did not mean to test it wants it to stop the test rather
// than be mistaken for an answer.
func conformTransactions(t *testing.T, a conformAdapter, n networks.Network) ([]networks.Reported, error) {
	t.Helper()
	seq, err := n.FetchTransactions(t.Context(), a.window(t, n))
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	return conformCollect(seq)
}

// conformCatalogue reads the catalogue whole. It has no immediate error to
// check for - a catalogue read has nothing checkable before contacting the
// network - and that absence is itself part of the contract.
func conformCatalogue(t *testing.T, n networks.Network) ([]networks.ReportedMerchant, error) {
	t.Helper()
	seq, err := n.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	return conformCollect(seq)
}

// TestConformanceEveryValueCarriesItsRawPayload is contract rule 1 (FR-032),
// and it is the rule that makes every other one recoverable.
//
// The verbatim fragment stored beside the normalised columns is what a
// normalisation bug is later re-derived from. Networks do not reliably serve
// their history twice, so a payload not captured at retrieval is a payload
// gone for good - and with it the ability to fix a member's credit without
// the network's cooperation. An adapter that normalised perfectly and stored
// nothing verbatim would look correct for exactly as long as it was correct.
func TestConformanceEveryValueCarriesItsRawPayload(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		adapter := a.open(t)

		reports, err := conformTransactions(t, a, adapter)
		if err != nil {
			t.Fatalf("reading a window this adapter says it has data for: %v", err)
		}
		if len(reports) == 0 {
			t.Fatal("the window this adapter offered holds no transactions, so nothing here is proved; the table's window must name a period with data in it")
		}
		for _, report := range reports {
			if len(report.RawPayload) == 0 {
				t.Errorf("transaction %s arrived with no payload", report.ExternalID)
			}
		}

		merchants, err := conformCatalogue(t, adapter)
		if err != nil {
			t.Fatalf("reading the catalogue: %v", err)
		}
		if len(merchants) == 0 {
			t.Fatal("the catalogue is empty, so nothing here is proved")
		}
		for _, merchant := range merchants {
			if len(merchant.RawPayload) == 0 {
				t.Errorf("retailer %s arrived with no payload", merchant.ExternalID)
			}
		}
	})
}

// TestConformanceAPayloadBelongsToItsCaller is the half of rule 1 that a
// presence check cannot reach.
//
// A payload is evidence, and evidence a later caller can edit is not
// evidence. An adapter that decoded its responses once and handed every
// caller the same backing array would pass every assertion above while
// letting one poll's write corrupt another's - and the corruption would
// surface as a stored payload that does not match the row beside it, months
// later, with nothing to say when it changed.
//
// Two adapters rather than two reads. Re-issuing a window asks the same
// question and does NOT promise the same answer (rule 4), so comparing one
// adapter's second read against its first would be asserting the opposite of
// the contract; two adapters opened side by side are the aliasing hazard
// itself, since a package-level decoded response is shared by every adapter
// in the process.
//
// The bytes are edited in place rather than reassigned, because reassigning
// tests only Go's value semantics and every adapter passes that.
func TestConformanceAPayloadBelongsToItsCaller(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		mine, theirs := a.open(t), a.open(t)

		fromMine, err := conformTransactions(t, a, mine)
		if err != nil {
			t.Fatalf("the first adapter's read failed: %v", err)
		}
		fromTheirs, err := conformTransactions(t, a, theirs)
		if err != nil {
			t.Fatalf("the second adapter's read failed: %v", err)
		}
		if len(fromMine) == 0 || len(fromTheirs) == 0 {
			t.Fatal("the window this adapter offered holds no transactions, so nothing here is proved")
		}
		if fromMine[0].ExternalID != fromTheirs[0].ExternalID {
			t.Skipf("two freshly opened adapters answered %s and %s for the same window, so there is no shared payload to corrupt",
				fromMine[0].ExternalID, fromTheirs[0].ExternalID)
		}

		// Captured as a string, which copies, so the comparison below is
		// against what the other caller was handed rather than against a view
		// of the same bytes.
		before := string(fromTheirs[0].RawPayload)
		if string(fromMine[0].RawPayload) != before {
			t.Skipf("two freshly opened adapters carried different payloads for %s, so there is no shared payload to corrupt", fromMine[0].ExternalID)
		}

		for i := range fromMine[0].RawPayload {
			fromMine[0].RawPayload[i] = 'X'
		}

		if got := string(fromTheirs[0].RawPayload); got != before {
			t.Errorf("editing one caller's payload changed what another caller was already holding:\n got %s\nwant %s", got, before)
		}
	})
}

// TestConformanceStatusMappingIsTotal is contract rule 2 (FR-033), and the
// one rule with no default branch anywhere.
//
// An unrecognised status word surfaces to an operator rather than being
// guessed at, because the cheapest wrong answer - call it pending - silently
// withholds a member's money, and the second cheapest - call it confirmed -
// pays out money the network never approved. Neither leaves a trace. The
// refusal does.
//
// What is asserted is the sentinel and the silence after it: a word nobody
// mapped stops the window, and nothing carrying a guess is yielded in its
// place. Reports that arrived BEFORE the unmappable one are ordinary and
// stay - the network really did report them, and an adapter that threw away
// a window's good reports because its last page held a surprise would make
// every mapping gap cost more than it should.
func TestConformanceStatusMappingIsTotal(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		if a.openUnmappable == nil {
			t.Skip("this adapter cannot be made to meet a status word nobody mapped, so rule 2 is unproved for it")
		}
		adapter := a.openUnmappable(t)

		got, err := conformTransactions(t, a, adapter)
		if !errors.Is(err, networks.ErrUnmappableStatus) {
			t.Fatalf("a word nobody mapped ended the read with %v, want one wrapping ErrUnmappableStatus", err)
		}
		for _, report := range got {
			if !report.Status.Valid() {
				t.Errorf("transaction %s arrived carrying %s, which is not a status this port has; the unmappable word was guessed at rather than refused",
					report.ExternalID, report.Status)
			}
		}
	})
}

// TestConformanceEveryReportKeepsTheNetworksOwnWord is the quieter half of
// rule 2, and the one an operator needs on the day a mapping turns out to be
// wrong.
//
// A mapped status is this repository's word; StatusRaw is the network's. Both
// are stored, and the raw one is what a wrong mapping is re-derived from
// without re-reading the network. An adapter that mapped and discarded would
// leave every mis-mapped transaction indistinguishable from a correctly
// mapped one.
func TestConformanceEveryReportKeepsTheNetworksOwnWord(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		adapter := a.open(t)

		reports, err := conformTransactions(t, a, adapter)
		if err != nil {
			t.Fatalf("reading a window this adapter says it has data for: %v", err)
		}
		if len(reports) == 0 {
			t.Fatal("the window this adapter offered holds no transactions, so nothing here is proved")
		}
		for _, report := range reports {
			if !report.Status.Valid() {
				t.Errorf("transaction %s carries %s, which is not a status this port has", report.ExternalID, report.Status)
			}
			if report.StatusRaw == "" {
				t.Errorf("transaction %s kept no word of its own; a wrong mapping could not be re-derived from it", report.ExternalID)
			}
		}

		merchants, err := conformCatalogue(t, adapter)
		if err != nil {
			t.Fatalf("reading the catalogue: %v", err)
		}
		if len(merchants) == 0 {
			t.Fatal("the catalogue is empty, so nothing here is proved")
		}
		for _, merchant := range merchants {
			if !merchant.Status.Valid() {
				t.Errorf("retailer %s carries %s, which is not a route status this port has", merchant.ExternalID, merchant.Status)
			}
			if merchant.StatusRaw == "" {
				t.Errorf("retailer %s kept no word of its own", merchant.ExternalID)
			}
		}
	})
}
