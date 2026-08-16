package ingestion_test

// Cycle-level tests for the poll loop: real httptest feed servers on one
// side, the real migrated store on the other (storePool skips when no
// DATABASE_URL is configured). Every test drives PollOnce directly - the
// exported single cycle - so nothing here waits on a ticker.
//
// Deliberately NOT parallel: a cycle walks every active source in the
// shared database, so two of these tests running at once would poll each
// other's servers and corrupt each other's request counts. Each test
// deactivates its sources on cleanup for the same reason.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/ingestion"
)

// newTestPoller builds a poller that fails fast and spaces tightly: one
// fetch attempt per source and a millisecond between sources, so a cycle
// over test servers (and over any unreachable leftovers other suites may
// have seeded) completes in test time. The hour-long interval is also the
// freshness gate: a source polled once is not polled again until a test
// backdates it with agePollState, which is how each test drives exactly
// the cycles it means to.
func newTestPoller(pool *pgxpool.Pool) *ingestion.Poller {
	return ingestion.NewPoller(
		slog.New(slog.DiscardHandler),
		pool,
		ingestion.PollConfig{
			Interval: time.Hour, // Run is never called; PollOnce gates on it
			Spacing:  time.Millisecond,
			Fetch: ingestion.FetchConfig{
				Timeout:     5 * time.Second,
				MaxAttempts: 1,
			},
		},
	)
}

// agePollState backdates a source's last attempt past the freshness gate,
// so the next cycle finds it due again. It touches last_polled_at only:
// a standing next_poll_not_before deferral is deliberately left in place,
// which is what lets a test tell the two gates apart.
func agePollState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`update source set last_polled_at = now() - interval '2 hours' where id = $1`,
		id.String()); err != nil {
		t.Fatalf("backdating last_polled_at: %v", err)
	}
}

// seedPollSource commits an active source pointing at the given feed URL
// and deactivates it on cleanup, so later tests' cycles do not dial a
// server that no longer exists.
func seedPollSource(t *testing.T, pool *pgxpool.Pool, feedURL string) uuid.UUID {
	t.Helper()
	suffix := uuid.NewString()
	var id string
	err := pool.QueryRow(context.Background(),
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'el', 'GR', $3) returning id`,
		"Poll Test Feed "+suffix, feedURL, "Extract and link permitted per feed terms ("+suffix+")",
	).Scan(&id)
	if err != nil {
		t.Fatalf("seeding source: %v", err)
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("parsing source id: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`update source set active = false where id = $1`, id); err != nil {
			t.Errorf("deactivating source: %v", err)
		}
	})
	return parsed
}

// pollState is the source row's poll columns, as one readable value.
type pollState struct {
	ETag              string
	LastModified      string
	LastPolledAt      *time.Time
	Error             *string
	Retrieved         int
	Duplicates        int
	NextPollNotBefore *time.Time
}

func readPollState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) pollState {
	t.Helper()
	var s pollState
	err := pool.QueryRow(context.Background(),
		`select etag, last_modified, last_polled_at, last_poll_error, last_poll_retrieved, last_poll_duplicates, next_poll_not_before
		   from source where id = $1`, id.String(),
	).Scan(&s.ETag, &s.LastModified, &s.LastPolledAt, &s.Error, &s.Retrieved, &s.Duplicates, &s.NextPollNotBefore)
	if err != nil {
		t.Fatalf("reading poll state: %v", err)
	}
	return s
}

func countSourceItems(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`select count(*) from source_item where source_id = $1`, id.String()).Scan(&n)
	if err != nil {
		t.Fatalf("counting source items: %v", err)
	}
	return n
}

// rssFeed renders a minimal RSS 2.0 document with one item per body.
func rssFeed(bodies ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>Poll Test</title>`)
	for i, body := range bodies {
		fmt.Fprintf(&b,
			`<item><title>Item %d</title><link>https://origin.example.test/%d</link><description>%s</description></item>`,
			i, i, body)
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

// recordingFeed serves the same document on every request, stamping the
// configured validator headers, and keeps every request's conditional
// headers for the test to read.
type recordingFeed struct {
	mu       sync.Mutex
	requests []http.Header
	body     string
	etag     string
	lastMod  string
}

func (f *recordingFeed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Header.Clone())
	f.mu.Unlock()
	if f.etag != "" {
		w.Header().Set("ETag", f.etag)
	}
	if f.lastMod != "" {
		w.Header().Set("Last-Modified", f.lastMod)
	}
	_, _ = io.WriteString(w, f.body)
}

func (f *recordingFeed) recorded() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]http.Header(nil), f.requests...)
}

func TestPollCycleRetrievesThenDeduplicates(t *testing.T) {
	pool := storePool(t)
	feed := &recordingFeed{
		body:    rssFeed("Πρώτο κείμενο ("+uuid.NewString()+")", "Δεύτερο κείμενο ("+uuid.NewString()+")"),
		etag:    `"v1"`,
		lastMod: "Mon, 02 Jun 2025 09:30:00 GMT",
	}
	server := httptest.NewServer(feed)
	t.Cleanup(server.Close)
	sourceID := seedPollSource(t, pool, server.URL)
	poller := newTestPoller(pool)

	ran, err := poller.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("first PollOnce() error: %v", err)
	}
	if !ran {
		t.Fatal("first PollOnce() did not run: the advisory lock should have been free")
	}

	state := readPollState(t, pool, sourceID)
	if state.Retrieved != 2 || state.Duplicates != 0 {
		t.Errorf("after first cycle: retrieved = %d, duplicates = %d, want 2 and 0", state.Retrieved, state.Duplicates)
	}
	if state.Error != nil {
		t.Errorf("after first cycle: last_poll_error = %q, want NULL", *state.Error)
	}
	if state.LastPolledAt == nil {
		t.Error("after first cycle: last_polled_at is NULL")
	}
	// The source's own tokens are stored for the next conditional GET.
	if state.ETag != `"v1"` || state.LastModified != feed.lastMod {
		t.Errorf("stored validators = (%q, %q), want (%q, %q)", state.ETag, state.LastModified, `"v1"`, feed.lastMod)
	}
	if n := countSourceItems(t, pool, sourceID); n != 2 {
		t.Errorf("source_item rows = %d, want 2", n)
	}
	// The items went through the provenance write path: snapshot present,
	// item.retrieved event committed with each row (I-2, I-4).
	var withProvenance int
	err = pool.QueryRow(context.Background(),
		`select count(*) from source_item si
		  where si.source_id = $1
		    and btrim(si.licence_snapshot) <> ''
		    and exists (select 1 from domain_event e
		                 where e.type = 'item.retrieved'
		                   and e.payload->>'source_item_id' = si.id::text)`,
		sourceID.String()).Scan(&withProvenance)
	if err != nil {
		t.Fatalf("checking provenance: %v", err)
	}
	if withProvenance != 2 {
		t.Errorf("items with snapshot and item.retrieved event = %d, want 2", withProvenance)
	}

	// Second cycle: the same document again. The stored validators must
	// arrive at the server, and every item is now a duplicate (FR-014).
	// The source is backdated first - the freshness gate would otherwise
	// rule a just-polled source out, which is its job.
	agePollState(t, pool, sourceID)
	if _, err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("second PollOnce() error: %v", err)
	}
	requests := feed.recorded()
	if len(requests) != 2 {
		t.Fatalf("feed requests = %d, want 2", len(requests))
	}
	if got := requests[0].Get("If-None-Match"); got != "" {
		t.Errorf("first request carried If-None-Match %q, want none: nothing was stored yet", got)
	}
	if got := requests[1].Get("If-None-Match"); got != `"v1"` {
		t.Errorf("second request If-None-Match = %q, want %q", got, `"v1"`)
	}
	if got := requests[1].Get("If-Modified-Since"); got != feed.lastMod {
		t.Errorf("second request If-Modified-Since = %q, want %q", got, feed.lastMod)
	}
	state = readPollState(t, pool, sourceID)
	if state.Retrieved != 0 || state.Duplicates != 2 {
		t.Errorf("after second cycle: retrieved = %d, duplicates = %d, want 0 and 2", state.Retrieved, state.Duplicates)
	}
	if n := countSourceItems(t, pool, sourceID); n != 2 {
		t.Errorf("source_item rows after re-poll = %d, want still 2", n)
	}
}

func TestPollCycleNotModifiedRefreshesValidatorsAndZeroesCounters(t *testing.T) {
	pool := storePool(t)
	body := rssFeed("Αναλλοίωτο κείμενο (" + uuid.NewString() + ")")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			// The 304 hands out a fresh token, as a rolling CDN does; the
			// loop must store it or the next poll asks with a stale one.
			w.Header().Set("ETag", `"v2"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	sourceID := seedPollSource(t, pool, server.URL)
	poller := newTestPoller(pool)

	if _, err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("first PollOnce() error: %v", err)
	}
	if state := readPollState(t, pool, sourceID); state.Retrieved != 1 {
		t.Fatalf("after first cycle: retrieved = %d, want 1", state.Retrieved)
	}
	// A stale failure on the row must not survive a poll that succeeded -
	// a 304 included, since NotModified is an outcome and not an error.
	if _, err := pool.Exec(context.Background(),
		`update source set last_poll_error = 'stale failure from an earlier cycle' where id = $1`,
		sourceID.String()); err != nil {
		t.Fatalf("planting stale error: %v", err)
	}

	agePollState(t, pool, sourceID)
	if _, err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("second PollOnce() error: %v", err)
	}
	state := readPollState(t, pool, sourceID)
	if state.Retrieved != 0 || state.Duplicates != 0 {
		t.Errorf("after 304: retrieved = %d, duplicates = %d, want 0 and 0", state.Retrieved, state.Duplicates)
	}
	if state.Error != nil {
		t.Errorf("after 304: last_poll_error = %q, want NULL: not modified is not an error", *state.Error)
	}
	if state.ETag != `"v2"` {
		t.Errorf("after 304: etag = %q, want the refreshed %q", state.ETag, `"v2"`)
	}
	if n := countSourceItems(t, pool, sourceID); n != 1 {
		t.Errorf("source_item rows = %d, want still 1", n)
	}
}

// storeFailureMarker names the body text the induced-failure trigger
// refuses. Only this file's feeds ever carry it, so concurrent suites
// writing source_item rows pass through the trigger untouched.
const storeFailureMarker = "POLL-TEST-STORE-FAILURE"

// installStoreFailure makes the provenance write path die on demand: a
// trigger refuses any source_item whose body carries storeFailureMarker,
// which is what a database failing mid-cycle looks like to recordItems -
// some items committed, then an error. Removed on cleanup.
func installStoreFailure(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`create or replace function poll_test_fail_on_marker() returns trigger
		 language plpgsql as $fail$
		 begin
		     if new.raw_body like '%`+storeFailureMarker+`%' then
		         raise exception 'poll test: induced store failure';
		     end if;
		     return new;
		 end $fail$`); err != nil {
		t.Fatalf("installing failure function: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`drop trigger if exists poll_test_fail on source_item`); err != nil {
		t.Fatalf("clearing stale failure trigger: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`create trigger poll_test_fail before insert on source_item
		 for each row execute function poll_test_fail_on_marker()`); err != nil {
		t.Fatalf("installing failure trigger: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`drop trigger if exists poll_test_fail on source_item`); err != nil {
			t.Errorf("dropping failure trigger: %v", err)
		}
		if _, err := pool.Exec(context.Background(),
			`drop function if exists poll_test_fail_on_marker()`); err != nil {
			t.Errorf("dropping failure function: %v", err)
		}
	})
}

func TestPollCycleStoreFailureKeepsStoredValidators(t *testing.T) {
	pool := storePool(t)
	installStoreFailure(t, pool)

	// Two items under ETag "v2": the first commits, the second dies in the
	// store. The response's validators must NOT be stored - a later 304
	// against them would confirm a document whose items were never fully
	// written, hiding the missing ones for as long as the document stands.
	feed := &recordingFeed{
		body: rssFeed(
			"Πρώτο κείμενο ("+uuid.NewString()+")",
			"Δεύτερο κείμενο "+storeFailureMarker+" ("+uuid.NewString()+")",
		),
		etag: `"v2"`,
	}
	server := httptest.NewServer(feed)
	t.Cleanup(server.Close)
	sourceID := seedPollSource(t, pool, server.URL)
	poller := newTestPoller(pool)

	if _, err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("first PollOnce() error: %v", err)
	}
	state := readPollState(t, pool, sourceID)
	if state.Error == nil {
		t.Fatal("after the store failure: last_poll_error is NULL, want the failure recorded")
	}
	if state.Retrieved != 1 {
		t.Errorf("after the store failure: retrieved = %d, want 1: the honest count of what committed", state.Retrieved)
	}
	if state.ETag != "" {
		t.Errorf("after the store failure: etag = %q, want empty: a failed store must not advance the validators", state.ETag)
	}
	if n := countSourceItems(t, pool, sourceID); n != 1 {
		t.Errorf("source_item rows = %d, want 1: only the first item committed", n)
	}

	// The next cycle must refetch unconditionally, so the source hands the
	// complete document over again; the already-stored item is absorbed by
	// the content_hash dedupe.
	agePollState(t, pool, sourceID)
	if _, err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("second PollOnce() error: %v", err)
	}
	requests := feed.recorded()
	if len(requests) != 2 {
		t.Fatalf("feed requests = %d, want 2", len(requests))
	}
	if got := requests[1].Get("If-None-Match"); got != "" {
		t.Errorf("second request carried If-None-Match %q, want none: a failed store must leave the next fetch unconditional", got)
	}
	if n := countSourceItems(t, pool, sourceID); n != 1 {
		t.Errorf("source_item rows after refetch = %d, want still 1: the dedupe absorbs the committed item", n)
	}
}

func TestPollCycleFailingSourceDoesNotStopOthers(t *testing.T) {
	pool := storePool(t)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporarily broken", http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	healthy := &recordingFeed{body: rssFeed("Υγιές κείμενο (" + uuid.NewString() + ")")}
	healthyServer := httptest.NewServer(healthy)
	t.Cleanup(healthyServer.Close)

	failingID := seedPollSource(t, pool, failing.URL)
	healthyID := seedPollSource(t, pool, healthyServer.URL)
	poller := newTestPoller(pool)

	if _, err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce() error: %v", err)
	}

	failed := readPollState(t, pool, failingID)
	if failed.Error == nil {
		t.Fatal("failing source: last_poll_error is NULL, want the failure recorded")
	}
	if !strings.Contains(*failed.Error, "500") {
		t.Errorf("failing source: last_poll_error = %q, want it to name the 500", *failed.Error)
	}
	if failed.Retrieved != 0 || failed.Duplicates != 0 {
		t.Errorf("failing source: retrieved = %d, duplicates = %d, want 0 and 0", failed.Retrieved, failed.Duplicates)
	}
	if failed.LastPolledAt == nil {
		t.Error("failing source: last_polled_at is NULL, want the attempt recorded")
	}

	// The failure stayed with its source: the healthy one was still polled
	// and its item stored.
	ok := readPollState(t, pool, healthyID)
	if ok.Error != nil {
		t.Errorf("healthy source: last_poll_error = %q, want NULL", *ok.Error)
	}
	if ok.Retrieved != 1 {
		t.Errorf("healthy source: retrieved = %d, want 1", ok.Retrieved)
	}
	if n := countSourceItems(t, pool, healthyID); n != 1 {
		t.Errorf("healthy source: source_item rows = %d, want 1", n)
	}
}

func TestPollCycleRetryAfterDefersOnlyThatSourceAcrossReplicas(t *testing.T) {
	pool := storePool(t)
	var (
		limitedMu    sync.Mutex
		limitedCount int
	)
	limitedRequests := func() int {
		limitedMu.Lock()
		defer limitedMu.Unlock()
		return limitedCount
	}
	limitedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		limitedMu.Lock()
		limitedCount++
		limitedMu.Unlock()
		w.Header().Set("Retry-After", "60")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	t.Cleanup(limitedServer.Close)
	steady := &recordingFeed{body: rssFeed("Σταθερό κείμενο (" + uuid.NewString() + ")")}
	steadyServer := httptest.NewServer(steady)
	t.Cleanup(steadyServer.Close)

	limitedID := seedPollSource(t, pool, limitedServer.URL)
	steadyID := seedPollSource(t, pool, steadyServer.URL)
	poller := newTestPoller(pool)

	// First cycle: the rate-limited source answers 429 with Retry-After,
	// and the ask lands on the row - the deferral is fleet state, not the
	// memory of the process that saw the 429.
	if _, err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("first PollOnce() error: %v", err)
	}
	if got := limitedRequests(); got != 1 {
		t.Fatalf("rate-limited source requests after first cycle = %d, want 1", got)
	}
	state := readPollState(t, pool, limitedID)
	if state.Error == nil {
		t.Fatal("rate-limited source: last_poll_error is NULL, want the refusal recorded")
	}
	if state.NextPollNotBefore == nil {
		t.Fatal("rate-limited source: next_poll_not_before is NULL, want the source's ask persisted")
	}
	if until := time.Until(*state.NextPollNotBefore); until <= 0 || until > time.Minute {
		t.Errorf("next_poll_not_before is %v away, want within the next minute: the source asked for 60s", until)
	}

	// Second cycle from a different replica: a fresh poller on a fresh
	// pool, with no in-process memory of the 429. Both sources are aged
	// past the freshness gate, so what keeps the limited one unasked can
	// only be the persisted deferral - and it defers that source alone.
	agePollState(t, pool, limitedID)
	agePollState(t, pool, steadyID)
	otherPool, err := pgxpool.New(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(otherPool.Close)
	replica := newTestPoller(otherPool)
	if _, err := replica.PollOnce(context.Background()); err != nil {
		t.Fatalf("second PollOnce() error: %v", err)
	}
	if got := limitedRequests(); got != 1 {
		t.Errorf("rate-limited source requests after the other replica's cycle = %d, want still 1: its ask binds the fleet", got)
	}
	if got := len(steady.recorded()); got != 2 {
		t.Errorf("steady source requests after the other replica's cycle = %d, want 2: only the limited source is deferred", got)
	}
}

func TestPollCycleSkipsSourcePolledWithinInterval(t *testing.T) {
	pool := storePool(t)
	feed := &recordingFeed{body: rssFeed("Φρέσκο κείμενο (" + uuid.NewString() + ")")}
	server := httptest.NewServer(feed)
	t.Cleanup(server.Close)
	seedPollSource(t, pool, server.URL)
	poller := newTestPoller(pool)

	if _, err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("first PollOnce() error: %v", err)
	}
	if got := len(feed.recorded()); got != 1 {
		t.Fatalf("feed requests after first cycle = %d, want 1", got)
	}

	// A second cycle beginning well inside the interval - another
	// replica's, in production - runs, and fetches nothing: the advisory
	// lock only prevents overlap, so it is last_polled_at that must keep
	// two interleaved schedules from doubling the requests to the source.
	ran, err := poller.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("second PollOnce() error: %v", err)
	}
	if !ran {
		t.Fatal("second PollOnce() did not run: the advisory lock should have been free")
	}
	if got := len(feed.recorded()); got != 1 {
		t.Errorf("feed requests after an intra-interval cycle = %d, want still 1: one interval is one fetch", got)
	}
}

func TestPollAdvisoryLockMakesConcurrentCycleANoOp(t *testing.T) {
	pool := storePool(t)
	arrived := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(arrived) })
		<-release
		_, _ = io.WriteString(w, rssFeed("Αργό κείμενο ("+uuid.NewString()+")"))
	}))
	t.Cleanup(func() {
		// Unblock any straggling handler before closing, or Close hangs.
		select {
		case <-release:
		default:
			close(release)
		}
		blocking.Close()
	})
	seedPollSource(t, pool, blocking.URL)

	// Two pools, as two replicas would hold: the advisory lock is
	// session-scoped, so only separate connections can contend for it.
	otherPool, err := pgxpool.New(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(otherPool.Close)

	first := newTestPoller(pool)
	second := newTestPoller(otherPool)

	type outcome struct {
		ran bool
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		ran, err := first.PollOnce(context.Background())
		done <- outcome{ran: ran, err: err}
	}()

	// Wait until the first cycle is provably mid-poll, holding the lock.
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the first cycle never reached the feed server")
	}

	ran, err := second.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("concurrent PollOnce() error: %v", err)
	}
	if ran {
		t.Error("concurrent PollOnce() ran, want it skipped: another replica held the poll lock")
	}

	close(release)
	got := <-done
	if got.err != nil {
		t.Fatalf("first PollOnce() error: %v", got.err)
	}
	if !got.ran {
		t.Error("first PollOnce() reported skipped, want it to have run")
	}

	// The lock is released with the cycle: a fresh cycle on the second
	// pool now runs.
	ran, err = second.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("post-release PollOnce() error: %v", err)
	}
	if !ran {
		t.Error("post-release PollOnce() skipped, want it to run: the lock should have been released")
	}
}
