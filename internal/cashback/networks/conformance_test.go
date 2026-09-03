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
	"context"
	"errors"
	"iter"
	"net/url"
	"strings"
	"sync"
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
		if limits.RequestsPerMinute <= 0 {
			t.Errorf("Limits().RequestsPerMinute is %d; a non-positive rate makes every request unpermitted", limits.RequestsPerMinute)
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

// TestConformanceEveryYieldedValueHasPassedItsOwnValidate is contract rule 7,
// and it is the rule the port cannot enforce for itself.
//
// [Reported] and [ReportedMerchant] are deliberately writable-invalid: an
// adapter decodes a payload field by field, so there is no constructor to put
// the check in, and nothing on the YIELDING path can call Validate for the
// adapter - the port declares an interface, and the values crossing it are
// built on the far side of it. (The package does call it again on the way to
// the database, at evidence.go, digest.go and supersede.go: the last gate
// before permanence is worth paying for twice. Neither of those runs while a
// sequence is being drained, which is where rule 7 lives.) Rule 7 makes
// calling it the adapter's obligation - and an obligation nothing asserts is
// a comment.
//
// What it buys is WHERE a bad mapping is caught, and the two failure modes it
// catches are not caught equally without it.
//
// A click reference that is present and blank IS refused by the schema
// (network_transaction_click_ref_not_blank), at an INSERT halfway through a
// window - after the rows before it were written, with a cursor that must not
// advance and a window that must be read again. Loud, late, expensive.
//
// A report whose sale and commission carry different currencies is not
// refused at all. cashback.network_transaction has ONE currency column
// (0012), checked only for its shape, and the writer stores the sale's
// (evidence.go: Currency: string(report.SaleAmount.Currency)) - so the
// commission's currency is dropped and the row is stored restated, exactly as
// [Reported.Validate]'s own doc warns it would be. The member's share is then
// computed from a figure the network never reported, and nothing anywhere
// says so.
//
// That second one is why this scenario exists. With rule 7 asserted, the
// adapter that made the value is the thing that fails, at the moment it made
// it, and neither failure reaches a row.
//
// Both sequences are drained, because both types carry the obligation and an
// adapter that honours it for transactions and forgets it for the catalogue
// has kept half a rule.
func TestConformanceEveryYieldedValueHasPassedItsOwnValidate(t *testing.T) {
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
			if err := report.Validate(); err != nil {
				t.Errorf("transaction %s was yielded without passing Reported.Validate: %v", report.ExternalID, err)
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
			if err := merchant.Validate(); err != nil {
				t.Errorf("retailer %s was yielded without passing ReportedMerchant.Validate: %v", merchant.ExternalID, err)
			}
		}
	})
}

// TestConformanceAWindowWiderThanTheLimitIsRefused is contract rule 3
// (FR-031), and the refusal matters more than the limit does.
//
// A window is REFUSED rather than trimmed. A caller that asked for 90 days
// and silently received 31 believes it has read a period it has not, and
// advances its cursor past the 59 days it never saw - so every transaction in
// them is never ingested, every member owed cashback on one is never
// credited, and nothing anywhere reports a problem. Backfill splits the
// period into windows the network allows and persists each before the cursor
// moves; that is only possible because the adapter says no.
//
// The refusal is the IMMEDIATE error, not a yielded one. The two channels
// mean different things to a caller: an immediate error is deterministic and
// about the request, a yielded one came from contacting the network and may
// clear on its own. A too-wide window reported through the sequence would be
// retried forever.
func TestConformanceAWindowWiderThanTheLimitIsRefused(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		adapter := a.open(t)
		window := a.window(t, adapter)

		// One nanosecond past what the adapter documents, anchored to the
		// window it offered so the period is one its network could otherwise
		// answer. Exactly MaxWindow must be accepted, so the boundary is
		// checked from the wrong side of it rather than from a wide guess.
		tooWide := networks.QueryWindow{
			From: window.To.Add(-adapter.Limits().MaxWindow).Add(-time.Nanosecond),
			To:   window.To,
		}
		seq, err := adapter.FetchTransactions(t.Context(), tooWide)
		if !errors.Is(err, networks.ErrWindowTooWide) {
			t.Fatalf("FetchTransactions(%s) returned %v, want an immediate error wrapping ErrWindowTooWide", tooWide, err)
		}
		if seq != nil {
			t.Error("a refused window came back with an iterator; a caller that ranged over it would read a period the adapter said it could not answer")
		}

		// The boundary itself, from the other side: a window exactly as wide
		// as the documented maximum is one the network answers, and an
		// adapter that refused it would halve every backfill.
		atTheLimit := networks.QueryWindow{
			From: window.To.Add(-adapter.Limits().MaxWindow),
			To:   window.To,
		}
		if _, err := adapter.FetchTransactions(t.Context(), atTheLimit); err != nil {
			t.Errorf("FetchTransactions(%s) refused a window exactly as wide as the documented maximum: %v", atTheLimit, err)
		}
	})
}

// TestConformanceAnUnusableWindowIsRefused holds the other immediate refusal.
// A window missing a bound or ending before it starts is not a period, and
// both bounds are required because the window is the unit of resumption: an
// open-ended one cannot be clamped against the limits, re-issued as the same
// question, or recorded as fully persisted.
func TestConformanceAnUnusableWindowIsRefused(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		adapter := a.open(t)
		window := a.window(t, adapter)

		for name, unusable := range map[string]networks.QueryWindow{
			"no bounds at all":       {},
			"no start":               {To: window.To},
			"no end":                 {From: window.From},
			"ending before it began": {From: window.To, To: window.From},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				seq, err := adapter.FetchTransactions(t.Context(), unusable)
				if !errors.Is(err, networks.ErrInvalidQueryWindow) {
					t.Fatalf("FetchTransactions() returned %v, want an immediate error wrapping ErrInvalidQueryWindow", err)
				}
				if seq != nil {
					t.Error("a refused window came back with an iterator")
				}
			})
		}
	})
}

// TestConformanceAWindowIsJudgedBeforeTheNetworkIsTouched pins the order an
// adapter does its work in, which is what keeps the two error channels
// meaning what they say.
//
// A deterministic refusal reported as a network failure - because a network
// failure happened to be waiting - would have a caller retry a window that
// can never be answered, forever, against a network that was never asked.
// The injected failure going unconsumed is the evidence that no request was
// made.
func TestConformanceAWindowIsJudgedBeforeTheNetworkIsTouched(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		if a.openReporting == nil {
			t.Skip("this adapter cannot be made unwell, so the order it checks in is unproved for it")
		}
		adapter := a.openReporting(t, networks.ErrNetworkUnavailable)
		window := a.window(t, adapter)

		tooWide := networks.QueryWindow{
			From: window.To.Add(-adapter.Limits().MaxWindow).Add(-time.Nanosecond),
			To:   window.To,
		}
		_, err := adapter.FetchTransactions(t.Context(), tooWide)
		if !errors.Is(err, networks.ErrWindowTooWide) {
			t.Fatalf("FetchTransactions(%s) returned %v, want one wrapping ErrWindowTooWide", tooWide, err)
		}
		if errors.Is(err, networks.ErrNetworkUnavailable) {
			t.Errorf("the adapter blamed the network for a window it could never answer: %v", err)
		}
	})
}

// conformIssuedRef mints a reference of the shape the click table requires:
// URL-safe and long enough to be unguessable. It is built from a uuid rather
// than written down so that two scenarios running at once never compare
// against each other's.
func conformIssuedRef(t *testing.T) networks.IssuedClickRef {
	t.Helper()
	ref, err := networks.NewIssuedClickRef(strings.ReplaceAll(uuid.NewString(), "-", ""))
	if err != nil {
		t.Fatalf("minting a click reference: %v", err)
	}
	return ref
}

// TestConformanceADeeplinkCarriesTheReferenceBack is contract rule 5
// (FR-021), and the one failure on this list a member feels directly.
//
// A deeplink that lost the reference still redirects. The member reaches the
// retailer, buys, and the commission is earned - and nothing anywhere can say
// whose purchase it was, so nobody is credited and nothing looks wrong. That
// is why the round trip is asserted rather than the URL: what an adapter
// builds is its network's business, and the only thing this port promises is
// that the reference comes back out of it under the name the route said.
func TestConformanceADeeplinkCarriesTheReferenceBack(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		adapter := a.open(t)
		target := a.deeplink(t, adapter)
		ref := conformIssuedRef(t)

		built, err := adapter.BuildDeeplink(t.Context(), target, ref)
		if err != nil {
			t.Fatalf("BuildDeeplink(): %v", err)
		}

		parsed, err := url.Parse(built)
		if err != nil {
			t.Fatalf("BuildDeeplink() returned something that is not a URL: %v", err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			t.Errorf("BuildDeeplink() returned %q, which is not something a Location header can carry", built)
		}
		if parsed.Host == "" {
			t.Errorf("BuildDeeplink() returned %q, which is relative; a redirect needs an absolute URL", built)
		}
		if carried := parsed.Query().Get(target.ClickRefParam); carried != ref.Ref() {
			t.Errorf("the deeplink carries %s=%q, want %q; a redirect that loses the reference earns the commission and credits nobody",
				target.ClickRefParam, carried, ref.Ref())
		}
	})
}

// TestConformanceADeeplinkRefusesInputsItCannotUse is the other half of rule
// 5: a refusal rather than a half-built URL, and a refusal that says which
// kind of problem it is.
//
// Everything here is deterministic - our own routing bug, or a route somebody
// has to fix - so every one wraps both ErrDeeplinkNotFormed and
// ErrDeeplinkInputsRefused. The pair is what decides whether the alert points
// at a network or at us, and whether the offer is one to stop publishing.
func TestConformanceADeeplinkRefusesInputsItCannotUse(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		adapter := a.open(t)
		ref := conformIssuedRef(t)

		relative := a.deeplink(t, adapter)
		relative.Template = "/c/9f3"

		elsewhere := a.deeplink(t, adapter)
		elsewhere.NetworkID = networks.NetworkID("conformance-other-network")

		refused := map[string]struct {
			target networks.DeeplinkTarget
			ref    networks.IssuedClickRef
		}{
			"a template that is not an absolute URL": {target: relative, ref: ref},
			"a route published on another network":   {target: elsewhere, ref: ref},
			"a reference that was never minted":      {target: a.deeplink(t, adapter), ref: networks.IssuedClickRef{}},
		}
		for name, refusal := range refused {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				built, err := adapter.BuildDeeplink(t.Context(), refusal.target, refusal.ref)
				if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
					t.Fatalf("BuildDeeplink() error = %v, want one wrapping ErrDeeplinkNotFormed", err)
				}
				if !errors.Is(err, networks.ErrDeeplinkInputsRefused) {
					t.Errorf("BuildDeeplink() error = %v, want one wrapping ErrDeeplinkInputsRefused too; without it an operator cannot tell a route to fix from a network having a bad day", err)
				}
				if built != "" {
					t.Errorf("BuildDeeplink() refused and still returned %q; a half-built URL still redirects, and the member is never credited", built)
				}
			})
		}
	})
}

// TestConformanceAnAbandonedReadSaysSo is contract rule 8, and the port names
// it the one thing an adapter must not get wrong.
//
// A range loop that ends having yielded no error is a caller's ONLY evidence
// that a window was read to the end, and that evidence is what a durable
// cursor advances on. An adapter that simply returned on ctx.Err() would let
// a poller record a window it read half of - every member owed cashback on
// the other half never credited, and nothing logged.
func TestConformanceAnAbandonedReadSaysSo(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		adapter := a.open(t)
		window := a.window(t, adapter)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		seq, err := adapter.FetchTransactions(ctx, window)
		if err != nil {
			t.Fatalf("FetchTransactions() refused the window rather than the context: %v", err)
		}
		got, err := conformCollect(seq)
		if !errors.Is(err, networks.ErrIterationAbandoned) {
			t.Fatalf("a cancelled read ended with %v, want one wrapping ErrIterationAbandoned", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("a cancelled read ended with %v, want one wrapping context.Canceled too; an operator reading a log has to know what stopped it", err)
		}
		if len(got) != 0 {
			t.Errorf("a read cancelled before it began yielded %d report(s)", len(got))
		}

		// The catalogue says it louder, because an import reads absence as
		// departure: a quiet stop at retailer 400 of 5000 has it mark 4600
		// live routes departed and members see an emptied catalogue.
		catalogue, err := adapter.FetchCatalogue(ctx)
		if err != nil {
			t.Fatalf("FetchCatalogue() returned an immediate error, and a catalogue read has no precondition to refuse on: %v", err)
		}
		merchants, err := conformCollect(catalogue)
		if !errors.Is(err, networks.ErrIterationAbandoned) {
			t.Fatalf("a cancelled catalogue read ended with %v, want one wrapping ErrIterationAbandoned", err)
		}
		if len(merchants) != 0 {
			t.Errorf("a catalogue read cancelled before it began yielded %d retailer(s)", len(merchants))
		}
	})
}

// TestConformanceACatalogueAbandonedMidPageSaysSo is rule 8 at the level a
// read cancelled before it began cannot reach.
//
// A cancellation landing between an adapter's pages is caught before the next
// one is touched. One landing while a page is still being walked is a
// different branch, and a real catalogue page carries hundreds of retailers -
// so an adapter that only looked between pages would hand out the rest of the
// page it was already inside and then, on the last page, report nothing at
// all. An import that read that as a whole answer would mark every retailer
// it never reached departed.
//
// It needs a catalogue with more than one retailer in it to have a "during"
// at all, and says so rather than passing vacuously when it does not.
func TestConformanceACatalogueAbandonedMidPageSaysSo(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		adapter := a.open(t)

		whole, err := conformCatalogue(t, adapter)
		if err != nil {
			t.Fatalf("reading the catalogue: %v", err)
		}
		if len(whole) < 2 {
			t.Skipf("this adapter's catalogue holds %d retailer(s), so there is no point during a read to cancel at", len(whole))
		}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		seq, err := adapter.FetchCatalogue(ctx)
		if err != nil {
			t.Fatalf("FetchCatalogue(): %v", err)
		}
		seen, abandoned := 0, error(nil)
		for _, err := range seq {
			if err != nil {
				abandoned = err
				break
			}
			seen++
			// Cancel after the first retailer and keep reading: the caller is
			// still willing, and it is the adapter that has to notice.
			cancel()
		}
		if seen == 0 {
			t.Fatal("nothing was yielded before the cancellation, so this proves nothing about stopping during a read")
		}
		if seen == len(whole) {
			t.Errorf("the whole catalogue arrived after the cancellation; the adapter finished the read it had been told to stop")
		}
		if !errors.Is(abandoned, networks.ErrIterationAbandoned) {
			t.Fatalf("the cancelled catalogue read ended with %v, want one wrapping ErrIterationAbandoned", abandoned)
		}
	})
}

// TestConformanceAnAbandonedWindowRunsAgainFromTheBeginning is contract rule
// 4, and the reason rule 8 is worth having.
//
// The port offers no resumption point inside a window: a cursor that could
// sit mid-window would advance over transactions that were yielded but not
// yet stored. So the only correct response to a read that stopped halfway is
// to run the whole window again, and that is only safe if running it again
// misses nothing. An adapter that treated a started read as consumed would
// lose exactly the reports the interrupted caller had not yet written.
//
// What is asserted is that the transaction the abandoned read saw is there
// again. Not that the answer is identical - re-issuing a window asks the same
// question and does not promise the same answer, because that is the entire
// mechanism by which a pending transaction is ever seen to become confirmed.
func TestConformanceAnAbandonedWindowRunsAgainFromTheBeginning(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		adapter := a.open(t)
		window := a.window(t, adapter)

		seq, err := adapter.FetchTransactions(t.Context(), window)
		if err != nil {
			t.Fatalf("FetchTransactions(): %v", err)
		}

		// Stopped the way a poller whose write failed stops: the caller
		// leaves the range at the first report it could not store.
		//
		// Breaking rather than cancelling, deliberately. A cancellation
		// landing after the last report of a one-report window is not an
		// abandonment at all - the window WAS read to the end - so a scenario
		// built on it would assert the contract only against adapters whose
		// windows happen to hold more than one transaction. A caller that
		// breaks has stopped halfway by construction, whatever the window
		// holds, and rule 4's promise is to exactly that caller.
		first := ""
		for report, err := range seq {
			if err != nil {
				t.Fatalf("an uncancelled read yielded %v before the caller had stopped anything", err)
			}
			first = report.ExternalID
			break
		}
		if first == "" {
			t.Fatal("the window this adapter offered yielded nothing, so there is no interrupted read to resume from")
		}

		// The same adapter, the same window, from the beginning. The same
		// adapter matters: a fresh one would prove only that the network
		// still holds the transaction, not that this adapter did not treat a
		// started read as a consumed one.
		again, err := conformTransactions(t, a, adapter)
		if err != nil {
			t.Fatalf("re-running the abandoned window failed: %v", err)
		}
		found := false
		for _, report := range again {
			if report.ExternalID == first {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("transaction %s was yielded by the read that was abandoned and is missing from the re-run; a caller that had not yet stored it has now lost it, with nothing to say so", first)
		}
	})
}

// TestConformanceACallerMayStopWithoutItBeingAFailure is the other way an
// iteration ends early, and the one an adapter must NOT report.
//
// A caller that has seen enough - a spot check, a search, an import unwinding
// from its own error - stops the range itself. Nothing is wrong. An adapter
// that yielded an abandonment here would have every such caller log a failure
// it caused on purpose, and an adapter that kept pushing values into a loop
// that has gone is a panic rather than a wrong answer.
func TestConformanceACallerMayStopWithoutItBeingAFailure(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		adapter := a.open(t)

		seq, err := adapter.FetchTransactions(t.Context(), a.window(t, adapter))
		if err != nil {
			t.Fatalf("FetchTransactions(): %v", err)
		}
		seen := 0
		for _, err := range seq {
			if err != nil {
				t.Fatalf("an uncancelled read yielded %v before the caller had stopped anything", err)
			}
			seen++
			break
		}
		if seen != 1 {
			t.Fatalf("the range ran %d time(s) after one break, want 1", seen)
		}

		catalogue, err := adapter.FetchCatalogue(t.Context())
		if err != nil {
			t.Fatalf("FetchCatalogue(): %v", err)
		}
		seen = 0
		for _, err := range catalogue {
			if err != nil {
				t.Fatalf("an uncancelled catalogue read yielded %v before the caller had stopped anything", err)
			}
			seen++
			break
		}
		if seen != 1 {
			t.Fatalf("the catalogue range ran %d time(s) after one break, want 1", seen)
		}
	})
}

// TestConformanceAFailureSaysWhichKindItIs is contract rule 9, and the
// classification decides what a poller does next rather than merely what it
// logs.
//
// Two of the three mean run the window again; the third means stop, leave the
// cursor where it is, and raise the account to an operator - because
// re-running it loops forever against a credential nobody has fixed. An
// adapter that collapsed them into one error would have a revoked credential
// read as a network having a bad day, and ingestion would halt silently.
//
// The failure arrives through the SEQUENCE, not as the immediate error. The
// immediate one covers only what is checkable without contacting the network,
// so an eager adapter and a lazy one report an expired credential through the
// same channel - which is what stops a caller needing to know which kind it
// was given.
func TestConformanceAFailureSaysWhichKindItIs(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		if a.openReporting == nil {
			t.Skip("this adapter cannot be made unwell, so rule 9 is unproved for it")
		}

		// Every sentinel, and every other sentinel it must not be mistaken
		// for. Reporting one of these as another is the whole failure this
		// rule exists to prevent, so the scenario checks the confusions as
		// well as the answer.
		others := []error{networks.ErrNetworkUnavailable, networks.ErrNetworkRefused, networks.ErrNetworkRateLimited}
		for _, want := range others {
			t.Run(want.Error(), func(t *testing.T) {
				t.Parallel()
				adapter := a.openReporting(t, want)

				seq, err := adapter.FetchTransactions(t.Context(), a.window(t, adapter))
				if err != nil {
					t.Fatalf("a network failure arrived as the immediate error, which is reserved for what is checkable without contacting the network: %v", err)
				}
				_, err = conformCollect(seq)
				if !errors.Is(err, want) {
					t.Fatalf("the read ended with %v, want one wrapping %v", err, want)
				}
				for _, other := range others {
					if errors.Is(other, want) {
						continue
					}
					if errors.Is(err, other) {
						t.Errorf("the failure reads as both %v and %v; a caller cannot tell whether to re-run the window or stop and raise the account", want, other)
					}
				}
			})
		}
	})
}

// conformPacingClock is a clock the rate limiter can be driven by without
// anything sleeping. It advances only when the limiter asks it to, which is
// what makes the assertion below about the limiter's arithmetic rather than
// about how busy the machine running the test happens to be.
type conformPacingClock struct {
	mu     sync.Mutex
	now    time.Time
	waited time.Duration
}

func (c *conformPacingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *conformPacingClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.waited += d
	return nil
}

func (c *conformPacingClock) total() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waited
}

// TestConformanceTheDeclaredRateCanHoldTheAdapter is the pacing half of
// contract rule 3.
//
// An adapter declares RequestsPerMinute and the poller holds it to that with
// a limiter; a rate that could not build one would leave the adapter unpaced,
// which the network answers with the rate-limited refusals above until
// somebody notices. So the declaration is checked by using it, and the
// adapter is then driven through the limiter for real - because an adapter
// that held a lock across a read would deadlock under pacing and pass every
// scenario above.
//
// The limiter runs on an injected clock, so what is asserted is its
// arithmetic rather than how busy this machine is. Nothing here re-proves the
// limiter itself; what is being proved is that the pair works, which is the
// arrangement the poller will actually run.
func TestConformanceTheDeclaredRateCanHoldTheAdapter(t *testing.T) {
	t.Parallel()

	eachAdapter(t, func(t *testing.T, a conformAdapter) {
		adapter := a.open(t)
		// The declaration is per minute, in the unit the network table
		// carries; the limiter takes a rate a second. The conversion is
		// written out here rather than folded into the expression below
		// because getting it backwards is the unit bug migration 0026
		// existed to fix, and a test that reproduced it would pass.
		const secondsPerMinute = 60.0
		rate := float64(adapter.Limits().RequestsPerMinute) / secondsPerMinute

		clock := &conformPacingClock{now: time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)}
		limiter, err := networks.NewRateLimiter(rate, 1, networks.WithRateLimiterClock(clock))
		if err != nil {
			t.Fatalf("the adapter declares %v requests a second, which builds no limiter: %v", rate, err)
		}

		const reads = 4
		window := a.window(t, adapter)
		read := limiter.Pace(func(ctx context.Context) error {
			seq, err := adapter.FetchTransactions(ctx, window)
			if err != nil {
				return err
			}
			_, err = conformCollect(seq)
			return err
		})
		for i := range reads {
			if err := read(t.Context()); err != nil {
				t.Fatalf("read %d through the limiter failed: %v", i+1, err)
			}
		}

		// One token is granted immediately; the rest are paced. Anything less
		// and the limiter is not holding the adapter to anything.
		//
		// The expectation is computed from the declaration itself, in
		// minutes, and not from the converted rate above. Deriving both from
		// one number is what would let the conversion be inverted and the
		// assertion still hold: a limiter built at sixty times the declared
		// rate would be compared against an expectation sixty times too
		// small, and pass while pacing nothing.
		want := time.Duration(float64(reads-1) * float64(time.Minute) / float64(adapter.Limits().RequestsPerMinute))
		if got := clock.total(); got < want {
			t.Errorf("%d reads at %v a second waited %v in total, want at least %v", reads, rate, got, want)
		}
	})
}
