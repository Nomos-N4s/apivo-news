package clickout_test

// Issuing a tracked redirect. The order is the subject: every case here is
// about what must have happened before something else, because that ordering
// is the whole of FR-020 and the difference between a member being credited
// and being redirected into nothing.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// clickedAt is the one instant every case pins its clock to.
var clickedAt = time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)

// bandEndsAt is when the fixture band stops being published.
var bandEndsAt = clickedAt.Add(30 * 24 * time.Hour)

// fakeOffers answers with a canned offer and records the moment it was asked
// about.
type fakeOffers struct {
	offer catalogue.Offer
	err   error

	askedAt  time.Time
	askedFor uuid.UUID
	reads    int
}

func (f *fakeOffers) LiveOffer(_ context.Context, id uuid.UUID, at time.Time) (catalogue.Offer, error) {
	f.askedFor, f.askedAt, f.reads = id, at, f.reads+1
	if f.err != nil {
		return catalogue.Offer{}, f.err
	}
	return f.offer, nil
}

// fakeDeeplinks answers with a canned URL and records what it was asked to
// build.
type fakeDeeplinks struct {
	url string
	err error

	target networks.DeeplinkTarget
	ref    networks.IssuedClickRef
	builds int
}

func (f *fakeDeeplinks) Build(_ context.Context, target networks.DeeplinkTarget, ref networks.IssuedClickRef) (string, error) {
	f.target, f.ref, f.builds = target, ref, f.builds+1
	if f.err != nil {
		return "", f.err
	}
	return f.url, nil
}

// anOffer is one live band, on a network with a click-reference parameter
// and a template.
func anOffer() catalogue.Offer {
	return catalogue.Offer{
		ID:               uuid.New(),
		MerchantID:       uuid.New(),
		MerchantSlug:     "a-retailer",
		NetworkID:        "awin",
		ClickRefParam:    "clickref",
		Rate:             catalogue.RateBand{Kind: catalogue.RatePercent, Percent: 400},
		MemberShare:      5000,
		ValidFrom:        clickedAt.Add(-24 * time.Hour),
		ValidTo:          bandEndsAt,
		DeeplinkTemplate: "https://awin.example.test/go?merchant=42",
	}
}

// issuer builds the service over the given parts, with the clock pinned.
func issuer(t *testing.T, offers clickout.Offers, clicks clickout.ClickStore, deeplinks clickout.Deeplinks) *clickout.ClickOuts {
	t.Helper()
	recorder, err := clickout.NewClicks(clicks)
	if err != nil {
		t.Fatalf("NewClicks(): %v", err)
	}
	issue, err := clickout.NewClickOuts(offers, recorder, deeplinks,
		clickout.WithClock(func() time.Time { return clickedAt }))
	if err != nil {
		t.Fatalf("NewClickOuts(): %v", err)
	}
	return issue
}

func TestIssuingARedirectSnapshotsTheBandTheMemberWasShown(t *testing.T) {
	t.Parallel()

	offer, member := anOffer(), uuid.New()
	offers := &fakeOffers{offer: offer}
	deeplinks := &fakeDeeplinks{url: "https://awin.example.test/go?merchant=42&clickref=abc"}
	clicks := &fakeStore{echo: true}

	issued, err := issuer(t, offers, clicks, deeplinks).Issue(t.Context(), member, offer.ID)
	if err != nil {
		t.Fatalf("Issue(): %v", err)
	}

	// One instant, not three. The liveness check, the snapshot and the click
	// all belong to the same moment; separate clock reads can straddle a
	// band's edge and snapshot a rate the member was never shown.
	if !offers.askedAt.Equal(clickedAt) {
		t.Errorf("the offer was read as at %s, want the pinned %s", offers.askedAt, clickedAt)
	}
	if offers.askedFor != offer.ID {
		t.Errorf("read offer %s, want %s", offers.askedFor, offer.ID)
	}

	// FR-013: what governs the credit is the band as published at that
	// moment, snapshotted whole onto the click.
	if issued.Click.Promised.Rate != offer.Rate {
		t.Errorf("snapshotted band %+v, want %+v", issued.Click.Promised.Rate, offer.Rate)
	}
	if issued.Click.Promised.MemberShare != offer.MemberShare {
		t.Errorf("snapshotted share %d, want %d", issued.Click.Promised.MemberShare, offer.MemberShare)
	}
	if issued.RedirectURL != deeplinks.url {
		t.Errorf("redirect = %q, want %q", issued.RedirectURL, deeplinks.url)
	}
	if !issued.OfferEndsAt.Equal(bandEndsAt) {
		t.Errorf("the band ends at %s, want %s", issued.OfferEndsAt, bandEndsAt)
	}
}

// TestTheRedirectIsBuiltFromTheBandsOwnRoute pins what the adapter is handed.
// Every field here is one a wrong value silently loses attribution on
// (FR-021), and none of them is a literal in this package.
func TestTheRedirectIsBuiltFromTheBandsOwnRoute(t *testing.T) {
	t.Parallel()

	offer, member := anOffer(), uuid.New()
	deeplinks := &fakeDeeplinks{url: "https://awin.example.test/go"}
	clicks := &fakeStore{echo: true}

	issued, err := issuer(t, &fakeOffers{offer: offer}, clicks, deeplinks).Issue(t.Context(), member, offer.ID)
	if err != nil {
		t.Fatalf("Issue(): %v", err)
	}

	want := networks.DeeplinkTarget{
		OfferID:       offer.ID,
		NetworkID:     networks.NetworkID(offer.NetworkID),
		ClickRefParam: offer.ClickRefParam,
		Template:      offer.DeeplinkTemplate,
	}
	if deeplinks.target != want {
		t.Errorf("built from %+v, want %+v", deeplinks.target, want)
	}
	// The reference handed to the adapter is the one the click carries: a
	// redirect built with any other value comes back matching nothing.
	if deeplinks.ref != issued.Click.Ref {
		t.Errorf("built with reference %q, want the click's %q", deeplinks.ref, issued.Click.Ref)
	}
}

// TestNothingIsRecordedWhenTheRedirectCannotBeBuilt is the ordering that
// matters most, and the one the contract states outright: a click row left
// behind for a redirect nobody received matches nothing forever, in the very
// table the unattributed queue is measured against.
func TestNothingIsRecordedWhenTheRedirectCannotBeBuilt(t *testing.T) {
	t.Parallel()

	offer, member := anOffer(), uuid.New()
	// Echoing, so a recorder that ran would SUCCEED: the assertion below has
	// to be what catches a reordering, not an incidental failure further on.
	clicks := &fakeStore{echo: true}
	deeplinks := &fakeDeeplinks{err: networks.ErrDeeplinkNotFormed}

	_, err := issuer(t, &fakeOffers{offer: offer}, clicks, deeplinks).Issue(t.Context(), member, offer.ID)

	if !errors.Is(err, clickout.ErrNoRedirect) {
		t.Fatalf("Issue() error = %v, want one wrapping %v", err, clickout.ErrNoRedirect)
	}
	if clicks.inserts != 0 {
		t.Errorf("%d click(s) were recorded for a redirect that was never built", clicks.inserts)
	}
	// The refusal is not swallowed on the way up: an operator reading the
	// log has to be able to tell a route to fix from a network having a bad
	// day.
	if !errors.Is(err, networks.ErrDeeplinkNotFormed) {
		t.Errorf("Issue() error = %v, want it to carry the adapter's own refusal", err)
	}
}

// TestARedirectIsOnlyBuiltForABandThatIsLive keeps a member off a rate
// nobody honours, and keeps the click table clean of rows for offers that
// were never clickable.
func TestARedirectIsOnlyBuiltForABandThatIsLive(t *testing.T) {
	t.Parallel()

	offerID, member := uuid.New(), uuid.New()
	clicks := &fakeStore{}
	deeplinks := &fakeDeeplinks{url: "https://awin.example.test/go"}
	offers := &fakeOffers{err: catalogue.ErrOfferNotLive}

	_, err := issuer(t, offers, clicks, deeplinks).Issue(t.Context(), member, offerID)

	if !errors.Is(err, clickout.ErrOfferNotAvailable) {
		t.Fatalf("Issue() error = %v, want one wrapping %v", err, clickout.ErrOfferNotAvailable)
	}
	if deeplinks.builds != 0 || clicks.inserts != 0 {
		t.Errorf("an unavailable offer built %d redirect(s) and recorded %d click(s), want none",
			deeplinks.builds, clicks.inserts)
	}
}

// TestAFailedOfferReadIsNotAnUnavailableOffer keeps the two apart. Reporting
// a database failure as "this offer has expired" tells a member something
// false about the catalogue and hides an outage.
func TestAFailedOfferReadIsNotAnUnavailableOffer(t *testing.T) {
	t.Parallel()

	offers := &fakeOffers{err: errors.New("connection reset")}
	_, err := issuer(t, offers, &fakeStore{}, &fakeDeeplinks{}).Issue(t.Context(), uuid.New(), uuid.New())

	if errors.Is(err, clickout.ErrOfferNotAvailable) {
		t.Fatal("a failed read reads as an unavailable offer")
	}
	if err == nil {
		t.Fatal("Issue() returned no error for a failed read")
	}
}

// TestAFailedRecordingLeavesNoRedirectIssued covers the last step: if the
// click cannot be written, the member must not be sent anywhere, because
// they would buy against a reference no row carries.
func TestAFailedRecordingLeavesNoRedirectIssued(t *testing.T) {
	t.Parallel()

	offer := anOffer()
	clicks := &fakeStore{insertErr: errors.New("connection reset")}
	issued, err := issuer(t, &fakeOffers{offer: offer}, clicks, &fakeDeeplinks{url: "https://awin.example.test/go"}).
		Issue(t.Context(), uuid.New(), offer.ID)

	if err == nil {
		t.Fatal("Issue() returned no error although the click was not recorded")
	}
	if issued.RedirectURL != "" {
		t.Errorf("Issue() returned the redirect %q beside its error", issued.RedirectURL)
	}
}

// TestABandWithNoPublishedEndHasNoEnd keeps the open-ended case from being
// rendered as an expiry in the past or in 1970.
func TestABandWithNoPublishedEndHasNoEnd(t *testing.T) {
	t.Parallel()

	offer, member := anOffer(), uuid.New()
	offer.ValidTo = time.Time{}
	clicks := &fakeStore{echo: true}

	issued, err := issuer(t, &fakeOffers{offer: offer}, clicks, &fakeDeeplinks{url: "https://x.test/go"}).
		Issue(t.Context(), member, offer.ID)
	if err != nil {
		t.Fatalf("Issue(): %v", err)
	}
	if !issued.OfferEndsAt.IsZero() {
		t.Errorf("a band with no published end ends at %s", issued.OfferEndsAt)
	}
}

func TestAServiceMissingAPartIsRefused(t *testing.T) {
	t.Parallel()

	clicks, err := clickout.NewClicks(&fakeStore{})
	if err != nil {
		t.Fatalf("NewClicks(): %v", err)
	}
	cases := []struct {
		name      string
		offers    clickout.Offers
		clicks    *clickout.Clicks
		deeplinks clickout.Deeplinks
	}{
		{name: "no offer reader", clicks: clicks, deeplinks: &fakeDeeplinks{}},
		{name: "no recorder", offers: &fakeOffers{}, deeplinks: &fakeDeeplinks{}},
		{name: "no deeplink builder", offers: &fakeOffers{}, clicks: clicks},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := clickout.NewClickOuts(tc.offers, tc.clicks, tc.deeplinks); !errors.Is(err, clickout.ErrNoClickOuts) {
				t.Fatalf("NewClickOuts() error = %v, want one wrapping %v", err, clickout.ErrNoClickOuts)
			}
		})
	}
}

// aFixedOffer is the other rate kind, for the case that a percent band would
// not catch.
func aFixedOffer(t *testing.T) catalogue.Offer {
	t.Helper()
	fixed, err := money.New(250, "EUR")
	if err != nil {
		t.Fatalf("building the fixed rate: %v", err)
	}
	offer := anOffer()
	offer.Rate = catalogue.RateBand{Kind: catalogue.RateFixed, Fixed: fixed}
	return offer
}

// TestAFixedBandIsSnapshottedWithItsCurrency covers the kind whose snapshot
// carries an amount rather than a number of basis points: a percent-only
// test would pass with the currency dropped (C-6).
func TestAFixedBandIsSnapshottedWithItsCurrency(t *testing.T) {
	t.Parallel()

	offer, member := aFixedOffer(t), uuid.New()
	clicks := &fakeStore{echo: true}

	issued, err := issuer(t, &fakeOffers{offer: offer}, clicks, &fakeDeeplinks{url: "https://x.test/go"}).
		Issue(t.Context(), member, offer.ID)
	if err != nil {
		t.Fatalf("Issue(): %v", err)
	}
	if issued.Click.Promised.Rate != offer.Rate {
		t.Errorf("snapshotted %+v, want %+v", issued.Click.Promised.Rate, offer.Rate)
	}
}
