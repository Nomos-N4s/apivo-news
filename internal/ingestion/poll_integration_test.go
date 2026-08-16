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
// have seeded) completes in test time.
func newTestPoller(pool *pgxpool.Pool) *ingestion.Poller {
	return ingestion.NewPoller(
		slog.New(slog.DiscardHandler),
		pool,
		ingestion.PollConfig{
			Interval: time.Hour, // Run is never called; PollOnce ignores it
			Spacing:  time.Millisecond,
			Fetch: ingestion.FetchConfig{
				Timeout:     5 * time.Second,
				MaxAttempts: 1,
			},
		},
	)
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
	ETag         string
	LastModified string
	LastPolledAt *time.Time
	Error        *string
	Retrieved    int
	Duplicates   int
}

func readPollState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) pollState {
	t.Helper()
	var s pollState
	err := pool.QueryRow(context.Background(),
		`select etag, last_modified, last_polled_at, last_poll_error, last_poll_retrieved, last_poll_duplicates
		   from source where id = $1`, id.String(),
	).Scan(&s.ETag, &s.LastModified, &s.LastPolledAt, &s.Error, &s.Retrieved, &s.Duplicates)
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

func TestPollCycleRetryAfterDefersOnlyThatSource(t *testing.T) {
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
		w.Header().Set("Retry-After", "20")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	t.Cleanup(limitedServer.Close)
	steady := &recordingFeed{body: rssFeed("Σταθερό κείμενο (" + uuid.NewString() + ")")}
	steadyServer := httptest.NewServer(steady)
	t.Cleanup(steadyServer.Close)

	limitedID := seedPollSource(t, pool, limitedServer.URL)
	seedPollSource(t, pool, steadyServer.URL)
	poller := newTestPoller(pool)

	// First cycle: the rate-limited source answers 429 with Retry-After,
	// which defers it - it alone - past the next cycle.
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

	// Second cycle, beginning well before the 20s wait has passed: the
	// deferred source is not asked again, the other one is.
	if _, err := poller.PollOnce(context.Background()); err != nil {
		t.Fatalf("second PollOnce() error: %v", err)
	}
	if got := limitedRequests(); got != 1 {
		t.Errorf("rate-limited source requests after second cycle = %d, want still 1: Retry-After defers it", got)
	}
	if got := len(steady.recorded()); got != 2 {
		t.Errorf("steady source requests after second cycle = %d, want 2: only the limited source is deferred", got)
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
