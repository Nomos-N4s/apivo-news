// The tests for detail.go, against the real schema (T104, US5 scenario 3).
//
// The claim is which bands a merchant page publishes, and every leg of that
// - the preferred route, the validity window, the active network - is a
// predicate in SQL. A fake store would agree with the code rather than with
// Postgres, which is where the predicates run. Everything happens inside a
// transaction that is rolled back, so two cases never meet.

package catalogue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// detailAt is the moment every page in this file is read at. A fixed
// instant rather than time.Now() so a band's edge is a decision the case
// states, not one the clock makes while it runs.
var detailAt = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// aRoute seeds one route from a merchant to a network and returns its id.
func aRoute(ctx context.Context, t *testing.T, tx pgx.Tx, merchant uuid.UUID, network string, preferred bool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.merchant_network (
			merchant_id, network_id, brand_id, external_merchant_id,
			retrieved_at, raw_payload, preferred)
		values ($1, $2, 'apivo', $3, $4, '{}'::jsonb, $5)
		returning id`,
		merchant, network, uuid.NewString(), detailAt, preferred).Scan(&id); err != nil {
		t.Fatalf("seeding a route: %v", err)
	}
	return id
}

// memberShare is the share of the commission every band in this file gives
// the member. Half, so that the rate a page quotes is visibly not the rate
// the band records - a share of the whole would make the two indistinguishable
// and every case below would pass whichever one the page published.
const memberShare = 5000

// aBand seeds one percent rate band at the standard share and returns its
// id. A nil closes means an open-ended band.
func aBand(ctx context.Context, t *testing.T, tx pgx.Tx, route uuid.UUID,
	bps int32, conditions, exclusions string, opens time.Time, closes any,
) uuid.UUID {
	t.Helper()
	return aSharedBand(ctx, t, tx, route, bps, memberShare, conditions, exclusions, opens, closes)
}

// aSharedBand seeds one percent rate band on a route and returns its id.
func aSharedBand(ctx context.Context, t *testing.T, tx pgx.Tx, route uuid.UUID,
	bps, share int32, conditions, exclusions string, opens time.Time, closes any,
) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.offer (
			merchant_network_id, rate_kind, rate_bps, member_share_bps,
			conditions, exclusions, valid_from, valid_to, deeplink_template)
		values ($1, 'percent', $2, $3, $4, $5, $6, $7, 'https://example.test/go')
		returning id`,
		route, bps, share, textOrNull(conditions), textOrNull(exclusions), opens, closes).Scan(&id); err != nil {
		t.Fatalf("seeding a band: %v", err)
	}
	return id
}

// textOrNull keeps the schema's not-blank checks happy: the columns are
// nullable and refuse the empty string, so "records none" is null.
func textOrNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// pageOf builds the reader over a transaction.
func pageOf(t *testing.T, tx pgx.Tx) *catalogue.MerchantReader {
	t.Helper()
	r, err := catalogue.NewMerchantReader(store.New(tx))
	if err != nil {
		t.Fatalf("NewMerchantReader(): %v", err)
	}
	return r
}

// TestAMerchantPageShowsEveryPublishedBand is US5 scenario 3 verbatim: a
// retailer whose rate varies publishes several bands at once, and each one
// arrives with the conditions and the exclusions that qualify IT.
func TestAMerchantPageShowsEveryPublishedBand(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	slug := "rates-" + uuid.NewString()[:8]

	merchant := aMerchant(ctx, t, tx, slug, nil, map[string]string{"de": "Vielfalt"})
	route := aRoute(ctx, t, tx, merchant, net.id.String(), true)
	aBand(ctx, t, tx, route, 800, "on electronics", "no gift cards", detailAt.Add(-time.Hour), nil)
	aBand(ctx, t, tx, route, 200, "on groceries", "", detailAt.Add(-time.Hour), nil)
	// Closed yesterday: published once, not published now.
	aBand(ctx, t, tx, route, 9000, "launch offer", "", detailAt.Add(-48*time.Hour), detailAt.Add(-24*time.Hour))
	// Agreed for next week: a rate we have committed to and have not
	// published. Quoting it today is promising a member something they
	// cannot earn until it opens.
	aBand(ctx, t, tx, route, 7000, "spring campaign", "", detailAt.Add(24*time.Hour), nil)

	page, err := pageOf(t, tx).Detail(ctx, slug, "de", detailAt)
	if err != nil {
		t.Fatalf("Detail(): %v", err)
	}

	if len(page.Bands) != 2 {
		t.Fatalf("page shows %d bands, want the two in force", len(page.Bands))
	}
	byRate := map[money.BasisPoints]catalogue.Band{}
	for _, band := range page.Bands {
		byRate[band.Rate.Percent] = band
	}
	if _, commission := byRate[800]; commission {
		t.Error("page quotes the network's 8% commission rather than the member's half of it")
	}
	if _, closed := byRate[4500]; closed {
		t.Error("page shows a band that closed yesterday")
	}
	if _, early := byRate[3500]; early {
		t.Error("page shows a band that does not open until next week")
	}
	electronics, ok := byRate[400]
	if !ok {
		t.Fatal("page does not show the member's half of the 8% band")
	}
	if electronics.Conditions != "on electronics" || electronics.Exclusions != "no gift cards" {
		t.Errorf("8%% band = %q / %q, want its own conditions and exclusions",
			electronics.Conditions, electronics.Exclusions)
	}
	groceries, ok := byRate[100]
	if !ok {
		t.Fatal("page does not show the member's half of the 2% band")
	}
	if groceries.Conditions != "on groceries" {
		t.Errorf("2%% band conditions = %q, want its own", groceries.Conditions)
	}
	if groceries.Exclusions != "" {
		t.Errorf("2%% band exclusions = %q, want none - the other band's exclusions are not this one's",
			groceries.Exclusions)
	}
	if !groceries.ValidTo.IsZero() {
		t.Errorf("open-ended band closes at %s, want the zero time", groceries.ValidTo)
	}
}

// TestAMerchantPageFallsBackToTheSourceLanguageAndSaysSo is US5 scenario 2
// on the page a member browsed TO, not just the list they came from.
func TestAMerchantPageFallsBackToTheSourceLanguageAndSaysSo(t *testing.T) {
	ctx, tx, _ := importTestTx(t)
	slug := "fallback-" + uuid.NewString()[:8]

	aMerchant(ctx, t, tx, slug, nil, map[string]string{merchantSourceLanguage: "Bücherhaus"})

	page, err := pageOf(t, tx).Detail(ctx, slug, "el", detailAt)
	if err != nil {
		t.Fatalf("Detail(): %v", err)
	}
	if page.Copy.Name != "Bücherhaus" {
		t.Errorf("name = %q, want the source-language copy", page.Copy.Name)
	}
	if page.Copy.Language != merchantSourceLanguage {
		t.Errorf("language = %q, want %q", page.Copy.Language, merchantSourceLanguage)
	}
	if !page.Copy.Fallback {
		t.Error("copy is not labelled a fallback, so the page would claim to be in Greek")
	}
	if page.Slug != slug {
		t.Errorf("slug = %q, want %q", page.Slug, slug)
	}
}

// TestAnUnpublishedMerchantIsNotAPage covers both halves of the contract's
// "404 unknown or inactive", and covers them with ONE error: which
// retailers we have stopped publishing is not a page request's business.
func TestAnUnpublishedMerchantIsNotAPage(t *testing.T) {
	ctx, tx, _ := importTestTx(t)

	paused := "paused-" + uuid.NewString()[:8]
	merchant := aMerchant(ctx, t, tx, paused, nil, map[string]string{"de": "Geschlossen"})
	if _, err := tx.Exec(ctx,
		`update cashback.merchant set status = 'paused' where id = $1`, merchant); err != nil {
		t.Fatalf("pausing the merchant: %v", err)
	}

	reader := pageOf(t, tx)
	for _, slug := range []string{paused, "never-imported-" + uuid.NewString()[:8], ""} {
		if _, err := reader.Detail(ctx, slug, "de", detailAt); !errors.Is(err, catalogue.ErrNoMerchant) {
			t.Errorf("Detail(%q) error = %v, want ErrNoMerchant", slug, err)
		}
	}
}

// TestAMerchantBetweenRatesIsStillAShop is why the page is two reads. A
// join would answer this case with zero rows and the handler would call a
// retailer we publish unknown.
func TestAMerchantBetweenRatesIsStillAShop(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	slug := "lapsed-" + uuid.NewString()[:8]

	merchant := aMerchant(ctx, t, tx, slug, nil, map[string]string{"de": "Zwischen den Aktionen"})
	route := aRoute(ctx, t, tx, merchant, net.id.String(), true)
	aBand(ctx, t, tx, route, 500, "", "", detailAt.Add(-48*time.Hour), detailAt.Add(-time.Hour))

	page, err := pageOf(t, tx).Detail(ctx, slug, "de", detailAt)
	if err != nil {
		t.Fatalf("Detail(): %v", err)
	}
	if len(page.Bands) != 0 {
		t.Errorf("page shows %d bands, want none in force", len(page.Bands))
	}
	if page.Copy.Name != "Zwischen den Aktionen" {
		t.Errorf("name = %q, want the retailer to still be a shop", page.Copy.Name)
	}
}

// TestOnlyThePublishedRoutesBandsAreShown. A retailer live on two networks
// has a band per network at two different rates, and exactly one of those
// networks is the one a click is issued through. Showing both would quote a
// rate no click can ever earn.
func TestOnlyThePublishedRoutesBandsAreShown(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	slug := "tworoutes-" + uuid.NewString()[:8]

	other := "othernet_" + uuid.NewString()[:8]
	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ($1, 'Other Network', 'clickref', 31, 20, true)`, other); err != nil {
		t.Fatalf("seeding the second network: %v", err)
	}

	merchant := aMerchant(ctx, t, tx, slug, nil, map[string]string{"de": "Zwei Wege"})
	published := aRoute(ctx, t, tx, merchant, net.id.String(), true)
	unpublished := aRoute(ctx, t, tx, merchant, other, false)
	aBand(ctx, t, tx, published, 400, "", "", detailAt.Add(-time.Hour), nil)
	aBand(ctx, t, tx, unpublished, 600, "", "", detailAt.Add(-time.Hour), nil)

	page, err := pageOf(t, tx).Detail(ctx, slug, "de", detailAt)
	if err != nil {
		t.Fatalf("Detail(): %v", err)
	}
	if len(page.Bands) != 1 {
		t.Fatalf("page shows %d bands, want only the published route's", len(page.Bands))
	}
	if page.Bands[0].Rate.Percent != 200 {
		t.Errorf("page quotes %d bps, want the member's half of the published route's 400", page.Bands[0].Rate.Percent)
	}
}

// TestASwitchedOffNetworkPublishesNothing. A network is born inactive and a
// half-configured one must not have its rates quoted to members - the same
// veto the click-out read applies, applied where the promise is made rather
// than only where it is redeemed.
func TestASwitchedOffNetworkPublishesNothing(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	slug := "darknet-" + uuid.NewString()[:8]

	merchant := aMerchant(ctx, t, tx, slug, nil, map[string]string{"de": "Abgeschaltet"})
	route := aRoute(ctx, t, tx, merchant, net.id.String(), true)
	aBand(ctx, t, tx, route, 700, "", "", detailAt.Add(-time.Hour), nil)
	if _, err := tx.Exec(ctx,
		`update cashback.network set active = false where id = $1`, net.id.String()); err != nil {
		t.Fatalf("switching the network off: %v", err)
	}

	page, err := pageOf(t, tx).Detail(ctx, slug, "de", detailAt)
	if err != nil {
		t.Fatalf("Detail(): %v", err)
	}
	if len(page.Bands) != 0 {
		t.Errorf("page quotes %d bands through a switched-off network", len(page.Bands))
	}
}

// TestADepartedRoutePublishesNothing. merchant_network.status is the
// retailer leaving ONE network, which is not the retailer being unpublished
// - so the page still exists and quotes nothing.
func TestADepartedRoutePublishesNothing(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	slug := "departed-" + uuid.NewString()[:8]

	merchant := aMerchant(ctx, t, tx, slug, nil, map[string]string{"de": "Weg"})
	route := aRoute(ctx, t, tx, merchant, net.id.String(), true)
	aBand(ctx, t, tx, route, 700, "", "", detailAt.Add(-time.Hour), nil)
	if _, err := tx.Exec(ctx,
		`update cashback.merchant_network set status = 'left_network' where id = $1`, route); err != nil {
		t.Fatalf("marking the route departed: %v", err)
	}

	page, err := pageOf(t, tx).Detail(ctx, slug, "de", detailAt)
	if err != nil {
		t.Fatalf("Detail(): %v", err)
	}
	if len(page.Bands) != 0 {
		t.Errorf("page quotes %d bands through a route the retailer has left", len(page.Bands))
	}
	if page.Copy.Name != "Weg" {
		t.Errorf("name = %q, want the retailer to still have a page", page.Copy.Name)
	}
}

// TestANamelessMerchantRefusesRatherThanRenderingBlank is the one place
// this reader is deliberately harsher than browsing: browse leaves an
// unnamed row out of a list of forty, and a page has nothing left to show.
func TestANamelessMerchantRefusesRatherThanRenderingBlank(t *testing.T) {
	ctx, tx, _ := importTestTx(t)
	slug := "nameless-" + uuid.NewString()[:8]

	aMerchant(ctx, t, tx, slug, nil, nil)

	if _, err := pageOf(t, tx).Detail(ctx, slug, "de", detailAt); !errors.Is(err, catalogue.ErrNoCopy) {
		t.Errorf("Detail() error = %v, want ErrNoCopy rather than a nameless page", err)
	}
}

// TestThePageQuotesWhatAMemberEarns. The band records the network's
// commission and the share of it the member receives; a page that published
// the first would be promising twice what arrives. The odd rate is here
// because half of it is half a basis point, which is the case that decides
// which direction the quote rounds - and it rounds the way the credit will,
// so the two never disagree over arithmetic.
func TestThePageQuotesWhatAMemberEarns(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	slug := "earns-" + uuid.NewString()[:8]

	merchant := aMerchant(ctx, t, tx, slug, nil, map[string]string{"de": "Anteil"})
	route := aRoute(ctx, t, tx, merchant, net.id.String(), true)
	aSharedBand(ctx, t, tx, route, 833, 5000, "", "", detailAt.Add(-time.Hour), nil)

	page, err := pageOf(t, tx).Detail(ctx, slug, "de", detailAt)
	if err != nil {
		t.Fatalf("Detail(): %v", err)
	}
	if len(page.Bands) != 1 {
		t.Fatalf("page shows %d bands, want one", len(page.Bands))
	}
	if got := page.Bands[0].Rate.Percent; got != 417 {
		t.Errorf("page quotes %d bps, want 417 - half of 833 rounded the member's way", got)
	}
}

// TestTheCatalogueRoundsAMemberShareTheWayTheLedgerWill. The direction is a
// product promise (Q4), and it is stated in two packages because a
// member-facing read has no business importing the earnings module. Held
// equal here rather than left to whoever edits one of them.
func TestTheCatalogueRoundsAMemberShareTheWayTheLedgerWill(t *testing.T) {
	// A share of a commission, computed both ways: the catalogue quotes a
	// rate, the ledger credits an amount, and for every share the two must
	// round in the same direction.
	for share := money.BasisPoints(0); share <= money.BasisPointsScale; share++ {
		quoted, _, err := money.BasisPoints(833).Split(share, catalogue.MemberFavour)
		if err != nil {
			t.Fatalf("quoting at a share of %d: %v", int32(share), err)
		}
		credited, _, err := money.BasisPoints(833).Split(share, earnings.MemberFavour)
		if err != nil {
			t.Fatalf("crediting at a share of %d: %v", int32(share), err)
		}
		if quoted != credited {
			t.Fatalf("at a share of %d the catalogue quotes %d bps and the ledger credits %d",
				int32(share), int32(quoted), int32(credited))
		}
	}
}
