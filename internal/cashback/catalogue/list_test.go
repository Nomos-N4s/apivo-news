package catalogue_test

// The listing's own rules (#545), over a staged store: what it refuses
// before reading anything, how it pages, what it searches, and that the
// bands it attaches are each retailer's own.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
)

// stubListStore stages what a listing reads. Merchants are answered for
// any known place, in the order staged - the lister sorts, so a store that
// hands them back shuffled is the case worth staging.
type stubListStore struct {
	places    map[string]uuid.UUID
	merchants []store.CashbackMerchant
	copies    []store.CashbackMerchantCopy
	bands     []store.PublishedBandsForMerchantsRow

	placeErr    error
	merchantErr error
	copyErr     error
	bandErr     error

	askedMerchants bool
	bandsAskedFor  []pgtype.UUID
	bandsAskedAt   time.Time
}

func (s *stubListStore) PlacesBySlugs(_ context.Context, slugs []string) ([]store.PlacesBySlugsRow, error) {
	if s.placeErr != nil {
		return nil, s.placeErr
	}
	rows := make([]store.PlacesBySlugsRow, 0, len(slugs))
	for _, slug := range slugs {
		if id, ok := s.places[slug]; ok {
			rows = append(rows, store.PlacesBySlugsRow{ID: pgtype.UUID{Bytes: id, Valid: true}, Slug: slug})
		}
	}
	return rows, nil
}

func (s *stubListStore) MerchantsForPlaces(context.Context, []pgtype.UUID) ([]store.CashbackMerchant, error) {
	s.askedMerchants = true
	if s.merchantErr != nil {
		return nil, s.merchantErr
	}
	return s.merchants, nil
}

func (s *stubListStore) CopyForMerchants(context.Context, []pgtype.UUID) ([]store.CashbackMerchantCopy, error) {
	if s.copyErr != nil {
		return nil, s.copyErr
	}
	return s.copies, nil
}

func (s *stubListStore) PublishedBandsForMerchants(_ context.Context, arg store.PublishedBandsForMerchantsParams) ([]store.PublishedBandsForMerchantsRow, error) {
	s.bandsAskedFor = arg.MerchantIds
	s.bandsAskedAt = arg.At.Time
	if s.bandErr != nil {
		return nil, s.bandErr
	}
	return s.bands, nil
}

// stagedShelf builds a lister over a staged store.
func stagedShelf(t *testing.T, stub *stubListStore) *catalogue.Lister {
	t.Helper()
	l, err := catalogue.NewLister(stub)
	if err != nil {
		t.Fatalf("NewLister(): %v", err)
	}
	return l
}

// shelfAt is the moment every listing in this file is read at.
var shelfAt = time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)

// aStagedShelf is two places and five retailers, staged out of slug order,
// every one described in German and two of them in Greek as well.
func aStagedShelf() *stubListStore {
	stub := &stubListStore{places: map[string]uuid.UUID{"munich": uuid.New(), "greece": uuid.New()}}
	for _, slug := range []string{"delta", "bravo", "echo", "alpha", "charlie"} {
		id := uuid.New()
		stub.merchants = append(stub.merchants, store.CashbackMerchant{
			ID: pgtype.UUID{Bytes: id, Valid: true}, Slug: slug,
			Country: pgtype.Text{String: "DE", Valid: true}, SourceLanguageCode: "de", Status: "active",
		})
		stub.copies = append(stub.copies, store.CashbackMerchantCopy{
			MerchantID: pgtype.UUID{Bytes: id, Valid: true}, LanguageCode: "de", Name: "Laden " + slug,
		})
		if slug == "alpha" || slug == "echo" {
			stub.copies = append(stub.copies, store.CashbackMerchantCopy{
				MerchantID: pgtype.UUID{Bytes: id, Valid: true}, LanguageCode: "el", Name: "Κατάστημα " + slug,
			})
		}
	}
	return stub
}

// merchant finds a staged retailer by slug.
func (s *stubListStore) merchant(slug string) store.CashbackMerchant {
	for _, m := range s.merchants {
		if m.Slug == slug {
			return m
		}
	}
	return store.CashbackMerchant{}
}

func itemSlugs(page catalogue.Page) []string {
	slugs := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		slugs = append(slugs, item.Slug)
	}
	return slugs
}

func list(t *testing.T, stub *stubListStore, q catalogue.ListQuery) catalogue.Page {
	t.Helper()
	page, err := stagedShelf(t, stub).List(context.Background(), q, shelfAt)
	if err != nil {
		t.Fatalf("List(%+v): %v", q, err)
	}
	return page
}

func TestTheListingRefusesWhatItCannotScope(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		q    catalogue.ListQuery
		want error
	}{
		"no language":            {catalogue.ListQuery{Places: []string{"munich"}}, catalogue.ErrNoLanguage},
		"a language that is not": {catalogue.ListQuery{Language: "greek", Places: []string{"munich"}}, catalogue.ErrNoLanguage},
		"a language with digits": {catalogue.ListQuery{Language: "e1", Places: []string{"munich"}}, catalogue.ErrNoLanguage},
		"no place":               {catalogue.ListQuery{Language: "de"}, catalogue.ErrNoPlace},
		"blank places":           {catalogue.ListQuery{Language: "de", Places: []string{" ", ""}}, catalogue.ErrNoPlace},
		"an unknown place":       {catalogue.ListQuery{Language: "de", Places: []string{"munich", "atlantis"}}, catalogue.ErrUnknownPlace},
		"a foreign cursor":       {catalogue.ListQuery{Language: "de", Places: []string{"munich"}, Cursor: "bm90LW1pbmU"}, catalogue.ErrBadCursor},
		"a huge cursor":          {catalogue.ListQuery{Language: "de", Places: []string{"munich"}, Cursor: strings.Repeat("a", 300)}, catalogue.ErrBadCursor},
		"garbage for a cursor":   {catalogue.ListQuery{Language: "de", Places: []string{"munich"}, Cursor: "%%%"}, catalogue.ErrBadCursor},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := aStagedShelf()
			_, err := stagedShelf(t, stub).List(context.Background(), tc.q, shelfAt)
			if !errors.Is(err, tc.want) {
				t.Fatalf("List() = %v, want %v", err, tc.want)
			}
			if stub.askedMerchants {
				t.Error("a refused listing still read retailers")
			}
			if errors.Is(tc.want, catalogue.ErrUnknownPlace) && !strings.Contains(err.Error(), `"atlantis"`) {
				t.Errorf("the refusal does not name the unknown place: %v", err)
			}
		})
	}
}

func TestTheListingPagesBySlugWhateverOrderTheStoreAnswersIn(t *testing.T) {
	t.Parallel()
	stub := aStagedShelf()
	q := catalogue.ListQuery{Language: "de", Places: []string{"munich", "greece", "munich"}, Limit: 2}

	first := list(t, stub, q)
	if got := itemSlugs(first); !slices.Equal(got, []string{"alpha", "bravo"}) {
		t.Fatalf("first page = %v, want alpha, bravo", got)
	}
	if first.NextCursor == "" {
		t.Fatal("the first page of five says it is the last")
	}
	q.Cursor = first.NextCursor
	second := list(t, stub, q)
	if got := itemSlugs(second); !slices.Equal(got, []string{"charlie", "delta"}) {
		t.Fatalf("second page = %v, want charlie, delta", got)
	}
	q.Cursor = second.NextCursor
	third := list(t, stub, q)
	if got := itemSlugs(third); !slices.Equal(got, []string{"echo"}) || third.NextCursor != "" {
		t.Fatalf("third page = %v with cursor %q, want echo and no cursor", got, third.NextCursor)
	}

	// The language a reader asked for and the language they are shown.
	if first.Items[0].Copy.Language != "de" || first.Items[0].Copy.Fallback {
		t.Errorf("a German reader is shown %+v", first.Items[0].Copy)
	}
	greek := list(t, stub, catalogue.ListQuery{Language: "EL ", Places: []string{"greece"}})
	for _, item := range greek.Items {
		wantFallback := item.Slug != "alpha" && item.Slug != "echo"
		if item.Copy.Fallback != wantFallback || (!wantFallback && item.Copy.Language != "el") {
			t.Errorf("%s shown to a Greek reader as %+v", item.Slug, item.Copy)
		}
	}
}

func TestTheListingDefaultsAndCapsThePage(t *testing.T) {
	t.Parallel()
	stub := &stubListStore{places: map[string]uuid.UUID{"greece": uuid.New()}}
	for i := range 150 {
		id := uuid.New()
		slug := "shop-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		stub.merchants = append(stub.merchants, store.CashbackMerchant{ID: pgtype.UUID{Bytes: id, Valid: true}, Slug: slug, SourceLanguageCode: "el", Status: "active"})
		stub.copies = append(stub.copies, store.CashbackMerchantCopy{MerchantID: pgtype.UUID{Bytes: id, Valid: true}, LanguageCode: "el", Name: slug})
	}
	if page := list(t, stub, catalogue.ListQuery{Language: "el", Places: []string{"greece"}}); len(page.Items) != catalogue.DefaultListLimit || page.NextCursor == "" {
		t.Errorf("no limit gave %d items (cursor %q), want the default %d and a cursor", len(page.Items), page.NextCursor, catalogue.DefaultListLimit)
	}
	if page := list(t, stub, catalogue.ListQuery{Language: "el", Places: []string{"greece"}, Limit: 5000}); len(page.Items) != catalogue.MaxListLimit || page.NextCursor == "" {
		t.Errorf("a limit of 5000 gave %d items, want it capped at %d with a cursor", len(page.Items), catalogue.MaxListLimit)
	}
}

func TestTheListingSearchesTheNameTheReaderSees(t *testing.T) {
	t.Parallel()
	stub := aStagedShelf()
	// A Greek reader searching in Greek finds the retailers with Greek
	// copy; searching for the German word finds the ones shown in
	// fallback, because that is the name on their screen.
	if got := itemSlugs(list(t, stub, catalogue.ListQuery{Language: "el", Places: []string{"greece"}, Search: "κατάστημα"})); !slices.Equal(got, []string{"alpha", "echo"}) {
		t.Errorf("a Greek search found %v, want alpha and echo", got)
	}
	if got := itemSlugs(list(t, stub, catalogue.ListQuery{Language: "el", Places: []string{"greece"}, Search: " LADEN "})); !slices.Equal(got, []string{"bravo", "charlie", "delta"}) {
		t.Errorf("a search for the fallback name found %v, want the three shown in German", got)
	}
	if got := list(t, stub, catalogue.ListQuery{Language: "de", Places: []string{"munich"}, Search: "nothing-here"}); len(got.Items) != 0 || got.Items == nil {
		t.Errorf("a search matching nothing = %+v, want an empty list", got)
	}
}

func TestARetailerWithNoCopyIsLeftOffTheShelf(t *testing.T) {
	t.Parallel()
	stub := aStagedShelf()
	nameless := stub.merchant("charlie").ID
	stub.copies = slices.DeleteFunc(stub.copies, func(c store.CashbackMerchantCopy) bool { return c.MerchantID == nameless })
	if got := itemSlugs(list(t, stub, catalogue.ListQuery{Language: "de", Places: []string{"munich"}})); slices.Contains(got, "charlie") {
		t.Errorf("a retailer with no name in any language is on the shelf: %v", got)
	}
}

func TestEachRetailerGetsItsOwnBands(t *testing.T) {
	t.Parallel()
	stub := aStagedShelf()
	alpha, bravo := stub.merchant("alpha").ID, stub.merchant("bravo").ID
	band := func(merchant pgtype.UUID, bps int32) store.PublishedBandsForMerchantsRow {
		return store.PublishedBandsForMerchantsRow{
			MerchantID: merchant, ID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
			RateKind: "percent", RateBps: pgtype.Int4{Int32: bps, Valid: true},
			MemberShareBps: 5000, ValidFrom: pgtype.Timestamptz{Time: shelfAt.Add(-time.Hour), Valid: true},
		}
	}
	stub.bands = []store.PublishedBandsForMerchantsRow{band(alpha, 400), band(bravo, 1000), band(alpha, 200)}

	page := list(t, stub, catalogue.ListQuery{Language: "de", Places: []string{"munich"}, Limit: 3})
	if !stub.bandsAskedAt.Equal(shelfAt) {
		t.Errorf("the bands were read at %s, want the moment the page was read, %s", stub.bandsAskedAt, shelfAt)
	}
	if len(stub.bandsAskedFor) != 3 {
		t.Errorf("the bands were read for %d retailers, want the three on the page and nobody else", len(stub.bandsAskedFor))
	}
	got := map[string]int{}
	for _, item := range page.Items {
		got[item.Slug] = len(item.Bands)
	}
	if got["alpha"] != 2 || got["bravo"] != 1 || got["charlie"] != 0 {
		t.Errorf("bands per retailer = %v, want alpha 2, bravo 1, charlie 0", got)
	}
	for _, band := range page.Items[0].Bands {
		if band.Rate.Kind != catalogue.RatePercent || (band.Rate.Percent != 200 && band.Rate.Percent != 100) {
			t.Errorf("alpha's band is %+v, want the member's half of 400 or 200 bps", band.Rate)
		}
	}

	// A malformed band fails the page rather than half-drawing it, the
	// shop page's rule.
	broken := band(alpha, 0)
	broken.RateKind = "percent"
	broken.RateBps = pgtype.Int4{}
	stub.bands = []store.PublishedBandsForMerchantsRow{broken}
	if _, err := stagedShelf(t, stub).List(context.Background(), catalogue.ListQuery{Language: "de", Places: []string{"munich"}}, shelfAt); !errors.Is(err, catalogue.ErrMalformedOffer) {
		t.Errorf("a malformed band = %v, want ErrMalformedOffer", err)
	}
}

func TestAFailedReadFailsTheListing(t *testing.T) {
	t.Parallel()
	boom := errors.New("the database is not answering")
	for name, stage := range map[string]func(*stubListStore){
		"places":    func(s *stubListStore) { s.placeErr = boom },
		"merchants": func(s *stubListStore) { s.merchantErr = boom },
		"copy":      func(s *stubListStore) { s.copyErr = boom },
		"bands":     func(s *stubListStore) { s.bandErr = boom },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stub := aStagedShelf()
			stage(stub)
			if _, err := stagedShelf(t, stub).List(context.Background(), catalogue.ListQuery{Language: "de", Places: []string{"munich"}}, shelfAt); !errors.Is(err, boom) {
				t.Errorf("List() = %v, want the store's own error", err)
			}
		})
	}
	if _, err := catalogue.NewLister(nil); err == nil {
		t.Error("NewLister(nil) succeeded")
	}
}
