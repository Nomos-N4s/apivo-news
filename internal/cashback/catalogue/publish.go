// Publishing a rate band on a route an operator names (#537).
//
// This is the one write path onto cashback.offer that is not the fixture
// seed. An imported route is a retailer with nothing to click: the catalogue
// import (T105) writes merchants and routes and deliberately leaves the
// network's commission structure in the raw payload, because a published
// rate is a promise to a member and the structure a network publishes -
// sale, lead and click categories, percent or flat, tiers - does not reduce
// to one number without a person deciding. This is where that person's
// decision becomes a row.
//
// What it refuses, it refuses BEFORE the row exists, because every refusal
// here is otherwise discovered at a member's first click: a band on a route
// no click could be issued against answers 409, and a band whose template
// the network cannot build a redirect from answers 502 - both to somebody
// who was promised money and is now looking at an error page.

package catalogue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// DefaultMemberShare is the share of the commission a band promises the
// member when the operator names none: 60%, founder decision Q4 of
// 2026-09-06 (specs/002-apivo-cashback-alpha/spec.md). A default rather
// than a fixed value, because the same decision allows it to vary per
// merchant; a constant rather than configuration, because a share is part
// of the band and is snapshotted with it at click (FR-013), not a property
// of the deployment.
const DefaultMemberShare money.BasisPoints = 6000

var (
	// ErrPublicationRefused reports a request that does not describe a band
	// the schema could hold: no retailer, a rate of the wrong shape, a share
	// above the whole, a window that closes before it opens.
	ErrPublicationRefused = errors.New("catalogue: the band cannot be published as described")
	// ErrRouteUnknown reports that no route at the network carries the
	// merchant id named. The import is what makes routes; a band cannot.
	ErrRouteUnknown = errors.New("catalogue: no route at this network carries that merchant id")
	// ErrRouteNotLive reports a route no click could be issued against: the
	// route, the retailer or the network is off. A band there is a row
	// GetLiveOffer would never return.
	ErrRouteNotLive = errors.New("catalogue: no click could ever be issued against a band on this route")
	// ErrBandInForce reports that a band identical to the one requested is
	// already in force on the route at the moment the new one would open.
	ErrBandInForce = errors.New("catalogue: an identical band is already in force on this route")
	// ErrNothingToReplace reports a --replaces that names no band open on
	// this route at the moment the new one opens.
	ErrNothingToReplace = errors.New("catalogue: the band to replace is not open on this route at that moment")
	// ErrTemplateUnusable reports a deeplink template the network adapter
	// refused to build a redirect from. It wraps the adapter's own refusal,
	// which names what is wrong with the template.
	ErrTemplateUnusable = errors.New("catalogue: the network cannot build a redirect from this deeplink template")
	// ErrNoRedirectBuilder reports a publisher built without the adapter
	// whose redirects the band's template has to satisfy.
	ErrNoRedirectBuilder = errors.New("catalogue: publishing needs the network adapter that will build the redirects")
)

// RedirectBuilder is the one thing the publisher needs of a network
// adapter: to try to build a redirect from the template, exactly as the
// click-out will. Named here per the boundary rules; every
// [networks.Network] satisfies it.
type RedirectBuilder interface {
	BuildDeeplink(ctx context.Context, target networks.DeeplinkTarget, ref networks.IssuedClickRef) (string, error)
}

// Publication is one band as an operator describes it.
type Publication struct {
	// Network is the network whose route the band is published on.
	Network networks.NetworkID
	// MerchantExternalID is the network's own id for the retailer, as the
	// import stored it on the route (merchant_network.external_merchant_id).
	MerchantExternalID string
	// Rate is the commission the network pays: percent of the sale in basis
	// points, or a fixed amount with its currency.
	Rate RateBand
	// MemberShare is the member's share of that commission, in basis
	// points. Zero means [DefaultMemberShare].
	MemberShare money.BasisPoints
	// Conditions and Exclusions are the band's published terms (FR-011).
	// Empty means none.
	Conditions string
	Exclusions string
	// ValidFrom is when the band opens; the zero time means now.
	ValidFrom time.Time
	// ValidTo is when it closes; the zero time means open-ended.
	ValidTo time.Time
	// DeeplinkTemplate is what the redirect is built from: the network's
	// tracking URL for this retailer, with the click reference left out.
	DeeplinkTemplate string
	// Replaces, when set, is a band on the same route to close at ValidFrom.
	Replaces uuid.UUID
}

// PublishedBand is one band on a route, as the report lists them.
type PublishedBand struct {
	ID          uuid.UUID
	Rate        RateBand
	MemberShare money.BasisPoints
	Conditions  string
	Exclusions  string
	ValidFrom   time.Time
	ValidTo     time.Time
}

// Published is what a publication produced.
type Published struct {
	// OfferID is the new band: the value POST /clickouts is called with.
	OfferID uuid.UUID
	// RouteID is the route it was published on.
	RouteID uuid.UUID
	// MerchantSlug is the retailer's address on the member surfaces.
	MerchantSlug string
	// Preferred says whether this route is the one the merchant page lists
	// bands from. A band on another route is clickable by id and invisible
	// on the page (PublishedBands), which an operator has to be told.
	Preferred bool
	// ValidFrom and ValidTo are the window as written, ValidFrom resolved
	// from "now" if it was.
	ValidFrom time.Time
	ValidTo   time.Time
	// Replaced is the band closed at ValidFrom, or uuid.Nil.
	Replaced uuid.UUID
	// InForce is every band on the route in force at ValidFrom after the
	// write, the new one included. Read back rather than assumed, so the
	// report shows what the route carries and not what was intended.
	InForce []PublishedBand
}

// Publisher writes bands. It holds the adapter so that every template is
// tried against the code that will use it before the row exists.
type Publisher struct {
	redirects RedirectBuilder
	now       func() time.Time
}

// PublisherOption configures a [Publisher].
type PublisherOption func(*Publisher)

// WithPublisherClock replaces the clock ValidFrom defaults to.
func WithPublisherClock(now func() time.Time) PublisherOption {
	return func(p *Publisher) {
		if now != nil {
			p.now = now
		}
	}
}

// NewPublisher builds a publisher over the adapter that will build the
// redirects of every band it publishes.
func NewPublisher(redirects RedirectBuilder, opts ...PublisherOption) (*Publisher, error) {
	if redirects == nil {
		return nil, ErrNoRedirectBuilder
	}
	p := &Publisher{redirects: redirects, now: time.Now}
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	return p, nil
}

// probeClickRef is the reference the template is tried with. Never stored,
// never issued: it exists so BuildDeeplink can be asked the question the
// click-out will ask, with an input the click table would accept.
const probeClickRef = "publish-probe-000000000000000000"

// Publish writes the band inside tx, refusing everything it can refuse
// first. The caller owns the transaction, so a refusal after a --replaces
// has closed a band rolls that back too: a rate change is one command and
// either both halves happen or neither.
func (p *Publisher) Publish(ctx context.Context, tx pgx.Tx, req Publication) (Published, error) {
	req, err := p.resolve(req)
	if err != nil {
		return Published{}, err
	}
	queries := store.New(tx)

	route, err := queries.RouteForPublishing(ctx, store.RouteForPublishingParams{
		NetworkID:          req.Network.String(),
		ExternalMerchantID: req.MerchantExternalID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Published{}, fmt.Errorf("%w: %s at %s - the catalogue import is what makes routes, so run it and look for the retailer there first",
			ErrRouteUnknown, strconv.Quote(req.MerchantExternalID), strconv.Quote(req.Network.String()))
	case err != nil:
		return Published{}, fmt.Errorf("catalogue: finding the route for %s at %s: %w",
			strconv.Quote(req.MerchantExternalID), strconv.Quote(req.Network.String()), err)
	}
	if err := refuseDeadRoute(req, route); err != nil {
		return Published{}, err
	}
	if err := p.tryTemplate(ctx, req, route.ClickRefParam); err != nil {
		return Published{}, err
	}

	routeID := uuid.UUID(route.ID.Bytes)
	at := pgtype.Timestamptz{Time: req.ValidFrom, Valid: true}
	inForce, err := bandsInForce(ctx, queries, routeID, at)
	if err != nil {
		return Published{}, err
	}
	for _, band := range inForce {
		if band.ID != req.Replaces && sameBand(band, req) {
			return Published{}, fmt.Errorf("%w: offer %s, in force since %s",
				ErrBandInForce, band.ID, band.ValidFrom.UTC().Format(time.RFC3339))
		}
	}

	if req.Replaces != uuid.Nil {
		closed, err := queries.CloseOffer(ctx, store.CloseOfferParams{
			At:      at,
			ID:      pgtype.UUID{Bytes: req.Replaces, Valid: true},
			RouteID: route.ID,
		})
		if err != nil {
			return Published{}, fmt.Errorf("catalogue: closing offer %s: %w", req.Replaces, err)
		}
		if closed == 0 {
			return Published{}, fmt.Errorf("%w: offer %s at %s - it is on another route, already closed, or opens at or after that moment",
				ErrNothingToReplace, req.Replaces, req.ValidFrom.UTC().Format(time.RFC3339))
		}
	}

	id, err := queries.InsertOffer(ctx, insertParams(req, route.ID))
	if err != nil {
		return Published{}, fmt.Errorf("catalogue: publishing the band on %s: %w", strconv.Quote(route.MerchantSlug), err)
	}

	// Read back rather than appended to the earlier list: what the route
	// carries now is a fact of the database, not a sum this code keeps.
	inForce, err = bandsInForce(ctx, queries, routeID, at)
	if err != nil {
		return Published{}, err
	}
	return Published{
		OfferID:      uuid.UUID(id.Bytes),
		RouteID:      routeID,
		MerchantSlug: route.MerchantSlug,
		Preferred:    route.Preferred,
		ValidFrom:    req.ValidFrom,
		ValidTo:      req.ValidTo,
		Replaced:     req.Replaces,
		InForce:      inForce,
	}, nil
}

// resolve fills the defaults and refuses a request that does not describe
// a band the schema could hold. Every refusal wraps [ErrPublicationRefused]
// and says what to change.
func (p *Publisher) resolve(req Publication) (Publication, error) {
	if err := req.Network.Validate(); err != nil {
		return req, fmt.Errorf("%w: %w", ErrPublicationRefused, err)
	}
	req.MerchantExternalID = strings.TrimSpace(req.MerchantExternalID)
	if req.MerchantExternalID == "" {
		return req, fmt.Errorf("%w: no merchant id names the retailer", ErrPublicationRefused)
	}
	if err := validateRate(req.Rate); err != nil {
		return req, err
	}
	if req.MemberShare == 0 {
		req.MemberShare = DefaultMemberShare
	}
	if !req.MemberShare.Valid() {
		return req, fmt.Errorf("%w: a member share of %d bps is outside 0..%d",
			ErrPublicationRefused, req.MemberShare, money.BasisPointsScale)
	}
	if req.ValidFrom.IsZero() {
		req.ValidFrom = p.now()
	}
	req.ValidFrom = req.ValidFrom.UTC()
	if !req.ValidTo.IsZero() {
		req.ValidTo = req.ValidTo.UTC()
		if !req.ValidTo.After(req.ValidFrom) {
			return req, fmt.Errorf("%w: the band would close at %s, which is not after it opens at %s",
				ErrPublicationRefused, req.ValidTo.Format(time.RFC3339), req.ValidFrom.Format(time.RFC3339))
		}
	}
	req.Conditions = strings.TrimSpace(req.Conditions)
	req.Exclusions = strings.TrimSpace(req.Exclusions)
	req.DeeplinkTemplate = strings.TrimSpace(req.DeeplinkTemplate)
	if req.DeeplinkTemplate == "" {
		return req, fmt.Errorf("%w: no deeplink template says where a click goes", ErrPublicationRefused)
	}
	return req, nil
}

// validateRate mirrors offer_rate_kind_fields (0011): exactly the fields
// for the kind, and a rate that pays something.
func validateRate(rate RateBand) error {
	switch rate.Kind {
	case RatePercent:
		if !rate.Fixed.IsZero() || rate.Fixed.Currency != "" {
			return fmt.Errorf("%w: a percent band carries no fixed amount", ErrPublicationRefused)
		}
		if !rate.Percent.Valid() {
			return fmt.Errorf("%w: a rate of %d bps is outside 0..%d", ErrPublicationRefused, rate.Percent, money.BasisPointsScale)
		}
		if rate.Percent == 0 {
			return fmt.Errorf("%w: a rate of 0 bps pays nothing, which is not a rate to publish", ErrPublicationRefused)
		}
	case RateFixed:
		if rate.Percent != 0 {
			return fmt.Errorf("%w: a fixed band carries no percentage", ErrPublicationRefused)
		}
		if err := rate.Fixed.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrPublicationRefused, err)
		}
		if !rate.Fixed.IsPositive() {
			return fmt.Errorf("%w: a fixed amount of %s pays nothing, which is not a rate to publish", ErrPublicationRefused, rate.Fixed)
		}
	default:
		return fmt.Errorf("%w: the rate is neither %s nor %s", ErrPublicationRefused, RatePercent, RateFixed)
	}
	return nil
}

// refuseDeadRoute is GetLiveOffer's three legs, asked before the row
// exists. Which leg failed IS distinguished here, unlike there: this is the
// operator who can fix it, and "the route is paused" sends them to the
// import's reason while "the network is off" sends them to connect-network.
func refuseDeadRoute(req Publication, route store.RouteForPublishingRow) error {
	switch {
	case route.Status != "active":
		return fmt.Errorf("%w: the route to %s at %s is %s - the import sets that from the programme's own status and terms (a programme that forbids cashback sites or deeplinking is paused), so look at the route's raw_payload rather than at this band",
			ErrRouteNotLive, strconv.Quote(req.MerchantExternalID), strconv.Quote(req.Network.String()), strconv.Quote(route.Status))
	case route.MerchantStatus != "active":
		return fmt.Errorf("%w: the retailer %s is %s, whatever its routes say",
			ErrRouteNotLive, strconv.Quote(route.MerchantSlug), strconv.Quote(route.MerchantStatus))
	case !route.NetworkActive:
		return fmt.Errorf("%w: the network %s is not active - connect-network without -inactive is what switches it on",
			ErrRouteNotLive, strconv.Quote(req.Network.String()))
	}
	return nil
}

// tryTemplate asks the adapter to build the redirect the click-out will
// build, with a reference the click table would accept. A template that
// fails here fails at every click; the adapter's own words say why.
func (p *Publisher) tryTemplate(ctx context.Context, req Publication, clickRefParam string) error {
	ref, err := networks.NewIssuedClickRef(probeClickRef)
	if err != nil {
		// The probe is a constant that satisfies the click table's rule; a
		// failure here is a change to that rule, not to this request.
		return fmt.Errorf("catalogue: the probe reference is no longer one the click table accepts: %w", err)
	}
	target := networks.DeeplinkTarget{
		// A throwaway id, so the adapter's refusals name something rather
		// than the zero uuid; the band does not exist yet.
		OfferID:       uuid.New(),
		NetworkID:     req.Network,
		ClickRefParam: clickRefParam,
		Template:      req.DeeplinkTemplate,
	}
	if _, err := p.redirects.BuildDeeplink(ctx, target, ref); err != nil {
		return fmt.Errorf("%w: %w", ErrTemplateUnusable, err)
	}
	return nil
}

// bandsInForce reads the route's bands at one moment into the report's
// shape, through the same mappers every other band read uses.
func bandsInForce(ctx context.Context, queries *store.Queries, routeID uuid.UUID, at pgtype.Timestamptz) ([]PublishedBand, error) {
	rows, err := queries.BandsInForceOnRoute(ctx, store.BandsInForceOnRouteParams{
		RouteID: pgtype.UUID{Bytes: routeID, Valid: true},
		At:      at,
	})
	if err != nil {
		return nil, fmt.Errorf("catalogue: reading the bands on route %s: %w", routeID, err)
	}
	bands := make([]PublishedBand, 0, len(rows))
	for _, row := range rows {
		id := uuid.UUID(row.ID.Bytes)
		rate, err := bandFromRow(id, rateRow{Kind: row.RateKind, Bps: row.RateBps, FixedMinor: row.RateFixedMinor, Currency: row.Currency})
		if err != nil {
			return nil, err
		}
		share, err := shareFromRow(id, row.MemberShareBps)
		if err != nil {
			return nil, err
		}
		from, err := bandStart(id, row.ValidFrom)
		if err != nil {
			return nil, err
		}
		to, err := bandEnd(id, row.ValidTo)
		if err != nil {
			return nil, err
		}
		bands = append(bands, PublishedBand{
			ID:          id,
			Rate:        rate,
			MemberShare: share,
			Conditions:  row.Conditions.String,
			Exclusions:  row.Exclusions.String,
			ValidFrom:   from,
			ValidTo:     to,
		})
	}
	return bands, nil
}

// sameBand says whether a band already in force is the one being asked for:
// the same rate, share and terms. Validity is deliberately not compared -
// the question is whether publishing would put the same promise on the
// route twice, and a second copy with a different end date is still that.
// Nor is the template: a new tracking URL for the same promise is what
// --replaces is for, and two live bands quoting one rate is what a member
// sees as one rate listed twice.
func sameBand(band PublishedBand, req Publication) bool {
	return band.Rate == req.Rate &&
		band.MemberShare == req.MemberShare &&
		band.Conditions == req.Conditions &&
		band.Exclusions == req.Exclusions
}

// insertParams renders the request in the schema's own shapes: exactly the
// fields for the rate kind, nulls for absent terms and an open end.
func insertParams(req Publication, routeID pgtype.UUID) store.InsertOfferParams {
	params := store.InsertOfferParams{
		RouteID:          routeID,
		RateKind:         string(req.Rate.Kind),
		MemberShareBps:   int32(req.MemberShare),
		Conditions:       pgtype.Text{String: req.Conditions, Valid: req.Conditions != ""},
		Exclusions:       pgtype.Text{String: req.Exclusions, Valid: req.Exclusions != ""},
		ValidFrom:        pgtype.Timestamptz{Time: req.ValidFrom, Valid: true},
		ValidTo:          pgtype.Timestamptz{Time: req.ValidTo, Valid: !req.ValidTo.IsZero()},
		DeeplinkTemplate: req.DeeplinkTemplate,
	}
	switch req.Rate.Kind {
	case RatePercent:
		params.RateBps = pgtype.Int4{Int32: int32(req.Rate.Percent), Valid: true}
	case RateFixed:
		params.RateFixedMinor = pgtype.Int8{Int64: req.Rate.Fixed.Minor, Valid: true}
		params.Currency = pgtype.Text{String: string(req.Rate.Fixed.Currency), Valid: true}
	}
	return params
}
