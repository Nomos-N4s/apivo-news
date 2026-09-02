// Which merchants a reader in one place may see, described in one language
// (T102, US5 scenario 1, FR-010).
//
// Two reads, not one join, because place and language are INDEPENDENT axes
// (constitution VII). Two readers in Munich, one reading Greek and one
// German, see the same merchants described differently; folding the scope
// and the copy into one query would make that independence accidental
// rather than structural, and would answer "no merchant" and "no Greek
// copy" with the same empty result.

package catalogue

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
)

// ErrNoPlace reports a browse with no place to scope it to.
//
// Refused rather than defaulted: the alternative to a place is every
// merchant everywhere, which is the answer a member is least likely to want
// and the one they cannot tell apart from a working catalogue.
var ErrNoPlace = errors.New("catalogue: browsing needs a place to scope to")

// Listing is one merchant as a reader browsing a place sees it.
type Listing struct {
	ID   uuid.UUID
	Slug string
	// Country is where the merchant trades, which is not where the reader
	// is: a Munich reader may see a merchant whose country is DE through
	// the place tree, and the two facts answer different questions.
	Country string
	// Copy is the description in the reader's language, or the source
	// language labelled as a fallback (T101).
	Copy Copy
}

// BrowseStore is the two reads this file makes.
type BrowseStore interface {
	MerchantsForPlace(ctx context.Context, placeID pgtype.UUID) ([]store.CashbackMerchant, error)
	CopyForMerchants(ctx context.Context, merchantIDs []pgtype.UUID) ([]store.CashbackMerchantCopy, error)
}

// Browser lists the merchants available in a place.
type Browser struct {
	store BrowseStore
}

// NewBrowser builds the reader the composition root wires, over
// store.New(pool) in production and a stub in tests.
func NewBrowser(s BrowseStore) (*Browser, error) {
	if s == nil {
		return nil, errors.New("catalogue: browsing needs somewhere to read merchants from")
	}
	return &Browser{store: s}, nil
}

// Browse lists what a reader in place, reading language, may see.
//
// A merchant with no copy in ANY language is left out rather than listed
// blank, and that is the resolver's rule (T101) applied here rather than
// re-decided: a card with no name on it tells a member nothing except that
// something is broken. It is a silent omission on purpose - a member
// browsing is not the person who can fix it, and an error would take the
// whole page down over one bad row.
//
// An empty result is an ordinary answer. A place with no merchants attached
// to it or its ancestors is a place this catalogue does not cover yet, which
// is a true thing to say and not a failure.
func (b *Browser) Browse(ctx context.Context, place uuid.UUID, language string) ([]Listing, error) {
	if place == uuid.Nil {
		return nil, ErrNoPlace
	}

	merchants, err := b.store.MerchantsForPlace(ctx, pgtype.UUID{Bytes: place, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("catalogue: reading the merchants for place %s: %w", place, err)
	}
	if len(merchants) == 0 {
		return nil, nil
	}

	ids := make([]pgtype.UUID, 0, len(merchants))
	for _, m := range merchants {
		ids = append(ids, m.ID)
	}
	rows, err := b.store.CopyForMerchants(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("catalogue: reading the copy for %d merchants: %w", len(merchants), err)
	}
	available := groupCopy(rows)

	listings := make([]Listing, 0, len(merchants))
	for _, m := range merchants {
		id := uuid.UUID(m.ID.Bytes)
		described, err := Resolve(language, m.SourceLanguageCode, available[id])
		if err != nil {
			// Left out, not listed blank. See the doc comment.
			continue
		}
		listings = append(listings, Listing{
			ID:      id,
			Slug:    m.Slug,
			Country: m.Country.String,
			Copy:    described,
		})
	}
	return listings, nil
}

// groupCopy turns the flat rows into the per-merchant shape [Resolve] takes.
func groupCopy(rows []store.CashbackMerchantCopy) map[uuid.UUID][]Available {
	grouped := make(map[uuid.UUID][]Available)
	for _, row := range rows {
		id := uuid.UUID(row.MerchantID.Bytes)
		grouped[id] = append(grouped[id], Available{
			Language: row.LanguageCode,
			Name:     row.Name,
			Summary:  row.Summary.String,
			Terms:    row.Terms.String,
		})
	}
	return grouped
}
