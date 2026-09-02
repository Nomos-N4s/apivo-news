// One merchant's page: who they are, described in the reader's language,
// and every rate band we publish for them right now (T104, US5 scenario 3,
// FR-011).
//
// Three reads, and each is allowed to fail differently. Whether we publish
// this retailer at all is a 404. Whether they have a live rate band today
// is an ordinary answer that happens to be empty. Whether they have a name
// in any language is a broken row. Folding any two of those together is
// how a member gets told a shop does not exist because its rates lapsed
// overnight.

package catalogue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// ErrNoMerchant reports an address that names no retailer we publish:
// never imported, or imported and since paused. The two are deliberately
// one answer - which retailers we have stopped publishing, and why, is
// catalogue state a page request is not entitled to - and the contract maps
// this to its 404 (http-api.md, GET /merchants/{slug}).
var ErrNoMerchant = errors.New("catalogue: no such merchant")

// Band is one published rate as a member reading a merchant page sees it.
//
// Every band a retailer publishes becomes one of these, which is US5
// scenario 3: a rate that varies by what you buy is several bands at once,
// and each carries the conditions and exclusions that qualify IT. Rolling
// them into one page-level note is how a member ends up expecting 8% on an
// order that pays 2%.
type Band struct {
	// ID is the band's identity, the value POST /clickouts is called with.
	// It is the only thing on this page that turns into a click.
	ID uuid.UUID
	// Rate is what the MEMBER earns on this band: percent basis points or a
	// fixed amount with its currency, never both (C-6).
	//
	// Not the network's commission, and the commission is deliberately not
	// on this type at all. A published band records the commission and,
	// separately, the share of it a member receives; showing the first is
	// promising a member roughly twice what arrives. Composing them here
	// rather than at each surface is what makes that mistake
	// unrepresentable - and it keeps the margin, which is a commercial
	// fact, off a member-facing read entirely.
	Rate RateBand
	// Conditions is what the band requires, verbatim as published (FR-011);
	// empty when it records none.
	Conditions string
	// Exclusions is what the band does not pay on, verbatim as published
	// (FR-011); empty when it records none.
	Exclusions string
	// ValidFrom is when the band came into force, or the zero time for one
	// in force since always.
	ValidFrom time.Time
	// ValidTo is when it closes, or the zero time for one with no published
	// end. Both bounds are already checked against the asked-for moment;
	// they are here because "until the end of the month" is part of what a
	// member is being offered.
	ValidTo time.Time
}

// Merchant is a retailer's page.
//
// It carries no typical confirmation window. The contract names one
// (http-api.md, GET /merchants/{slug}) and the schema has nothing to
// compute it from - not on the retailer, not on the route, not on the
// network - so this type does not invent one. A number a member reads as
// "you will be paid in about six weeks" is not a field to fill in with a
// plausible constant.
type Merchant struct {
	// ID is the retailer's identity.
	ID uuid.UUID
	// Slug is the address this page was reached at.
	Slug string
	// Country is where the retailer trades, empty when they are bound to
	// none. Not where the reader is: those answer different questions
	// (constitution VII).
	Country string
	// Copy is the description in the reader's language, or the source
	// language labelled as a fallback (T101).
	Copy Copy
	// Bands is every rate in force at the moment the page was read, newest
	// first. Empty is an ordinary answer: a retailer we publish whose rates
	// have lapsed pays nothing today and is still a shop that exists.
	Bands []Band
}

// DetailStore is the three reads this file makes. CopyForMerchants is
// browse's own query, called with one merchant rather than many: the
// language rule a member sees while browsing is the one they see on the
// page they browsed to.
type DetailStore interface {
	MerchantBySlug(ctx context.Context, slug string) (store.CashbackMerchant, error)
	PublishedBands(ctx context.Context, arg store.PublishedBandsParams) ([]store.PublishedBandsRow, error)
	CopyForMerchants(ctx context.Context, merchantIDs []pgtype.UUID) ([]store.CashbackMerchantCopy, error)
}

// MerchantReader reads one retailer's page.
type MerchantReader struct {
	store DetailStore
}

// NewMerchantReader builds the reader the composition root wires, over
// store.New(pool) in production and a stub in tests.
func NewMerchantReader(s DetailStore) (*MerchantReader, error) {
	if s == nil {
		return nil, errors.New("catalogue: a merchant page needs somewhere to read it from")
	}
	return &MerchantReader{store: s}, nil
}

// Detail returns the retailer at slug as a reader of language sees them at
// the moment at, or an error wrapping [ErrNoMerchant] when we publish no
// such retailer.
//
// The moment is a parameter rather than now() for the reason GetLiveOffer
// takes one: one instant pins the whole page, instead of a read of the
// clock per band that can straddle an edge and render a page that
// contradicts itself.
//
// A retailer with no copy in ANY language fails with [ErrNoCopy] rather
// than being answered blank, and that is the opposite of what browsing does
// with the same row - browse leaves it out of the list. The difference is
// what the member asked for. One unnamed card among forty is a page that
// still works, and taking the whole list down over it would punish the
// thirty-nine; one unnamed page is the whole of what they asked for, and
// answering it with a nameless shell tells them nothing except that
// something is broken. An error says the same thing to somebody who can fix
// it.
func (r *MerchantReader) Detail(ctx context.Context, slug, language string, at time.Time) (Merchant, error) {
	row, err := r.store.MerchantBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		// The driver's vocabulary stops here, so no handler can couple its
		// 404 to the database driver by accident.
		return Merchant{}, fmt.Errorf("%w: %s", ErrNoMerchant, slug)
	}
	if err != nil {
		return Merchant{}, fmt.Errorf("catalogue: reading merchant %s: %w", slug, err)
	}
	if !row.ID.Valid {
		// An invalid pgtype.UUID converts to sixteen zero bytes without
		// complaint, and a page whose click-outs point at the zero merchant
		// is exactly the silent zero-fill this package refuses elsewhere.
		return Merchant{}, fmt.Errorf("%w %s: row carries an unset id", ErrMalformedOffer, slug)
	}
	id := uuid.UUID(row.ID.Bytes)

	copyRows, err := r.store.CopyForMerchants(ctx, []pgtype.UUID{row.ID})
	if err != nil {
		return Merchant{}, fmt.Errorf("catalogue: reading the copy for merchant %s: %w", slug, err)
	}
	described, err := Resolve(language, row.SourceLanguageCode, groupCopy(copyRows)[id])
	if err != nil {
		return Merchant{}, fmt.Errorf("catalogue: merchant %s: %w", slug, err)
	}

	bandRows, err := r.store.PublishedBands(ctx, store.PublishedBandsParams{
		MerchantID: row.ID,
		At:         pgtype.Timestamptz{Time: at, Valid: true},
	})
	if err != nil {
		return Merchant{}, fmt.Errorf("catalogue: reading the rates for merchant %s: %w", slug, err)
	}
	bands, err := bandsFromRows(bandRows)
	if err != nil {
		return Merchant{}, err
	}

	return Merchant{
		ID:      id,
		Slug:    row.Slug,
		Country: row.Country.String,
		Copy:    described,
		Bands:   bands,
	}, nil
}

// bandsFromRows maps the published bands, refusing the whole page if any
// one of them is malformed.
//
// The whole page, deliberately. Dropping the bad band and rendering the
// rest would publish a rate table that is silently missing a row, and a
// member comparing it against what they were paid has no way to tell that
// from a band that was never published. The schema already forbids every
// such row (0011, offer_rate_kind_fields); one arriving here means the
// schema and this mapping disagree, which is a fault to find rather than a
// page to half-draw.
func bandsFromRows(rows []store.PublishedBandsRow) ([]Band, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	bands := make([]Band, 0, len(rows))
	for _, row := range rows {
		if !row.ID.Valid {
			return nil, fmt.Errorf("%w: band row carries an unset id", ErrMalformedOffer)
		}
		id := uuid.UUID(row.ID.Bytes)

		rate, err := bandFromRow(id, rateRow{
			Kind:       row.RateKind,
			Bps:        row.RateBps,
			FixedMinor: row.RateFixedMinor,
			Currency:   row.Currency,
		})
		if err != nil {
			return nil, err
		}
		earned, err := rate.Earned(money.BasisPoints(row.MemberShareBps), MemberFavour)
		if err != nil {
			return nil, fmt.Errorf("%w %s: %w", ErrMalformedOffer, id, err)
		}
		validFrom, err := bandStart(id, row.ValidFrom)
		if err != nil {
			return nil, err
		}
		validTo, err := bandEnd(id, row.ValidTo)
		if err != nil {
			return nil, err
		}

		bands = append(bands, Band{
			ID:         id,
			Rate:       earned,
			Conditions: row.Conditions.String,
			Exclusions: row.Exclusions.String,
			ValidFrom:  validFrom,
			ValidTo:    validTo,
		})
	}
	return bands, nil
}

// MemberFavour is the direction a member's share rounds when a rate does not
// divide evenly, and it is exported because it is part of what this
// catalogue promises rather than an implementation detail of it.
//
// It is earnings.MemberFavour, the constant the money path itself uses, and
// it is repeated here rather than imported because a member-facing read has
// no business depending on the earnings module. A test holds the two equal,
// so the number a member is shown and the number they are later credited
// round the same way and any difference between them is about the
// commission the network actually reported - which is a real difference
// worth seeing - rather than about arithmetic.
const MemberFavour = money.RoundCeil
