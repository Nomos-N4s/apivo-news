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

// fakeEnrolment answers whether the member is in cashback, and counts how
// often it was asked.
type fakeEnrolment struct {
	in  bool
	err error

	asked int
}

func (f *fakeEnrolment) Participating(context.Context, uuid.UUID) (bool, error) {
	f.asked++
	if f.err != nil {
		return false, f.err
	}
	return f.in, nil
}

// issuer builds the service over the given parts for a member who has opted
// in, with the clock pinned and no click rule.
func issuer(t *testing.T, offers clickout.Offers, clicks clickout.ClickStore, deeplinks clickout.Deeplinks) *clickout.ClickOuts {
	t.Helper()
	return issuerWith(t, offers, clicks, deeplinks)
}

// issuerWith is the same, plus whatever options a case needs.
func issuerWith(t *testing.T, offers clickout.Offers, clicks clickout.ClickStore, deeplinks clickout.Deeplinks, opts ...clickout.Option) *clickout.ClickOuts {
	t.Helper()
	return issuerFor(t, &fakeEnrolment{in: true}, offers, clicks, deeplinks, opts...)
}

// issuerFor is the same over a given enrolment, for the cases about the
// member's opt-in rather than their click.
func issuerFor(t *testing.T, enrolment clickout.Enrolment, offers clickout.Offers, clicks clickout.ClickStore, deeplinks clickout.Deeplinks, opts ...clickout.Option) *clickout.ClickOuts {
	t.Helper()
	recorder, err := clickout.NewClicks(clicks)
	if err != nil {
		t.Fatalf("NewClicks(): %v", err)
	}
	issue, err := clickout.NewClickOuts(offers, enrolment, recorder, deeplinks,
		append([]clickout.Option{clickout.WithClock(func() time.Time { return clickedAt })}, opts...)...)
	if err != nil {
		t.Fatalf("NewClickOuts(): %v", err)
	}
	return issue
}

// TestAMemberWhoHasNotOptedInIsRefusedBeforeAnythingIsRead is FR-110. The
// refusal comes before the catalogue is read and long before anything is
// minted or recorded: a click a non-member could take would be a credit that
// precedes the consent it rests on, and FR-002's opt-in is not one if that
// can happen.
func TestAMemberWhoHasNotOptedInIsRefusedBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()
	offers := &fakeOffers{offer: anOffer()}
	deeplinks := &fakeDeeplinks{url: "https://awin.example.test/go?merchant=42&clickref=abc"}
	clicks := &fakeStore{echo: true}
	enrolment := &fakeEnrolment{in: false}

	_, err := issuerFor(t, enrolment, offers, clicks, deeplinks).Issue(context.Background(),
		clickout.Request{Member: uuid.New(), OfferID: offers.offer.ID})
	if !errors.Is(err, clickout.ErrNotOptedIn) {
		t.Fatalf("Issue() = %v, want ErrNotOptedIn", err)
	}
	if enrolment.asked != 1 {
		t.Errorf("the enrolment was read %d times, want once", enrolment.asked)
	}
	if offers.reads != 0 {
		t.Error("the catalogue was read for a member who cannot click")
	}
	if deeplinks.builds != 0 {
		t.Error("a redirect was built for a member who cannot click")
	}
	if clicks.inserts != 0 {
		t.Error("a click was recorded for a member who has not opted in")
	}
}

// TestAnEnrolmentThatCannotBeReadIsNotARefusal. A participation that could
// not be read is a failure, not a verdict: answering "not opted in" would
// tell a member to accept terms they already accepted because a database
// was down - and either way, nothing is recorded.
func TestAnEnrolmentThatCannotBeReadIsNotARefusal(t *testing.T) {
	t.Parallel()
	offers := &fakeOffers{offer: anOffer()}
	clicks := &fakeStore{echo: true}
	enrolment := &fakeEnrolment{err: errors.New("the database is not answering")}

	_, err := issuerFor(t, enrolment, offers, clicks, &fakeDeeplinks{url: "https://x.test/go"}).Issue(context.Background(),
		clickout.Request{Member: uuid.New(), OfferID: offers.offer.ID})
	if err == nil || errors.Is(err, clickout.ErrNotOptedIn) {
		t.Fatalf("Issue() = %v, want a failure that is not ErrNotOptedIn", err)
	}
	if clicks.inserts != 0 {
		t.Error("a click was recorded although the member's opt-in could not be read")
	}
}

func TestIssuingARedirectSnapshotsTheBandTheMemberWasShown(t *testing.T) {
	t.Parallel()

	offer, member := anOffer(), uuid.New()
	offers := &fakeOffers{offer: offer}
	deeplinks := &fakeDeeplinks{url: "https://awin.example.test/go?merchant=42&clickref=abc"}
	clicks := &fakeStore{echo: true}

	issued, err := issuer(t, offers, clicks, deeplinks).Issue(t.Context(), clickout.Request{Member: member, OfferID: offer.ID})
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

	issued, err := issuer(t, &fakeOffers{offer: offer}, clicks, deeplinks).Issue(t.Context(), clickout.Request{Member: member, OfferID: offer.ID})
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

	_, err := issuer(t, &fakeOffers{offer: offer}, clicks, deeplinks).Issue(t.Context(), clickout.Request{Member: member, OfferID: offer.ID})

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

	_, err := issuer(t, offers, clicks, deeplinks).Issue(t.Context(), clickout.Request{Member: member, OfferID: offerID})

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
	_, err := issuer(t, offers, &fakeStore{}, &fakeDeeplinks{}).Issue(t.Context(), clickout.Request{Member: uuid.New(), OfferID: uuid.New()})

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
		Issue(t.Context(), clickout.Request{Member: uuid.New(), OfferID: offer.ID})

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
		Issue(t.Context(), clickout.Request{Member: member, OfferID: offer.ID})
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
	// Every part as its interface, including the recorder: the absent case
	// is a nil INTERFACE, which is what a composition root that skipped a
	// part would pass. A nil *Clicks boxed into one is a non-nil interface
	// holding a nil pointer, and would say this test passes while the
	// service panicked on its first click.
	enrolled := &fakeEnrolment{in: true}
	cases := []struct {
		name      string
		offers    clickout.Offers
		enrolment clickout.Enrolment
		clicks    clickout.Recorder
		deeplinks clickout.Deeplinks
	}{
		{name: "no offer reader", enrolment: enrolled, clicks: clicks, deeplinks: &fakeDeeplinks{}},
		{name: "no enrolment reader", offers: &fakeOffers{}, clicks: clicks, deeplinks: &fakeDeeplinks{}},
		{name: "no recorder", offers: &fakeOffers{}, enrolment: enrolled, deeplinks: &fakeDeeplinks{}},
		{name: "no deeplink builder", offers: &fakeOffers{}, enrolment: enrolled, clicks: clicks},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := clickout.NewClickOuts(tc.offers, tc.enrolment, tc.clicks, tc.deeplinks); !errors.Is(err, clickout.ErrNoClickOuts) {
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
		Issue(t.Context(), clickout.Request{Member: member, OfferID: offer.ID})
	if err != nil {
		t.Fatalf("Issue(): %v", err)
	}
	if issued.Click.Promised.Rate != offer.Rate {
		t.Errorf("snapshotted %+v, want %+v", issued.Click.Promised.Rate, offer.Rate)
	}
}
