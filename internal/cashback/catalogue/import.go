// Importing a network's catalogue (T105, FR-012): the code that turns what
// [networks.Network.FetchCatalogue] yields into retailers a member can click,
// and that notices when one has gone.
//
// One file, because the two halves cannot be separated safely. Writing a
// route and concluding a route has departed are the same operation seen from
// opposite ends - the second is what the first did NOT touch - and a reader
// who saw only one of them would not know why every write stamps a time.

package catalogue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// The sentinels an import is refused or abandoned with.
var (
	// ErrImportNotConfigured reports an importer built without something it
	// cannot invent: the brand that publishes the routes, or the language
	// the network's copy is supplied in. Refused at construction rather
	// than at the first retailer, so a misconfigured deployment fails
	// before it has written half a catalogue.
	ErrImportNotConfigured = errors.New("catalogue: the importer was not told enough to write a route")
	// ErrImportIncomplete reports a catalogue read that did not finish. It
	// wraps whatever the adapter reported, and it is the reason no route is
	// ever reconciled to left_network on that run: absence only means
	// departure in an answer that was whole (contract rule 8).
	ErrImportIncomplete = errors.New("catalogue: the catalogue was not read to the end, so nothing may be concluded from absence")
	// ErrRouteConflict reports two retailers the import cannot tell apart:
	// a fallback slug that is already taken by a merchant with a route at
	// this network. It is not reachable from consistent data - a fallback
	// slug is derived from the very pair the route table keys on - and
	// exists because the alternative is a constraint violation whose
	// message names an index rather than a retailer.
	ErrRouteConflict = errors.New("catalogue: the retailer cannot be told apart from one already imported")
)

// Importer writes one network's catalogue into the merchant tables.
//
// It holds no database handle. The handle is passed to [Importer.Run],
// because a caller may want every retailer in one transaction or each in its
// own, and both are safe: the reconciliation that marks absent routes as
// departed is gated on a complete read, not on a transaction boundary.
type Importer struct {
	// brandID is which brand publishes these routes (ADR-0004). Written
	// once per route, on insert, and never updated: an import that rewrote
	// it would move a retailer between tenants on a schedule.
	brandID string
	// sourceLanguage is the language the network supplies its copy in, as
	// an operator states it. It is configuration and not detection: no
	// network this port speaks to says what language a programme name is
	// in, and merchant_copy's whole design is that a fallback is LABELLED
	// rather than guessed - so the label has to come from somebody who
	// knows.
	sourceLanguage string
	// now is the clock, injectable so a test can pin the instant every
	// route in one run is stamped with.
	now func() time.Time
}

// NewImporter builds the importer, refusing one that could not write a row.
func NewImporter(brandID, sourceLanguage string, opts ...ImporterOption) (*Importer, error) {
	if brandID == "" {
		return nil, fmt.Errorf("%w: no brand publishes these routes", ErrImportNotConfigured)
	}
	if sourceLanguage == "" {
		return nil, fmt.Errorf("%w: nothing says what language the network supplies its copy in", ErrImportNotConfigured)
	}
	i := &Importer{brandID: brandID, sourceLanguage: sourceLanguage, now: time.Now}
	for _, opt := range opts {
		opt(i)
	}
	return i, nil
}

// ImporterOption adjusts an [Importer] at construction.
type ImporterOption func(*Importer)

// WithClock replaces the clock, so a test can pin the instant a run stamps.
func WithClock(now func() time.Time) ImporterOption {
	return func(i *Importer) {
		if now != nil {
			i.now = now
		}
	}
}

// ImportResult is what a run did, in the numbers an operator reads to decide
// whether it did something reasonable.
type ImportResult struct {
	// StartedAt is the instant every route this run touched is stamped
	// with, and the boundary Departed was computed against.
	StartedAt time.Time
	// Seen is how many retailers the network returned.
	Seen int
	// Created is how many of them we had never seen before.
	Created int
	// Departed is how many routes the network stopped listing. Worth
	// looking at before the next run: an import that reports thousands is
	// one where something changed at the network rather than in the world.
	Departed int64
}

// Run reads the whole catalogue and writes it, then reconciles what was not
// in it.
//
// The order is the contract. Every retailer the network returned is written
// first, each stamped with the instant the run began; only if the iteration
// ended with NO error at all does the run then mark every route at this
// network that was not stamped as departed. An import that stopped at
// retailer 400 of 5000 and reconciled anyway would withdraw 4600 live
// routes, stop publishing their offers, and empty the catalogue members see -
// from a run that reported nothing wrong.
func (i *Importer) Run(ctx context.Context, db store.DBTX, adapter networks.Network) (ImportResult, error) {
	if db == nil {
		return ImportResult{}, fmt.Errorf("%w: no database handle", ErrImportNotConfigured)
	}
	q := store.New(db)
	result := ImportResult{StartedAt: i.now().UTC()}
	networkID := adapter.ID().String()

	seq, err := adapter.FetchCatalogue(ctx)
	if err != nil {
		return result, fmt.Errorf("%w: %w", ErrImportIncomplete, err)
	}

	for merchant, err := range seq {
		if err != nil {
			// The adapter has already said whether this is the network
			// being unwell or an answer that made no sense; what is added
			// here is that nothing may be concluded from what is missing.
			return result, fmt.Errorf("%w: %w", ErrImportIncomplete, err)
		}
		created, err := i.writeRoute(ctx, q, networkID, result.StartedAt, merchant)
		if err != nil {
			return result, err
		}
		result.Seen++
		if created {
			result.Created++
		}
	}

	departed, err := q.MarkRoutesNotSeen(ctx, store.MarkRoutesNotSeenParams{
		NetworkID:       networkID,
		ImportStartedAt: pgtype.Timestamptz{Time: result.StartedAt, Valid: true},
	})
	if err != nil {
		return result, fmt.Errorf("catalogue: reconciling the routes %s did not return: %w",
			strconv.Quote(networkID), err)
	}
	result.Departed = departed
	return result, nil
}

// writeRoute writes one retailer and its route, reporting whether the
// retailer was new to us.
func (i *Importer) writeRoute(ctx context.Context, q *store.Queries, networkID string, at time.Time, m networks.ReportedMerchant) (bool, error) {
	merchantID, created, err := i.resolveMerchant(ctx, q, networkID, m)
	if err != nil {
		return false, err
	}

	if err := q.UpsertMerchantCopy(ctx, store.UpsertMerchantCopyParams{
		MerchantID:   merchantID,
		LanguageCode: i.sourceLanguage,
		Name:         m.Name,
	}); err != nil {
		return false, fmt.Errorf("catalogue: writing the name of retailer %s at %s: %w",
			strconv.Quote(m.ExternalID), strconv.Quote(networkID), err)
	}

	// A retailer's first route becomes the one the catalogue publishes, so
	// there is something to publish at all. After that arbitration is an
	// operator's: merchant_network_one_preferred is a partial unique index,
	// so a second preferred route is refused by the database rather than
	// silently accepted, and the upsert above never rewrites the flag.
	preferred, err := q.MerchantHasPreferredRoute(ctx, merchantID)
	if err != nil {
		return false, fmt.Errorf("catalogue: reading the preferred route of retailer %s: %w",
			strconv.Quote(m.ExternalID), err)
	}

	if _, err := q.UpsertRoute(ctx, store.UpsertRouteParams{
		MerchantID:         merchantID,
		NetworkID:          networkID,
		BrandID:            i.brandID,
		ExternalMerchantID: m.ExternalID,
		RetrievedAt:        pgtype.Timestamptz{Time: at, Valid: true},
		RawPayload:         m.RawPayload,
		Status:             m.Status.String(),
		Preferred:          !preferred,
	}); err != nil {
		return false, fmt.Errorf("catalogue: writing the route to retailer %s at %s: %w",
			strconv.Quote(m.ExternalID), strconv.Quote(networkID), err)
	}
	return created, nil
}

// resolveMerchant answers which retailer a reported route belongs to,
// creating one if it is new.
//
// The order of the questions is the whole of it, and the first one is the
// only identity that cannot drift: the network's own id. An advertiser who
// renames "Gartenhaus" to "Gartenhaus DE" between two imports would, under
// name matching, get a second merchant with its own history and its own
// offers while the first quietly went absent and was marked departed.
func (i *Importer) resolveMerchant(ctx context.Context, q *store.Queries, networkID string, m networks.ReportedMerchant) (pgtype.UUID, bool, error) {
	route, err := q.FindRouteByExternalID(ctx, store.FindRouteByExternalIDParams{
		NetworkID:          networkID,
		ExternalMerchantID: m.ExternalID,
	})
	switch {
	case err == nil:
		return route.MerchantID, false, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return pgtype.UUID{}, false, fmt.Errorf("catalogue: reading the route to retailer %s at %s: %w",
			strconv.Quote(m.ExternalID), strconv.Quote(networkID), err)
	}

	slug := Slug(m.Name)
	if slug == "" {
		// A name in a script with no Latin form. Not an error: refusing the
		// retailer would fail the whole import, and an import that fails is
		// one whose absent routes cannot be reconciled - so one Greek
		// retailer name would freeze the catalogue.
		if slug, err = FallbackSlug(networkID, m.ExternalID); err != nil {
			return pgtype.UUID{}, false, err
		}
	}
	return i.merchantForSlug(ctx, q, networkID, slug, m)
}

// merchantForSlug finds or creates the retailer a slug names, falling back
// when the slug turns out to belong to somebody else.
func (i *Importer) merchantForSlug(ctx context.Context, q *store.Queries, networkID, slug string, m networks.ReportedMerchant) (pgtype.UUID, bool, error) {
	existing, err := q.FindMerchantBySlug(ctx, store.FindMerchantBySlugParams{
		NetworkID: networkID,
		Slug:      slug,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return i.createMerchant(ctx, q, slug, m)
	case err != nil:
		return pgtype.UUID{}, false, fmt.Errorf("catalogue: reading the retailer %s: %w", strconv.Quote(slug), err)
	case !existing.HasRouteAtNetwork:
		// The same retailer, reached through a network we already import.
		// That is what merchant_network is for, and it is how a member gets
		// the better of two rates.
		return existing.ID, false, nil
	}

	// The slug belongs to a DIFFERENT retailer that is already on this
	// network: two programmes called "Fashion Store" are two businesses.
	fallback, err := FallbackSlug(networkID, m.ExternalID)
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	if fallback == slug {
		// Unreachable from consistent data: we only reach here when the
		// slug came from a name, and a fallback slug carries the network's
		// own id. Reported rather than looped on.
		return pgtype.UUID{}, false, fmt.Errorf("%w: retailer %s at %s already holds the slug %s",
			ErrRouteConflict, strconv.Quote(m.ExternalID), strconv.Quote(networkID), strconv.Quote(slug))
	}

	byFallback, err := q.FindMerchantBySlug(ctx, store.FindMerchantBySlugParams{
		NetworkID: networkID,
		Slug:      fallback,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return i.createMerchant(ctx, q, fallback, m)
	case err != nil:
		return pgtype.UUID{}, false, fmt.Errorf("catalogue: reading the retailer %s: %w", strconv.Quote(fallback), err)
	case byFallback.HasRouteAtNetwork:
		// A fallback slug carries the network's own id for this retailer,
		// so a merchant holding it with a route at this network would be
		// this very route - which the first lookup would have found.
		return pgtype.UUID{}, false, fmt.Errorf("%w: the slug %s is held by a retailer already routed at %s",
			ErrRouteConflict, strconv.Quote(fallback), strconv.Quote(networkID))
	}
	return byFallback.ID, false, nil
}

// createMerchant inserts a retailer we have not seen before.
func (i *Importer) createMerchant(ctx context.Context, q *store.Queries, slug string, m networks.ReportedMerchant) (pgtype.UUID, bool, error) {
	row, err := q.CreateMerchant(ctx, store.CreateMerchantParams{
		Slug:               slug,
		Country:            countryOrNull(m.Country),
		SourceLanguageCode: i.sourceLanguage,
	})
	if err != nil {
		return pgtype.UUID{}, false, fmt.Errorf("catalogue: creating the retailer %s: %w", strconv.Quote(slug), err)
	}
	return row.ID, true, nil
}

// countryOrNull spells an unbound retailer the way the column does. The port
// carries "bound to no single country" as the empty string and the schema
// carries it as a null, and the two must not collapse into a blank that the
// format check would refuse anyway.
func countryOrNull(country string) pgtype.Text {
	if country == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: country, Valid: true}
}
