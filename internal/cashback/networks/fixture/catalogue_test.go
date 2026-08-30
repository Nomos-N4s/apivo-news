// The tests for catalogue.go: the recorded catalogue read whole, and the
// refusals that keep a truncated answer from being read as a mass departure.

package fixture

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// fixtureTestCatalogue reads the whole catalogue, failing the test on an
// immediate error - which this method has none of, and that is itself part of
// the contract.
func fixtureTestCatalogue(t *testing.T, adapter *Network) ([]networks.ReportedMerchant, error) {
	t.Helper()
	seq, err := adapter.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	return fixtureTestCollect(seq)
}

// TestFetchCataloguePlaysTheRecordedCatalogue holds the three route states an
// import has to handle and the one column it must be able to leave null: a
// retailer bound to no single country is ordinary, and the port spells it as
// the empty string rather than as a separate presence flag.
func TestFetchCataloguePlaysTheRecordedCatalogue(t *testing.T) {
	t.Parallel()

	want := []networks.ReportedMerchant{
		{ExternalID: "FIXM-77", Name: "Fixture Outdoor Co", Country: "DE", StatusRaw: "live", Status: networks.MerchantStatusActive},
		{ExternalID: "FIXM-41", Name: "Fixture Books Ltd", Country: "", StatusRaw: "suspended", Status: networks.MerchantStatusPaused},
		{ExternalID: "FIXM-12", Name: "Fixture Bygone Ltd", Country: "GB", StatusRaw: "gone", Status: networks.MerchantStatusLeftNetwork},
	}

	got, err := fixtureTestCatalogue(t, fixtureTestAdapter(t))
	if err != nil {
		t.Fatalf("reading the catalogue: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("the catalogue holds %d retailers, want %d", len(got), len(want))
	}
	for i, entry := range got {
		expect := want[i]
		switch {
		case entry.ExternalID != expect.ExternalID || entry.Name != expect.Name:
			t.Errorf("entry %d is %q named %q, want %q named %q", i, entry.ExternalID, entry.Name, expect.ExternalID, expect.Name)
		case entry.Country != expect.Country:
			t.Errorf("entry %s carries country %q, want %q", entry.ExternalID, entry.Country, expect.Country)
		case entry.Status != expect.Status || entry.StatusRaw != expect.StatusRaw:
			t.Errorf("entry %s is %s normalised from %q, want %s from %q",
				entry.ExternalID, entry.Status, entry.StatusRaw, expect.Status, expect.StatusRaw)
		}
	}
}

// TestFetchCatalogueYieldsOnlyValuesThatValidate is contract rule 7 for the
// catalogue, and TestFetchCatalogueCarriesTheVerbatimPayload beside it is
// FR-012: a normalisation fix for a catalogue row is re-derived from what the
// network said, not re-fetched from an import that has since changed.
func TestFetchCatalogueYieldsOnlyValuesThatValidate(t *testing.T) {
	t.Parallel()

	got, err := fixtureTestCatalogue(t, fixtureTestAdapter(t))
	if err != nil {
		t.Fatalf("reading the catalogue: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the catalogue yielded nothing, so this rule judged nothing")
	}
	for _, entry := range got {
		if err := entry.Validate(); err != nil {
			t.Errorf("entry %s does not validate: %v", entry.ExternalID, err)
		}
	}
}

func TestFetchCatalogueCarriesTheVerbatimPayload(t *testing.T) {
	t.Parallel()

	got, err := fixtureTestCatalogue(t, fixtureTestAdapter(t))
	if err != nil {
		t.Fatalf("reading the catalogue: %v", err)
	}
	for _, entry := range got {
		if !json.Valid(entry.RawPayload) {
			t.Errorf("entry %s carries a payload that is not JSON: %s", entry.ExternalID, entry.RawPayload)
		}
		if !strings.Contains(string(entry.RawPayload), "primary_url") {
			t.Errorf("entry %s carries a payload holding only the normalised columns, which could re-derive nothing", entry.ExternalID)
		}
	}
}

// TestFetchCatalogueCrossesAPageBoundary is the same awkwardness the
// transaction window has, and it matters more here: an import that stopped at
// the end of the first page would read every retailer on the second as having
// left the network.
func TestFetchCatalogueCrossesAPageBoundary(t *testing.T) {
	t.Parallel()

	adapter := fixtureTestAdapter(t)
	if pages := adapter.recorded.merchantPages(false); len(pages) < 2 {
		t.Fatalf("the catalogue records %d pages, so nothing here crosses a boundary", len(pages))
	}
	got, err := fixtureTestCatalogue(t, adapter)
	if err != nil {
		t.Fatalf("reading the catalogue: %v", err)
	}
	if len(got) != 3 || got[len(got)-1].ExternalID != "FIXM-12" {
		t.Errorf("the catalogue read %d retailers ending at %q, want all three across both pages", len(got), got[len(got)-1].ExternalID)
	}
}

// TestFetchCatalogueYieldsAbandonedWhenItStopsEarly is why contract rule 8
// matters more to this method than to any other. An import reads absence as
// departure: a read that ended quietly at retailer 400 of 5000 would have it
// mark 4600 live routes departed, every offer on them stop being published,
// and members see an emptied catalogue - from an import that reported nothing
// wrong.
func TestFetchCatalogueYieldsAbandonedWhenItStopsEarly(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	seq, err := fixtureTestAdapter(t).FetchCatalogue(ctx)
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	got, err := fixtureTestCollect(seq)
	if !errors.Is(err, networks.ErrIterationAbandoned) || !errors.Is(err, context.Canceled) {
		t.Fatalf("iteration ended with %v, want one wrapping both ErrIterationAbandoned and context.Canceled", err)
	}
	if len(got) != 0 {
		t.Errorf("a cancelled catalogue read yielded %d retailers", len(got))
	}
}

// TestFetchCatalogueYieldsAnInjectedFailure holds contract rule 9 for the one
// method with no immediate error at all: a catalogue read has nothing
// checkable before contacting the network, so every failure - including one
// on the very first page - is yielded.
func TestFetchCatalogueYieldsAnInjectedFailure(t *testing.T) {
	t.Parallel()

	adapter := fixtureTestAdapter(t, WithFailure(FailureRateLimited, FailureAlways))
	seq, err := adapter.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue() refused immediately with %v; rule 9 says a network failure is yielded", err)
	}
	if _, err := fixtureTestCollect(seq); !errors.Is(err, networks.ErrNetworkRateLimited) {
		t.Fatalf("iteration ended with %v, want one wrapping ErrNetworkRateLimited", err)
	}
}

// TestFetchCatalogueReportsAWordNobodyMapped is contract rule 2's catalogue
// half proved against a real adapter: a retailer who has left is exactly what
// a catalogue poll exists to discover, so a word defaulted to active goes on
// publishing an offer on a route that can no longer pay.
func TestFetchCatalogueReportsAWordNobodyMapped(t *testing.T) {
	t.Parallel()

	got, err := fixtureTestCatalogue(t, fixtureTestAdapter(t, WithUnmappableStatus()))
	if !errors.Is(err, networks.ErrUnmappableStatus) {
		t.Fatalf("iteration ended with %v, want one wrapping ErrUnmappableStatus", err)
	}
	if len(got) == 0 {
		t.Error("the unmappable page came first, so nothing proves the entries before it were still delivered")
	}
}

// TestFetchCatalogueDoesNotMoveTheTransactionLifecycle keeps the two apart.
// The lifecycle is a story about one purchase; the catalogue is what the
// account could promote, and re-reading it must not walk a pending
// transaction on to confirmed behind a caller's back.
func TestFetchCatalogueDoesNotMoveTheTransactionLifecycle(t *testing.T) {
	t.Parallel()

	adapter := fixtureTestAdapter(t)
	for range 3 {
		if _, err := fixtureTestCatalogue(t, adapter); err != nil {
			t.Fatalf("reading the catalogue: %v", err)
		}
	}
	if got := adapter.Stage(); got != StageClick {
		t.Errorf("three catalogue reads moved the transaction clock to %s, want %s", got, StageClick)
	}
}
