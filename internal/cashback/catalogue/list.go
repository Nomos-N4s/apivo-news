// The listing (#545, the server half of T106): which retailers a reader
// following some places may see, described in their language, with the
// rates each one publishes, a page at a time.
//
// Built from the reads the shelf and the shop page already make - browse's
// scope walk and copy resolution, the shop page's published bands - so that
// a rate on the shelf and the same rate on the shop's page come from one
// definition of "published", and a name on the shelf is resolved by the
// rule the page uses. What is new here is only what a list needs and a page
// does not: several places at once, a search, and a cursor.

package catalogue

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
)

const (
	// DefaultListLimit is a page when the reader names no size.
	DefaultListLimit = 20
	// MaxListLimit caps a page. Above it the size is capped rather than
	// refused, as the wallet's statement does: a client that asked for too
	// much wanted a page, not an error.
	MaxListLimit = 100

	// listCursorTag marks a cursor as this list's, so a cursor issued by
	// another list decodes to a refusal here rather than to a position.
	listCursorTag = "catalogue"
	// listCursorSeparator joins the tag and the slug. A slug cannot
	// contain it: merchant_slug_format allows only lower-case letters,
	// digits and hyphens.
	listCursorSeparator = "|"
	// maxListCursorBytes bounds what is even looked at: a real cursor is a
	// short tag and a slug, and anything larger is malformed by construction.
	maxListCursorBytes = 256
)

var (
	// ErrNoLanguage reports a listing asked for in no language, or in
	// something that is not a BCP-47 primary subtag. The shop page falls
	// back to the source language when nothing is asked for; a listing does
	// not, because a shelf of forty names in a language the reader did not
	// choose is a shelf they cannot use, and the contract says the language
	// is required.
	ErrNoLanguage = errors.New("catalogue: a listing needs the reader's language as a BCP-47 primary subtag")
	// ErrUnknownPlace reports a place slug that names no place. Refused
	// rather than dropped: a listing scoped to fewer places than the reader
	// asked for looks exactly like a listing.
	ErrUnknownPlace = errors.New("catalogue: no such place")
	// ErrBadCursor reports a cursor this listing did not issue.
	ErrBadCursor = errors.New("catalogue: the cursor is not one this listing issued")
	// ErrCategoryUnbuilt reports the one query parameter the contract names
	// and nothing implements. Refused rather than ignored, because a filter
	// that is silently ignored is a page of the wrong shops with no error
	// on it (#414).
	ErrCategoryUnbuilt = errors.New("catalogue: category has nothing behind it yet (#414)")
)

// languageSubtag is what a reader's language must look like: a BCP-47
// primary subtag, two or three letters, after normalisation. Nothing here
// knows which subtags exist; what it refuses is a value that could not be
// one, so a typo answers 400 rather than a shelf named in the fallback.
var languageSubtag = regexp.MustCompile(`^[a-z]{2,3}$`)

// ListQuery is what a reader asks for.
type ListQuery struct {
	// Language is the reader's, as a BCP-47 primary subtag. Required.
	Language string
	// Places is the slugs of the places the reader follows. At least one,
	// each of which must exist.
	Places []string
	// Search narrows the page to retailers whose resolved name contains
	// it, case-insensitively. Empty means every retailer.
	Search string
	// Limit is the page size; zero means DefaultListLimit, more than
	// MaxListLimit means MaxListLimit.
	Limit int
	// Cursor is where the previous page left off, as its NextCursor said;
	// empty for the first page.
	Cursor string
}

// Item is one retailer on the shelf: what browsing shows, plus the bands
// published for them at the moment the page was read.
type Item struct {
	Listing
	Bands []Band
}

// Page is one page of the listing.
type Page struct {
	Items []Item
	// NextCursor is where the next page starts, or "" when this was the
	// last one.
	NextCursor string
}

// ListStore is the four reads a listing makes. CopyForMerchants is
// browse's and the bands read is the shop page's, widened to a page of
// retailers, so the shelf and the page cannot disagree about either.
type ListStore interface {
	PlacesBySlugs(ctx context.Context, slugs []string) ([]store.PlacesBySlugsRow, error)
	MerchantsForPlaces(ctx context.Context, placeIDs []pgtype.UUID) ([]store.CashbackMerchant, error)
	CopyForMerchants(ctx context.Context, merchantIDs []pgtype.UUID) ([]store.CashbackMerchantCopy, error)
	PublishedBandsForMerchants(ctx context.Context, arg store.PublishedBandsForMerchantsParams) ([]store.PublishedBandsForMerchantsRow, error)
}

// Lister lists the catalogue a page at a time.
type Lister struct {
	store ListStore
}

// NewLister builds the reader the composition root wires, over
// store.New(pool) in production and a stub in tests.
func NewLister(s ListStore) (*Lister, error) {
	if s == nil {
		return nil, errors.New("catalogue: a listing needs somewhere to read retailers from")
	}
	return &Lister{store: s}, nil
}

// List answers one page of what a reader may see at the moment at.
//
// The order is by slug, in byte order, decided HERE rather than by the
// query: the cursor is a slug, and a keyset that the database ordered under
// one collation and Go compared under another would skip or repeat a
// retailer at every page edge that a hyphen sits on. Slugs are lower-case
// letters, digits and hyphens by constraint, so byte order is a reasonable
// order to read them in and the only one two systems agree on.
//
// A retailer with no copy in any language is left out, as browsing leaves
// it out, and the search applies to the name the reader would see - the
// resolved one, fallback included - so a Greek reader searching for a
// retailer shown to them in German finds it under the name on their screen.
func (l *Lister) List(ctx context.Context, q ListQuery, at time.Time) (Page, error) {
	language := normaliseLanguage(q.Language)
	if !languageSubtag.MatchString(language) {
		return Page{}, fmt.Errorf("%w: got %q", ErrNoLanguage, q.Language)
	}
	slugs := distinctSlugs(q.Places)
	if len(slugs) == 0 {
		return Page{}, ErrNoPlace
	}
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = DefaultListLimit
	case limit > MaxListLimit:
		limit = MaxListLimit
	}
	after, err := decodeListCursor(q.Cursor)
	if err != nil {
		return Page{}, err
	}

	places, err := l.placeIDs(ctx, slugs)
	if err != nil {
		return Page{}, err
	}
	merchants, err := l.store.MerchantsForPlaces(ctx, places)
	if err != nil {
		return Page{}, fmt.Errorf("catalogue: reading the retailers for %d place(s): %w", len(places), err)
	}
	slices.SortFunc(merchants, func(a, b store.CashbackMerchant) int { return strings.Compare(a.Slug, b.Slug) })
	candidates := merchants[:0:0]
	for _, m := range merchants {
		if m.Slug > after {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 0 {
		return Page{Items: []Item{}}, nil
	}

	ids := make([]pgtype.UUID, 0, len(candidates))
	for _, m := range candidates {
		ids = append(ids, m.ID)
	}
	copyRows, err := l.store.CopyForMerchants(ctx, ids)
	if err != nil {
		return Page{}, fmt.Errorf("catalogue: reading the copy for %d retailers: %w", len(ids), err)
	}
	available := groupCopy(copyRows)
	search := strings.ToLower(strings.TrimSpace(q.Search))

	items := make([]Item, 0, limit+1)
	for _, m := range candidates {
		id := uuid.UUID(m.ID.Bytes)
		described, err := Resolve(language, m.SourceLanguageCode, available[id])
		if err != nil {
			// Left out, not listed blank - browse's rule, for browse's reason.
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(described.Name), search) {
			continue
		}
		items = append(items, Item{Listing: Listing{ID: id, Slug: m.Slug, Country: m.Country.String, Copy: described}})
		if len(items) > limit {
			break
		}
	}
	page := Page{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = encodeListCursor(items[limit-1].Slug)
	}

	if err := l.attachBands(ctx, page.Items, at); err != nil {
		return Page{}, err
	}
	return page, nil
}

// placeIDs resolves slugs to places, refusing any that names none.
func (l *Lister) placeIDs(ctx context.Context, slugs []string) ([]pgtype.UUID, error) {
	rows, err := l.store.PlacesBySlugs(ctx, slugs)
	if err != nil {
		return nil, fmt.Errorf("catalogue: resolving %d place(s): %w", len(slugs), err)
	}
	found := make(map[string]pgtype.UUID, len(rows))
	for _, row := range rows {
		found[row.Slug] = row.ID
	}
	ids := make([]pgtype.UUID, 0, len(slugs))
	for _, slug := range slugs {
		id, ok := found[slug]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownPlace, slug)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// attachBands reads the published bands for one page of retailers and hands
// each its own, in the shop page's order. A retailer with none keeps an
// empty list, which is an ordinary answer: a shop whose rates have lapsed
// is still a shop that exists.
func (l *Lister) attachBands(ctx context.Context, items []Item, at time.Time) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]pgtype.UUID, 0, len(items))
	for _, item := range items {
		ids = append(ids, pgtype.UUID{Bytes: item.ID, Valid: true})
	}
	rows, err := l.store.PublishedBandsForMerchants(ctx, store.PublishedBandsForMerchantsParams{
		MerchantIds: ids,
		At:          pgtype.Timestamptz{Time: at, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("catalogue: reading the rates for %d retailers: %w", len(ids), err)
	}
	byMerchant := make(map[uuid.UUID][]store.PublishedBandsRow, len(items))
	for _, row := range rows {
		byMerchant[uuid.UUID(row.MerchantID.Bytes)] = append(byMerchant[uuid.UUID(row.MerchantID.Bytes)], store.PublishedBandsRow{
			ID:             row.ID,
			RateKind:       row.RateKind,
			RateBps:        row.RateBps,
			RateFixedMinor: row.RateFixedMinor,
			Currency:       row.Currency,
			MemberShareBps: row.MemberShareBps,
			Conditions:     row.Conditions,
			Exclusions:     row.Exclusions,
			ValidFrom:      row.ValidFrom,
			ValidTo:        row.ValidTo,
		})
	}
	for i := range items {
		bands, err := bandsFromRows(byMerchant[items[i].ID])
		if err != nil {
			return err
		}
		items[i].Bands = bands
	}
	return nil
}

// distinctSlugs trims, drops empties and de-duplicates, keeping the
// reader's order so a refusal names the first unknown they typed.
func distinctSlugs(raw []string) []string {
	seen := make(map[string]bool, len(raw))
	slugs := make([]string, 0, len(raw))
	for _, one := range raw {
		slug := strings.TrimSpace(one)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		slugs = append(slugs, slug)
	}
	return slugs
}

// encodeListCursor renders a position - the slug of the last retailer on a
// page - as the contract's opaque cursor: base64url of "<tag>|<slug>".
// Clients only ever echo back what next_cursor gave them.
func encodeListCursor(slug string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(listCursorTag + listCursorSeparator + slug))
}

// decodeListCursor reads a cursor encodeListCursor produced, reporting
// ErrBadCursor for anything else. Empty is the first page, not an error.
func decodeListCursor(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if len(raw) > maxListCursorBytes {
		return "", ErrBadCursor
	}
	blob, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", ErrBadCursor
	}
	tag, slug, ok := strings.Cut(string(blob), listCursorSeparator)
	if !ok || tag != listCursorTag || slug == "" {
		return "", ErrBadCursor
	}
	return slug, nil
}
