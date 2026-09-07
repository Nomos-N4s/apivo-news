// The published slot moving between routes, against the real schema
// (FR-100, spec 004 T209/T210).
//
// merchant_network_preferred_is_publishable (0035) refuses a preferred
// route that is not active, so a route that pauses or leaves the network
// while holding the slot forces the importer to withdraw it first and hand
// it to a survivor. What is asserted here is that hand-over: who holds the
// slot afterwards, what the run reports, and that the movement is on the
// stream.

package catalogue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// anotherNetwork seeds a second, active network and answers its id.
func anotherNetwork(ctx context.Context, t *testing.T, tx pgx.Tx) string {
	t.Helper()
	id := "othernet_" + uuid.NewString()[:8]
	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ($1, 'Other Network', 'clickref', 31, 20, true)`, id); err != nil {
		t.Fatalf("seeding the second network: %v", err)
	}
	return id
}

// merchantOf answers the retailer behind a route the import wrote.
func merchantOf(ctx context.Context, t *testing.T, tx pgx.Tx, networkID, externalID string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		select merchant_id from cashback.merchant_network
		 where network_id = $1 and external_merchant_id = $2`, networkID, externalID).Scan(&id); err != nil {
		t.Fatalf("reading the retailer behind %s at %s: %v", externalID, networkID, err)
	}
	return id
}

// publishedRoute answers which of the retailer's routes holds the slot, or
// the nil uuid when none does.
func publishedRoute(ctx context.Context, t *testing.T, tx pgx.Tx, merchant uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		select id from cashback.merchant_network where merchant_id = $1 and preferred`, merchant).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return uuid.Nil
	case err != nil:
		t.Fatalf("reading the published route of %s: %v", merchant, err)
	}
	return id
}

// announced counts the route events about a retailer.
func announced(ctx context.Context, t *testing.T, tx pgx.Tx, eventType string, merchant uuid.UUID) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(ctx,
		`select count(*) from domain_event where type = $1 and subject = $2`,
		eventType, merchant.String()).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", eventType, err)
	}
	return n
}

// TestAPausedPublishedRouteHandsOverToASurvivor is FR-100 for a pause: the
// retailer's other route takes the slot, the run says so, and the stream
// records which route replaced which and why.
func TestAPausedPublishedRouteHandsOverToASurvivor(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "7", "Two Ways", "DE", networks.MerchantStatusActive),
	}
	if _, err := anImporter(t, importTestAt).Run(ctx, tx, net); err != nil {
		t.Fatalf("the first import failed: %v", err)
	}
	merchant := merchantOf(ctx, t, tx, net.id.String(), "7")
	first := publishedRoute(ctx, t, tx, merchant)
	if first == uuid.Nil {
		t.Fatal("the first route was not published")
	}
	survivor := aRoute(ctx, t, tx, merchant, anotherNetwork(ctx, t, tx), false)

	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "7", "Two Ways", "DE", networks.MerchantStatusPaused),
	}
	got, err := anImporter(t, importTestAt.Add(time.Hour)).Run(ctx, tx, net)
	if err != nil {
		t.Fatalf("the pausing import failed: %v", err)
	}
	if status, preferred := routeStatus(ctx, t, tx, net.id.String(), "7"); status != "paused" || preferred {
		t.Errorf("the paused route is %q, preferred %v; want paused and demoted", status, preferred)
	}
	if now := publishedRoute(ctx, t, tx, merchant); now != survivor {
		t.Errorf("the published route is %s, want the survivor %s", now, survivor)
	}
	if got.Republished != 1 || got.LostPublication != 0 {
		t.Errorf("the run reported %d republished and %d lost, want 1 and 0", got.Republished, got.LostPublication)
	}
	if n := announced(ctx, t, tx, catalogue.TypeRoutePublished, merchant); n != 1 {
		t.Errorf("the stream holds %d hand-overs for the retailer, want 1", n)
	}
	if len(got.PublishingNothing) != 0 {
		t.Errorf("the run lists %+v as publishing nothing; the survivor publishes", got.PublishingNothing)
	}
}

// TestADepartedPublishedRouteHandsOverToASurvivor is the same for a route
// the network stopped listing: the reconciliation withdraws the slot before
// marking the route departed, and the survivor takes over.
func TestADepartedPublishedRouteHandsOverToASurvivor(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "3", "Leaves One Way", "DE", networks.MerchantStatusActive),
	}
	if _, err := anImporter(t, importTestAt).Run(ctx, tx, net); err != nil {
		t.Fatalf("the first import failed: %v", err)
	}
	merchant := merchantOf(ctx, t, tx, net.id.String(), "3")
	survivor := aRoute(ctx, t, tx, merchant, anotherNetwork(ctx, t, tx), false)

	net.merchants = nil
	got, err := anImporter(t, importTestAt.Add(time.Hour)).Run(ctx, tx, net)
	if err != nil {
		t.Fatalf("the emptying import failed: %v", err)
	}
	if status, preferred := routeStatus(ctx, t, tx, net.id.String(), "3"); status != "left_network" || preferred {
		t.Errorf("the departed route is %q, preferred %v; want left_network and demoted", status, preferred)
	}
	if now := publishedRoute(ctx, t, tx, merchant); now != survivor {
		t.Errorf("the published route is %s, want the survivor %s", now, survivor)
	}
	if got.Departed != 1 || got.Republished != 1 {
		t.Errorf("the run reported %d departed and %d republished, want 1 and 1", got.Departed, got.Republished)
	}
	if n := announced(ctx, t, tx, catalogue.TypeRoutePublished, merchant); n != 1 {
		t.Errorf("the stream holds %d hand-overs for the retailer, want 1", n)
	}
}

// TestAPublishedRouteWithNoSurvivorLeavesTheRetailerUnpublished. Nothing to
// promote is a fact too: the run counts it, the stream records it, and when
// the route comes back the retailer publishes again.
func TestAPublishedRouteWithNoSurvivorLeavesTheRetailerUnpublished(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "8", "One Way Only", "DE", networks.MerchantStatusActive),
	}
	if _, err := anImporter(t, importTestAt).Run(ctx, tx, net); err != nil {
		t.Fatalf("the first import failed: %v", err)
	}
	merchant := merchantOf(ctx, t, tx, net.id.String(), "8")

	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "8", "One Way Only", "DE", networks.MerchantStatusPaused),
	}
	got, err := anImporter(t, importTestAt.Add(time.Hour)).Run(ctx, tx, net)
	if err != nil {
		t.Fatalf("the pausing import failed: %v", err)
	}
	if now := publishedRoute(ctx, t, tx, merchant); now != uuid.Nil {
		t.Errorf("the retailer still publishes through %s after its only route paused", now)
	}
	if got.LostPublication != 1 || got.Republished != 0 {
		t.Errorf("the run reported %d lost and %d republished, want 1 and 0", got.LostPublication, got.Republished)
	}
	if n := announced(ctx, t, tx, catalogue.TypeRouteUnpublished, merchant); n != 1 {
		t.Errorf("the stream holds %d unpublications for the retailer, want 1", n)
	}
	// A paused route is not a publishable one, so the retailer is not
	// listed as publishing nothing it could publish.
	if len(got.PublishingNothing) != 0 {
		t.Errorf("the run lists %+v as publishing nothing, want nobody: the only route is paused", got.PublishingNothing)
	}

	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "8", "One Way Only", "DE", networks.MerchantStatusActive),
	}
	if _, err := anImporter(t, importTestAt.Add(2*time.Hour)).Run(ctx, tx, net); err != nil {
		t.Fatalf("the reviving import failed: %v", err)
	}
	if _, preferred := routeStatus(ctx, t, tx, net.id.String(), "8"); !preferred {
		t.Error("the route came back active and the retailer still publishes nothing")
	}
	if n := announced(ctx, t, tx, catalogue.TypeRoutePublished, merchant); n != 1 {
		t.Errorf("the stream holds %d publications for the retailer after the revival, want 1", n)
	}
}

// TestAFirstRouteImportedPausedIsNotPublished. The first route becomes the
// published one only if it can be published; a retailer whose first route
// arrives paused waits for one that is not.
func TestAFirstRouteImportedPausedIsNotPublished(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	net.merchants = []networks.ReportedMerchant{
		aReportedMerchant(t, "5", "Arrives Paused", "DE", networks.MerchantStatusPaused),
	}
	if _, err := anImporter(t, importTestAt).Run(ctx, tx, net); err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if status, preferred := routeStatus(ctx, t, tx, net.id.String(), "5"); status != "paused" || preferred {
		t.Errorf("the route is %q, preferred %v; want paused and not published", status, preferred)
	}
}

// TestARetailerThatCouldPublishAndDoesNotIsListed is T210: a retailer with a
// publishable route on a network this import does not read, and no
// published route, is named by the run rather than left silent.
func TestARetailerThatCouldPublishAndDoesNotIsListed(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	slug := "quiet-" + uuid.NewString()[:8]
	merchant := aMerchant(ctx, t, tx, slug, nil, map[string]string{"de": "Still"})
	aRoute(ctx, t, tx, merchant, anotherNetwork(ctx, t, tx), false)

	got, err := anImporter(t, importTestAt).Run(ctx, tx, net)
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	var listed bool
	for _, r := range got.PublishingNothing {
		if r.ID == merchant {
			listed = true
			if r.Slug != slug || r.PublishableRoutes != 1 {
				t.Errorf("listed as %+v, want slug %s with 1 publishable route", r, slug)
			}
		}
	}
	if !listed {
		t.Errorf("the retailer with a publishable route and nothing published is not listed: %+v", got.PublishingNothing)
	}
}
