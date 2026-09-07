package main

// The publish-offer command (#537): what its flags refuse, what its report
// says, and one run against the seed's scratch database - because committing
// is what the command does, and the fixture catalogue it publishes on has to
// have been imported by the seed first.

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// publishOfferArgs is a complete, well-formed invocation every refusal case
// starts from and breaks in one place.
func publishOfferArgs() []string {
	return []string{publishOfferName, "-merchant", "FIX-1", "-rate-bps", "1500", "-deeplink", "https://example.com/shop/FIX-1"}
}

// without drops one flag and its value from an invocation.
func without(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func TestPublishOfferRefusesFlagsThatDescribeNoBand(t *testing.T) {
	t.Parallel()
	const unreachable = "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable"
	for _, tc := range []struct {
		name string
		args []string
		env  func(map[string]string)
		want string
	}{
		{"an argument", append(publishOfferArgs(), "extra"), nil, "takes no arguments"},
		{"no merchant", without(publishOfferArgs(), "-merchant"), nil, "-merchant is required"},
		{"no deeplink", without(publishOfferArgs(), "-deeplink"), nil, "-deeplink is required"},
		{"no rate", without(publishOfferArgs(), "-rate-bps"), nil, "no rate"},
		{"two rates", append(publishOfferArgs(), "-fixed-minor", "250", "-currency", "EUR"), nil, "two shapes of one rate"},
		{"a currency on a percentage", append(publishOfferArgs(), "-currency", "EUR"), nil, "-currency goes with -fixed-minor"},
		{"a rate above the whole", append(without(publishOfferArgs(), "-rate-bps"), "-rate-bps", "10001"), nil, "outside 1..10000"},
		{"a negative rate", append(without(publishOfferArgs(), "-rate-bps"), "-rate-bps", "-5"), nil, "outside 1..10000"},
		{"a fixed amount with no currency", append(without(publishOfferArgs(), "-rate-bps"), "-fixed-minor", "250"), nil, "needs -currency"},
		{"a currency that is not one", append(without(publishOfferArgs(), "-rate-bps"), "-fixed-minor", "250", "-currency", "euros"), nil, "-currency"},
		{"a fixed amount of nothing", append(without(publishOfferArgs(), "-rate-bps"), "-fixed-minor", "-1", "-currency", "EUR"), nil, "-fixed-minor"},
		{"a share above the whole", append(publishOfferArgs(), "-share-bps", "10001"), nil, "-share-bps 10001 is outside"},
		{"a start that is not a date", append(publishOfferArgs(), "-valid-from", "next tuesday"), nil, "-valid-from"},
		{"an end that is not a date", append(publishOfferArgs(), "-valid-to", "soon"), nil, "-valid-to"},
		{"a replaces that is not an offer id", append(publishOfferArgs(), "-replaces", "the-old-one"), nil, "-replaces"},
		{"no network", publishOfferArgs(), func(env map[string]string) { delete(env, "NETWORKS") }, "NETWORKS names no network"},
		{"a network missing a key", publishOfferArgs(), func(env map[string]string) { delete(env, "NETWORK_FIXTURE_ACCOUNT_ID") }, "NETWORK_FIXTURE_ACCOUNT_ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := seedEnv(unreachable)
			if tc.env != nil {
				tc.env(env)
			}
			var out bytes.Buffer
			err := run(context.Background(), tc.args, func(k string) string { return env[k] }, &out)
			if err == nil {
				t.Fatalf("run(%q) succeeded; it must refuse, and its output was %q", tc.args, out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("run(%q) = %q, want it to mention %q", tc.args, err, tc.want)
			}
		})
	}
}

func TestPublicationFromFlagsCarriesEveryFlag(t *testing.T) {
	t.Parallel()
	replaces := uuid.New()
	req, err := publicationFromFlags("fixture", "FIX-1", 0, 250, "eur", 7000,
		"2026-09-08", "2026-10-08T00:00:00Z", " shoes only ", "gift cards", "https://example.com/shop/FIX-1", replaces.String())
	if err != nil {
		t.Fatalf("publicationFromFlags(): %v", err)
	}
	want := catalogue.Publication{
		Network:            "fixture",
		MerchantExternalID: "FIX-1",
		Rate:               catalogue.RateBand{Kind: catalogue.RateFixed, Fixed: money.Amount{Minor: 250, Currency: "EUR"}},
		MemberShare:        7000,
		Conditions:         " shoes only ",
		Exclusions:         "gift cards",
		ValidFrom:          time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC),
		ValidTo:            time.Date(2026, time.October, 8, 0, 0, 0, 0, time.UTC),
		DeeplinkTemplate:   "https://example.com/shop/FIX-1",
		Replaces:           replaces,
	}
	if req != want {
		t.Errorf("publicationFromFlags() = %+v\nwant %+v", req, want)
	}
	// The terms are handed over as typed: trimming is the publisher's, so
	// that what the row holds is decided in one place.
	percent, err := publicationFromFlags("fixture", "FIX-1", 1500, 0, "", int(catalogue.DefaultMemberShare), "", "", "", "", "https://example.com/x", "")
	if err != nil {
		t.Fatalf("publicationFromFlags() for a percentage: %v", err)
	}
	if percent.Rate != (catalogue.RateBand{Kind: catalogue.RatePercent, Percent: 1500}) || percent.MemberShare != catalogue.DefaultMemberShare || !percent.ValidFrom.IsZero() {
		t.Errorf("a percentage with defaults became %+v", percent)
	}
}

func TestPublishOfferReportRendersWhatTheRouteCarries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		bps  money.BasisPoints
		want string
	}{{6000, "60.00%"}, {5, "0.05%"}, {10000, "100.00%"}, {1234, "12.34%"}} {
		if got := describeShare(tc.bps); got != tc.want {
			t.Errorf("describeShare(%d) = %q, want %q", tc.bps, got, tc.want)
		}
	}
	if got := describeRate(catalogue.RateBand{Kind: catalogue.RatePercent, Percent: 1500}); got != "1500 bps = 15.00% of the sale" {
		t.Errorf("describeRate(percent) = %q", got)
	}
	if got := describeRate(catalogue.RateBand{Kind: catalogue.RateFixed, Fixed: money.Amount{Minor: 250, Currency: "EUR"}}); got != "250 minor = 250 EUR per sale" {
		t.Errorf("describeRate(fixed) = %q", got)
	}
	if got := describeRate(catalogue.RateBand{Kind: "odd"}); got != "odd" {
		t.Errorf("describeRate(unknown) = %q", got)
	}
	opened := time.Date(2026, time.September, 8, 0, 0, 0, 0, time.UTC)
	if got := describeWindow(opened, time.Time{}); got != "2026-09-08T00:00:00Z -> open" {
		t.Errorf("describeWindow(open) = %q", got)
	}

	newID, oldID, other := uuid.New(), uuid.New(), uuid.New()
	req := catalogue.Publication{Network: "fixture", Rate: catalogue.RateBand{Kind: catalogue.RatePercent, Percent: 1500}, MemberShare: 6000}
	published := catalogue.Published{
		OfferID: newID, RouteID: uuid.New(), MerchantSlug: "fixture-outdoor-co", Preferred: false,
		ValidFrom: opened, Replaced: oldID,
		InForce: []catalogue.PublishedBand{
			{ID: other, Rate: catalogue.RateBand{Kind: catalogue.RatePercent, Percent: 500}, MemberShare: 7000, ValidFrom: opened.Add(-time.Hour)},
			{ID: newID, Rate: req.Rate, MemberShare: 6000, ValidFrom: opened},
		},
	}
	var out bytes.Buffer
	if err := reportPublication(&out, req, published); err != nil {
		t.Fatalf("reportPublication(): %v", err)
	}
	for _, want := range []string{
		"published band " + newID.String(),
		`"fixture-outdoor-co"`,
		"NOT the preferred route",
		"replaced        " + oldID.String() + ", closed at 2026-09-08T00:00:00Z",
		"2 band(s) on this route",
		"* " + newID.String(),
		"  " + other.String() + "  500 bps = 5.00% of the sale, member share 70.00%",
		`{"offer_id":"` + newID.String() + `"}`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report lacks %q:\n%s", want, out.String())
		}
	}
	published.Preferred = true
	out.Reset()
	if err := reportPublication(&out, req, published); err != nil || !strings.Contains(out.String(), "the preferred route") {
		t.Errorf("a preferred route is not reported as one (err %v):\n%s", err, out.String())
	}
}

// publishOffer runs the command with the seed's environment, amended by
// the caller, and answers what it printed and what it refused with.
func publishOffer(t *testing.T, dbURL string, amend func(map[string]string), args ...string) (string, error) {
	t.Helper()
	env := seedEnv(dbURL)
	if amend != nil {
		amend(env)
	}
	var out bytes.Buffer
	err := run(context.Background(), append([]string{publishOfferName}, args...), func(k string) string { return env[k] }, &out)
	return out.String(), err
}

// publishTestTemplate is what every band this file publishes points at.
// Distinct from the seed's own template, so the bands can be found and
// removed afterwards without touching the seed's.
const publishTestTemplate = "https://example.com/publish-offer-test/"

// TestPublishOfferOnTheSeededCatalogue is the command end to end: refused
// while no publisher account is connected, then a band on the fixture's one
// publishable route, then the same band refused, then a replacement that
// closes the first - each checked by reading the rows back.
//
// It runs in the SEED's scratch database rather than one of its own, and
// removes the bands it published on the way out. A database of its own
// would cost a second `create database` per run of this package, and that
// is a cluster-wide stall the lock-timeout suites elsewhere feel as a
// failure (#419); and the seed's own test counts the bands on this
// network, so what this test leaves behind has to be nothing.
func TestPublishOfferOnTheSeededCatalogue(t *testing.T) {
	dbURL, pool := seedTestDB(t)
	ctx := context.Background()
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `delete from cashback.offer where deeplink_template like $1`, publishTestTemplate+"%"); err != nil {
			t.Errorf("removing the bands this test published: %v", err)
		}
	})

	// The seed connects the fixture account and imports its catalogue,
	// which is the state an operator publishes into. Idempotent, so it
	// does not matter whether the seed's own test ran first.
	seedOnce(t, dbURL)

	// An account nobody connected: the template cannot be tried against an
	// adapter that does not exist yet, and the command says what to run.
	notConnected := func(env map[string]string) { env["NETWORK_FIXTURE_ACCOUNT_ID"] = "nobody-connected" }
	if _, err := publishOffer(t, dbURL, notConnected, "-merchant", "FIX-1", "-rate-bps", "1500", "-deeplink", publishTestTemplate+"x"); err == nil || !strings.Contains(err.Error(), "run "+connectNetworkName+" first") {
		t.Fatalf("publish-offer with no connected account = %v, want a refusal naming connect-network", err)
	}

	var merchant string
	if err := pool.QueryRow(ctx,
		`select external_merchant_id from cashback.merchant_network where network_id = 'fixture' and status = 'active'`).Scan(&merchant); err != nil {
		t.Fatalf("finding the fixture's publishable route: %v", err)
	}

	template := publishTestTemplate + merchant
	out, err := publishOffer(t, dbURL, nil, "-merchant", merchant, "-rate-bps", "1234", "-conditions", "Command test.", "-deeplink", template)
	if err != nil {
		t.Fatalf("publish-offer: %v (%s)", err, out)
	}
	line, _, _ := strings.Cut(out, "\n")
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "published" {
		t.Fatalf("the report does not open with the band: %q", out)
	}
	offerID, err := uuid.Parse(fields[2])
	if err != nil {
		t.Fatalf("the report names no offer id: %q", line)
	}
	var live bool
	if err := pool.QueryRow(ctx, `
		select exists (select 1 from cashback.offer
		                where id = $1 and rate_bps = 1234 and member_share_bps = $2 and conditions = 'Command test.'
		                  and deeplink_template = $3 and valid_from <= now() and valid_to is null)`,
		offerID, int32(catalogue.DefaultMemberShare), template).Scan(&live); err != nil || !live {
		t.Errorf("the band the report names is not the band that was asked for (err %v)", err)
	}
	if !strings.Contains(out, "the preferred route") || !strings.Contains(out, `{"offer_id":"`+offerID.String()+`"}`) {
		t.Errorf("the report is missing the route note or the click-through line:\n%s", out)
	}

	// The same band again is the same promise twice.
	if _, err := publishOffer(t, dbURL, nil, "-merchant", merchant, "-rate-bps", "1234", "-conditions", "Command test.", "-deeplink", template); err == nil || !strings.Contains(err.Error(), "already in force") {
		t.Errorf("publishing the same band again = %v, want a refusal", err)
	}

	// A rate change is one command: the new band opens and the old closes
	// at the same instant.
	out, err = publishOffer(t, dbURL, nil, "-merchant", merchant, "-rate-bps", "2000", "-conditions", "Command test.", "-deeplink", template, "-replaces", offerID.String())
	if err != nil {
		t.Fatalf("publish-offer -replaces: %v (%s)", err, out)
	}
	var closedAt, openedAt time.Time
	if err := pool.QueryRow(ctx, `select valid_to from cashback.offer where id = $1`, offerID).Scan(&closedAt); err != nil {
		t.Fatalf("reading the replaced band: %v", err)
	}
	if err := pool.QueryRow(ctx, `select valid_from from cashback.offer where rate_bps = 2000 and deeplink_template = $1`, template).Scan(&openedAt); err != nil {
		t.Fatalf("reading the replacing band: %v", err)
	}
	if !closedAt.Equal(openedAt) {
		t.Errorf("the old band closed at %s and the new opened at %s; they must be one instant", closedAt, openedAt)
	}
	if !strings.Contains(out, "replaced        "+offerID.String()) {
		t.Errorf("the report does not say what was replaced:\n%s", out)
	}
}
