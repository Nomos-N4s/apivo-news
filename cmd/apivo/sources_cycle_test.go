package main

// The whole point of the cycle block on the sources screen (#86), asserted
// across modules: the numbers GET /api/v1/editorial/sources reports come
// from the poll state a real poll wrote, not from anywhere else. The write
// path is ingestion's, the read path editorial's, and the arch test
// forbids either importing the other - so the one place they may meet is
// here, the composition root, exactly as with the front-page flow test.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/ingestion"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// cycleBody is the slice of the sources payload this test reads.
type cycleBody struct {
	Cycle struct {
		Retrieved         int64    `json:"retrieved"`
		DuplicatesSkipped int64    `json:"duplicates_skipped"`
		Failures          []string `json:"failures"`
	} `json:"cycle"`
}

// readCycle asks the editorial endpoint for the cycle, the way the screen
// does.
func readCycle(t *testing.T, h http.Handler) cycleBody {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/editorial/sources?limit=1", nil)
	req.Header.Set("Authorization", "Bearer probe")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET sources = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body cycleBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshalling sources body: %v", err)
	}
	return body
}

// fixtureFeedURL makes one test server's address unique to this seeding.
//
// The seeded rows below are rolled back, so they leak nothing - but
// source_url_key is unique across the whole table, and other suites seed
// live server addresses that COMMIT. httptest.NewServer takes an ephemeral
// port and the kernel reuses ports, so a database used over many runs
// accumulates loopback addresses until this test draws one already on
// record and the seeding fails on a unique violation with nothing wrong in
// the code under test. Rolling back protects the other suites from this
// one; it does not protect this one from them. In CI, where the database is
// new every time, it never happens.
//
// A query parameter rather than a path segment: this test fetches the
// server's own URL directly and never reads the seeded one back, so the
// suffix has to travel nowhere - but a query keeps the path the feed's own,
// which is what a reader comparing the two would expect.
func fixtureFeedURL(t *testing.T, feedURL, suffix string) string {
	t.Helper()
	parsed, err := url.Parse(feedURL)
	if err != nil {
		t.Fatalf("parsing the feed URL %q: %v", feedURL, err)
	}
	query := parsed.Query()
	query.Set("fixture", suffix)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// seedCycleSource inserts one source inside the transaction.
func seedCycleSource(ctx context.Context, t *testing.T, tx pgx.Tx, name, feedURL string) uuid.UUID {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'el', 'GR', 'Extract and link permitted (cycle test)') returning id`,
		name, feedURL).Scan(&id); err != nil {
		t.Fatalf("seed source %s: %v", name, err)
	}
	return uuid.MustParse(id)
}

// TestTheCycleCountsComeFromThePollState polls one fake feed - a real
// fetch, a real store of each item, a real poll-state write - and then
// asserts the endpoint's cycle reports exactly that poll: what was
// retrieved, what the content fingerprint deduplicated (FR-014), and which
// feed failed, by name. Everything runs in one REPEATABLE READ transaction
// that is rolled back: the frozen snapshot is what makes delta assertions
// exact on a source table shared with every other suite.
func TestTheCycleCountsComeFromThePollState(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the poll-to-cycle chain")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `set transaction isolation level repeatable read`); err != nil {
		t.Fatalf("setting isolation: %v", err)
	}

	h := editorial.NewHandler(discardLogger(), editorial.NewPGStore(tx), alwaysEditor{})
	baseline := readCycle(t, h)

	// The fake feed: four items, one of them an exact repeat, so a single
	// poll produces both counters - three stored, one recognised by the
	// content fingerprint as already on record.
	item := func(n int, body string) string {
		return fmt.Sprintf(`<item><title>Στοιχείο %d</title><link>https://origin.example.test/cycle/%d</link><description>%s</description></item>`, n, n, body)
	}
	repeated := `<item><title>Επανάληψη</title><link>https://origin.example.test/cycle/again</link><description>Το ίδιο σώμα, δεύτερη φορά.</description></item>`
	feedXML := `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>Cycle Feed</title>` +
		item(1, "Πρώτο σώμα του κύκλου.") + item(2, "Δεύτερο σώμα του κύκλου.") +
		repeated + repeated +
		`</channel></rss>`
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(feedXML))
	}))
	t.Cleanup(healthy.Close)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the feed is on fire", http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)

	suffix := randomHex(t)
	healthyName := "Cycle Healthy " + suffix
	failingName := "Cycle Failing " + suffix
	pausedName := "Cycle Paused " + suffix
	healthyID := seedCycleSource(ctx, t, tx, healthyName, fixtureFeedURL(t, healthy.URL, suffix))
	failingID := seedCycleSource(ctx, t, tx, failingName, fixtureFeedURL(t, failing.URL, suffix))
	pausedID := seedCycleSource(ctx, t, tx, pausedName, fixtureFeedURL(t, failing.URL+"/paused", suffix))

	// One poll of the healthy feed: fetch, store every item, record the
	// outcome - the same three steps the poll loop performs, over the same
	// stores, inside this transaction.
	fetchCfg := ingestion.FetchConfig{Timeout: 5 * time.Second, MaxAttempts: 1}
	result, err := fetchCfg.Fetch(ctx, healthy.URL, ingestion.Validators{})
	if err != nil {
		t.Fatalf("fetching the fake feed: %v", err)
	}
	itemStore := ingestion.NewStore(tx)
	var retrieved, duplicates int
	for _, item := range result.Items {
		stored, err := itemStore.RecordRetrieval(ctx, healthyID, item)
		if err != nil {
			t.Fatalf("storing item %q: %v", item.Title, err)
		}
		if stored.Duplicate {
			duplicates++
		} else {
			retrieved++
		}
	}
	if retrieved != 3 || duplicates != 1 {
		t.Fatalf("poll stored (%d, %d), want (3, 1) - the repeated item must dedupe", retrieved, duplicates)
	}
	sourceStore := ingestion.NewSourceStore(tx)
	if err := sourceStore.RecordPollOutcome(ctx, healthyID, ingestion.PollOutcome{
		Retrieved: retrieved, Duplicates: duplicates,
	}); err != nil {
		t.Fatalf("recording the healthy outcome: %v", err)
	}

	// One poll of the failing feed: the fetch error is the outcome.
	if _, err := fetchCfg.Fetch(ctx, failing.URL, ingestion.Validators{}); err == nil {
		t.Fatal("fetching the failing feed succeeded; the fixture is broken")
	} else if err := sourceStore.RecordPollOutcome(ctx, failingID, ingestion.PollOutcome{Error: err.Error()}); err != nil {
		t.Fatalf("recording the failing outcome: %v", err)
	}

	// The paused feed carries loud counters and a failure - and then it is
	// paused, so none of it may reach the cycle: paused state describes a
	// poll no longer running.
	if err := sourceStore.RecordPollOutcome(ctx, pausedID, ingestion.PollOutcome{
		Retrieved: 99, Duplicates: 99, Error: "a failure the cycle must not report",
	}); err != nil {
		t.Fatalf("recording the paused outcome: %v", err)
	}
	if _, err := tx.Exec(ctx, `update source set active = false where id = $1`, pausedID.String()); err != nil {
		t.Fatalf("pausing the source: %v", err)
	}

	cycle := readCycle(t, h)
	if got := cycle.Cycle.Retrieved - baseline.Cycle.Retrieved; got != 3 {
		t.Errorf("retrieved grew by %d, want 3 - the poll's own reading", got)
	}
	if got := cycle.Cycle.DuplicatesSkipped - baseline.Cycle.DuplicatesSkipped; got != 1 {
		t.Errorf("duplicates_skipped grew by %d, want the 1 the fingerprint caught", got)
	}
	if !slices.Contains(cycle.Cycle.Failures, failingName) {
		t.Errorf("failures = %v, want the failing feed named %q", cycle.Cycle.Failures, failingName)
	}
	if slices.Contains(cycle.Cycle.Failures, healthyName) || slices.Contains(cycle.Cycle.Failures, pausedName) {
		t.Errorf("failures = %v, want neither the healthy nor the paused feed in it", cycle.Cycle.Failures)
	}
	// Exactly one failure joined the frozen snapshot's list: the name, not
	// the error prose - the recorded error stays on the row.
	if got := len(cycle.Cycle.Failures) - len(baseline.Cycle.Failures); got != 1 {
		t.Errorf("failures grew by %d, want exactly the one failing feed", got)
	}
}

// TestTheCycleSeedSurvivesAnAddressAlreadyOnRecord is the reproduction of
// the failure fixtureFeedURL exists to stop: another suite has committed a
// source at a loopback address, this run's httptest server draws that same
// port back, and the seeding dies on source_url_key with nothing wrong in
// the code under test.
//
// Both rows go in one transaction that is rolled back, so the occupied
// address is real to the unique index and to nothing else - the test cannot
// itself become the litter it is about.
func TestTheCycleSeedSurvivesAnAddressAlreadyOnRecord(t *testing.T) {
	t.Parallel()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the seeding")
	}
	if err := db.Migrate(dsn); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(server.Close)

	suffix := randomHex(t)
	// Whoever got here first: the bare address, exactly as the pre-fix
	// seeders wrote it.
	seedCycleSource(ctx, t, tx, "Occupier "+suffix, server.URL)

	// The same address, drawn again by this run. Before fixtureFeedURL this
	// second seeding failed on source_url_key.
	seedCycleSource(ctx, t, tx, "Cycle Occupied "+suffix, fixtureFeedURL(t, server.URL, suffix))
}
