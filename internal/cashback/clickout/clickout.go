// Issuing a tracked redirect: the order the whole flow turns on (T064,
// FR-013, FR-020, FR-021).

package clickout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

var (
	// ErrNoClickOuts reports a service built without one of its parts.
	ErrNoClickOuts = errors.New("clickout: issuing a redirect needs an offer reader, a recorder and a deeplink builder")
	// ErrOfferNotAvailable reports an offer that is not published at this
	// moment: expired, not yet started, or with an inactive leg in its
	// chain. The endpoint answers 409 - the member is looking at a stale
	// page, not at a broken system.
	ErrOfferNotAvailable = errors.New("clickout: the offer is not available")
	// ErrNoRedirect reports that no redirect could be built, so no click was
	// recorded. Both halves matter: a half-formed URL still redirects, and a
	// member who reaches the retailer without a recorded click buys and is
	// never credited.
	ErrNoRedirect = errors.New("clickout: no redirect could be built, so no click was recorded")
)

// Offers is the catalogue read this module needs, named here per the
// boundary rules. *catalogue.OfferReader satisfies it.
//
// The moment is a parameter rather than the reader's own now(), which is
// what lets one instant pin the liveness check, the rate snapshot and the
// click together instead of three reads of the clock that can straddle a
// band's edge.
type Offers interface {
	LiveOffer(ctx context.Context, id uuid.UUID, at time.Time) (catalogue.Offer, error)
}

// Deeplinks builds the redirect for a target, using the adapter for the
// network that target names.
//
// One method rather than a registry lookup and a call, because the target
// already carries the network: which adapter serves it is the
// implementation's business, and a caller that had to choose could choose
// wrong - a click assembled from one network's template and recognised by
// neither (see [networks.ValidateDeeplinkInputs]).
type Deeplinks interface {
	Build(ctx context.Context, target networks.DeeplinkTarget, ref networks.IssuedClickRef) (string, error)
}

// Issued is a tracked redirect: the click that was recorded, where to send
// the member, and when the band they clicked stops being published.
type Issued struct {
	// Click is the row that was written, read back.
	Click Click
	// RedirectURL is the absolute URL to send the member to.
	RedirectURL string
	// OfferEndsAt is the band's published end, or the zero time for a band
	// with no published end. It is about the OFFER: the credit this click
	// earns is governed by the snapshot on the click whatever happens to the
	// band afterwards (FR-013).
	OfferEndsAt time.Time
}

// ClickOuts issues tracked redirects. Build it with [NewClickOuts].
type ClickOuts struct {
	offers    Offers
	clicks    *Clicks
	minter    *Minter
	deeplinks Deeplinks
	now       func() time.Time
}

// Option configures a [ClickOuts].
type Option func(*ClickOuts)

// WithClock replaces the clock the liveness check and the snapshot are
// pinned to. For tests; production reads the wall clock.
func WithClock(now func() time.Time) Option {
	return func(c *ClickOuts) { c.now = now }
}

// NewClickOuts builds the service, refusing one that is missing a part.
func NewClickOuts(offers Offers, clicks *Clicks, deeplinks Deeplinks, opts ...Option) (*ClickOuts, error) {
	if offers == nil || clicks == nil || deeplinks == nil {
		return nil, ErrNoClickOuts
	}
	c := &ClickOuts{
		offers:    offers,
		clicks:    clicks,
		minter:    NewMinter(),
		deeplinks: deeplinks,
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Issue records a click for the member on the offer, and answers the
// redirect to send them to.
//
// The order is the whole of this function, and each step is where it is for a
// reason a member loses money over:
//
//  1. One instant is read, and everything is decided against it. The
//     liveness check, the band that is snapshotted and the click all belong
//     to the same moment; three reads of the clock can straddle a band's
//     edge and snapshot a rate the member was never shown.
//  2. The reference is minted before the redirect is built, because
//     [networks.ValidateDeeplinkInputs] refuses to build one without it -
//     FR-020's ordering, checked rather than remembered.
//  3. The redirect is built BEFORE the click is recorded. That is what makes
//     "nothing is recorded when the deeplink fails" true by construction
//     rather than by a rollback somebody has to get right, and it keeps an
//     adapter call out of the middle of a database transaction.
//  4. The click is recorded, and only then does the caller have a redirect
//     to return - so the record exists before the redirect is issued to the
//     member, which is the rest of FR-020.
//
// The one ordering this does NOT have is recording first and building after.
// It would satisfy FR-020 too, and it would leave a click row behind every
// time a template was wrong - rows that match nothing, in the table the
// unattributed queue is measured against.
func (c *ClickOuts) Issue(ctx context.Context, member, offerID uuid.UUID) (Issued, error) {
	at := c.now()

	offer, err := c.offers.LiveOffer(ctx, offerID, at)
	switch {
	case errors.Is(err, catalogue.ErrOfferNotLive):
		return Issued{}, fmt.Errorf("%w: %s at %s", ErrOfferNotAvailable, offerID, at.UTC().Format(time.RFC3339))
	case err != nil:
		return Issued{}, fmt.Errorf("clickout: reading offer %s: %w", offerID, err)
	}

	ref, err := c.minter.Mint()
	if err != nil {
		return Issued{}, err
	}

	redirect, err := c.deeplinks.Build(ctx, networks.DeeplinkTarget{
		OfferID:       offer.ID,
		NetworkID:     networks.NetworkID(offer.NetworkID),
		ClickRefParam: offer.ClickRefParam,
		Template:      offer.DeeplinkTemplate,
	}, ref)
	if err != nil {
		return Issued{}, fmt.Errorf("%w: offer %s on %s: %w", ErrNoRedirect, offer.ID, offer.NetworkID, err)
	}

	click, err := c.clicks.Record(ctx, NewClick{
		Ref:       ref,
		AccountID: member,
		OfferID:   offer.ID,
		// The band and the share as published at `at`, snapshotted whole:
		// this, and not the offer row as it stands when the money is finally
		// paid, is what governs the credit (FR-013).
		Promised: Promise{Rate: offer.Rate, MemberShare: offer.MemberShare},
	})
	if err != nil {
		return Issued{}, err
	}
	return Issued{Click: click, RedirectURL: redirect, OfferEndsAt: offer.ValidTo}, nil
}
