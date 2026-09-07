// The listing's reads against the real schema (#545, US5 scenario 1).
//
// Two claims a fake store would only echo: that several places each scope
// up their own tree and meet as one row per retailer, and that the bands
// on the shelf are exactly the bands the shop page publishes - the
// preferred route's, in force at the moment of reading. Everything happens
// inside a transaction that is rolled back.

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
)

// aSluggedPlace seeds one place a reader can name, under an optional parent.
func aSluggedPlace(ctx context.Context, t *testing.T, tx pgx.Tx, slug string, parent uuid.UUID) uuid.UUID {
	t.Helper()
	id := aPlace(ctx, t, tx, "Place "+slug, parent)
	if _, err := tx.Exec(ctx, `update place set slug = $2 where id = $1`, id, slug); err != nil {
		t.Fatalf("naming place %s: %v", slug, err)
	}
	return id
}

// shelving builds the lister over a transaction.
func shelving(t *testing.T, tx pgx.Tx) *catalogue.Lister {
	t.Helper()
	l, err := catalogue.NewLister(store.New(tx))
	if err != nil {
		t.Fatalf("NewLister(): %v", err)
	}
	return l
}

// TestTheShelfScopesEachPlaceUpItsOwnTree. A reader following Munich and
// Greece sees the national German retailer once, the Munich one, the Greek
// one - and neither the Hamburg one nor the paused one.
func TestTheShelfScopesEachPlaceUpItsOwnTree(t *testing.T) {
	ctx, tx, _ := importTestTx(t)
	suffix := uuid.NewString()[:8]

	germany := aSluggedPlace(ctx, t, tx, "germany-"+suffix, uuid.Nil)
	bavaria := aSluggedPlace(ctx, t, tx, "bavaria-"+suffix, germany)
	munich := aSluggedPlace(ctx, t, tx, "munich-"+suffix, bavaria)
	hamburg := aSluggedPlace(ctx, t, tx, "hamburg-"+suffix, germany)
	greece := aSluggedPlace(ctx, t, tx, "greece-"+suffix, uuid.Nil)

	aMerchant(ctx, t, tx, "national-"+suffix, []uuid.UUID{germany}, map[string]string{"de": "Bundesweit"})
	aMerchant(ctx, t, tx, "local-"+suffix, []uuid.UUID{munich}, map[string]string{"de": "Münchner Laden"})
	aMerchant(ctx, t, tx, "elsewhere-"+suffix, []uuid.UUID{hamburg}, map[string]string{"de": "Hamburger Laden"})
	aMerchant(ctx, t, tx, "hellenic-"+suffix, []uuid.UUID{greece}, map[string]string{"el": "Ελληνικό", "de": "Griechisch"})
	paused := aMerchant(ctx, t, tx, "paused-"+suffix, []uuid.UUID{munich}, map[string]string{"de": "Pausiert"})
	if _, err := tx.Exec(ctx, `update cashback.merchant set status = 'paused' where id = $1`, paused); err != nil {
		t.Fatalf("pausing a merchant: %v", err)
	}

	// Munich and Bavaria both walk up to Germany; the national retailer is
	// still one row.
	page, err := shelving(t, tx).List(ctx, catalogue.ListQuery{
		Language: "el",
		Places:   []string{"munich-" + suffix, "bavaria-" + suffix, "greece-" + suffix},
	}, detailAt)
	if err != nil {
		t.Fatalf("List(): %v", err)
	}

	got := itemSlugs(page)
	want := []string{"hellenic-" + suffix, "local-" + suffix, "national-" + suffix}
	if len(got) != len(want) {
		t.Fatalf("the shelf holds %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("the shelf holds %v, want %v in slug order", got, want)
		}
	}
	if page.NextCursor != "" {
		t.Errorf("next cursor = %q, want none on a shelf that fits one page", page.NextCursor)
	}
	// Place and language are independent axes: the Greek reader gets the
	// Greek copy where it exists and the labelled German copy where not.
	if page.Items[0].Copy.Name != "Ελληνικό" || page.Items[0].Copy.Fallback {
		t.Errorf("the Greek retailer is described as %+v, want its Greek copy", page.Items[0].Copy)
	}
	if page.Items[1].Copy.Name != "Münchner Laden" || !page.Items[1].Copy.Fallback {
		t.Errorf("the Munich retailer is described as %+v, want the German copy labelled as a fallback", page.Items[1].Copy)
	}
}

// TestTheShelfCarriesTheBandsTheShopPagePublishes. The same definition of
// "published" as GET /merchants/{slug}: the preferred route's bands, in
// force at the moment of reading - not a lapsed one, not one on a route
// the click will not go through.
func TestTheShelfCarriesTheBandsTheShopPagePublishes(t *testing.T) {
	ctx, tx, net := importTestTx(t)
	suffix := uuid.NewString()[:8]
	place := aSluggedPlace(ctx, t, tx, "place-"+suffix, uuid.Nil)

	rated := aMerchant(ctx, t, tx, "rated-"+suffix, []uuid.UUID{place}, map[string]string{"de": "Bewertet"})
	route := aRoute(ctx, t, tx, rated, net.id.String(), true)
	aBand(ctx, t, tx, route, 800, "on electronics", "", detailAt.Add(-time.Hour), nil)
	aBand(ctx, t, tx, route, 200, "lapsed", "", detailAt.Add(-48*time.Hour), detailAt.Add(-time.Hour))
	otherNet := "othernet_" + uuid.NewString()[:8]
	if _, err := tx.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ($1, 'Other Network', 'clickref', 31, 20, true)`, otherNet); err != nil {
		t.Fatalf("seeding the second network: %v", err)
	}
	other := aRoute(ctx, t, tx, rated, otherNet, false)
	aBand(ctx, t, tx, other, 1000, "on the other route", "", detailAt.Add(-time.Hour), nil)
	aMerchant(ctx, t, tx, "unrated-"+suffix, []uuid.UUID{place}, map[string]string{"de": "Unbewertet"})

	page, err := shelving(t, tx).List(ctx, catalogue.ListQuery{
		Language: "de", Places: []string{"place-" + suffix},
	}, detailAt)
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if got := itemSlugs(page); len(got) != 2 || got[0] != "rated-"+suffix || got[1] != "unrated-"+suffix {
		t.Fatalf("the shelf holds %v, want the rated then the unrated retailer", got)
	}

	bands := page.Items[0].Bands
	if len(bands) != 1 {
		t.Fatalf("the rated retailer carries %d bands, want the one live band on the preferred route", len(bands))
	}
	if bands[0].Rate.Kind != catalogue.RatePercent || bands[0].Rate.Percent != 400 {
		t.Errorf("the band is %+v, want the member's half of 800 bps", bands[0].Rate)
	}
	if bands[0].Conditions != "on electronics" {
		t.Errorf("conditions = %q, want the live band's own", bands[0].Conditions)
	}
	if len(page.Items[1].Bands) != 0 {
		t.Errorf("the unrated retailer carries %+v, want no bands", page.Items[1].Bands)
	}

	// A page read for other retailers does not carry these bands.
	if _, err := shelving(t, tx).List(ctx, catalogue.ListQuery{
		Language: "de", Places: []string{"nowhere-" + suffix},
	}, detailAt); !errors.Is(err, catalogue.ErrUnknownPlace) {
		t.Errorf("a place that names nothing = %v, want ErrUnknownPlace", err)
	}
}
