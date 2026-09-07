package main

// The import-catalogue command: what it refuses before a database is opened,
// what its report says, and one run against the seed's scratch database -
// the same catalogue the scheduled job maintains, imported by hand.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	"github.com/Nomos-N4s/apivo-news/internal/platform/scheduler"
)

func TestImportCatalogueRefusesWhatItCannotImport(t *testing.T) {
	t.Parallel()
	const unreachable = "postgres://nobody@127.0.0.1:1/nothing?sslmode=disable"
	for _, tc := range []struct {
		name string
		args []string
		env  func(map[string]string)
		want string
	}{
		{"an argument", []string{importCatalogueName, "linkwise"}, nil, "takes no arguments"},
		{"no network", []string{importCatalogueName}, func(env map[string]string) { delete(env, "NETWORKS") }, "NETWORKS names no network"},
		{"a network missing a key", []string{importCatalogueName}, func(env map[string]string) { delete(env, "NETWORK_FIXTURE_ACCOUNT_ID") }, "NETWORK_FIXTURE_ACCOUNT_ID"},
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

func TestImportCatalogueReportSaysWhatTheRunDidAndWhatIsNowPublishable(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, time.September, 7, 5, 0, 0, 0, time.UTC)

	// A run that withdrew and added none is called out; a route that is not
	// active is counted and not listed; the next command is named.
	var out bytes.Buffer
	err := reportImport(&out, "fixture",
		catalogue.ImportResult{StartedAt: started, Seen: 2, Created: 0, Departed: 3},
		[]importedRoute{{"FIXM-77", "fixture-outdoor-co", "active"}, {"FIXM-41", "fixture-books-ltd", "paused"}})
	if err != nil {
		t.Fatalf("reportImport(): %v", err)
	}
	for _, want := range []string{
		`imported the "fixture" catalogue: 2 retailer(s) seen, 0 new, 3 departed`,
		"started at      2026-09-07T05:00:00Z",
		"LOOK            this run withdrew retailers and added none",
		"routes          2 at this network, 1 active and publishable",
		"FIXM-77                  fixture-outdoor-co",
		"publish a rate  apivo " + publishOfferName + " -merchant",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "fixture-books-ltd") {
		t.Errorf("a route nobody can publish on is listed as if they could:\n%s", out.String())
	}

	// An ordinary run says nothing alarming, and a network with more
	// publishable routes than the report shows says how many it left out
	// rather than scrolling them all past.
	many := make([]importedRoute, 0, importedRoutesShown+3)
	for i := range importedRoutesShown + 3 {
		many = append(many, importedRoute{fmt.Sprintf("P-%03d", i), fmt.Sprintf("shop-%03d", i), "active"})
	}
	out.Reset()
	if err := reportImport(&out, "linkwise", catalogue.ImportResult{StartedAt: started, Seen: 28, Created: 28}, many); err != nil {
		t.Fatalf("reportImport(): %v", err)
	}
	if strings.Contains(out.String(), "LOOK") {
		t.Errorf("a run that added retailers is reported as alarming:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "... and 3 more") || !strings.Contains(out.String(), "P-024 ") || strings.Contains(out.String(), "P-025 ") {
		t.Errorf("the report does not stop at %d routes and say how many follow:\n%s", importedRoutesShown, out.String())
	}

	// A network with nothing publishable is not told to publish.
	out.Reset()
	if err := reportImport(&out, "fixture", catalogue.ImportResult{StartedAt: started}, nil); err != nil || strings.Contains(out.String(), "publish a rate") {
		t.Errorf("an empty catalogue is told to publish a rate on it (err %v):\n%s", err, out.String())
	}
}

// importCatalogue runs the command with the seed's environment, amended by
// the caller, and answers what it printed and what it refused with.
func importCatalogue(t *testing.T, dbURL string, amend func(map[string]string)) (string, error) {
	t.Helper()
	env := seedEnv(dbURL)
	if amend != nil {
		amend(env)
	}
	var out bytes.Buffer
	err := run(context.Background(), []string{importCatalogueName}, func(k string) string { return env[k] }, &out)
	return out.String(), err
}

// TestImportCatalogueOnTheSeededCatalogue is the command end to end: refused
// while no publisher account is connected, refused without a brand, refused
// while the scheduled job holds the lock, and then the fixture's catalogue
// imported by hand and reported - the same rows the seed's import wrote, so
// the run finds everything already there and says so.
//
// It runs in the SEED's scratch database rather than one of its own, for
// the reason the publish-offer test does (#419), and it writes nothing the
// seed's own test would count.
func TestImportCatalogueOnTheSeededCatalogue(t *testing.T) {
	dbURL, pool := seedTestDB(t)
	ctx := context.Background()
	seedOnce(t, dbURL)

	notConnected := func(env map[string]string) { env["NETWORK_FIXTURE_ACCOUNT_ID"] = "nobody-connected" }
	if _, err := importCatalogue(t, dbURL, notConnected); err == nil || !strings.Contains(err.Error(), "run "+connectNetworkName+" first") {
		t.Fatalf("import-catalogue with no connected account = %v, want a refusal naming connect-network", err)
	}

	noBrand := func(env map[string]string) { delete(env, "BRAND_DIR") }
	if _, err := importCatalogue(t, dbURL, noBrand); err == nil || !strings.Contains(err.Error(), "BRAND_DIR unset") {
		t.Fatalf("import-catalogue with no brand = %v, want a refusal naming BRAND_DIR", err)
	}

	// The scheduled job's lock, held from another session: the command
	// must not run a second import underneath it.
	lock, held, err := scheduler.NewAdvisoryLocker(pool, scheduler.LockerConfig{}).TryLock(ctx, catalogue.ImportJobName)
	if err != nil || !held {
		t.Fatalf("taking the import lock for the test: held %v, err %v", held, err)
	}
	_, err = importCatalogue(t, dbURL, nil)
	if releaseErr := lock.Release(ctx); releaseErr != nil {
		t.Fatalf("releasing the import lock: %v", releaseErr)
	}
	if err == nil || !strings.Contains(err.Error(), "importing this catalogue right now") {
		t.Fatalf("import-catalogue under the scheduled job's lock = %v, want a refusal", err)
	}

	out, err := importCatalogue(t, dbURL, nil)
	if err != nil {
		t.Fatalf("import-catalogue: %v (%s)", err, out)
	}
	var merchant string
	if err := pool.QueryRow(ctx,
		`select external_merchant_id from cashback.merchant_network where network_id = 'fixture' and status = 'active'`).Scan(&merchant); err != nil {
		t.Fatalf("finding the fixture's publishable route: %v", err)
	}
	for _, want := range []string{
		`imported the "fixture" catalogue:`,
		"retailer(s) seen, 0 new, 0 departed",
		"routes          3 at this network, 1 active and publishable",
		merchant,
		"publish a rate  apivo " + publishOfferName,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "LOOK") {
		t.Errorf("re-importing an unchanged catalogue is reported as alarming:\n%s", out)
	}
}
