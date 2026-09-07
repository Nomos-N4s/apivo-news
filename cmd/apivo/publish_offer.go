// The `publish-offer` command (#537): the supported way to put a rate band
// on a route of the configured network.
//
// A file of its own for the reason connect-network has one: it is a
// subcommand whose whole job is a write an operator would otherwise do by
// hand against production, and the fixture seed - the only other thing that
// writes cashback.offer - refuses every network but the fixture.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/config"
	platformdb "github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// publishOfferName is the subcommand as an operator types it.
const publishOfferName = "publish-offer"

// publishOfferCommand runs the subcommand: one band, on one route, of the
// network this deployment polls.
//
// The network comes from the environment and not from a flag, for the reason
// connect-network's does: NETWORKS is what the running process resolves its
// adapter from, so an operator cannot publish a band on a network this
// deployment will never issue a click through. Everything else is a flag,
// because everything else is a decision about this band.
//
// The rate is taken in the schema's own units - basis points, minor units
// with a currency - rather than parsed from "15%" or "2.50". A published
// rate is money a member is promised, and the one place a decimal must not
// be turned into an integer by code that might round is the place the
// promise is written down (C-6).
func publishOfferCommand(ctx context.Context, args []string, getenv func(string) string, stdout io.Writer) error {
	flags := flag.NewFlagSet(publishOfferName, flag.ContinueOnError)
	flags.SetOutput(stdout)
	merchant := flags.String("merchant", "",
		"the network's own id for the retailer, as the catalogue import stored it on the route (Linkwise: the programme id). Required.")
	rateBPS := flags.Int("rate-bps", 0,
		"the commission the network pays, as basis points of the sale: 1500 is 15%. Exactly one of -rate-bps and -fixed-minor.")
	fixedMinor := flags.Int64("fixed-minor", 0,
		"the commission as a fixed amount per sale, in minor units of -currency: 250 with EUR is 2.50 EUR.")
	currency := flags.String("currency", "",
		"the ISO 4217 code -fixed-minor is in. Required with it, refused without it.")
	shareBPS := flags.Int("share-bps", int(catalogue.DefaultMemberShare),
		"the member's share of the commission, in basis points. The default is founder decision Q4.")
	validFrom := flags.String("valid-from", "",
		"when the band opens (2026-09-08 or an RFC3339 instant). Now when absent.")
	validTo := flags.String("valid-to", "",
		"when the band closes. Open-ended when absent.")
	conditions := flags.String("conditions", "", "the conditions published with the band (FR-011).")
	exclusions := flags.String("exclusions", "", "the exclusions published with the band (FR-011).")
	deeplink := flags.String("deeplink", "",
		"the template the redirect is built from: the network's tracking URL for this retailer with the click reference LEFT OUT - the adapter adds it in the network's own parameter. Required.")
	replaces := flags.String("replaces", "",
		"an offer id on the same route to close at the moment this band opens, so a rate change is one command.")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if rest := flags.Args(); len(rest) > 0 {
		return fmt.Errorf("%s takes no arguments, got %q", publishOfferName, rest)
	}

	cfg, err := config.FromEnv(getenv)
	if err != nil {
		return err
	}
	network, wired := theConfiguredNetwork(cfg.Cashback)
	if !wired {
		return fmt.Errorf("%s names no network, and a network is what this band would be published on", config.NetworksKey)
	}
	if missing := network.MissingKeys(); len(missing) > 0 {
		return fmt.Errorf("%s: %s", publishOfferName,
			config.UnusableNetwork{Driver: network.Driver, Missing: missing})
	}

	req, err := publicationFromFlags(network.Driver, *merchant, *rateBPS, *fixedMinor, *currency, *shareBPS,
		*validFrom, *validTo, *conditions, *exclusions, *deeplink, *replaces)
	if err != nil {
		return err
	}

	pool, err := platformdb.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The adapter the click-out will build this band's redirects with,
	// resolved exactly as the server resolves it. A deployment whose
	// publisher account is not connected yet cannot try the template, and
	// is told what to run first.
	adapter, _, err := connectNetwork(ctx, network, pool)
	var off *ingestionOff
	switch {
	case errors.As(err, &off):
		return fmt.Errorf("%s: %s - run %s first, because the band's template is tried against the adapter that will build its redirects",
			publishOfferName, off.reason, connectNetworkName)
	case err != nil:
		return err
	}
	publisher, err := catalogue.NewPublisher(adapter)
	if err != nil {
		return err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", publishOfferName, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	published, err := publisher.Publish(ctx, tx, req)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%s: %w", publishOfferName, err)
	}
	return reportPublication(stdout, req, published)
}

// publicationFromFlags turns the flags into a request, refusing every
// combination that does not describe one band. The refusals are about the
// FLAGS - which was given, which was missing - and say so in the flag's own
// name; what the values then mean is the publisher's to refuse.
func publicationFromFlags(driver, merchant string, rateBPS int, fixedMinor int64, currency string, shareBPS int,
	validFrom, validTo, conditions, exclusions, deeplink, replaces string) (catalogue.Publication, error) {
	var req catalogue.Publication
	if strings.TrimSpace(merchant) == "" {
		return req, fmt.Errorf("%s: -merchant is required: the network's own id for the retailer", publishOfferName)
	}
	if strings.TrimSpace(deeplink) == "" {
		return req, fmt.Errorf("%s: -deeplink is required: the tracking URL the redirect is built from", publishOfferName)
	}

	switch {
	case rateBPS != 0 && fixedMinor != 0:
		return req, fmt.Errorf("%s: -rate-bps and -fixed-minor are two shapes of one rate; give one", publishOfferName)
	case rateBPS == 0 && fixedMinor == 0:
		return req, fmt.Errorf("%s: no rate: give -rate-bps (1500 is 15%%) or -fixed-minor with -currency", publishOfferName)
	case rateBPS != 0:
		if currency != "" {
			return req, fmt.Errorf("%s: -currency goes with -fixed-minor; a percentage has none", publishOfferName)
		}
		if rateBPS < 0 || rateBPS > int(money.BasisPointsScale) {
			return req, fmt.Errorf("%s: -rate-bps %d is outside 1..%d", publishOfferName, rateBPS, money.BasisPointsScale)
		}
		req.Rate = catalogue.RateBand{Kind: catalogue.RatePercent, Percent: money.BasisPoints(rateBPS)}
	default:
		if currency == "" {
			return req, fmt.Errorf("%s: -fixed-minor needs -currency: minor units of what?", publishOfferName)
		}
		if fixedMinor < 0 {
			// money.New allows a negative amount - a posting needs one - but
			// a network does not pay a negative commission, and letting it
			// through here would have the publisher refuse it after a
			// database round trip instead of the flag refusing it now.
			return req, fmt.Errorf("%s: -fixed-minor %d is not an amount a network pays", publishOfferName, fixedMinor)
		}
		// Upper-cased before it is judged: ISO 4217 codes are upper-case by
		// definition and "eur" cannot mean anything but EUR, so the only
		// effect of refusing it would be a second run.
		code, err := money.ParseCurrency(strings.ToUpper(strings.TrimSpace(currency)))
		if err != nil {
			return req, fmt.Errorf("%s: -currency: %w", publishOfferName, err)
		}
		amount, err := money.New(fixedMinor, code)
		if err != nil {
			return req, fmt.Errorf("%s: -fixed-minor: %w", publishOfferName, err)
		}
		req.Rate = catalogue.RateBand{Kind: catalogue.RateFixed, Fixed: amount}
	}

	if shareBPS < 0 || shareBPS > int(money.BasisPointsScale) {
		return req, fmt.Errorf("%s: -share-bps %d is outside 0..%d", publishOfferName, shareBPS, money.BasisPointsScale)
	}
	from, err := parseInstantFlag("-valid-from", validFrom)
	if err != nil {
		return req, err
	}
	to, err := parseInstantFlag("-valid-to", validTo)
	if err != nil {
		return req, err
	}
	var replaced uuid.UUID
	if replaces != "" {
		replaced, err = uuid.Parse(replaces)
		if err != nil {
			return req, fmt.Errorf("%s: -replaces %s is not an offer id: %w", publishOfferName, strconv.Quote(replaces), err)
		}
	}

	req.Network = networks.NetworkID(driver)
	req.MerchantExternalID = merchant
	req.MemberShare = money.BasisPoints(shareBPS)
	req.Conditions = conditions
	req.Exclusions = exclusions
	req.ValidFrom = from
	req.ValidTo = to
	req.DeeplinkTemplate = deeplink
	req.Replaces = replaced
	return req, nil
}

// parseInstantFlag reads a date flag in the layouts connect-network
// accepts, allowing it to be absent.
func parseInstantFlag(name, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range backfillFromLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%s: %s %s is not a date: write 2026-09-08 or an RFC3339 instant",
		publishOfferName, name, strconv.Quote(value))
}

// reportPublication prints what the route now carries, which is read back
// from the database rather than echoed from the flags.
func reportPublication(stdout io.Writer, req catalogue.Publication, p catalogue.Published) error {
	lines := []string{
		fmt.Sprintf("published band %s on %s at %s (%s, member share %s)",
			p.OfferID, strconv.Quote(p.MerchantSlug), strconv.Quote(req.Network.String()),
			describeRate(req.Rate), describeShare(req.MemberShare)),
		fmt.Sprintf("  route           %s%s", p.RouteID, preferredNote(p.Preferred)),
		fmt.Sprintf("  in force        %s", describeWindow(p.ValidFrom, p.ValidTo)),
	}
	if p.Replaced != uuid.Nil {
		lines = append(lines, fmt.Sprintf("  replaced        %s, closed at %s", p.Replaced, p.ValidFrom.UTC().Format(time.RFC3339)))
	}
	lines = append(lines, fmt.Sprintf("  now in force    %d band(s) on this route at %s:", len(p.InForce), p.ValidFrom.UTC().Format(time.RFC3339)))
	for _, band := range p.InForce {
		marker := "  "
		if band.ID == p.OfferID {
			marker = "* "
		}
		lines = append(lines, fmt.Sprintf("                  %s%s  %s, member share %s, %s",
			marker, band.ID, describeRate(band.Rate), describeShare(band.MemberShare), describeWindow(band.ValidFrom, band.ValidTo)))
	}
	lines = append(lines, fmt.Sprintf("  click through   POST /api/v1/cashback/clickouts {\"offer_id\":%s}", strconv.Quote(p.OfferID.String())))
	_, err := fmt.Fprintln(stdout, strings.Join(lines, "\n"))
	return err
}

// describeRate renders a band the way an operator typed it, so the report
// and the flag agree to the digit.
func describeRate(rate catalogue.RateBand) string {
	switch rate.Kind {
	case catalogue.RatePercent:
		return fmt.Sprintf("%d bps = %s of the sale", rate.Percent, describeShare(rate.Percent))
	case catalogue.RateFixed:
		return fmt.Sprintf("%d minor = %s per sale", rate.Fixed.Minor, rate.Fixed)
	default:
		return string(rate.Kind)
	}
}

// describeShare renders basis points as a percentage with two decimals,
// exactly - 6000 is "60.00%" - without a float anywhere near it.
func describeShare(bps money.BasisPoints) string {
	return fmt.Sprintf("%d.%02d%%", int(bps)/100, int(bps)%100)
}

// describeWindow renders a validity window, an open end as the word.
func describeWindow(from, to time.Time) string {
	end := "open"
	if !to.IsZero() {
		end = to.UTC().Format(time.RFC3339)
	}
	return from.UTC().Format(time.RFC3339) + " -> " + end
}

// preferredNote is the one thing about a route an operator would not
// otherwise learn until a member could not find the band.
func preferredNote(preferred bool) string {
	if preferred {
		return " (the preferred route: the merchant page lists this band)"
	}
	return " (NOT the preferred route: the band is clickable by id and the merchant page does not list it)"
}
