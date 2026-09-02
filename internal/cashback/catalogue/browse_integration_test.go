// The tests for browse.go, against the real schema (T102, US5 scenario 1).
//
// The claim is a recursive walk up place.parent_id, and a fake store would
// agree with the code rather than with Postgres - which is exactly where the
// walk runs. Everything happens inside a transaction that is rolled back, so
// two cases never meet and neither leaves a place behind.

package catalogue_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
)

// aPlace seeds one place under an optional parent and returns its id.
func aPlace(ctx context.Context, t *testing.T, tx pgx.Tx, name string, parent uuid.UUID) uuid.UUID {
	t.Helper()
	var (
		id        uuid.UUID
		parentArg any
	)
	if parent != uuid.Nil {
		parentArg = parent
	}
	if err := tx.QueryRow(ctx,
		`insert into place (name, country, parent_id) values ($1, 'DE', $2) returning id`,
		name, parentArg).Scan(&id); err != nil {
		t.Fatalf("seeding place %s: %v", name, err)
	}
	return id
}

// merchantSourceLanguage is what every merchant in this file is sourced in.
// One value, because these cases are about PLACE and about the fallback
// LABEL - which language the source is does not change either, and varying
// it would suggest it did.
const merchantSourceLanguage = "de"

// aMerchant seeds one merchant, its copy, and its place attachments.
func aMerchant(ctx context.Context, t *testing.T, tx pgx.Tx, slug string, places []uuid.UUID, copies map[string]string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := tx.QueryRow(ctx,
		`insert into cashback.merchant (slug, country, source_language_code) values ($1, 'DE', $2) returning id`,
		slug, merchantSourceLanguage).Scan(&id); err != nil {
		t.Fatalf("seeding merchant %s: %v", slug, err)
	}
	for language, name := range copies {
		if _, err := tx.Exec(ctx,
			`insert into cashback.merchant_copy (merchant_id, language_code, name) values ($1, $2, $3)`,
			id, language, name); err != nil {
			t.Fatalf("seeding %s copy for %s: %v", language, slug, err)
		}
	}
	for _, place := range places {
		if _, err := tx.Exec(ctx,
			`insert into cashback.merchant_place (merchant_id, place_id) values ($1, $2)`,
			id, place); err != nil {
			t.Fatalf("attaching %s to a place: %v", slug, err)
		}
	}
	return id
}

// browsing builds the reader over a transaction.
func browsing(t *testing.T, tx pgx.Tx) *catalogue.Browser {
	t.Helper()
	b, err := catalogue.NewBrowser(store.New(tx))
	if err != nil {
		t.Fatalf("NewBrowser(): %v", err)
	}
	return b
}

// slugsOf is what a listing shows, in the order it shows it.
func slugsOf(listings []catalogue.Listing) []string {
	slugs := make([]string, 0, len(listings))
	for _, one := range listings {
		slugs = append(slugs, one.Slug)
	}
	return slugs
}

// TestAMunichReaderSeesMunichAndGermany is US5 scenario 1, and the whole
// reason the scope walks UP the tree: a national retailer is one row
// attached to the country, not one row per town.
func TestAMunichReaderSeesMunichAndGermany(t *testing.T) {
	ctx, tx, _ := importTestTx(t)
	suffix := uuid.NewString()[:8]

	germany := aPlace(ctx, t, tx, "Germany "+suffix, uuid.Nil)
	bavaria := aPlace(ctx, t, tx, "Bavaria "+suffix, germany)
	munich := aPlace(ctx, t, tx, "Munich "+suffix, bavaria)
	hamburg := aPlace(ctx, t, tx, "Hamburg "+suffix, germany)

	aMerchant(ctx, t, tx, "national-"+suffix, []uuid.UUID{germany}, map[string]string{"de": "Bundesweit"})
	aMerchant(ctx, t, tx, "local-"+suffix, []uuid.UUID{munich}, map[string]string{"de": "Münchner Laden"})
	aMerchant(ctx, t, tx, "elsewhere-"+suffix, []uuid.UUID{hamburg}, map[string]string{"de": "Hamburger Laden"})

	listings, err := browsing(t, tx).Browse(ctx, munich, "de")
	if err != nil {
		t.Fatalf("Browse(): %v", err)
	}

	got := slugsOf(listings)
	want := map[string]bool{"national-" + suffix: true, "local-" + suffix: true}
	for _, slug := range got {
		if !want[slug] {
			t.Errorf("Munich sees %q, which is scoped elsewhere", slug)
		}
		delete(want, slug)
	}
	for slug := range want {
		t.Errorf("Munich does not see %q", slug)
	}
}

// TestAMerchantAttachedToNoPlaceIsSeenNowhere. The alternative reading -
// unscoped means everywhere - turns an import that forgot to attach places
// into a retailer flooding every catalogue in every country, which nobody
// sees until a member clicks something they cannot buy from.
func TestAMerchantAttachedToNoPlaceIsSeenNowhere(t *testing.T) {
	ctx, tx, _ := importTestTx(t)
	suffix := uuid.NewString()[:8]

	germany := aPlace(ctx, t, tx, "Germany "+suffix, uuid.Nil)
	munich := aPlace(ctx, t, tx, "Munich "+suffix, germany)
	aMerchant(ctx, t, tx, "unscoped-"+suffix, nil, map[string]string{"de": "Nirgendwo"})

	listings, err := browsing(t, tx).Browse(ctx, munich, "de")
	if err != nil {
		t.Fatalf("Browse(): %v", err)
	}
	for _, slug := range slugsOf(listings) {
		if slug == "unscoped-"+suffix {
			t.Error("a merchant attached to no place was listed anyway")
		}
	}
}

// TestBrowsingCarriesTheFallbackLabel. Place and language are independent
// axes, so scoping a merchant in does not decide what language it is
// described in - and the label travels with the listing (T101).
func TestBrowsingCarriesTheFallbackLabel(t *testing.T) {
	ctx, tx, _ := importTestTx(t)
	suffix := uuid.NewString()[:8]

	munich := aPlace(ctx, t, tx, "Munich "+suffix, uuid.Nil)
	aMerchant(ctx, t, tx, "translated-"+suffix, []uuid.UUID{munich},
		map[string]string{"de": "Möbelhaus", "el": "Έπιπλα"})
	aMerchant(ctx, t, tx, "untranslated-"+suffix, []uuid.UUID{munich},
		map[string]string{"de": "Gartenhaus"})

	listings, err := browsing(t, tx).Browse(ctx, munich, "el")
	if err != nil {
		t.Fatalf("Browse(): %v", err)
	}

	var sawTranslated, sawFallback bool
	for _, one := range listings {
		switch one.Slug {
		case "translated-" + suffix:
			sawTranslated = true
			if one.Copy.Fallback || one.Copy.Language != "el" {
				t.Errorf("the translated merchant came back as %s fallback=%v, want el and no label",
					one.Copy.Language, one.Copy.Fallback)
			}
		case "untranslated-" + suffix:
			sawFallback = true
			if !one.Copy.Fallback || one.Copy.Language != "de" {
				t.Errorf("the untranslated merchant came back as %s fallback=%v, want de labelled",
					one.Copy.Language, one.Copy.Fallback)
			}
		}
	}
	if !sawTranslated || !sawFallback {
		t.Errorf("saw translated=%v fallback=%v, want both listed", sawTranslated, sawFallback)
	}
}

// TestAMerchantWithNoCopyAtAllIsLeftOut, silently. A card with no name tells
// a member nothing except that something is broken, and a member browsing is
// not the person who can fix it - so it is omitted rather than blank, and
// the page does not fail over one bad row.
func TestAMerchantWithNoCopyAtAllIsLeftOut(t *testing.T) {
	ctx, tx, _ := importTestTx(t)
	suffix := uuid.NewString()[:8]

	munich := aPlace(ctx, t, tx, "Munich "+suffix, uuid.Nil)
	aMerchant(ctx, t, tx, "nameless-"+suffix, []uuid.UUID{munich}, nil)
	aMerchant(ctx, t, tx, "named-"+suffix, []uuid.UUID{munich}, map[string]string{"de": "Möbelhaus"})

	listings, err := browsing(t, tx).Browse(ctx, munich, "de")
	if err != nil {
		t.Fatalf("Browse(): %v", err)
	}

	var sawNamed bool
	for _, one := range listings {
		if one.Slug == "nameless-"+suffix {
			t.Errorf("a merchant with no copy was listed as %q", one.Copy.Name)
		}
		if one.Slug == "named-"+suffix {
			sawNamed = true
		}
	}
	if !sawNamed {
		t.Error("one bad row took the whole listing down")
	}
}

// TestAPausedMerchantIsNotBrowsed. merchant.status is whether we publish
// them at all, independent of any network route: a paused retailer keeps
// its rows, its history and its offers, and stops appearing.
func TestAPausedMerchantIsNotBrowsed(t *testing.T) {
	ctx, tx, _ := importTestTx(t)
	suffix := uuid.NewString()[:8]

	munich := aPlace(ctx, t, tx, "Munich "+suffix, uuid.Nil)
	paused := aMerchant(ctx, t, tx, "paused-"+suffix, []uuid.UUID{munich}, map[string]string{"de": "Pausiert"})
	if _, err := tx.Exec(ctx, `update cashback.merchant set status = 'paused' where id = $1`, paused); err != nil {
		t.Fatalf("pausing the merchant: %v", err)
	}

	listings, err := browsing(t, tx).Browse(ctx, munich, "de")
	if err != nil {
		t.Fatalf("Browse(): %v", err)
	}
	for _, slug := range slugsOf(listings) {
		if slug == "paused-"+suffix {
			t.Error("a paused merchant was browsed")
		}
	}
}

// TestBrowsingWithNoPlaceIsRefused. The alternative to a place is every
// merchant everywhere, which is the answer a member is least likely to want
// and the one they cannot tell apart from a working catalogue.
func TestBrowsingWithNoPlaceIsRefused(t *testing.T) {
	ctx, tx, _ := importTestTx(t)

	if _, err := browsing(t, tx).Browse(ctx, uuid.Nil, "de"); err == nil {
		t.Error("browsing with no place succeeded, want a refusal")
	}
	if _, err := catalogue.NewBrowser(nil); err == nil {
		t.Error("a browser with nowhere to read from was built, want a refusal")
	}
}

// TestAPlaceNobodyPublishesInIsEmptyNotBroken. A place with no merchants is
// a place this catalogue does not cover yet, which is a true thing to say.
func TestAPlaceNobodyPublishesInIsEmptyNotBroken(t *testing.T) {
	ctx, tx, _ := importTestTx(t)
	suffix := uuid.NewString()[:8]
	empty := aPlace(ctx, t, tx, "Nowhere "+suffix, uuid.Nil)

	listings, err := browsing(t, tx).Browse(ctx, empty, "de")
	if err != nil {
		t.Fatalf("Browse() on an empty place: %v", err)
	}
	if len(listings) != 0 {
		t.Errorf("got %d listings for a place with no merchants, want none", len(listings))
	}
}
