package catalogue_test

// The publisher against the schema that holds what it writes (#537).
//
// Every refusal here is one a member would otherwise meet at their first
// click, and every write is checked by reading it back through the same
// statement the click-out reads bands with - so what this file proves is
// not that a row was inserted but that a click could be issued against it.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// publishAt is the clock every publication in this file opens at unless it
// says otherwise: after the import's instant, so a route seeded "now" is
// already there.
var publishAt = importTestAt.Add(time.Hour)

// redirectStub is a [catalogue.RedirectBuilder] that builds, or refuses
// with the error a case chose. It records the template it was handed so a
// case can assert the publisher tried what the operator typed.
type redirectStub struct {
	refuse error
	tried  []networks.DeeplinkTarget
}

func (r *redirectStub) BuildDeeplink(_ context.Context, target networks.DeeplinkTarget, ref networks.IssuedClickRef) (string, error) {
	r.tried = append(r.tried, target)
	if r.refuse != nil {
		return "", r.refuse
	}
	return target.Template + "?" + target.ClickRefParam + "=" + ref.Ref(), nil
}

// aPublisher builds a publisher over the stub with the clock pinned.
func aPublisher(t *testing.T, redirects catalogue.RedirectBuilder) *catalogue.Publisher {
	t.Helper()
	p, err := catalogue.NewPublisher(redirects, catalogue.WithPublisherClock(func() time.Time { return publishAt }))
	if err != nil {
		t.Fatalf("NewPublisher(): %v", err)
	}
	return p
}

// routeShape is what a seeded route looks like on each of the three legs a
// live band needs.
type routeShape struct {
	route, merchant string
	preferred       bool
}

var liveRoute = routeShape{route: "active", merchant: "active", preferred: true}

// seedRoute writes a retailer and its route at the case's network, and
// answers the external id the publisher will name it by.
func seedRoute(ctx context.Context, t *testing.T, tx pgx.Tx, networkID networks.NetworkID, shape routeShape) string {
	t.Helper()
	suffix := uuid.NewString()[:8]
	var merchant uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.merchant (slug, country, source_language_code, status)
		values ($1, 'GR', 'el', $2) returning id`, "publish-"+suffix, shape.merchant).Scan(&merchant); err != nil {
		t.Fatalf("seeding the merchant: %v", err)
	}
	external := "prog-" + suffix
	if _, err := tx.Exec(ctx, `
		insert into cashback.merchant_network
		    (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload, status, preferred)
		values ('fixture', $1, $2, $3, now(), '{"id":"publish"}'::jsonb, $4, $5)`,
		merchant, networkID.String(), external, shape.route, shape.preferred); err != nil {
		t.Fatalf("seeding the route: %v", err)
	}
	return external
}

// aPublication is the band every case starts from: 15% of the sale at the
// default share, open from the pinned clock.
func aPublication(network networks.NetworkID, merchant string) catalogue.Publication {
	return catalogue.Publication{
		Network:            network,
		MerchantExternalID: merchant,
		Rate:               catalogue.RateBand{Kind: catalogue.RatePercent, Percent: 1500},
		DeeplinkTemplate:   "https://go.example.test/z/1-2/PUB/?lnkurl=https%3A%2F%2Fshop.example%2F",
	}
}

// liveAt reads the band back through the click-out's own statement, which
// is the only reading that matters: a band that is not live to GetLiveOffer
// is not live.
func liveAt(ctx context.Context, t *testing.T, tx pgx.Tx, id uuid.UUID, at time.Time) (store.GetLiveOfferRow, bool) {
	t.Helper()
	row, err := store.New(tx).GetLiveOffer(ctx, store.GetLiveOfferParams{
		ID: pgtype.UUID{Bytes: id, Valid: true},
		At: pgtype.Timestamptz{Time: at, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.GetLiveOfferRow{}, false
	}
	if err != nil {
		t.Fatalf("GetLiveOffer(): %v", err)
	}
	return row, true
}

func TestPublishRefusesBeforeTouchingTheDatabase(t *testing.T) {
	t.Parallel()
	if _, err := catalogue.NewPublisher(nil); !errors.Is(err, catalogue.ErrNoRedirectBuilder) {
		t.Errorf("NewPublisher(nil) = %v, want ErrNoRedirectBuilder", err)
	}
	fixed := func(minor int64, currency string) catalogue.RateBand {
		return catalogue.RateBand{Kind: catalogue.RateFixed, Fixed: money.Amount{Minor: minor, Currency: money.Currency(currency)}}
	}
	base := aPublication("pubnet", "prog-1")
	for _, tc := range []struct {
		name  string
		amend func(*catalogue.Publication)
		want  string
	}{
		{"no network", func(p *catalogue.Publication) { p.Network = "" }, "network"},
		{"no merchant", func(p *catalogue.Publication) { p.MerchantExternalID = "  " }, "no merchant id"},
		{"a percent band carrying a fixed amount", func(p *catalogue.Publication) { p.Rate.Fixed = money.Amount{Minor: 1, Currency: "EUR"} }, "carries no fixed amount"},
		{"a rate above the whole", func(p *catalogue.Publication) { p.Rate.Percent = 10001 }, "outside 0..10000"},
		{"a rate of nothing", func(p *catalogue.Publication) { p.Rate.Percent = 0 }, "pays nothing"},
		{"a fixed band carrying a percentage", func(p *catalogue.Publication) { p.Rate = fixed(250, "EUR"); p.Rate.Percent = 5 }, "carries no percentage"},
		{"a fixed band with no currency", func(p *catalogue.Publication) { p.Rate = fixed(250, "") }, "currency"},
		{"a fixed amount of nothing", func(p *catalogue.Publication) { p.Rate = fixed(0, "EUR") }, "pays nothing"},
		{"a rate of no kind", func(p *catalogue.Publication) { p.Rate = catalogue.RateBand{} }, "neither percent nor fixed"},
		{"a share above the whole", func(p *catalogue.Publication) { p.MemberShare = 10001 }, "member share"},
		{"a window that closes before it opens", func(p *catalogue.Publication) { p.ValidFrom = publishAt; p.ValidTo = publishAt }, "not after it opens"},
		{"no template", func(p *catalogue.Publication) { p.DeeplinkTemplate = " " }, "no deeplink template"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := base
			tc.amend(&req)
			stub := &redirectStub{}
			// A nil transaction: every refusal here must happen before
			// the database is asked anything, or it would panic.
			_, err := aPublisher(t, stub).Publish(context.Background(), nil, req)
			if !errors.Is(err, catalogue.ErrPublicationRefused) {
				t.Fatalf("Publish() = %v, want ErrPublicationRefused", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say %q: %v", tc.want, err)
			}
			if len(stub.tried) != 0 {
				t.Errorf("the template was tried before the request was refused")
			}
		})
	}
}

func TestPublishAgainstTheRealSchema(t *testing.T) {
	t.Parallel()
	ctx, tx, network := importTestTx(t)

	each := func(name string, scenario func(t *testing.T, tx pgx.Tx)) {
		t.Run(name, func(t *testing.T) {
			sub, err := tx.Begin(ctx)
			if err != nil {
				t.Fatalf("savepoint: %v", err)
			}
			defer func() { _ = sub.Rollback(ctx) }()
			scenario(t, sub)
		})
	}

	each("a route nobody imported is refused, and the import is named", func(t *testing.T, tx pgx.Tx) {
		_, err := aPublisher(t, &redirectStub{}).Publish(ctx, tx, aPublication(network.id, "prog-nobody-imported"))
		if !errors.Is(err, catalogue.ErrRouteUnknown) || !strings.Contains(err.Error(), "import") {
			t.Fatalf("Publish() = %v, want ErrRouteUnknown naming the import", err)
		}
	})

	each("a route no click could be issued against is refused by the leg that fails", func(t *testing.T, tx pgx.Tx) {
		for _, tc := range []struct {
			name  string
			shape routeShape
			off   func(pgx.Tx)
			want  string
		}{
			// A paused route cannot hold the published slot (0035), so it
			// is seeded the way the importer leaves one: demoted.
			{"the route is paused", routeShape{route: "paused", merchant: "active", preferred: false}, nil, "raw_payload"},
			{"the retailer is off", routeShape{route: "active", merchant: "paused", preferred: true}, nil, "the retailer"},
			{"the network is off", liveRoute, func(tx pgx.Tx) {
				if _, err := tx.Exec(ctx, `update cashback.network set active = false where id = $1`, network.id.String()); err != nil {
					t.Fatalf("switching the network off: %v", err)
				}
			}, "connect-network"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				sub, err := tx.Begin(ctx)
				if err != nil {
					t.Fatalf("savepoint: %v", err)
				}
				defer func() { _ = sub.Rollback(ctx) }()
				merchant := seedRoute(ctx, t, sub, network.id, tc.shape)
				if tc.off != nil {
					tc.off(sub)
				}
				stub := &redirectStub{}
				_, err = aPublisher(t, stub).Publish(ctx, sub, aPublication(network.id, merchant))
				if !errors.Is(err, catalogue.ErrRouteNotLive) {
					t.Fatalf("Publish() = %v, want ErrRouteNotLive", err)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("the refusal does not point at %q: %v", tc.want, err)
				}
				if len(stub.tried) != 0 {
					t.Error("the template was tried on a route no click could use")
				}
			})
		}
	})

	each("a template the network cannot build a redirect from is refused in the network's words", func(t *testing.T, tx pgx.Tx) {
		merchant := seedRoute(ctx, t, tx, network.id, liveRoute)
		refusal := errors.New("the template already sets clickref")
		stub := &redirectStub{refuse: refusal}
		_, err := aPublisher(t, stub).Publish(ctx, tx, aPublication(network.id, merchant))
		if !errors.Is(err, catalogue.ErrTemplateUnusable) || !errors.Is(err, refusal) {
			t.Fatalf("Publish() = %v, want ErrTemplateUnusable wrapping the adapter's refusal", err)
		}
		// Tried as the click-out would try it: this network, its own
		// parameter, the operator's template, and a reference the click
		// table would accept.
		if len(stub.tried) != 1 {
			t.Fatalf("the template was tried %d time(s), want 1", len(stub.tried))
		}
		target := stub.tried[0]
		if target.NetworkID != network.id || target.ClickRefParam != "clickref" || target.Template != aPublication(network.id, merchant).DeeplinkTemplate {
			t.Errorf("the probe target was %+v", target)
		}
		var rows int
		if err := tx.QueryRow(ctx, `select count(*) from cashback.offer o join cashback.merchant_network mn on mn.id = o.merchant_network_id where mn.external_merchant_id = $1`, merchant).Scan(&rows); err != nil || rows != 0 {
			t.Errorf("%d band(s) were written for a refused template (err %v), want 0", rows, err)
		}
	})

	each("a published band is one the click-out can issue a click against", func(t *testing.T, tx pgx.Tx) {
		merchant := seedRoute(ctx, t, tx, network.id, liveRoute)
		req := aPublication(network.id, merchant)
		req.Conditions = "  On full-price items.  "
		req.ValidTo = publishAt.Add(30 * 24 * time.Hour)

		published, err := aPublisher(t, &redirectStub{}).Publish(ctx, tx, req)
		if err != nil {
			t.Fatalf("Publish(): %v", err)
		}
		row, live := liveAt(ctx, t, tx, published.OfferID, publishAt)
		if !live {
			t.Fatal("the band is not live to GetLiveOffer at the moment it opened")
		}
		if row.RateKind != "percent" || !row.RateBps.Valid || row.RateBps.Int32 != 1500 || row.RateFixedMinor.Valid || row.Currency.Valid {
			t.Errorf("the band was stored as %s/%v/%v/%v, want percent 1500 and nothing fixed", row.RateKind, row.RateBps, row.RateFixedMinor, row.Currency)
		}
		if row.MemberShareBps != int32(catalogue.DefaultMemberShare) {
			t.Errorf("member share %d, want the Q4 default %d applied when none was given", row.MemberShareBps, catalogue.DefaultMemberShare)
		}
		if row.Conditions.String != "On full-price items." || row.Exclusions.Valid {
			t.Errorf("terms stored as %q / %v, want the trimmed conditions and no exclusions", row.Conditions.String, row.Exclusions)
		}
		if !row.ValidFrom.Time.Equal(publishAt) || !row.ValidTo.Valid || !row.ValidTo.Time.Equal(req.ValidTo) {
			t.Errorf("window stored as %v -> %v, want %s -> %s", row.ValidFrom.Time, row.ValidTo.Time, publishAt, req.ValidTo)
		}
		if _, live := liveAt(ctx, t, tx, published.OfferID, req.ValidTo); live {
			t.Error("the band is still live at the instant it closes")
		}
		if published.MerchantSlug == "" || !published.Preferred || published.ValidFrom != publishAt || len(published.InForce) != 1 || published.InForce[0].ID != published.OfferID {
			t.Errorf("the report says %+v", published)
		}
	})

	each("a fixed band round-trips in minor units with its currency", func(t *testing.T, tx pgx.Tx) {
		merchant := seedRoute(ctx, t, tx, network.id, liveRoute)
		req := aPublication(network.id, merchant)
		req.Rate = catalogue.RateBand{Kind: catalogue.RateFixed, Fixed: money.Amount{Minor: 250, Currency: "EUR"}}
		req.MemberShare = 7000

		published, err := aPublisher(t, &redirectStub{}).Publish(ctx, tx, req)
		if err != nil {
			t.Fatalf("Publish(): %v", err)
		}
		row, live := liveAt(ctx, t, tx, published.OfferID, publishAt)
		if !live || row.RateKind != "fixed" || row.RateBps.Valid || row.RateFixedMinor.Int64 != 250 || row.Currency.String != "EUR" || row.MemberShareBps != 7000 {
			t.Errorf("the band was stored as live=%t %s/%v/%v/%v share %d", live, row.RateKind, row.RateBps, row.RateFixedMinor, row.Currency, row.MemberShareBps)
		}
		if published.InForce[0].Rate != req.Rate || published.InForce[0].MemberShare != 7000 {
			t.Errorf("the report reads the band back as %+v", published.InForce[0])
		}
	})

	each("the same promise is not put on a route twice", func(t *testing.T, tx pgx.Tx) {
		merchant := seedRoute(ctx, t, tx, network.id, liveRoute)
		publisher := aPublisher(t, &redirectStub{})
		if _, err := publisher.Publish(ctx, tx, aPublication(network.id, merchant)); err != nil {
			t.Fatalf("first Publish(): %v", err)
		}
		// The same rate, share and terms with a different end is still the
		// same promise.
		again := aPublication(network.id, merchant)
		again.ValidTo = publishAt.Add(24 * time.Hour)
		if _, err := publisher.Publish(ctx, tx, again); !errors.Is(err, catalogue.ErrBandInForce) {
			t.Fatalf("second Publish() = %v, want ErrBandInForce", err)
		}
		// Different terms, a different share or a different rate are each a
		// different band: a retailer whose rate varies by what you buy
		// publishes several at once (US5), and a promise that differs in
		// any one of the three is not the same promise.
		for i, differ := range []func(*catalogue.Publication){
			func(p *catalogue.Publication) { p.Conditions = "Shoes only." },
			func(p *catalogue.Publication) { p.MemberShare = 5000 },
			func(p *catalogue.Publication) { p.Rate.Percent = 2000 },
		} {
			other := aPublication(network.id, merchant)
			differ(&other)
			published, err := publisher.Publish(ctx, tx, other)
			if err != nil {
				t.Fatalf("Publish() differing in field %d = %v, want it published beside the first", i, err)
			}
			if len(published.InForce) != i+2 {
				t.Errorf("%d band(s) in force after the %dth variant, want %d", len(published.InForce), i+1, i+2)
			}
		}
	})

	each("a band that opens later is not yet in force, so the same promise now is not a duplicate", func(t *testing.T, tx pgx.Tx) {
		merchant := seedRoute(ctx, t, tx, network.id, liveRoute)
		publisher := aPublisher(t, &redirectStub{})
		later := aPublication(network.id, merchant)
		later.ValidFrom = publishAt.Add(48 * time.Hour)
		if _, err := publisher.Publish(ctx, tx, later); err != nil {
			t.Fatalf("Publish() of the later band: %v", err)
		}
		now, err := publisher.Publish(ctx, tx, aPublication(network.id, merchant))
		if err != nil {
			t.Fatalf("Publish() now = %v, want it accepted: the identical band does not open for two days", err)
		}
		if len(now.InForce) != 1 || now.InForce[0].ID != now.OfferID {
			t.Errorf("bands in force now: %+v, want the new one alone", now.InForce)
		}
	})

	each("a replaced band closes at the instant the new one opens", func(t *testing.T, tx pgx.Tx) {
		merchant := seedRoute(ctx, t, tx, network.id, liveRoute)
		publisher := aPublisher(t, &redirectStub{})
		old, err := publisher.Publish(ctx, tx, aPublication(network.id, merchant))
		if err != nil {
			t.Fatalf("first Publish(): %v", err)
		}
		later := publishAt.Add(7 * 24 * time.Hour)
		req := aPublication(network.id, merchant)
		req.Rate.Percent = 2000
		req.ValidFrom = later
		req.Replaces = old.OfferID

		published, err := publisher.Publish(ctx, tx, req)
		if err != nil {
			t.Fatalf("replacing Publish(): %v", err)
		}
		if published.Replaced != old.OfferID {
			t.Errorf("the report names %s as replaced, want %s", published.Replaced, old.OfferID)
		}
		// Before the switch the old band is live and the new is not; after
		// it, the reverse. The old band's own end is the new band's start.
		if _, live := liveAt(ctx, t, tx, old.OfferID, later.Add(-time.Second)); !live {
			t.Error("the old band is not live a second before the switch")
		}
		if _, live := liveAt(ctx, t, tx, old.OfferID, later); live {
			t.Error("the old band is still live at the switch")
		}
		if _, live := liveAt(ctx, t, tx, published.OfferID, later); !live {
			t.Error("the new band is not live at the switch")
		}
		if len(published.InForce) != 1 || published.InForce[0].ID != published.OfferID {
			t.Errorf("bands in force at the switch: %+v, want the new one alone", published.InForce)
		}
	})

	each("the same promise with a new tracking URL is a replacement, not a duplicate", func(t *testing.T, tx pgx.Tx) {
		merchant := seedRoute(ctx, t, tx, network.id, liveRoute)
		publisher := aPublisher(t, &redirectStub{})
		old, err := publisher.Publish(ctx, tx, aPublication(network.id, merchant))
		if err != nil {
			t.Fatalf("first Publish(): %v", err)
		}
		// Without --replaces, the same promise is refused whatever its
		// template: the merchant page would show one rate twice.
		moved := aPublication(network.id, merchant)
		moved.DeeplinkTemplate = "https://go.example.test/z/1-3/PUB/?lnkurl=https%3A%2F%2Fshop.example%2Fnew"
		moved.ValidFrom = publishAt.Add(time.Hour)
		if _, err := publisher.Publish(ctx, tx, moved); !errors.Is(err, catalogue.ErrBandInForce) {
			t.Fatalf("Publish() of the same promise on a new template = %v, want ErrBandInForce", err)
		}
		// With it, the band being replaced is exempt from the comparison -
		// it is the one going away - and the promise carries on under the
		// new URL from the instant the old one closes.
		moved.Replaces = old.OfferID
		published, err := publisher.Publish(ctx, tx, moved)
		if err != nil {
			t.Fatalf("Publish() replacing with the same promise = %v, want it accepted", err)
		}
		row, live := liveAt(ctx, t, tx, published.OfferID, moved.ValidFrom)
		if !live || row.DeeplinkTemplate != moved.DeeplinkTemplate {
			t.Errorf("the new band is live=%t with template %q", live, row.DeeplinkTemplate)
		}
		if _, live := liveAt(ctx, t, tx, old.OfferID, moved.ValidFrom); live {
			t.Error("the old band is still live after being replaced")
		}
	})

	each("a band that is not open on this route at that moment cannot be replaced", func(t *testing.T, tx pgx.Tx) {
		publisher := aPublisher(t, &redirectStub{})
		mine := seedRoute(ctx, t, tx, network.id, liveRoute)
		theirs := seedRoute(ctx, t, tx, network.id, liveRoute)
		onTheirs, err := publisher.Publish(ctx, tx, aPublication(network.id, theirs))
		if err != nil {
			t.Fatalf("Publish() on the other route: %v", err)
		}
		closed := aPublication(network.id, mine)
		closed.ValidTo = publishAt.Add(time.Hour)
		alreadyClosed, err := publisher.Publish(ctx, tx, closed)
		if err != nil {
			t.Fatalf("Publish() of the closing band: %v", err)
		}
		opensLater := aPublication(network.id, mine)
		opensLater.Conditions = "Later."
		opensLater.ValidFrom = publishAt.Add(48 * time.Hour)
		notYetOpen, err := publisher.Publish(ctx, tx, opensLater)
		if err != nil {
			t.Fatalf("Publish() of the later band: %v", err)
		}

		for _, tc := range []struct {
			name     string
			replaces uuid.UUID
			at       time.Time
		}{
			{"a band on another route", onTheirs.OfferID, publishAt.Add(time.Minute)},
			{"a band already closed", alreadyClosed.OfferID, publishAt.Add(2 * time.Hour)},
			{"a band that opens at that very moment", notYetOpen.OfferID, opensLater.ValidFrom},
			{"a band that never existed", uuid.New(), publishAt.Add(time.Minute)},
		} {
			req := aPublication(network.id, mine)
			req.Rate.Percent = 1234
			req.ValidFrom = tc.at
			req.Replaces = tc.replaces
			_, err := publisher.Publish(ctx, tx, req)
			if !errors.Is(err, catalogue.ErrNothingToReplace) {
				t.Errorf("%s: Publish() = %v, want ErrNothingToReplace", tc.name, err)
			}
		}
		// Nothing above wrote a band: the refusal happened before the
		// insert and the closed band was not re-closed at another instant.
		row, _ := liveAt(ctx, t, tx, alreadyClosed.OfferID, publishAt)
		if !row.ValidTo.Time.Equal(closed.ValidTo) {
			t.Errorf("the closed band's end moved to %v", row.ValidTo.Time)
		}
	})

	each("a band on a route that is not the preferred one says so", func(t *testing.T, tx pgx.Tx) {
		merchant := seedRoute(ctx, t, tx, network.id, routeShape{route: "active", merchant: "active", preferred: false})
		published, err := aPublisher(t, &redirectStub{}).Publish(ctx, tx, aPublication(network.id, merchant))
		if err != nil {
			t.Fatalf("Publish(): %v", err)
		}
		if published.Preferred {
			t.Error("the report calls a non-preferred route preferred; the merchant page will not list this band")
		}
		if _, live := liveAt(ctx, t, tx, published.OfferID, publishAt); !live {
			t.Error("a band on a non-preferred route is still clickable by id, and this one is not")
		}
	})
}
