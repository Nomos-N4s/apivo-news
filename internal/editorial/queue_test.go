package editorial_test

// Contract tests for GET /api/v1/editorial/queue (T019): shape, column
// backing, pagination, the language filter, auth and the correction-candidate
// flagging that brings a withdrawn origin back into the queue.
//
// The database-backed test exercises the whole handler-to-Postgres path -
// the real union query, the real partial indexes - inside a transaction that
// is rolled back, so it leaves nothing behind.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// A transaction satisfies the module's DB seam exactly as the pool does,
// which is what lets the database-backed contract test roll everything back.
var _ editorial.DB = (pgx.Tx)(nil)

// queueStore answers with a canned page and records the query the handler
// derived from the request.
type queueStore struct {
	page editorial.QueuePage
	got  editorial.QueueQuery
}

func (s *queueStore) CreateSource(context.Context, editorial.NewSource) (editorial.Source, error) {
	return editorial.Source{}, errUnexpectedCall
}

func (s *queueStore) Approve(context.Context, editorial.NewApproval) (editorial.Article, error) {
	return editorial.Article{}, errUnexpectedCall
}

func (s *queueStore) Publish(context.Context, uuid.UUID, uuid.UUID) (editorial.Article, error) {
	return editorial.Article{}, errUnexpectedCall
}

func (s *queueStore) Withdraw(context.Context, uuid.UUID, uuid.UUID, string) (editorial.Withdrawal, error) {
	return editorial.Withdrawal{}, errUnexpectedCall
}

func (s *queueStore) Provenance(context.Context, uuid.UUID) (editorial.Provenance, error) {
	return editorial.Provenance{}, errUnexpectedCall
}

func (s *queueStore) ReviewQueue(_ context.Context, q editorial.QueueQuery) (editorial.QueuePage, error) {
	s.got = q
	return s.page, nil
}

// queueBody is the decoded queue payload, kept deliberately close to the
// contract's wire shape so a renamed field fails the test.
type queueBody struct {
	Items []struct {
		SourceItemID        string  `json:"source_item_id"`
		TranslationID       *string `json:"translation_id"`
		SourceName          string  `json:"source_name"`
		HeadlineOriginal    *string `json:"headline_original"`
		HeadlineTranslated  *string `json:"headline_translated"`
		ExtractTranslated   *string `json:"extract_translated"`
		RetrievedAt         string  `json:"retrieved_at"`
		LicenceSnapshot     string  `json:"licence_snapshot"`
		CorrectionCandidate bool    `json:"correction_candidate"`
		Withdrawals         []struct {
			ArticleID   string `json:"article_id"`
			WithdrawnAt string `json:"withdrawn_at"`
			WithdrawnBy string `json:"withdrawn_by"`
			Reason      string `json:"reason"`
		} `json:"withdrawals"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// getQueue issues a queue request with the editor token unless another is
// named, and decodes a 200 body.
func getQueue(t *testing.T, h http.Handler, query string) (*httptest.ResponseRecorder, queueBody) {
	t.Helper()
	rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/queue"+query, editorToken, "")
	var body queueBody
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshalling queue body %q: %v", rec.Body.String(), err)
		}
	}
	return rec, body
}

func TestReviewQueueAuth(t *testing.T) {
	t.Parallel()
	h := newHandler(t, errStore{err: errors.New("store must not be reached")})

	t.Run("401 without a token", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/queue", "", "")
		wantProblem(t, rec, http.StatusUnauthorized, "bearer token is required")
	})
	t.Run("401 with an invalid token", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/queue", "not-a-real-token", "")
		wantProblem(t, rec, http.StatusUnauthorized, "invalid")
	})
	t.Run("403 for a non-editor", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/queue", readerToken, "")
		wantProblem(t, rec, http.StatusForbidden, "editor role")
	})
}

func TestReviewQueueStoreFailure(t *testing.T) {
	t.Parallel()
	h := newHandler(t, errStore{err: errors.New("connection torn down")})
	rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/queue", editorToken, "")
	wantProblem(t, rec, http.StatusInternalServerError, "")
	if strings.Contains(rec.Body.String(), "torn down") {
		t.Error("internal error detail leaked to the wire")
	}
}

func TestReviewQueueQueryValidation(t *testing.T) {
	t.Parallel()

	bad := []struct {
		name   string
		query  string
		detail string
	}{
		{name: "limit is not a number", query: "?limit=many", detail: "limit"},
		// A supplied-but-empty limit is a malformed request, not an absent
		// parameter: answering it with the default page would read as
		// acceptance of whatever the caller meant to send.
		{name: "limit is supplied but empty", query: "?limit=", detail: "limit"},
		{name: "limit is zero", query: "?limit=0", detail: "limit"},
		{name: "limit is negative", query: "?limit=-1", detail: "limit"},
		{name: "limit exceeds the maximum", query: "?limit=101", detail: "limit"},
		{name: "limit is fractional", query: "?limit=1.5", detail: "limit"},
		{name: "lang is blank", query: "?lang=", detail: "lang"},
		{name: "lang is a combined locale tag", query: "?lang=de-DE", detail: "lang"},
		{name: "lang is upper case", query: "?lang=DE", detail: "lang"},
		{name: "lang is too long", query: "?lang=deutsch", detail: "lang"},
		{name: "cursor is not base64", query: "?cursor=%21%21%21", detail: "cursor"},
		{name: "cursor is base64 of nonsense", query: "?cursor=bm9uc2Vuc2U", detail: "cursor"},
		{name: "unknown parameter", query: "?language=de", detail: "language"},
		{name: "unknown parameter alongside a valid one", query: "?lang=de&offset=10", detail: "offset"},
		// url.Values keeps both values but Get returns only the first, so
		// answering a contradictory request would silently pick one of them.
		{name: "limit is repeated", query: "?limit=10&limit=20", detail: "at most once"},
		{name: "lang is repeated", query: "?lang=el&lang=de", detail: "at most once"},
		{name: "cursor is repeated", query: "?cursor=a&cursor=b", detail: "at most once"},
	}
	for _, tc := range bad {
		t.Run("400 when "+tc.name, func(t *testing.T) {
			t.Parallel()
			// The store must never be reached by an invalid request.
			h := newHandler(t, errStore{err: errors.New("store must not be reached")})
			rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/queue"+tc.query, editorToken, "")
			wantProblem(t, rec, http.StatusBadRequest, tc.detail)
		})
	}
}

func TestReviewQueueDefaultsAndFilters(t *testing.T) {
	t.Parallel()

	t.Run("no query means twenty items, every language, from the top", func(t *testing.T) {
		t.Parallel()
		store := &queueStore{}
		h := editorial.NewHandler(discardLogger(), store, fakeAuth{})
		rec, body := getQueue(t, h, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if store.got.Limit != 20 {
			t.Errorf("limit = %d, want the contract default of 20", store.got.Limit)
		}
		if store.got.Lang != "" {
			t.Errorf("lang = %q, want no filter", store.got.Lang)
		}
		if store.got.Cursor != nil {
			t.Errorf("cursor = %+v, want none", store.got.Cursor)
		}
		// An empty queue is an empty list, never a null and never a 500.
		if body.Items == nil {
			t.Error("items = null, want []")
		}
		if body.NextCursor != nil {
			t.Errorf("next_cursor = %v, want null on an exhausted queue", *body.NextCursor)
		}
	})

	t.Run("lang and limit reach the store", func(t *testing.T) {
		t.Parallel()
		store := &queueStore{}
		h := editorial.NewHandler(discardLogger(), store, fakeAuth{})
		if rec, _ := getQueue(t, h, "?lang=el&limit=100"); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if store.got.Lang != "el" || store.got.Limit != 100 {
			t.Errorf("query = %+v, want lang el and limit 100", store.got)
		}
	})
}

// TestReviewQueueResponseShape pins the wire shape of both origin shapes and
// of the correction-candidate flagging, against a canned page.
func TestReviewQueueResponseShape(t *testing.T) {
	t.Parallel()

	sourceItemID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	translationID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	articleID := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	withdrawnBy := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	retrievedAt := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	withdrawnAt := time.Date(2026, 8, 15, 11, 30, 0, 0, time.UTC)
	title, headline, extract := "Πρωτότυπος τίτλος", "Übersetzte Überschrift", "Übersetzter Auszug"

	page := editorial.QueuePage{
		Items: []editorial.QueueItem{
			{
				SourceItemID:       sourceItemID,
				TranslationID:      &translationID,
				SourceName:         "Kathimerini",
				HeadlineOriginal:   &title,
				HeadlineTranslated: &headline,
				ExtractTranslated:  &extract,
				RetrievedAt:        retrievedAt,
				LicenceSnapshot:    "Extract and link permitted v1",
				Cursor:             editorial.QueueCursor{RetrievedAt: retrievedAt, RowID: translationID},
			},
			{
				SourceItemID:    sourceItemID,
				SourceName:      "Kathimerini",
				RetrievedAt:     retrievedAt,
				LicenceSnapshot: "Extract and link permitted v1",
				Withdrawals: []editorial.Withdrawal{{
					ArticleID:   articleID,
					WithdrawnAt: withdrawnAt,
					WithdrawnBy: withdrawnBy,
					Reason:      "source retracted the story",
				}},
				Cursor: editorial.QueueCursor{RetrievedAt: retrievedAt, RowID: sourceItemID},
			},
		},
		NextCursor: &editorial.QueueCursor{RetrievedAt: retrievedAt, RowID: sourceItemID},
	}
	h := editorial.NewHandler(discardLogger(), &queueStore{page: page}, fakeAuth{})

	rec, body := getQueue(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}

	translated := body.Items[0]
	if translated.SourceItemID != sourceItemID.String() {
		t.Errorf("source_item_id = %q, want %q", translated.SourceItemID, sourceItemID)
	}
	if translated.TranslationID == nil || *translated.TranslationID != translationID.String() {
		t.Errorf("translation_id = %v, want %q", translated.TranslationID, translationID)
	}
	if translated.HeadlineOriginal == nil || *translated.HeadlineOriginal != title {
		t.Errorf("headline_original = %v, want %q", translated.HeadlineOriginal, title)
	}
	if translated.HeadlineTranslated == nil || *translated.HeadlineTranslated != headline {
		t.Errorf("headline_translated = %v, want %q", translated.HeadlineTranslated, headline)
	}
	if translated.ExtractTranslated == nil || *translated.ExtractTranslated != extract {
		t.Errorf("extract_translated = %v, want %q", translated.ExtractTranslated, extract)
	}
	if translated.RetrievedAt != "2026-08-14T09:00:00Z" {
		t.Errorf("retrieved_at = %q, want RFC 3339 UTC", translated.RetrievedAt)
	}
	if translated.LicenceSnapshot != "Extract and link permitted v1" {
		t.Errorf("licence_snapshot = %q", translated.LicenceSnapshot)
	}
	if translated.CorrectionCandidate {
		t.Error("correction_candidate = true on an origin that was never published")
	}
	if translated.Withdrawals == nil {
		t.Error("withdrawals = null on a fresh candidate, want []")
	}

	untranslated := body.Items[1]
	if untranslated.TranslationID != nil {
		t.Errorf("translation_id = %v on an untranslated origin, want null", *untranslated.TranslationID)
	}
	if untranslated.HeadlineTranslated != nil || untranslated.ExtractTranslated != nil {
		t.Error("translated columns must be null on an untranslated origin")
	}
	if untranslated.HeadlineOriginal != nil {
		t.Errorf("headline_original = %v, want null when the feed provided no title", *untranslated.HeadlineOriginal)
	}
	if !untranslated.CorrectionCandidate {
		t.Error("correction_candidate = false on an origin whose only article was withdrawn")
	}
	if len(untranslated.Withdrawals) != 1 {
		t.Fatalf("withdrawals = %d, want the one recorded withdrawal", len(untranslated.Withdrawals))
	}
	wd := untranslated.Withdrawals[0]
	if wd.ArticleID != articleID.String() || wd.WithdrawnBy != withdrawnBy.String() {
		t.Errorf("withdrawal = %+v, want article %s withdrawn by %s", wd, articleID, withdrawnBy)
	}
	if wd.WithdrawnAt != "2026-08-15T11:30:00Z" {
		t.Errorf("withdrawn_at = %q, want RFC 3339 UTC", wd.WithdrawnAt)
	}
	if wd.Reason != "source retracted the story" {
		t.Errorf("reason = %q", wd.Reason)
	}

	if body.NextCursor == nil {
		t.Fatal("next_cursor = null, want a cursor when a further page exists")
	}
}

// TestReviewQueueCursorRoundTrip proves the opaque cursor a page hands out
// is understood on the way back in, and positions the next page exactly
// where the previous one stopped.
func TestReviewQueueCursorRoundTrip(t *testing.T) {
	t.Parallel()
	rowID := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	// A microsecond-precision instant: Postgres timestamptz resolution. A
	// cursor that truncated it would move the keyset boundary and silently
	// skip or repeat a row.
	retrievedAt := time.Date(2026, 8, 14, 9, 0, 0, 123456000, time.UTC)

	first := &queueStore{page: editorial.QueuePage{
		NextCursor: &editorial.QueueCursor{RetrievedAt: retrievedAt, RowID: rowID},
	}}
	h := editorial.NewHandler(discardLogger(), first, fakeAuth{})
	rec, body := getQueue(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body.NextCursor == nil {
		t.Fatal("next_cursor = null, want a cursor")
	}

	second := &queueStore{}
	h = editorial.NewHandler(discardLogger(), second, fakeAuth{})
	if rec, _ := getQueue(t, h, "?cursor="+*body.NextCursor); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if second.got.Cursor == nil {
		t.Fatal("the cursor did not reach the store")
	}
	if !second.got.Cursor.RetrievedAt.Equal(retrievedAt) {
		t.Errorf("cursor retrieved_at = %v, want %v", second.got.Cursor.RetrievedAt, retrievedAt)
	}
	if second.got.Cursor.RowID != rowID {
		t.Errorf("cursor row id = %v, want %v", second.got.Cursor.RowID, rowID)
	}
}

// queueFixture is a seeded world for the queue query, isolated in one
// transaction and, so that a shared database cannot leak other tests' rows
// into the assertions, addressed through two private language codes.
type queueFixture struct {
	sourceLang, targetLang string
	sourceName             string
	editorID               string
	licence                string

	// Retrieved newest-first: fresh, translatedLive, approved, withdrawn.
	fresh          string // no article anywhere; carries a fresh translation
	freshTitle     string
	freshTr        string // fresh translation of fresh
	freshHeadline  string
	freshExtract   string
	titleless      string // no original_title; its translation IS approved
	titlelessTr    string // approved and live - must be absent from the queue
	approved       string // approved untranslated - must be absent
	withdrawn      string // approved, published, then withdrawn - a candidate
	withdrawnID    string // the withdrawn article
	withdrawReason string
}

func seedQueueFixture(ctx context.Context, t *testing.T, tx pgx.Tx) queueFixture {
	t.Helper()
	suffix := randomSuffix(t)
	sourceLang, targetLang := languageCodes(t)
	f := queueFixture{
		sourceLang:     sourceLang,
		targetLang:     targetLang,
		sourceName:     "Queue Feed " + suffix,
		licence:        "Extract and link permitted (queue test " + suffix + ")",
		freshTitle:     "Φρέσκος τίτλος " + suffix,
		freshHeadline:  "Frische Überschrift " + suffix,
		freshExtract:   "Frischer Auszug " + suffix,
		withdrawReason: "correction required " + suffix,
	}
	for _, code := range []string{f.sourceLang, f.targetLang} {
		if _, err := tx.Exec(ctx, `insert into language (code) values ($1)`, code); err != nil {
			t.Fatalf("seed language %s: %v", code, err)
		}
	}
	if err := tx.QueryRow(ctx,
		`insert into account (email, display_name, role) values ($1, $2, 'editor') returning id`,
		"queue-editor-"+suffix+"@example.test", "Queue Test Editor "+suffix).Scan(&f.editorID); err != nil {
		t.Fatalf("seed editor: %v", err)
	}
	var sourceID string
	if err := tx.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, $3, 'GR', $4) returning id`,
		f.sourceName, "https://example.test/queue/"+suffix, f.sourceLang, f.licence).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	base := time.Now().UTC().Truncate(time.Microsecond)
	item := func(name string, title *string, age time.Duration) string {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx,
			`insert into source_item (source_id, source_url, original_title, raw_body, retrieved_at)
			 values ($1, $2, $3, $4, $5) returning id`,
			sourceID, "https://example.test/queue/"+suffix+"/"+name, title,
			"Σώμα "+name+" "+suffix, base.Add(-age)).Scan(&id); err != nil {
			t.Fatalf("seed source_item %s: %v", name, err)
		}
		return id
	}
	translate := func(itemID, headline, extract string) string {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx,
			`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
			 values ($1, $2, 'test-model-1', 'prompt-v1', $3, $4, 1200) returning id`,
			itemID, f.targetLang, headline, extract).Scan(&id); err != nil {
			t.Fatalf("seed translation: %v", err)
		}
		return id
	}
	approve := func(translationID, sourceItemID *string) string {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx,
			`insert into article (translation_id, source_item_id, approved_by, published_at, attribution_block)
			 values ($1, $2, $3, now(), $4) returning id`,
			translationID, sourceItemID, f.editorID, "Quelle: "+f.sourceName).Scan(&id); err != nil {
			t.Fatalf("seed article: %v", err)
		}
		return id
	}

	f.fresh = item("fresh", &f.freshTitle, 1*time.Hour)
	f.freshTr = translate(f.fresh, f.freshHeadline, f.freshExtract)

	// No original_title: the queue must render headline_original as null.
	f.titleless = item("titleless", nil, 2*time.Hour)
	f.titlelessTr = translate(f.titleless, "Überschrift "+suffix, "Auszug "+suffix)
	approve(&f.titlelessTr, nil)

	f.approved = item("approved", strptr("Εγκεκριμένο "+suffix), 3*time.Hour)
	approve(nil, &f.approved)

	f.withdrawn = item("withdrawn", strptr("Αποσυρμένο "+suffix), 4*time.Hour)
	f.withdrawnID = approve(nil, &f.withdrawn)
	if _, err := tx.Exec(ctx,
		`update article
		    set withdrawn_at = now(), withdrawn_by = $2, withdrawal_reason = $3
		  where id = $1`, f.withdrawnID, f.editorID, f.withdrawReason); err != nil {
		t.Fatalf("withdraw article: %v", err)
	}
	return f
}

// TestReviewQueueAgainstSchema exercises the endpoint against the real,
// migrated schema: the union of item and translation origins, exclusion of
// origins with a live article, the correction candidate a withdrawal frees,
// the language filter and keyset pagination.
func TestReviewQueueAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the queue against Postgres")
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

	f := seedQueueFixture(ctx, t, tx)
	h := editorial.NewHandler(discardLogger(), editorial.NewPGStore(tx), fakeAuth{})

	t.Run("untranslated origins, newest retrieval first", func(t *testing.T) {
		rec, body := getQueue(t, h, "?lang="+f.sourceLang)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		want := []string{f.fresh, f.titleless, f.withdrawn}
		got := make([]string, 0, len(body.Items))
		for _, item := range body.Items {
			got = append(got, item.SourceItemID)
			if item.TranslationID != nil {
				t.Errorf("translation_id = %q on a %s row, want null", *item.TranslationID, f.sourceLang)
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("queue = %v, want %v (the approved origin %s must be absent; the withdrawn one %s present)",
				got, want, f.approved, f.withdrawn)
		}
		if body.NextCursor != nil {
			t.Errorf("next_cursor = %q, want null on the last page", *body.NextCursor)
		}

		// Column backing and the shape of a fresh untranslated candidate.
		fresh := body.Items[0]
		if fresh.SourceName != f.sourceName {
			t.Errorf("source_name = %q, want %q", fresh.SourceName, f.sourceName)
		}
		if fresh.HeadlineOriginal == nil || *fresh.HeadlineOriginal != f.freshTitle {
			t.Errorf("headline_original = %v, want source_item.original_title %q", fresh.HeadlineOriginal, f.freshTitle)
		}
		if fresh.LicenceSnapshot != f.licence {
			t.Errorf("licence_snapshot = %q, want the retrieval-time snapshot %q", fresh.LicenceSnapshot, f.licence)
		}
		if _, err := time.Parse(time.RFC3339Nano, fresh.RetrievedAt); err != nil {
			t.Errorf("retrieved_at %q is not RFC 3339: %v", fresh.RetrievedAt, err)
		}
		if fresh.CorrectionCandidate || len(fresh.Withdrawals) != 0 {
			t.Errorf("fresh candidate flagged as a correction: %+v", fresh)
		}

		// A feed that provided no title renders null, not an empty string.
		if titleless := body.Items[1]; titleless.HeadlineOriginal != nil {
			t.Errorf("headline_original = %q, want null when the feed provided no title", *titleless.HeadlineOriginal)
		}
	})

	t.Run("a withdrawn origin returns flagged with its history", func(t *testing.T) {
		_, body := getQueue(t, h, "?lang="+f.sourceLang)
		var found bool
		for _, item := range body.Items {
			if item.SourceItemID != f.withdrawn {
				continue
			}
			found = true
			if !item.CorrectionCandidate {
				t.Error("correction_candidate = false on a withdrawn origin")
			}
			if len(item.Withdrawals) != 1 {
				t.Fatalf("withdrawals = %d, want the one recorded withdrawal", len(item.Withdrawals))
			}
			wd := item.Withdrawals[0]
			if wd.ArticleID != f.withdrawnID {
				t.Errorf("withdrawal article_id = %q, want %q", wd.ArticleID, f.withdrawnID)
			}
			if wd.WithdrawnBy != f.editorID {
				t.Errorf("withdrawn_by = %q, want the editor %q", wd.WithdrawnBy, f.editorID)
			}
			if wd.Reason != f.withdrawReason {
				t.Errorf("reason = %q, want %q", wd.Reason, f.withdrawReason)
			}
			if _, err := time.Parse(time.RFC3339Nano, wd.WithdrawnAt); err != nil {
				t.Errorf("withdrawn_at %q is not RFC 3339: %v", wd.WithdrawnAt, err)
			}
		}
		if !found {
			t.Fatalf("withdrawn origin %s is absent from the queue; withdrawal must free it for a correction", f.withdrawn)
		}
	})

	t.Run("translated origins are listed under the target locale", func(t *testing.T) {
		rec, body := getQueue(t, h, "?lang="+f.targetLang)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if len(body.Items) != 1 {
			t.Fatalf("queue = %d items, want only the fresh translation (the approved translation %s must be absent)",
				len(body.Items), f.titlelessTr)
		}
		item := body.Items[0]
		if item.TranslationID == nil || *item.TranslationID != f.freshTr {
			t.Fatalf("translation_id = %v, want %q", item.TranslationID, f.freshTr)
		}
		if item.SourceItemID != f.fresh {
			t.Errorf("source_item_id = %q, want the translated item %q", item.SourceItemID, f.fresh)
		}
		if item.HeadlineTranslated == nil || *item.HeadlineTranslated != f.freshHeadline {
			t.Errorf("headline_translated = %v, want translation.headline %q", item.HeadlineTranslated, f.freshHeadline)
		}
		if item.ExtractTranslated == nil || *item.ExtractTranslated != f.freshExtract {
			t.Errorf("extract_translated = %v, want translation.extract %q", item.ExtractTranslated, f.freshExtract)
		}
		if item.HeadlineOriginal == nil || *item.HeadlineOriginal != f.freshTitle {
			t.Errorf("headline_original = %v, want the retrieved title %q alongside the translation", item.HeadlineOriginal, f.freshTitle)
		}
	})

	// An item whose translation is approved is still its own unapproved
	// origin: the one-per-origin indexes are separate, so approving the
	// translation must not quietly retire the untranslated candidate.
	t.Run("an approved translation leaves its item in the queue", func(t *testing.T) {
		_, body := getQueue(t, h, "?lang="+f.sourceLang)
		var found bool
		for _, item := range body.Items {
			found = found || item.SourceItemID == f.titleless
		}
		if !found {
			t.Errorf("item %s is absent although only its translation was approved", f.titleless)
		}
	})

	t.Run("keyset pagination walks the queue exactly once", func(t *testing.T) {
		rec, first := getQueue(t, h, "?lang="+f.sourceLang+"&limit=2")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if len(first.Items) != 2 {
			t.Fatalf("first page = %d items, want 2", len(first.Items))
		}
		if first.NextCursor == nil {
			t.Fatal("next_cursor = null although a third row exists")
		}
		rec, second := getQueue(t, h, "?lang="+f.sourceLang+"&limit=2&cursor="+*first.NextCursor)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if len(second.Items) != 1 {
			t.Fatalf("second page = %d items, want the remaining 1", len(second.Items))
		}
		if second.NextCursor != nil {
			t.Errorf("next_cursor = %q, want null once the queue is exhausted", *second.NextCursor)
		}
		got := []string{first.Items[0].SourceItemID, first.Items[1].SourceItemID, second.Items[0].SourceItemID}
		if !slices.Equal(got, []string{f.fresh, f.titleless, f.withdrawn}) {
			t.Errorf("paged queue = %v, want every row exactly once in retrieval order", got)
		}
	})

	t.Run("an unknown language yields an empty queue, not an error", func(t *testing.T) {
		rec, body := getQueue(t, h, "?lang=zzz")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if len(body.Items) != 0 {
			t.Errorf("items = %d, want none", len(body.Items))
		}
	})
}

// TestReviewQueueRejectsAnUnboundedLimit covers the seam the endpoint does
// not: Store is reachable directly, and the page's one-row overfetch is
// int32 arithmetic, so an unbounded Limit would overflow into a negative
// SQL LIMIT. The guard runs before any statement, which is why a store with
// no database behind it is enough to prove it.
func TestReviewQueueRejectsAnUnboundedLimit(t *testing.T) {
	t.Parallel()
	store := editorial.NewPGStore(nil)
	for _, limit := range []int32{math.MaxInt32, 101, 0, -1} {
		if _, err := store.ReviewQueue(t.Context(), editorial.QueueQuery{Limit: limit}); err == nil {
			t.Errorf("ReviewQueue(limit=%d) = nil error, want a refusal before any query runs", limit)
		}
	}
}

// languageCodes returns two distinct private language codes: three
// lower-case letters, the shape `language_code_is_bcp47_subtag` accepts.
// They give the fixture its isolation - the queue spans the whole database,
// so filtering by a code no other test uses is what keeps the assertions
// about a shared Postgres deterministic.
func languageCodes(t *testing.T) (source, target string) {
	t.Helper()
	source = randomCode(t)
	target = randomCode(t)
	for target == source {
		target = randomCode(t)
	}
	return source, target
}

func randomCode(t *testing.T) string {
	t.Helper()
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	for i, v := range b {
		b[i] = alphabet[int(v)%len(alphabet)]
	}
	return string(b)
}

func strptr(s string) *string { return &s }
