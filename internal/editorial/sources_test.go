package editorial_test

// Contract tests for GET /api/v1/editorial/sources (#86): the registered
// feeds newest first, the active filter, strict query parsing, and the
// last poll cycle read from the poll state rather than invented.
//
// The database-backed tests run inside one REPEATABLE READ transaction:
// the source table is shared with every other suite touching this
// database, and a frozen snapshot is what lets a keyset walk over it be
// deterministic while neighbours commit.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// sourcesStore answers the source list with canned pages and records what
// the handler asked for.
type sourcesStore struct {
	page  editorial.SourcesPage
	cycle editorial.PollCycle
	err   error

	gotQuery editorial.SourcesQuery
}

func (s *sourcesStore) CreateSource(context.Context, editorial.NewSource) (editorial.Source, error) {
	return editorial.Source{}, errUnexpectedCall
}

func (s *sourcesStore) ListSources(_ context.Context, q editorial.SourcesQuery) (editorial.SourcesPage, error) {
	s.gotQuery = q
	return s.page, s.err
}

func (s *sourcesStore) LastPollCycle(context.Context) (editorial.PollCycle, error) {
	return s.cycle, s.err
}

func (s *sourcesStore) ReviewQueue(context.Context, editorial.QueueQuery) (editorial.QueuePage, error) {
	return editorial.QueuePage{}, errUnexpectedCall
}

func (s *sourcesStore) Approve(context.Context, editorial.NewApproval) (editorial.Article, error) {
	return editorial.Article{}, errUnexpectedCall
}

func (s *sourcesStore) Publish(context.Context, uuid.UUID, uuid.UUID) (editorial.Article, error) {
	return editorial.Article{}, errUnexpectedCall
}

func (s *sourcesStore) Withdraw(context.Context, uuid.UUID, uuid.UUID, string) (editorial.Withdrawal, error) {
	return editorial.Withdrawal{}, errUnexpectedCall
}

func (s *sourcesStore) Provenance(context.Context, uuid.UUID) (editorial.Provenance, error) {
	return editorial.Provenance{}, errUnexpectedCall
}

// sourceListBody is the decoded 200 payload, kept close to the wire shape
// so a renamed field fails the test.
type sourceListBody struct {
	Items []struct {
		ID                 string  `json:"id"`
		Name               string  `json:"name"`
		URL                string  `json:"url"`
		Language           string  `json:"language"`
		Jurisdiction       string  `json:"jurisdiction"`
		LicenceTerms       string  `json:"licence_terms"`
		UsageRule          string  `json:"usage_rule"`
		PermissionEvidence *string `json:"permission_evidence"`
		Active             bool    `json:"active"`
		LastPolledAt       *string `json:"last_polled_at"`
		CreatedAt          string  `json:"created_at"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
	Cycle      struct {
		Retrieved         int64    `json:"retrieved"`
		DuplicatesSkipped int64    `json:"duplicates_skipped"`
		Failures          []string `json:"failures"`
	} `json:"cycle"`
}

func getSources(t *testing.T, h http.Handler, query string) (*httptest.ResponseRecorder, sourceListBody) {
	t.Helper()
	rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/sources"+query, editorToken, "")
	var body sourceListBody
	if rec.Code == http.StatusOK {
		decodeInto(t, rec, &body)
	}
	return rec, body
}

func TestSourcesRequiresAnEditor(t *testing.T) {
	t.Parallel()
	h := newHandler(t, errStore{err: errUnexpectedCall})

	t.Run("401 without a token", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/sources", "", "")
		wantProblem(t, rec, http.StatusUnauthorized, "bearer token is required")
	})
	t.Run("403 for a non-editor", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/sources", readerToken, "")
		wantProblem(t, rec, http.StatusForbidden, "editor role")
	})
}

func TestSourcesRejectsAnUnknownQueryParameter(t *testing.T) {
	t.Parallel()
	// The store must never be reached by a request that was not accepted.
	h := newHandler(t, errStore{err: errUnexpectedCall})
	rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/sources?lang=el", editorToken, "")
	wantProblem(t, rec, http.StatusBadRequest, "unknown query parameter")
}

func TestSourcesRejectsARepeatedParameter(t *testing.T) {
	t.Parallel()
	h := newHandler(t, errStore{err: errUnexpectedCall})

	cases := []struct{ name, query string }{
		// Two values for one filter are two contradictory requests; Get
		// would silently answer the first, which reads as acceptance.
		{name: "active", query: "?active=true&active=false"},
		{name: "limit", query: "?limit=10&limit=20"},
		{name: "cursor", query: "?cursor=a&cursor=b"},
	}
	for _, tc := range cases {
		t.Run("400 on a repeated "+tc.name, func(t *testing.T) {
			t.Parallel()
			rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/sources"+tc.query, editorToken, "")
			wantProblem(t, rec, http.StatusBadRequest, "supplied 2 times")
		})
	}
}

func TestSourcesQueryValidation(t *testing.T) {
	t.Parallel()
	h := newHandler(t, errStore{err: errUnexpectedCall})

	cases := []struct{ name, query, detail string }{
		{name: "a non-boolean active", query: "?active=1", detail: "active must be true or false"},
		{name: "an empty active", query: "?active=", detail: "active must be true or false"},
		{name: "a limit above the cap", query: "?limit=101", detail: "between 1 and 100"},
		{name: "a limit of zero", query: "?limit=0", detail: "between 1 and 100"},
		{name: "an unparseable limit", query: "?limit=ten", detail: "between 1 and 100"},
		{name: "a cursor this endpoint never issued", query: "?cursor=%21%21", detail: "cursor is not one this endpoint issued"},
	}
	for _, tc := range cases {
		t.Run("400 on "+tc.name, func(t *testing.T) {
			t.Parallel()
			rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/sources"+tc.query, editorToken, "")
			wantProblem(t, rec, http.StatusBadRequest, tc.detail)
		})
	}
}

// TestSourcesResponseShape pins the wire shape - the row under the shared
// `url` name, permission_evidence present behind the gate, the cycle, and
// the cursor round trip.
func TestSourcesResponseShape(t *testing.T) {
	t.Parallel()
	evidence := "written permission of 2026-05-01, on file"
	polled := time.Date(2026, 8, 14, 6, 12, 0, 0, time.UTC)
	created := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	next := editorial.SourceCursor{CreatedAt: created, ID: uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")}

	store := &sourcesStore{
		page: editorial.SourcesPage{
			Items: []editorial.ListedSource{
				{
					ID:                 uuid.MustParse("11111111-2222-4333-8444-555555555555"),
					Name:               "Shape Feed",
					URL:                "https://example.test/feed/shape",
					Language:           "el",
					Jurisdiction:       "GR",
					LicenceTerms:       "Extract and link permitted",
					UsageRule:          "full_text",
					PermissionEvidence: &evidence,
					Active:             true,
					LastPolledAt:       &polled,
					CreatedAt:          created,
				},
				{
					ID:           uuid.MustParse("66666666-7777-4888-8999-aaaaaaaaaaaa"),
					Name:         "Never Polled Feed",
					URL:          "https://example.test/feed/quiet",
					Language:     "de",
					Jurisdiction: "DE",
					LicenceTerms: "Extract and link permitted",
					UsageRule:    "extract_and_link",
					Active:       false,
					CreatedAt:    created,
				},
			},
			NextCursor: &next,
		},
		cycle: editorial.PollCycle{Retrieved: 14, Duplicates: 9, Failures: []string{"Broken Feed"}},
	}
	h := editorial.NewHandler(discardLogger(), store, fakeAuth{})

	rec, body := getSources(t, h, "?active=true&limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if store.gotQuery.Active == nil || !*store.gotQuery.Active {
		t.Errorf("active filter reaching the store = %v, want true", store.gotQuery.Active)
	}
	if store.gotQuery.Limit != 2 {
		t.Errorf("limit reaching the store = %d, want 2", store.gotQuery.Limit)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}

	first := body.Items[0]
	if first.URL != "https://example.test/feed/shape" {
		t.Errorf("url = %q, want the feed URL under the shared name", first.URL)
	}
	if first.LicenceTerms != "Extract and link permitted" {
		t.Errorf("licence_terms = %q, want the current terms", first.LicenceTerms)
	}
	if first.PermissionEvidence == nil || *first.PermissionEvidence != evidence {
		t.Errorf("permission_evidence = %v, want it served behind the gate", first.PermissionEvidence)
	}
	if first.LastPolledAt == nil || *first.LastPolledAt != "2026-08-14T06:12:00Z" {
		t.Errorf("last_polled_at = %v, want RFC 3339 UTC", first.LastPolledAt)
	}

	second := body.Items[1]
	if second.PermissionEvidence != nil {
		t.Errorf("permission_evidence = %q, want null when none is on record", *second.PermissionEvidence)
	}
	if second.LastPolledAt != nil {
		t.Errorf("last_polled_at = %q, want null for a never-polled feed", *second.LastPolledAt)
	}
	if second.Active {
		t.Error("active = true, want the paused feed reported paused")
	}

	if body.Cycle.Retrieved != 14 || body.Cycle.DuplicatesSkipped != 9 {
		t.Errorf("cycle = (%d, %d), want the poll-state sums (14, 9)", body.Cycle.Retrieved, body.Cycle.DuplicatesSkipped)
	}
	if len(body.Cycle.Failures) != 1 || body.Cycle.Failures[0] != "Broken Feed" {
		t.Errorf("failures = %v, want the failing feed by name", body.Cycle.Failures)
	}

	// The cursor round trip: what next_cursor said comes back as the same
	// keyset position.
	if body.NextCursor == nil {
		t.Fatal("next_cursor is null with a further page on offer")
	}
	if _, _ = getSources(t, h, "?cursor="+*body.NextCursor); store.gotQuery.Cursor == nil {
		t.Fatal("echoed cursor did not reach the store")
	}
	if got := *store.gotQuery.Cursor; !got.CreatedAt.Equal(next.CreatedAt) || got.ID != next.ID {
		t.Errorf("cursor round trip = %+v, want %+v", got, next)
	}
}

// sourcesFixture seeds this test's own registered feeds. Their created_at
// stamps sit in the FUTURE, spaced apart: the list orders on registration
// time, every other suite registers at now(), so these rows are the
// newest the snapshot can hold and the first page is deterministic even
// on a shared table.
type sourcesFixture struct {
	newest, middle, oldest string // ids, newest registration first
	paused                 string // == oldest; registered paused
}

func seedSourcesFixture(ctx context.Context, t *testing.T, tx pgx.Tx) sourcesFixture {
	t.Helper()
	suffix := randomSuffix(t)
	seed := func(name string, hoursAhead int, active bool) string {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx,
			`insert into source (name, url, language_code, jurisdiction, licence_terms, active, created_at)
			 values ($1, $2, 'el', 'GR', 'Extract and link permitted (sources test)', $3, now() + make_interval(hours => $4))
			 returning id`,
			"Sources Test "+name+" "+suffix, "https://example.test/sources/"+suffix+"/"+name, active, hoursAhead).Scan(&id); err != nil {
			t.Fatalf("seed source %s: %v", name, err)
		}
		return id
	}
	f := sourcesFixture{
		newest: seed("newest", 3, true),
		middle: seed("middle", 2, true),
		oldest: seed("oldest", 1, false),
	}
	f.paused = f.oldest
	return f
}

// sourcesTx opens the shared-table transaction the database-backed tests
// run in: migrated, REPEATABLE READ, rolled back.
func sourcesTx(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the source list against Postgres")
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
	// The snapshot freeze that makes a walk over a shared table
	// deterministic: neighbours' commits stay invisible for the duration.
	if _, err := tx.Exec(ctx, `set transaction isolation level repeatable read`); err != nil {
		t.Fatalf("setting isolation: %v", err)
	}
	return ctx, tx
}

func TestSourcesListsRegisteredFeedsNewestFirst(t *testing.T) {
	t.Parallel()
	ctx, tx := sourcesTx(t)
	f := seedSourcesFixture(ctx, t, tx)
	h := newHandler(t, editorial.NewPGStore(tx))

	rec, body := getSources(t, h, "?limit=3")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if len(body.Items) != 3 {
		t.Fatalf("items = %d, want the page filled to its limit", len(body.Items))
	}
	got := []string{body.Items[0].ID, body.Items[1].ID, body.Items[2].ID}
	want := []string{f.newest, f.middle, f.oldest}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("items[%d] = %s, want %s - newest registration first", i, got[i], want[i])
		}
	}

	first := body.Items[0]
	if first.URL == "" || first.Language != "el" || first.Jurisdiction != "GR" {
		t.Errorf("row = %+v, want the registered identity carried whole", first)
	}
	if first.UsageRule != "extract_and_link" || first.PermissionEvidence != nil {
		t.Errorf("licensing = (%q, %v), want the registration default with no evidence", first.UsageRule, first.PermissionEvidence)
	}
	if first.LastPolledAt != nil {
		t.Errorf("last_polled_at = %q, want null for a feed never polled", *first.LastPolledAt)
	}
}

// TestSourcesKeysetPageBoundary walks the whole list two rows at a time:
// next_cursor is null exactly when the list is exhausted, no row repeats,
// and no seeded row is skipped.
func TestSourcesKeysetPageBoundary(t *testing.T) {
	t.Parallel()
	ctx, tx := sourcesTx(t)
	f := seedSourcesFixture(ctx, t, tx)
	h := newHandler(t, editorial.NewPGStore(tx))

	seen := make(map[string]int)
	cursor := ""
	for page := 0; ; page++ {
		if page > 500 {
			t.Fatal("the walk did not terminate; next_cursor never went null")
		}
		query := "?limit=2"
		if cursor != "" {
			query += "&cursor=" + cursor
		}
		rec, body := getSources(t, h, query)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status = %d (body %q)", page, rec.Code, rec.Body.String())
		}
		for _, item := range body.Items {
			seen[item.ID]++
		}
		if body.NextCursor == nil {
			break
		}
		// A cursor was issued, so the walk must go on: an empty page behind
		// a non-null cursor would mean "ask again and find out".
		if len(body.Items) != 2 {
			t.Fatalf("page %d: %d items yet next_cursor is set; a short page is the last page", page, len(body.Items))
		}
		cursor = *body.NextCursor
	}

	for _, id := range []string{f.newest, f.middle, f.oldest} {
		if seen[id] != 1 {
			t.Errorf("row %s appeared %d times across the walk, want exactly once", id, seen[id])
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %s appeared %d times; the keyset must neither skip nor repeat", id, n)
		}
	}
}

// TestSourcesKeysetTieBreakOnIdenticalCreatedAt seeds two sources with the
// SAME created_at and walks the boundary between them one row at a time.
// The `, id desc` tie-break is the exact clause keyset pagination exists
// for: without it the order within the tie is unspecified, and a cursor
// cut between the twins can skip one forever - which the hour-spaced
// fixture above could never catch.
func TestSourcesKeysetTieBreakOnIdenticalCreatedAt(t *testing.T) {
	t.Parallel()
	ctx, tx := sourcesTx(t)
	suffix := randomSuffix(t)

	// now() is the transaction timestamp, identical across both inserts;
	// +5 hours outruns every other suite's now()-stamped registrations, so
	// the twins are the snapshot's newest rows and page one starts on them.
	twins := make([]string, 0, 2)
	for _, name := range []string{"twin-a", "twin-b"} {
		var id string
		if err := tx.QueryRow(ctx,
			`insert into source (name, url, language_code, jurisdiction, licence_terms, active, created_at)
			 values ($1, $2, 'el', 'GR', 'Extract and link permitted (tie-break test)', true, now() + interval '5 hours')
			 returning id`,
			"Sources Tie "+name+" "+suffix, "https://example.test/ties/"+suffix+"/"+name).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		twins = append(twins, id)
	}
	h := newHandler(t, editorial.NewPGStore(tx))

	seen := make(map[string]int)
	var order []string
	cursor := ""
	for page := 0; ; page++ {
		if page > 500 {
			t.Fatal("the walk did not terminate; next_cursor never went null")
		}
		query := "?limit=1"
		if cursor != "" {
			query += "&cursor=" + cursor
		}
		rec, body := getSources(t, h, query)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status = %d (body %q)", page, rec.Code, rec.Body.String())
		}
		for _, item := range body.Items {
			seen[item.ID]++
			order = append(order, item.ID)
		}
		if body.NextCursor == nil {
			break
		}
		cursor = *body.NextCursor
	}

	for _, id := range twins {
		if seen[id] != 1 {
			t.Errorf("twin %s appeared %d times across the walk, want exactly once - the id tie-break is what keeps a cursor cut inside the tie from skipping its sibling", id, seen[id])
		}
	}
	// The declared order inside the tie: id descending, so the twins must
	// arrive larger id first, back to back at the head of the walk.
	bigger, smaller := twins[0], twins[1]
	if smaller > bigger {
		bigger, smaller = smaller, bigger
	}
	if len(order) < 2 || order[0] != bigger || order[1] != smaller {
		t.Errorf("walk began %v, want the twins back to back as (%s, %s) - identical created_at ordered by id descending", order[:min(len(order), 2)], bigger, smaller)
	}
}

// TestSourcesFailuresAreOrderedByName seeds two failing feeds in reverse
// alphabetical order and asserts the cycle reports them sorted: the
// ordering is documented in the schema comment, the contract and the query
// itself, and membership-only assertions would let the `order by name`
// clause vanish without a test noticing.
func TestSourcesFailuresAreOrderedByName(t *testing.T) {
	t.Parallel()
	ctx, tx := sourcesTx(t)
	suffix := randomSuffix(t)

	// Seeded reverse-alphabetically, so an array_agg without its order-by
	// would surface them in insertion order and fail the sort assertion.
	names := []string{"Sources Zeta Failing " + suffix, "Sources Alpha Failing " + suffix}
	for i, name := range names {
		if _, err := tx.Exec(ctx,
			`insert into source (name, url, language_code, jurisdiction, licence_terms, active, last_poll_error, last_polled_at)
			 values ($1, $2, 'el', 'GR', 'Extract and link permitted (failures test)', true, 'connection refused', now())`,
			name, "https://example.test/failures/"+suffix+"/"+strconv.Itoa(i)); err != nil {
			t.Fatalf("seed failing source %q: %v", name, err)
		}
	}
	h := newHandler(t, editorial.NewPGStore(tx))

	rec, body := getSources(t, h, "?limit=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	for _, name := range names {
		if !slices.Contains(body.Cycle.Failures, name) {
			t.Errorf("failures = %v, missing the seeded failing feed %q", body.Cycle.Failures, name)
		}
	}
	if !slices.IsSorted(body.Cycle.Failures) {
		t.Errorf("failures = %v, want them sorted by name so the same broken feeds read the same way on every refresh", body.Cycle.Failures)
	}
}

func TestSourcesActiveFilterSeparatesPausedFeeds(t *testing.T) {
	t.Parallel()
	ctx, tx := sourcesTx(t)
	f := seedSourcesFixture(ctx, t, tx)
	h := newHandler(t, editorial.NewPGStore(tx))

	firstPageIDs := func(query string) map[string]bool {
		t.Helper()
		rec, body := getSources(t, h, query)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		ids := make(map[string]bool, len(body.Items))
		for _, item := range body.Items {
			ids[item.ID] = true
			if query == "?active=true&limit=100" && !item.Active {
				t.Errorf("active=true returned paused row %s", item.ID)
			}
			if query == "?active=false&limit=100" && item.Active {
				t.Errorf("active=false returned active row %s", item.ID)
			}
		}
		return ids
	}

	// The fixture's rows are the newest in the snapshot, so a first page
	// of 100 must hold whichever of them match the filter - and a
	// wrongly-included one would land on this page too, not hide beyond it.
	active := firstPageIDs("?active=true&limit=100")
	if !active[f.newest] || !active[f.middle] {
		t.Error("active=true is missing the fixture's polled feeds")
	}
	if active[f.paused] {
		t.Error("active=true returned the paused feed; the pause switch is the point of the filter")
	}

	paused := firstPageIDs("?active=false&limit=100")
	if !paused[f.paused] {
		t.Error("active=false is missing the fixture's paused feed")
	}
	if paused[f.newest] || paused[f.middle] {
		t.Error("active=false returned a polled feed")
	}
}
