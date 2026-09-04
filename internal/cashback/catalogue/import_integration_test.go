// The tests for import.go, against the real schema. Every rule the importer
// keeps is a constraint or a unique index in migration 0011 - the slug
// format, one route per network, one preferred route per retailer - and a
// fake store would agree with the code instead of with Postgres.

package catalogue_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// importTestBrand is the brand that publishes the imported routes, and
// importTestLanguage the language an operator says the network supplies its
// copy in. "en" is one of the seeded language codes the column references.
const (
	importTestBrand    = "importtest"
	importTestLanguage = "en"
)

// importTestAt is the instant a run stamps its routes with, pinned so a
// reconciliation boundary is exact rather than "about now".
var importTestAt = time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)

// stubNetwork is a [networks.Network] that yields a scripted catalogue. It is
// not the fixture adapter: what is under test here is what the IMPORTER does
// with an answer, so the answer has to be the test's to choose - including
// the answers a recording could not produce, like one that stops halfway.
type stubNetwork struct {
	id        networks.NetworkID
	account   networks.PublisherAccount
	merchants []networks.ReportedMerchant
	// failAfter, when positive, is how many retailers are yielded before
	// the read reports it could not finish.
	failAfter int
	// immediate, when set, is the error FetchCatalogue itself returns.
	immediate error
}

func (s *stubNetwork) ID() networks.NetworkID             { return s.id }
func (s *stubNetwork) Account() networks.PublisherAccount { return s.account }
func (s *stubNetwork) Limits() networks.Limits {
	return networks.Limits{MaxWindow: 31 * 24 * time.Hour, RequestsPerMinute: 20}
}

func (s *stubNetwork) BuildDeeplink(context.Context, networks.DeeplinkTarget, networks.IssuedClickRef) (string, error) {
	return "", errors.New("stubNetwork: no deeplink in a catalogue test")
}

func (s *stubNetwork) FetchTransactions(context.Context, networks.QueryWindow) (iter.Seq2[networks.Reported, error], error) {
	return nil, errors.New("stubNetwork: no transactions in a catalogue test")
}

func (s *stubNetwork) FetchCatalogue(context.Context) (iter.Seq2[networks.ReportedMerchant, error], error) {
	if s.immediate != nil {
		return nil, s.immediate
	}
	return func(yield func(networks.ReportedMerchant, error) bool) {
		for n, merchant := range s.merchants {
			if s.failAfter > 0 && n == s.failAfter {
				yield(networks.ReportedMerchant{},
					networks.AbandonedIteration(networks.ErrNetworkUnavailable))
				return
			}
			if !yield(merchant, nil) {
				return
			}
		}
	}, nil
}

// aReportedMerchant is one retailer as a network reports it, already through
// the port's own validation so a test cannot assert on a value no adapter
// could yield.
func aReportedMerchant(t *testing.T, externalID, name, country string, status networks.MerchantStatus) networks.ReportedMerchant {
	t.Helper()
	m := networks.ReportedMerchant{
		ExternalID: externalID,
		Name:       name,
		Country:    country,
		StatusRaw:  "joined",
		Status:     status,
		RawPayload: json.RawMessage(fmt.Sprintf(`{"id":%s,"name":%q}`, externalID, name)),
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("the test built a merchant no adapter could yield: %v", err)
	}
	return m
}

// importTestTx migrates, opens a transaction the test throws away, and seeds
// a network of its own so two cases never meet.
func importTestTx(t *testing.T) (context.Context, pgx.Tx, *stubNetwork) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the catalogue import")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	// network.id is constrained to ^[a-z][a-z0-9_]*$.
	networkID := "importnet_" + hex.EncodeToString(suffix)
	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ($1, 'Import Network', 'clickref', 31, 20, true)`, networkID); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	account, err := networks.NewPublisherAccount(uuid.New(), networks.NetworkID(networkID), "123456")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}
	return ctx, tx, &stubNetwork{id: networks.NetworkID(networkID), account: account}
}

// anImporter builds the importer with the clock pinned to at.
func anImporter(t *testing.T, at time.Time) *catalogue.Importer {
	t.Helper()
	importer, err := catalogue.NewImporter(importTestBrand, importTestLanguage,
		catalogue.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatalf("NewImporter(): %v", err)
	}
	return importer
}

// routeStatus reads back what the route row says.
func routeStatus(ctx context.Context, t *testing.T, tx pgx.Tx, networkID, externalID string) (string, bool) {
	t.Helper()
	var status string
	var preferred bool
	if err := tx.QueryRow(ctx, `
		select status, preferred from cashback.merchant_network
		 where network_id = $1 and external_merchant_id = $2`, networkID, externalID).Scan(&status, &preferred); err != nil {
		t.Fatalf("reading the route to %s: %v", externalID, err)
	}
	return status, preferred
}

func TestAFirstImportCreatesSomethingAMemberCouldClick(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "4471", "Gartenhaus", "DE", networks.MerchantStatusActive),
	}

	got, err := anImporter(t, importTestAt).Run(ctx, tx, net)
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if got.Seen != 1 || got.Created != 1 || got.Departed != 0 {
		t.Errorf("Run() = %+v, want one seen, one created, none departed", got)
	}

	var (
		slug     string
		country  *string
		language string
		name     string
	)
	if err := tx.QueryRow(ctx, `
		select m.slug, m.country, m.source_language_code, c.name
		  from cashback.merchant m
		  join cashback.merchant_copy c on c.merchant_id = m.id and c.language_code = $2
		  join cashback.merchant_network mn on mn.merchant_id = m.id
		 where mn.network_id = $1`, net.id.String(), importTestLanguage).
		Scan(&slug, &country, &language, &name); err != nil {
		t.Fatalf("reading back the retailer: %v", err)
	}
	if slug != "gartenhaus" {
		t.Errorf("slug = %q, want %q", slug, "gartenhaus")
	}
	if country == nil || *country != "DE" {
		t.Errorf("country = %v, want DE", country)
	}
	if name != "Gartenhaus" {
		t.Errorf("the name a member reads is %q, want %q", name, "Gartenhaus")
	}

	// The first route is the one the catalogue publishes, so there is
	// something to publish at all.
	if status, preferred := routeStatus(ctx, t, tx, net.id.String(), "4471"); status != "active" || !preferred {
		t.Errorf("the route is status=%q preferred=%v, want active and preferred", status, preferred)
	}
}

// TestARenamedProgrammeIsTheSameRetailer is why the network's own id is the
// first question asked. Under name matching an advertiser's rename would
// create a second merchant with its own history and its own offers, while the
// first went absent and was marked departed.
func TestARenamedProgrammeIsTheSameRetailer(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "4471", "Gartenhaus", "DE", networks.MerchantStatusActive),
	}
	if _, err := anImporter(t, importTestAt).Run(ctx, tx, net); err != nil {
		t.Fatalf("the first import failed: %v", err)
	}

	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "4471", "Gartenhaus DE", "DE", networks.MerchantStatusActive),
	}
	got, err := anImporter(t, importTestAt.Add(time.Hour)).Run(ctx, tx, net)
	if err != nil {
		t.Fatalf("the second import failed: %v", err)
	}
	if got.Created != 0 {
		t.Errorf("the rename created %d retailers, want none", got.Created)
	}
	if got.Departed != 0 {
		t.Errorf("the rename marked %d routes departed, want none", got.Departed)
	}

	var merchants int
	if err := tx.QueryRow(ctx, `
		select count(*) from cashback.merchant_network where network_id = $1`, net.id.String()).Scan(&merchants); err != nil {
		t.Fatalf("counting routes: %v", err)
	}
	if merchants != 1 {
		t.Errorf("the network has %d routes after a rename, want 1", merchants)
	}

	// The slug does not follow the name: it is in URLs members have.
	var slug, name string
	if err := tx.QueryRow(ctx, `
		select m.slug, c.name
		  from cashback.merchant m
		  join cashback.merchant_copy c on c.merchant_id = m.id and c.language_code = $2
		  join cashback.merchant_network mn on mn.merchant_id = m.id
		 where mn.network_id = $1`, net.id.String(), importTestLanguage).Scan(&slug, &name); err != nil {
		t.Fatalf("reading back the retailer: %v", err)
	}
	if slug != "gartenhaus" {
		t.Errorf("the rename moved the slug to %q; a link a member holds would break", slug)
	}
	if name != "Gartenhaus DE" {
		t.Errorf("the name a member reads is %q, want the new one", name)
	}
}

// TestARetailerTheNetworkStopsListingHasLeft is absence-means-departure, and
// the reason a partial read must be an error.
func TestARetailerTheNetworkStopsListingHasLeft(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "1", "Stayer", "DE", networks.MerchantStatusActive),
		aReportedMerchant(t, "2", "Leaver", "DE", networks.MerchantStatusActive),
	}
	if _, err := anImporter(t, importTestAt).Run(ctx, tx, net); err != nil {
		t.Fatalf("the first import failed: %v", err)
	}

	net.merchants = net.merchants[:1]
	got, err := anImporter(t, importTestAt.Add(time.Hour)).Run(ctx, tx, net)
	if err != nil {
		t.Fatalf("the second import failed: %v", err)
	}
	if got.Departed != 1 {
		t.Errorf("Run() reported %d departures, want 1", got.Departed)
	}
	if status, _ := routeStatus(ctx, t, tx, net.id.String(), "2"); status != "left_network" {
		t.Errorf("the retailer the network stopped listing is %q, want left_network", status)
	}
	if status, _ := routeStatus(ctx, t, tx, net.id.String(), "1"); status != "active" {
		t.Errorf("the retailer still listed is %q, want active", status)
	}
}

// TestAPartialReadConcludesNothing is contract rule 8 where it costs money. A
// read that stopped at retailer 400 of 5000 and reconciled anyway would
// withdraw 4600 live routes and empty the catalogue members see, from a run
// that reported nothing wrong.
func TestAPartialReadConcludesNothing(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "1", "First", "DE", networks.MerchantStatusActive),
		aReportedMerchant(t, "2", "Second", "DE", networks.MerchantStatusActive),
		aReportedMerchant(t, "3", "Third", "DE", networks.MerchantStatusActive),
	}
	if _, err := anImporter(t, importTestAt).Run(ctx, tx, net); err != nil {
		t.Fatalf("the first import failed: %v", err)
	}

	net.failAfter = 1
	got, err := anImporter(t, importTestAt.Add(time.Hour)).Run(ctx, tx, net)
	if !errors.Is(err, catalogue.ErrImportIncomplete) {
		t.Fatalf("a partial read ended with %v, want one wrapping ErrImportIncomplete", err)
	}
	if got.Departed != 0 {
		t.Fatalf("a partial read reconciled %d routes; every one of them is a live retailer withdrawn", got.Departed)
	}
	for _, externalID := range []string{"1", "2", "3"} {
		if status, _ := routeStatus(ctx, t, tx, net.id.String(), externalID); status != "active" {
			t.Errorf("retailer %s is %q after a partial read, want active", externalID, status)
		}
	}
}

// TestAFailureBeforeTheFirstPageConcludesNothingEither covers the other
// error channel: one the adapter reports before yielding anything.
func TestAFailureBeforeTheFirstPageConcludesNothingEither(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "1", "First", "DE", networks.MerchantStatusActive),
	}
	if _, err := anImporter(t, importTestAt).Run(ctx, tx, net); err != nil {
		t.Fatalf("the first import failed: %v", err)
	}

	net.immediate = networks.ErrNetworkRefused
	got, err := anImporter(t, importTestAt.Add(time.Hour)).Run(ctx, tx, net)
	if !errors.Is(err, catalogue.ErrImportIncomplete) {
		t.Fatalf("a refused read ended with %v, want one wrapping ErrImportIncomplete", err)
	}
	if got.Departed != 0 {
		t.Errorf("a refused read reconciled %d routes", got.Departed)
	}
	if status, _ := routeStatus(ctx, t, tx, net.id.String(), "1"); status != "active" {
		t.Errorf("the retailer is %q after a refused read, want active", status)
	}
}

// TestTwoRetailersWithOneNameStayTwoBusinesses: merchant_slug_unique and
// merchant_network_one_route_per_network would both refuse the naive answer,
// and merging them would pay one retailer's commission into the other's page.
func TestTwoRetailersWithOneNameStayTwoBusinesses(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "10", "Fashion Store", "DE", networks.MerchantStatusActive),
		aReportedMerchant(t, "11", "Fashion Store", "FR", networks.MerchantStatusActive),
	}

	got, err := anImporter(t, importTestAt).Run(ctx, tx, net)
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if got.Created != 2 {
		t.Errorf("Run() created %d retailers, want 2", got.Created)
	}

	var slugs []string
	rows, err := tx.Query(ctx, `
		select m.slug from cashback.merchant m
		  join cashback.merchant_network mn on mn.merchant_id = m.id
		 where mn.network_id = $1 order by mn.external_merchant_id`, net.id.String())
	if err != nil {
		t.Fatalf("reading the slugs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		slugs = append(slugs, slug)
	}
	if len(slugs) != 2 {
		t.Fatalf("the network has %d routes, want 2: %v", len(slugs), slugs)
	}
	if slugs[0] == slugs[1] {
		t.Fatalf("both retailers got the slug %q", slugs[0])
	}
	if slugs[0] != "fashion-store" {
		t.Errorf("the first retailer got %q, want the name's own slug", slugs[0])
	}
}

// TestAGreekRetailerGetsATransliteratedSlugAndNotAFallback.
//
// It began as "still gets imported", against the bar that refusing it would
// fail the whole import - one Greek retailer name freezing the catalogue.
// That bar is still met and is no longer the interesting one. Since T259 the
// name transliterates, so the assertion is the stronger one: the slug is
// derived from the NAME, which means FallbackSlug did not fire.
//
// That distinction is the whole commercial point. A fallback slug embeds the
// network's own id, and merchantForSlug unifies a retailer across networks by
// slug equality alone - so a Greek retailer on a fallback slug could never be
// recognised as the same shop when the second network reports it. Two
// merchant rows, two catalogue entries, two rates, and nothing anywhere
// saying they are one shop.
func TestAGreekRetailerGetsATransliteratedSlugAndNotAFallback(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "77", "Καταστήματα", "", networks.MerchantStatusActive),
	}

	got, err := anImporter(t, importTestAt).Run(ctx, tx, net)
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if got.Created != 1 {
		t.Fatalf("Run() = %+v, want the retailer created", got)
	}

	var slug string
	var country *string
	var name string
	if err := tx.QueryRow(ctx, `
		select m.slug, m.country, c.name
		  from cashback.merchant m
		  join cashback.merchant_copy c on c.merchant_id = m.id and c.language_code = $2
		  join cashback.merchant_network mn on mn.merchant_id = m.id
		 where mn.network_id = $1`, net.id.String(), importTestLanguage).Scan(&slug, &country, &name); err != nil {
		t.Fatalf("reading back the retailer: %v", err)
	}
	if slug != "katastimata" {
		t.Errorf("slug = %q, want the transliterated name; a slug carrying %q would be a fallback, which no second network could ever match",
			slug, net.id.String())
	}
	// The name a member reads is untouched; only the URL is transliterated.
	if name != "Καταστήματα" {
		t.Errorf("the name a member reads is %q, want the network's own", name)
	}
	// A retailer bound to no country is a null, not a blank.
	if country != nil {
		t.Errorf("country = %q, want null for a retailer bound to no country", *country)
	}
}

// TestAPausedRouteIsImportedPaused: the adapter's mapping is what decides
// whether a member can click, and the importer must not flatten it.
func TestAPausedRouteIsImportedPaused(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "5", "Suspended Shop", "DE", networks.MerchantStatusPaused),
	}

	if _, err := anImporter(t, importTestAt).Run(ctx, tx, net); err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if status, _ := routeStatus(ctx, t, tx, net.id.String(), "5"); status != "paused" {
		t.Errorf("the route is %q, want paused", status)
	}
}

// TestAReturningRetailerComesBack: left_network is a state the next import
// clears, because a suspension that was reinstated must publish again without
// an operator touching the database.
func TestAReturningRetailerComesBack(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "9", "Comes Back", "DE", networks.MerchantStatusActive),
	}
	if _, err := anImporter(t, importTestAt).Run(ctx, tx, net); err != nil {
		t.Fatalf("the first import failed: %v", err)
	}

	net.merchants = nil
	if _, err := anImporter(t, importTestAt.Add(time.Hour)).Run(ctx, tx, net); err != nil {
		t.Fatalf("the emptying import failed: %v", err)
	}
	if status, _ := routeStatus(ctx, t, tx, net.id.String(), "9"); status != "left_network" {
		t.Fatalf("the route is %q, want left_network", status)
	}

	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "9", "Comes Back", "DE", networks.MerchantStatusActive),
	}
	if _, err := anImporter(t, importTestAt.Add(2*time.Hour)).Run(ctx, tx, net); err != nil {
		t.Fatalf("the returning import failed: %v", err)
	}
	if status, _ := routeStatus(ctx, t, tx, net.id.String(), "9"); status != "active" {
		t.Errorf("the returning route is %q, want active", status)
	}
}

func TestAnImporterThatCouldNotWriteARowIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := catalogue.NewImporter("", importTestLanguage); !errors.Is(err, catalogue.ErrImportNotConfigured) {
		t.Errorf("NewImporter() with no brand = %v, want one wrapping ErrImportNotConfigured", err)
	}
	if _, err := catalogue.NewImporter(importTestBrand, ""); !errors.Is(err, catalogue.ErrImportNotConfigured) {
		t.Errorf("NewImporter() with no language = %v, want one wrapping ErrImportNotConfigured", err)
	}
}
