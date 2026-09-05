package linkwise_test

// Reading the catalogue (T245, FR-012), against a recording of the real
// programme list.
//
// The nine programmes in testdata/programs.json were chosen out of the three
// hundred and thirty-four this account is joined to: six for the things that
// vary and decide something - a non-EUR currency, deeplinking refused,
// cashback refused, one country against two, a programme with no categories -
// and three more because the recorded TRANSACTIONS name them, so the two
// recordings join for real rather than by construction. Every other field is
// exactly as the network sent it, Greek marketing HTML included, because the
// payload has to survive the jsonb column that will hold it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// collectMerchants drains a catalogue read.
func collectMerchants(t *testing.T, seq func(func(networks.ReportedMerchant, error) bool)) ([]networks.ReportedMerchant, error) {
	t.Helper()
	var got []networks.ReportedMerchant
	var failed error
	for merchant, err := range seq {
		if err != nil {
			failed = err
			break
		}
		got = append(got, merchant)
	}
	return got, failed
}

// theRecordedCatalogue reads every programme back out of the recording.
func theRecordedCatalogue(t *testing.T) ([]networks.ReportedMerchant, error) {
	t.Helper()
	client := servingRecording(t, "programs.json")
	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	return collectMerchants(t, seq)
}

func TestEveryRecordedProgrammeBecomesACatalogueEntry(t *testing.T) {
	t.Parallel()

	got, failed := theRecordedCatalogue(t)
	if failed != nil {
		t.Fatalf("the catalogue read ended with %v", failed)
	}
	if len(got) != 9 {
		t.Fatalf("the recording yielded %d entries, want the 9 programmes it carries", len(got))
	}
	for _, merchant := range got {
		if err := merchant.Validate(); err != nil {
			t.Errorf("a yielded entry does not satisfy the port: %v", err)
		}
	}
}

// TestTheOrdinaryProgrammeIsActive: running advertiser, accepted publisher,
// cashback permitted.
func TestTheOrdinaryProgrammeIsActive(t *testing.T) {
	t.Parallel()

	got, failed := theRecordedCatalogue(t)
	if failed != nil {
		t.Fatalf("the catalogue read ended with %v", failed)
	}
	found := false
	for _, merchant := range got {
		if merchant.ExternalID != "14174" {
			continue
		}
		found = true
		if merchant.Name != "Dazzle" {
			t.Errorf("Name = %q, want the network's own name", merchant.Name)
		}
		if merchant.Status != networks.MerchantStatusActive {
			t.Errorf("Status = %q, want active", merchant.Status)
		}
		// Two countries, so no single one - the port spells that as the zero
		// value rather than picking the first.
		if merchant.Country != "" {
			t.Errorf("Country = %q; the programme names GR,CY, and choosing one would publish a claim the network never made", merchant.Country)
		}
		for _, part := range []string{"program_status=Active", "affiliate_status=Accepted", "cashback_sites=allowed"} {
			if !strings.Contains(merchant.StatusRaw, part) {
				t.Errorf("StatusRaw = %q, want it to carry %q", merchant.StatusRaw, part)
			}
		}
	}
	if !found {
		t.Fatal("programme 14174 is not in the catalogue")
	}
}

// TestAProgrammeThatForbidsCashbackIsPaused is the one judgement this adapter
// makes, and the one worth arguing with.
//
// Fifteen of the three hundred and thirty-four joined programmes set
// terms.promotion_methods.cashback_sites.allow to false. Reporting those as
// active would publish offers on programmes whose own terms forbid what this
// product is: members click through routes the advertiser may refuse to pay
// on, and Apivo breaches terms it agreed to. Paused is the port's word for
// "not a route we may send anybody down now", and an operator can reverse it
// from the row if the terms change.
func TestAProgrammeThatForbidsCashbackIsPaused(t *testing.T) {
	t.Parallel()

	got, failed := theRecordedCatalogue(t)
	if failed != nil {
		t.Fatalf("the catalogue read ended with %v", failed)
	}
	found := false
	for _, merchant := range got {
		if merchant.ExternalID != "13117" {
			continue
		}
		found = true
		if merchant.Status != networks.MerchantStatusPaused {
			t.Errorf("Status = %q, want paused: this programme's own terms refuse cashback sites", merchant.Status)
		}
		// The evidence has to say WHY, because the network called this
		// programme active and the row says paused.
		if !strings.Contains(merchant.StatusRaw, "cashback_sites=not-allowed") {
			t.Errorf("StatusRaw = %q, and nothing in it explains why an active programme was stored as paused", merchant.StatusRaw)
		}
		if !strings.Contains(merchant.StatusRaw, "program_status=Active") {
			t.Errorf("StatusRaw = %q, want it to still carry what the network called the programme", merchant.StatusRaw)
		}
	}
	if !found {
		t.Fatal("programme 13117 is not in the catalogue")
	}
}

// TestASoleCountryIsCarried is the other half of the country rule.
func TestASoleCountryIsCarried(t *testing.T) {
	t.Parallel()

	got, failed := theRecordedCatalogue(t)
	if failed != nil {
		t.Fatalf("the catalogue read ended with %v", failed)
	}
	var single, none int
	for _, merchant := range got {
		switch merchant.Country {
		case "":
			none++
		case "GR", "CY":
			single++
		default:
			t.Errorf("merchant %s carries the country %q, which is not one the recording names", merchant.ExternalID, merchant.Country)
		}
	}
	if single == 0 {
		t.Error("no entry carried a sole country, so the rule is exercised in one direction only")
	}
	if none == 0 {
		t.Error("no entry carried an empty country, so the multi-country rule is exercised in one direction only")
	}
}

// TestTheRawPayloadIsTheProgrammesOwnBytes is contract rule 1 for the
// catalogue, and it matters more here than for a transaction: the merchant's
// localised copy is built from this payload, and the adapter reads six of the
// row's twenty-six fields.
func TestTheRawPayloadIsTheProgrammesOwnBytes(t *testing.T) {
	t.Parallel()

	got, failed := theRecordedCatalogue(t)
	if failed != nil {
		t.Fatalf("the catalogue read ended with %v", failed)
	}
	var withCategories int
	for _, merchant := range got {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(merchant.RawPayload, &payload); err != nil {
			t.Fatalf("merchant %s: the payload is not the row's own object: %v", merchant.ExternalID, err)
		}
		for _, field := range []string{
			"commissions", "currency", "deeplinks", "terms",
			"datafeed_list", "description", "short_description", "logo", "url",
		} {
			if _, ok := payload[field]; !ok {
				t.Errorf("merchant %s: the payload does not carry %q; it is a re-encoding of what the adapter reads rather than what the network sent",
					merchant.ExternalID, field)
			}
		}
		// categories is the one field that is NOT on every row - two of the
		// three hundred and thirty-four omit it entirely, and one of them is
		// in this recording deliberately. Asserted as a presence somewhere
		// rather than everywhere, because a payload that carried it on every
		// row would mean something had filled it in.
		if _, ok := payload["categories"]; ok {
			withCategories++
		}
	}
	if withCategories == 0 || withCategories == len(got) {
		t.Errorf("%d of %d payloads carry categories; the recording was chosen to hold one of each, so neither extreme is what the network sent",
			withCategories, len(got))
	}
}

// TestTheCurrencyIsPerProgrammeAndNotUniform records the finding that the
// transaction side has no answer for.
//
// The transaction report carries no currency field at all; the programme list
// carries one per programme, and across the three hundred and thirty-four
// this account is joined to they are not all the same - three report in PLN
// and two in USD. So a per-account currency declaration is measurably wrong
// for five of them, and this test exists so that fact is in the suite rather
// than only in a comment.
func TestTheCurrencyIsPerProgrammeAndNotUniform(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "programs.json"))
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}
	var rows []struct {
		ID       int64 `json:"id"`
		Currency struct {
			Code string `json:"code"`
		} `json:"currency"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("the recording will not parse: %v", err)
	}
	seen := map[string]int{}
	for _, row := range rows {
		if row.Currency.Code == "" {
			t.Errorf("programme %d carries no currency, and the programme list is the only place one exists", row.ID)
		}
		seen[row.Currency.Code]++
	}
	if len(seen) < 2 {
		t.Errorf("every recorded programme reports in %v, so this recording cannot hold the per-account currency assumption to anything", seen)
	}
}

// TestTheCatalogueAsksForJoinedProgrammesAtEveryStatus.
//
// status=all is the half that matters. The endpoint's default is
// status=active, under which an advertiser who paused their programme simply
// VANISHES from the answer - and absence is how an import spells "left the
// network". The default would turn every temporary pause into a departure.
func TestTheCatalogueAsksForJoinedProgrammesAtEveryStatus(t *testing.T) {
	t.Parallel()

	var seen http.Request
	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		seen = *r.Clone(context.Background())
		_, _ = w.Write([]byte(`[]`))
	})
	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	if _, failed := collectMerchants(t, seq); failed != nil {
		t.Fatalf("the catalogue read ended with %v", failed)
	}

	if got, want := seen.URL.Path, "/api/1.1/programs.html"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if got := seen.URL.Query().Get("joined"); got != "yes" {
		t.Errorf("joined = %q, want yes; a route we have not been accepted into cannot be promoted", got)
	}
	if got := seen.URL.Query().Get("status"); got != "all" {
		t.Errorf("status = %q, want all; the default is active, under which a paused programme vanishes and reads as a departure", got)
	}
	if got := seen.URL.Query().Get("format"); got != "json" {
		t.Errorf("format = %q, want json", got)
	}
}

// TestACatalogueFailureIsYieldedRatherThanReturned. A catalogue read has no
// precondition checkable without contacting the network, so every failure -
// including one on the very first byte - travels through the sequence.
func TestACatalogueFailureIsYieldedRatherThanReturned(t *testing.T) {
	t.Parallel()

	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue() refused before contacting the network: %v", err)
	}
	got, failed := collectMerchants(t, seq)
	if !errors.Is(failed, networks.ErrNetworkRefused) {
		t.Fatalf("the read ended with %v, want ErrNetworkRefused", failed)
	}
	if len(got) != 0 {
		t.Errorf("%d entries were yielded before the refusal", len(got))
	}
}

// TestACancelledCatalogueReadSaysSo is contract rule 8, and it matters more
// here than anywhere else in this adapter: an import reads absence as
// departure, so a read that ended early and quietly would have every retailer
// it did not reach marked left_network and every offer on them retired.
func TestACancelledCatalogueReadSaysSo(t *testing.T) {
	t.Parallel()

	client := servingRecording(t, "programs.json")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	seq, err := client.FetchCatalogue(ctx)
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	_, failed := collectMerchants(t, seq)
	if !errors.Is(failed, networks.ErrIterationAbandoned) {
		t.Fatalf("a cancelled read ended with %v, want one wrapping ErrIterationAbandoned", failed)
	}
	if !errors.Is(failed, context.Canceled) {
		t.Errorf("a cancelled read ended with %v, want one wrapping context.Canceled too", failed)
	}
}

// TestACallersOwnBreakEndsTheCatalogueSilently.
func TestACallersOwnBreakEndsTheCatalogueSilently(t *testing.T) {
	t.Parallel()

	client := servingRecording(t, "programs.json")
	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	for _, err := range seq {
		if err != nil {
			t.Fatalf("the read yielded %v before the caller stopped", err)
		}
		break
	}
}

// TestAStateNobodyMappedStopsTheImport is contract rule 2, and where the
// refusal lands is the point: an import may only reconcile a retailer it did
// not see to left_network after iteration ended with NO error, so a refusal
// here stops the import rather than emptying the catalogue.
func TestAStateNobodyMappedStopsTheImport(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ name, row string }{
		{
			// joined=yes should only ever return accepted programmes, so
			// this says the query no longer means what it meant.
			name: "an affiliate status that is not accepted",
			row:  `{"id":1,"name":"X","countries":"GR","program_status":"Active","affiliate_status":"Pending","terms":{"promotion_methods":{"cashback_sites":{"allow":true}}}}`,
		},
		{
			name: "a programme status nobody has seen",
			row:  `{"id":1,"name":"X","countries":"GR","program_status":"Suspended","affiliate_status":"Accepted","terms":{"promotion_methods":{"cashback_sites":{"allow":true}}}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("[" + tt.row + "]"))
			})
			seq, err := client.FetchCatalogue(t.Context())
			if err != nil {
				t.Fatalf("FetchCatalogue(): %v", err)
			}
			got, failed := collectMerchants(t, seq)
			if !errors.Is(failed, networks.ErrUnmappableStatus) {
				t.Fatalf("the read ended with %v, want ErrUnmappableStatus", failed)
			}
			if len(got) != 0 {
				t.Errorf("%d entries were yielded before the refusal", len(got))
			}
		})
	}
}

// TestAMissingCashbackFlagIsReadAsNotAllowed. A programme that says nothing
// about cashback has not said yes, and the two costs are not symmetric: being
// wrong this way loses a retailer nobody could have promoted anyway, and
// being wrong the other way promotes a route the advertiser forbids.
func TestAMissingCashbackFlagIsReadAsNotAllowed(t *testing.T) {
	t.Parallel()

	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"name":"X","countries":"GR","program_status":"Active","affiliate_status":"Accepted","terms":{"promotion_methods":{}}}]`))
	})
	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	got, failed := collectMerchants(t, seq)
	if failed != nil {
		t.Fatalf("the read ended with %v", failed)
	}
	if len(got) != 1 {
		t.Fatalf("the read yielded %d entries, want 1", len(got))
	}
	if got[0].Status != networks.MerchantStatusPaused {
		t.Errorf("Status = %q, want paused: a programme that says nothing about cashback has not said yes", got[0].Status)
	}
}

// TestAProgrammeWithNoNameIsRefused, because a retailer nobody can name
// cannot be published and the alternative is an empty string shown to
// members.
func TestAProgrammeWithNoNameIsRefused(t *testing.T) {
	t.Parallel()

	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"name":"  ","countries":"GR","program_status":"Active","affiliate_status":"Accepted","terms":{"promotion_methods":{"cashback_sites":{"allow":true}}}}]`))
	})
	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	if _, failed := collectMerchants(t, seq); !errors.Is(failed, networks.ErrMissingMerchantName) {
		t.Fatalf("the read ended with %v, want ErrMissingMerchantName", failed)
	}
}

// TestAnEmptyCatalogueIsNotAnError, although an import reading it would
// retire everything. That is the import's decision to make with the whole
// answer in hand, not this adapter's to hide.
func TestAnEmptyCatalogueIsNotAnError(t *testing.T) {
	t.Parallel()

	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	seq, err := client.FetchCatalogue(t.Context())
	if err != nil {
		t.Fatalf("FetchCatalogue(): %v", err)
	}
	got, failed := collectMerchants(t, seq)
	if failed != nil {
		t.Fatalf("an empty catalogue ended with %v", failed)
	}
	if len(got) != 0 {
		t.Errorf("an empty catalogue yielded %d entries", len(got))
	}
}
