package editorial_test

// Contract tests for POST /api/v1/editorial/articles/{id}/withdrawal
// (T021, FR-016): publication ends, every record remains, and the audit
// says who and why.
//
// The database-backed test asserts the article.withdrawn domain event the
// 0002 trigger writes - exactly one of it, proving this package does not
// emit a duplicate - and closes the loop with T019 by finding the freed
// origin back in the review queue as a correction candidate.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// withdrawalStore answers withdrawals with a canned result and records what
// the handler asked for.
type withdrawalStore struct {
	withdrawal editorial.Withdrawal
	err        error

	gotArticleID uuid.UUID
	gotEditorID  uuid.UUID
	gotReason    string
}

func (s *withdrawalStore) CreateSource(context.Context, editorial.NewSource) (editorial.Source, error) {
	return editorial.Source{}, errUnexpectedCall
}

func (s *withdrawalStore) ReviewQueue(context.Context, editorial.QueueQuery) (editorial.QueuePage, error) {
	return editorial.QueuePage{}, errUnexpectedCall
}

func (s *withdrawalStore) Approve(context.Context, editorial.NewApproval) (editorial.Article, error) {
	return editorial.Article{}, errUnexpectedCall
}

func (s *withdrawalStore) Publish(context.Context, uuid.UUID, uuid.UUID) (editorial.Article, error) {
	return editorial.Article{}, errUnexpectedCall
}

func (s *withdrawalStore) Withdraw(_ context.Context, articleID, editorID uuid.UUID, reason string) (editorial.Withdrawal, error) {
	s.gotArticleID, s.gotEditorID, s.gotReason = articleID, editorID, reason
	return s.withdrawal, s.err
}

func (s *withdrawalStore) Provenance(context.Context, uuid.UUID) (editorial.Provenance, error) {
	return editorial.Provenance{}, errUnexpectedCall
}

// withdrawalBody is the decoded 200 payload, kept close to the contract's
// wire shape so a renamed field fails the test.
type withdrawalBody struct {
	ArticleID   string `json:"article_id"`
	WithdrawnAt string `json:"withdrawn_at"`
	WithdrawnBy string `json:"withdrawn_by"`
	Reason      string `json:"reason"`
}

func postWithdrawal(t *testing.T, h http.Handler, articleID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, h, http.MethodPost, "/api/v1/editorial/articles/"+articleID+"/withdrawal", editorToken, body)
}

func TestWithdrawalAuth(t *testing.T) {
	t.Parallel()
	h := newHandler(t, errStore{err: errUnexpectedCall})
	const id = "11111111-1111-4111-8111-111111111111"
	body := `{"reason":"source retracted the story"}`

	t.Run("401 without a token", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodPost, "/api/v1/editorial/articles/"+id+"/withdrawal", "", body)
		wantProblem(t, rec, http.StatusUnauthorized, "bearer token is required")
	})
	t.Run("403 for a non-editor", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodPost, "/api/v1/editorial/articles/"+id+"/withdrawal", readerToken, body)
		wantProblem(t, rec, http.StatusForbidden, "editor role")
	})
}

func TestWithdrawalValidation(t *testing.T) {
	t.Parallel()
	const id = "11111111-1111-4111-8111-111111111111"

	cases := []struct {
		name      string
		articleID string
		body      string
		detail    string
	}{
		{name: "missing reason", articleID: id, body: `{}`, detail: "reason"},
		{name: "blank reason", articleID: id, body: `{"reason":"   "}`, detail: "reason"},
		{name: "empty reason", articleID: id, body: `{"reason":""}`, detail: "reason"},
		{name: "malformed JSON", articleID: id, body: `{"reason":`, detail: "not valid JSON"},
		{name: "unknown field", articleID: id, body: `{"reason":"r","withdrawn_by":"someone else"}`, detail: "not valid JSON"},
		{name: "path id is not a uuid", articleID: "not-a-uuid", body: `{"reason":"r"}`, detail: "uuid"},
	}
	for _, tc := range cases {
		t.Run("400 on "+tc.name, func(t *testing.T) {
			t.Parallel()
			// The store must never be reached by an invalid request.
			h := newHandler(t, errStore{err: errUnexpectedCall})
			wantProblem(t, postWithdrawal(t, h, tc.articleID, tc.body), http.StatusBadRequest, tc.detail)
		})
	}
}

// TestWithdrawalStoreVerdicts pins the mapping from the database's verdicts
// to the contract's status codes.
func TestWithdrawalStoreVerdicts(t *testing.T) {
	t.Parallel()
	const id = "11111111-1111-4111-8111-111111111111"
	body := `{"reason":"source retracted the story"}`

	cases := []struct {
		name   string
		err    error
		status int
		detail string
	}{
		{name: "404 for an unknown article", err: editorial.ErrArticleNotFound, status: http.StatusNotFound, detail: "no article"},
		{name: "404 for an article that was never published", err: editorial.ErrArticleNotPublished, status: http.StatusNotFound, detail: "no published article"},
		{name: "409 when it is already withdrawn", err: editorial.ErrAlreadyWithdrawn, status: http.StatusConflict, detail: "already withdrawn"},
		{name: "403 when the database refuses a non-editor", err: editorial.ErrNotEditor, status: http.StatusForbidden, detail: "editor role"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := editorial.NewHandler(discardLogger(), &withdrawalStore{err: tc.err}, fakeAuth{})
			wantProblem(t, postWithdrawal(t, h, id, body), tc.status, tc.detail)
		})
	}

	t.Run("500 on an unrecognised failure, with nothing leaked", func(t *testing.T) {
		t.Parallel()
		h := editorial.NewHandler(discardLogger(), &withdrawalStore{err: errors.New("connection torn down")}, fakeAuth{})
		rec := postWithdrawal(t, h, id, body)
		wantProblem(t, rec, http.StatusInternalServerError, "")
		if strings.Contains(rec.Body.String(), "torn down") {
			t.Error("internal error detail leaked to the wire")
		}
	})
}

// TestWithdrawalResponseShape pins the 200 payload and that the withdrawer
// of record is the authenticated editor, never anything from the body.
func TestWithdrawalResponseShape(t *testing.T) {
	t.Parallel()
	articleID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	withdrawnAt := time.Date(2026, 8, 15, 14, 15, 0, 0, time.UTC)

	store := &withdrawalStore{withdrawal: editorial.Withdrawal{
		ArticleID:   articleID,
		WithdrawnAt: withdrawnAt,
		WithdrawnBy: testEditor.ID,
		Reason:      "source retracted the story",
	}}
	h := editorial.NewHandler(discardLogger(), store, fakeAuth{})
	rec := postWithdrawal(t, h, articleID.String(), `{"reason":"  source retracted the story  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body withdrawalBody
	decodeInto(t, rec, &body)
	if body.ArticleID != articleID.String() {
		t.Errorf("article_id = %q, want %q", body.ArticleID, articleID)
	}
	if body.WithdrawnAt != "2026-08-15T14:15:00Z" {
		t.Errorf("withdrawn_at = %q, want RFC 3339 UTC", body.WithdrawnAt)
	}
	if body.WithdrawnBy != testEditor.ID.String() {
		t.Errorf("withdrawn_by = %q, want the authenticated editor %q", body.WithdrawnBy, testEditor.ID)
	}

	if store.gotArticleID != articleID {
		t.Errorf("article id reaching the store = %v, want %v", store.gotArticleID, articleID)
	}
	if store.gotEditorID != testEditor.ID {
		t.Errorf("editor reaching the store = %v, want the authenticated editor %v", store.gotEditorID, testEditor.ID)
	}
	if store.gotReason != "source retracted the story" {
		t.Errorf("reason reaching the store = %q, want it trimmed", store.gotReason)
	}
}

// TestWithdrawalResponseCarriesTheRecordedReason pins that the reason on
// the wire is the one the store recorded, not an echo of the request body.
// The confirmation banner renders this field as its only text, so a
// response without it reports a real, audited, irreversible write as a
// blank box - the exact inversion of the no-success-without-a-record rule.
func TestWithdrawalResponseCarriesTheRecordedReason(t *testing.T) {
	t.Parallel()
	articleID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")

	// The canned record deliberately differs from the request's reason:
	// equality with the request would also pass for a handler that echoes
	// the input, which is not the claim - the record is.
	const recorded = "the record's own reason"
	store := &withdrawalStore{withdrawal: editorial.Withdrawal{
		ArticleID:   articleID,
		WithdrawnAt: time.Date(2026, 8, 15, 14, 15, 0, 0, time.UTC),
		WithdrawnBy: testEditor.ID,
		Reason:      recorded,
	}}
	h := editorial.NewHandler(discardLogger(), store, fakeAuth{})
	rec := postWithdrawal(t, h, articleID.String(), `{"reason":"what the request said"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body withdrawalBody
	decodeInto(t, rec, &body)
	if body.Reason != recorded {
		t.Errorf("reason = %q, want the stored %q - the response reports the record, not the request", body.Reason, recorded)
	}
}

// TestWithdrawalResponseReasonMatchesTheStoredRow proves, against the real
// schema, that the reason on the wire is byte-for-byte the value the
// database froze into article.withdrawal_reason - guarded all-or-none by
// article_withdrawal_all_or_none and permanent under I-5 - so the
// confirmation banner renders the audit record itself, not a paraphrase of
// the request.
func TestWithdrawalResponseReasonMatchesTheStoredRow(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise withdrawal against Postgres")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	f := seedWithdrawalFixture(ctx, t, tx)
	h := editorial.NewHandler(discardLogger(), editorial.NewPGStore(tx),
		staticAuth{editor: editorial.Editor{ID: uuid.MustParse(f.editorID), Email: "editor@example.test", DisplayName: "Reason Editor"}})

	// Sent padded: the handler trims before writing, so the response can
	// only match the stored row by reporting the row, not the input.
	rec := postWithdrawal(t, h, f.published, `{"reason":"  ο εκδότης ζήτησε την απόσυρση  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var body withdrawalBody
	decodeInto(t, rec, &body)

	var storedWhy string
	if err := tx.QueryRow(ctx, `select withdrawal_reason from article where id = $1`, f.published).Scan(&storedWhy); err != nil {
		t.Fatalf("reading the withdrawn article: %v", err)
	}
	if storedWhy == "" {
		t.Fatal("withdrawal_reason is empty; the all-or-none guard should have made that impossible")
	}
	if body.Reason != storedWhy {
		t.Errorf("reason on the wire = %q, want the stored %q", body.Reason, storedWhy)
	}
}

// withdrawalFixture is a seeded world for the withdrawal endpoint: an
// editor, a reader, and one article per lifecycle state the endpoint must
// distinguish. Every article is born from its own origin, because the
// one-per-origin indexes allow no two live articles to share one.
type withdrawalFixture struct {
	editorID, readerID string
	sourceLang         string

	published    string // published and live: the withdrawal succeeds
	publishedTwo string // published and live: for the already-withdrawn case
	forReader    string // published and live: for the database's role check
	unpublished  string // approved, never released: a 404

	// publishedOrigin is the source item behind `published`; after the
	// withdrawal it must reappear in the review queue.
	publishedOrigin string
}

func seedWithdrawalFixture(ctx context.Context, t *testing.T, tx pgx.Tx) withdrawalFixture {
	t.Helper()
	suffix := randomSuffix(t)
	sourceLang, _ := languageCodes(t)
	f := withdrawalFixture{sourceLang: sourceLang}

	if _, err := tx.Exec(ctx, `insert into language (code) values ($1)`, f.sourceLang); err != nil {
		t.Fatalf("seed language: %v", err)
	}
	account := func(role string) string {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx,
			`insert into account (email, display_name, role) values ($1, $2, $3) returning id`,
			"withdrawal-"+role+"-"+suffix+"@example.test", "Withdrawal Test "+role+" "+suffix, role).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", role, err)
		}
		return id
	}
	f.editorID = account("editor")
	f.readerID = account("reader")

	var sourceID string
	if err := tx.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, $3, 'GR', 'Extract and link permitted (withdrawal test)') returning id`,
		"Withdrawal Feed "+suffix, "https://example.test/withdrawal/"+suffix, f.sourceLang).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	article := func(name string, published bool) (articleID, itemID string) {
		t.Helper()
		if err := tx.QueryRow(ctx,
			`insert into source_item (source_id, source_url, original_title, raw_body)
			 values ($1, $2, $3, $4) returning id`,
			sourceID, "https://example.test/withdrawal/"+suffix+"/"+name,
			"Τίτλος "+name+" "+suffix, "Σώμα "+name+" "+suffix).Scan(&itemID); err != nil {
			t.Fatalf("seed source_item %s: %v", name, err)
		}
		if err := tx.QueryRow(ctx,
			`insert into article (source_item_id, approved_by, published_at, attribution_block)
			 values ($1, $2, case when $3::boolean then now() else null end, $4) returning id`,
			itemID, f.editorID, published, "Πηγή: Withdrawal Feed "+suffix).Scan(&articleID); err != nil {
			t.Fatalf("seed article %s: %v", name, err)
		}
		return articleID, itemID
	}

	f.published, f.publishedOrigin = article("published", true)
	f.publishedTwo, _ = article("published-two", true)
	f.forReader, _ = article("for-reader", true)
	f.unpublished, _ = article("unpublished", false)
	return f
}

// TestWithdrawalAgainstSchema exercises the endpoint against the real,
// migrated schema: the guarded one-way transition, the trigger-written
// audit event, the preserved record, and the origin's return to the queue.
func TestWithdrawalAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise withdrawal against Postgres")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	f := seedWithdrawalFixture(ctx, t, tx)
	editorID := uuid.MustParse(f.editorID)
	h := editorial.NewHandler(discardLogger(), editorial.NewPGStore(tx),
		staticAuth{editor: editorial.Editor{ID: editorID, Email: "editor@example.test", DisplayName: "Withdrawal Editor"}})

	const reason = "the source retracted the story"

	t.Run("200 recording who ended publication and why", func(t *testing.T) {
		rec := postWithdrawal(t, h, f.published, `{"reason":`+jsonString(t, reason)+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var body withdrawalBody
		decodeInto(t, rec, &body)
		if body.ArticleID != f.published {
			t.Errorf("article_id = %q, want %q", body.ArticleID, f.published)
		}
		if body.WithdrawnBy != f.editorID {
			t.Errorf("withdrawn_by = %q, want the authenticated editor %q", body.WithdrawnBy, f.editorID)
		}
		if _, err := time.Parse(time.RFC3339Nano, body.WithdrawnAt); err != nil {
			t.Errorf("withdrawn_at %q is not RFC 3339: %v", body.WithdrawnAt, err)
		}

		// The record survives whole (FR-016, I-5): publication ended, and
		// the approval, its approver and the attribution are untouched.
		var (
			approvedBy  string
			publishedAt *time.Time
			withdrawnBy string
			storedWhy   string
			attribution string
		)
		if err := tx.QueryRow(ctx,
			`select approved_by, published_at, withdrawn_by, withdrawal_reason, attribution_block
			   from article where id = $1`, f.published).
			Scan(&approvedBy, &publishedAt, &withdrawnBy, &storedWhy, &attribution); err != nil {
			t.Fatalf("reading the withdrawn article: %v", err)
		}
		if approvedBy != f.editorID || publishedAt == nil {
			t.Errorf("approval record changed: approved_by %q, published_at %v", approvedBy, publishedAt)
		}
		if withdrawnBy != f.editorID {
			t.Errorf("withdrawn_by = %q, want the editor %q", withdrawnBy, f.editorID)
		}
		if storedWhy != reason {
			t.Errorf("withdrawal_reason = %q, want %q", storedWhy, reason)
		}
		if attribution == "" {
			t.Error("attribution_block was cleared; withdrawal preserves every record")
		}
	})

	// The audit event comes from the 0002 trigger, in the same transaction
	// as the update. Exactly one is the assertion that matters: two would
	// mean the endpoint emitted a duplicate of its own.
	t.Run("the trigger wrote exactly one article.withdrawn event", func(t *testing.T) {
		var (
			events      int
			eventBy     string
			eventReason string
		)
		if err := tx.QueryRow(ctx,
			`select count(*) from domain_event
			  where type = 'article.withdrawn' and payload->>'article_id' = $1`, f.published).Scan(&events); err != nil {
			t.Fatalf("counting withdrawal events: %v", err)
		}
		if events != 1 {
			t.Fatalf("article.withdrawn events = %d, want exactly 1 - the trigger's, never a duplicate", events)
		}
		if err := tx.QueryRow(ctx,
			`select payload->>'withdrawn_by', payload->>'reason' from domain_event
			  where type = 'article.withdrawn' and payload->>'article_id' = $1`, f.published).
			Scan(&eventBy, &eventReason); err != nil {
			t.Fatalf("reading the withdrawal event: %v", err)
		}
		if eventBy != f.editorID || eventReason != reason {
			t.Errorf("audit event = (%q, %q), want (%q, %q)", eventBy, eventReason, f.editorID, reason)
		}
	})

	t.Run("409 withdrawing it again", func(t *testing.T) {
		rec := postWithdrawal(t, h, f.published, `{"reason":"changed my mind"}`)
		wantProblem(t, rec, http.StatusConflict, "already withdrawn")

		// The frozen record is untouched, and no second audit event exists.
		var storedWhy string
		if err := tx.QueryRow(ctx, `select withdrawal_reason from article where id = $1`, f.published).Scan(&storedWhy); err != nil {
			t.Fatalf("reading the withdrawn article: %v", err)
		}
		if storedWhy != reason {
			t.Errorf("withdrawal_reason = %q, want the original %q - withdrawal is one-way and final", storedWhy, reason)
		}
		var events int
		if err := tx.QueryRow(ctx,
			`select count(*) from domain_event
			  where type = 'article.withdrawn' and payload->>'article_id' = $1`, f.published).Scan(&events); err != nil {
			t.Fatalf("counting withdrawal events: %v", err)
		}
		if events != 1 {
			t.Errorf("article.withdrawn events after the rejected repeat = %d, want still 1", events)
		}
	})

	t.Run("404 for an unknown article", func(t *testing.T) {
		wantProblem(t, postWithdrawal(t, h, uuid.NewString(), `{"reason":"r"}`), http.StatusNotFound, "no article")
	})

	// An approval that was never released has no publication to end. It
	// answers 404, not 409: the existence of unpublished work is not
	// something this endpoint confirms either.
	t.Run("404 for an article that was never published", func(t *testing.T) {
		wantProblem(t, postWithdrawal(t, h, f.unpublished, `{"reason":"r"}`), http.StatusNotFound, "no published article")
		var withdrawnAt *time.Time
		if err := tx.QueryRow(ctx, `select withdrawn_at from article where id = $1`, f.unpublished).Scan(&withdrawnAt); err != nil {
			t.Fatalf("reading the unpublished article: %v", err)
		}
		if withdrawnAt != nil {
			t.Error("an unpublished article was withdrawn; the database forbids it and so must the endpoint")
		}
	})

	// Withdrawal is an editorial decision, and the database checks the role
	// again on the transition - symmetrically with approval.
	t.Run("403 when the database says the withdrawer is not an editor", func(t *testing.T) {
		readerHandler := editorial.NewHandler(discardLogger(), editorial.NewPGStore(tx),
			staticAuth{editor: editorial.Editor{ID: uuid.MustParse(f.readerID)}})
		rec := doJSON(t, readerHandler, http.MethodPost,
			"/api/v1/editorial/articles/"+f.forReader+"/withdrawal", editorToken, `{"reason":"r"}`)
		wantProblem(t, rec, http.StatusForbidden, "editor role")

		var withdrawnAt *time.Time
		if err := tx.QueryRow(ctx, `select withdrawn_at from article where id = $1`, f.forReader).Scan(&withdrawnAt); err != nil {
			t.Fatalf("reading the article: %v", err)
		}
		if withdrawnAt != nil {
			t.Error("a reader withdrew an article; the guard runs before the row is written")
		}
	})

	// The loop back to T019: withdrawal frees the origin, so it returns to
	// the review queue flagged as a correction candidate carrying the
	// history of why it left.
	t.Run("the freed origin returns to the review queue as a correction candidate", func(t *testing.T) {
		_, queue := getQueue(t, h, "?lang="+f.sourceLang)
		// The fixture's language is private to this test, so the queue for
		// it is exactly the origins this test freed: one. The other three
		// articles still hold their origins - withdrawal frees one origin,
		// not all of them - and an approved-but-unpublished article holds
		// its origin just as firmly as a published one.
		if len(queue.Items) != 1 {
			t.Fatalf("queue = %d items, want only the freed origin %s", len(queue.Items), f.publishedOrigin)
		}
		item := queue.Items[0]
		if item.SourceItemID != f.publishedOrigin {
			t.Fatalf("queue item = %q, want the freed origin %q", item.SourceItemID, f.publishedOrigin)
		}
		if !item.CorrectionCandidate {
			t.Error("correction_candidate = false on the origin whose article was just withdrawn")
		}
		if len(item.Withdrawals) != 1 {
			t.Fatalf("withdrawals = %d, want the one just recorded", len(item.Withdrawals))
		}
		wd := item.Withdrawals[0]
		if wd.ArticleID != f.published {
			t.Errorf("withdrawal article_id = %q, want %q", wd.ArticleID, f.published)
		}
		if wd.WithdrawnBy != f.editorID || wd.Reason != reason {
			t.Errorf("withdrawal history = (%q, %q), want (%q, %q)", wd.WithdrawnBy, wd.Reason, f.editorID, reason)
		}
	})
}
