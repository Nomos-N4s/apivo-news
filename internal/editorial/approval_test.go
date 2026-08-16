package editorial_test

// Contract tests for POST /api/v1/editorial/approvals and
// POST /api/v1/editorial/articles/{id}/publication (T020).
//
// The database-backed test is the one that matters most here: 403 and 409
// are the database's verdicts - the editor-role trigger and the
// one-per-origin partial indexes - and a test that only exercised a fake
// store would be asserting this package's opinions rather than the
// guarantees the schema actually provides.

import (
	"context"
	"encoding/json"
	"errors"
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

// staticAuth resolves every token to one fixed editor. The database-backed
// tests need the authenticated identity to be a real account row, which the
// canned fakeAuth identity is not.
type staticAuth struct{ editor editorial.Editor }

func (a staticAuth) AuthenticateEditor(context.Context, string) (editorial.Editor, error) {
	return a.editor, nil
}

// approvalStore answers approvals and publications with canned results and
// records what the handler asked for.
type approvalStore struct {
	article editorial.Article
	err     error

	gotApproval  editorial.NewApproval
	gotArticleID uuid.UUID
	gotEditorID  uuid.UUID
}

func (s *approvalStore) CreateSource(context.Context, editorial.NewSource) (editorial.Source, error) {
	return editorial.Source{}, errUnexpectedCall
}

func (s *approvalStore) ReviewQueue(context.Context, editorial.QueueQuery) (editorial.QueuePage, error) {
	return editorial.QueuePage{}, errUnexpectedCall
}

func (s *approvalStore) Approve(_ context.Context, a editorial.NewApproval) (editorial.Article, error) {
	s.gotApproval = a
	return s.article, s.err
}

func (s *approvalStore) Publish(_ context.Context, articleID, editorID uuid.UUID) (editorial.Article, error) {
	s.gotArticleID, s.gotEditorID = articleID, editorID
	return s.article, s.err
}

func (s *approvalStore) Withdraw(context.Context, uuid.UUID, uuid.UUID, string) (editorial.Withdrawal, error) {
	return editorial.Withdrawal{}, errUnexpectedCall
}

// approvalBody is the decoded 201 payload, kept close to the contract's
// wire shape so a renamed field fails the test.
type approvalBody struct {
	ArticleID   string  `json:"article_id"`
	ApprovedBy  string  `json:"approved_by"`
	ApprovedAt  string  `json:"approved_at"`
	PublishedAt *string `json:"published_at"`
}

// publicationBody is the decoded 200 payload of the publication endpoint.
type publicationBody struct {
	ArticleID   string `json:"article_id"`
	PublishedAt string `json:"published_at"`
}

func postApproval(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, h, http.MethodPost, "/api/v1/editorial/approvals", editorToken, body)
}

func postPublication(t *testing.T, h http.Handler, articleID string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, h, http.MethodPost, "/api/v1/editorial/articles/"+articleID+"/publication", editorToken, "")
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("unmarshalling %q: %v", rec.Body.String(), err)
	}
}

func TestApprovalAuth(t *testing.T) {
	t.Parallel()
	h := newHandler(t, errStore{err: errUnexpectedCall})
	body := `{"translation_id":"11111111-1111-4111-8111-111111111111","attribution":"Source: Feed","publish":false,"places":["munich"]}`

	t.Run("401 without a token", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodPost, "/api/v1/editorial/approvals", "", body)
		wantProblem(t, rec, http.StatusUnauthorized, "bearer token is required")
	})
	t.Run("403 for a non-editor", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodPost, "/api/v1/editorial/approvals", readerToken, body)
		wantProblem(t, rec, http.StatusForbidden, "editor role")
	})
	t.Run("401 on the publication route without a token", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodPost,
			"/api/v1/editorial/articles/11111111-1111-4111-8111-111111111111/publication", "", "")
		wantProblem(t, rec, http.StatusUnauthorized, "bearer token is required")
	})
	t.Run("403 on the publication route for a non-editor", func(t *testing.T) {
		t.Parallel()
		rec := doJSON(t, h, http.MethodPost,
			"/api/v1/editorial/articles/11111111-1111-4111-8111-111111111111/publication", readerToken, "")
		wantProblem(t, rec, http.StatusForbidden, "editor role")
	})
}

func TestApprovalValidation(t *testing.T) {
	t.Parallel()

	const someUUID = "11111111-1111-4111-8111-111111111111"
	cases := []struct {
		name   string
		body   string
		detail string
	}{
		{
			name:   "both origins",
			body:   `{"translation_id":"` + someUUID + `","source_item_id":"` + someUUID + `","attribution":"Source: Feed"}`,
			detail: "exactly one origin",
		},
		{
			name:   "neither origin",
			body:   `{"attribution":"Source: Feed"}`,
			detail: "exactly one origin",
		},
		{
			name:   "explicit nulls are still neither origin",
			body:   `{"translation_id":null,"source_item_id":null,"attribution":"Source: Feed"}`,
			detail: "exactly one origin",
		},
		{
			name:   "missing attribution",
			body:   `{"translation_id":"` + someUUID + `"}`,
			detail: "attribution",
		},
		{
			name:   "blank attribution",
			body:   `{"translation_id":"` + someUUID + `","attribution":"   "}`,
			detail: "attribution",
		},
		{
			name:   "translation_id is not a uuid",
			body:   `{"translation_id":"not-a-uuid","attribution":"Source: Feed","places":["munich"]}`,
			detail: "translation_id",
		},
		{
			name:   "source_item_id is not a uuid",
			body:   `{"source_item_id":"not-a-uuid","attribution":"Source: Feed","places":["munich"]}`,
			detail: "source_item_id",
		},
		{
			name:   "empty places",
			body:   `{"translation_id":"` + someUUID + `","attribution":"Source: Feed","places":[]}`,
			detail: "at least one place",
		},
		{
			name:   "a blank place slug",
			body:   `{"translation_id":"` + someUUID + `","attribution":"Source: Feed","places":["munich","  "]}`,
			detail: "blank slug",
		},
		{
			name:   "a place supplied twice",
			body:   `{"translation_id":"` + someUUID + `","attribution":"Source: Feed","places":["munich","munich"]}`,
			detail: `place "munich" was supplied more than once`,
		},
		{
			name:   "malformed JSON",
			body:   `{"attribution":`,
			detail: "not valid JSON",
		},
		{
			name:   "unknown field",
			body:   `{"translation_id":"` + someUUID + `","attribution":"Source: Feed","approved_by":"someone else"}`,
			detail: "not valid JSON",
		},
	}
	for _, tc := range cases {
		t.Run("400 on "+tc.name, func(t *testing.T) {
			t.Parallel()
			// The store must never be reached by an invalid payload.
			h := newHandler(t, errStore{err: errUnexpectedCall})
			wantProblem(t, postApproval(t, h, tc.body), http.StatusBadRequest, tc.detail)
		})
	}
}

// TestApprovalStoreVerdicts pins the mapping from the database's verdicts to
// the contract's status codes.
func TestApprovalStoreVerdicts(t *testing.T) {
	t.Parallel()
	body := `{"source_item_id":"11111111-1111-4111-8111-111111111111","attribution":"Source: Feed","places":["munich"]}`

	cases := []struct {
		name   string
		err    error
		status int
		detail string
	}{
		{name: "409 when the origin already has a live article", err: editorial.ErrOriginAlreadyApproved, status: http.StatusConflict, detail: "already has"},
		{name: "400 when the origin does not exist", err: editorial.ErrUnknownOrigin, status: http.StatusBadRequest, detail: "does not exist"},
		{name: "400 when an untranslated origin has no title", err: editorial.ErrUntitledOrigin, status: http.StatusBadRequest, detail: "no title"},
		{name: "403 when the database refuses a non-editor", err: editorial.ErrNotEditor, status: http.StatusForbidden, detail: "editor role"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := editorial.NewHandler(discardLogger(), &approvalStore{err: tc.err}, fakeAuth{})
			wantProblem(t, postApproval(t, h, body), tc.status, tc.detail)
		})
	}

	t.Run("500 on an unrecognised failure, with nothing leaked", func(t *testing.T) {
		t.Parallel()
		h := editorial.NewHandler(discardLogger(), &approvalStore{err: errors.New("connection torn down")}, fakeAuth{})
		rec := postApproval(t, h, body)
		wantProblem(t, rec, http.StatusInternalServerError, "")
		if strings.Contains(rec.Body.String(), "torn down") {
			t.Error("internal error detail leaked to the wire")
		}
	})
}

// TestApprovalResponseShape pins the 201 payload and that the authenticated
// editor - never a body field - is recorded as the approver.
func TestApprovalResponseShape(t *testing.T) {
	t.Parallel()
	articleID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	originID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	approvedAt := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	t.Run("publish false leaves published_at null", func(t *testing.T) {
		t.Parallel()
		store := &approvalStore{article: editorial.Article{
			ID:         articleID,
			ApprovedBy: testEditor.ID,
			ApprovedAt: approvedAt,
		}}
		h := editorial.NewHandler(discardLogger(), store, fakeAuth{})
		rec := postApproval(t, h, `{"translation_id":"`+originID.String()+`","attribution":"  Source: Feed  ","publish":false,"places":["munich","greece"]}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
		}
		var body approvalBody
		decodeInto(t, rec, &body)
		if body.ArticleID != articleID.String() {
			t.Errorf("article_id = %q, want %q", body.ArticleID, articleID)
		}
		if body.ApprovedBy != testEditor.ID.String() {
			t.Errorf("approved_by = %q, want the authenticated editor %q", body.ApprovedBy, testEditor.ID)
		}
		if body.ApprovedAt != "2026-08-15T12:00:00Z" {
			t.Errorf("approved_at = %q, want RFC 3339 UTC", body.ApprovedAt)
		}
		if body.PublishedAt != nil {
			t.Errorf("published_at = %q, want null on the publish: false path", *body.PublishedAt)
		}

		// The approver is the authenticated editor and the attribution is
		// stored trimmed - the database's not-blank check compares the
		// stored value.
		if store.gotApproval.ApprovedBy != testEditor.ID {
			t.Errorf("approved_by reaching the store = %v, want %v", store.gotApproval.ApprovedBy, testEditor.ID)
		}
		if store.gotApproval.Attribution != "Source: Feed" {
			t.Errorf("attribution reaching the store = %q, want it trimmed", store.gotApproval.Attribution)
		}
		if store.gotApproval.TranslationID == nil || *store.gotApproval.TranslationID != originID {
			t.Errorf("translation_id reaching the store = %v, want %v", store.gotApproval.TranslationID, originID)
		}
		if store.gotApproval.SourceItemID != nil {
			t.Errorf("source_item_id reaching the store = %v, want none", *store.gotApproval.SourceItemID)
		}
		if want := []string{"munich", "greece"}; !slices.Equal(store.gotApproval.Places, want) {
			t.Errorf("places reaching the store = %v, want %v", store.gotApproval.Places, want)
		}
	})

	t.Run("publish true carries published_at", func(t *testing.T) {
		t.Parallel()
		publishedAt := approvedAt
		store := &approvalStore{article: editorial.Article{
			ID:          articleID,
			ApprovedBy:  testEditor.ID,
			ApprovedAt:  approvedAt,
			PublishedAt: &publishedAt,
		}}
		h := editorial.NewHandler(discardLogger(), store, fakeAuth{})
		rec := postApproval(t, h, `{"source_item_id":"`+originID.String()+`","attribution":"Source: Feed","publish":true,"places":["munich"]}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
		}
		var body approvalBody
		decodeInto(t, rec, &body)
		if body.PublishedAt == nil || *body.PublishedAt != "2026-08-15T12:00:00Z" {
			t.Errorf("published_at = %v, want the publication instant", body.PublishedAt)
		}
		if !store.gotApproval.Publish {
			t.Error("publish did not reach the store")
		}
		if store.gotApproval.SourceItemID == nil || *store.gotApproval.SourceItemID != originID {
			t.Errorf("source_item_id reaching the store = %v, want %v", store.gotApproval.SourceItemID, originID)
		}
	})
}

func TestPublicationEndpoint(t *testing.T) {
	t.Parallel()
	articleID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	publishedAt := time.Date(2026, 8, 15, 13, 45, 0, 0, time.UTC)

	t.Run("200 with the article and its publication instant", func(t *testing.T) {
		t.Parallel()
		store := &approvalStore{article: editorial.Article{ID: articleID, PublishedAt: &publishedAt}}
		h := editorial.NewHandler(discardLogger(), store, fakeAuth{})
		rec := postPublication(t, h, articleID.String())
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var body publicationBody
		decodeInto(t, rec, &body)
		if body.ArticleID != articleID.String() {
			t.Errorf("article_id = %q, want %q", body.ArticleID, articleID)
		}
		if body.PublishedAt != "2026-08-15T13:45:00Z" {
			t.Errorf("published_at = %q, want RFC 3339 UTC", body.PublishedAt)
		}
		if store.gotArticleID != articleID {
			t.Errorf("article id reaching the store = %v, want %v", store.gotArticleID, articleID)
		}
		if store.gotEditorID != testEditor.ID {
			t.Errorf("editor reaching the store = %v, want the authenticated editor %v", store.gotEditorID, testEditor.ID)
		}
	})

	t.Run("404 for an unknown article", func(t *testing.T) {
		t.Parallel()
		h := editorial.NewHandler(discardLogger(), &approvalStore{err: editorial.ErrArticleNotFound}, fakeAuth{})
		wantProblem(t, postPublication(t, h, articleID.String()), http.StatusNotFound, "no article")
	})
	t.Run("409 when it is already published", func(t *testing.T) {
		t.Parallel()
		h := editorial.NewHandler(discardLogger(), &approvalStore{err: editorial.ErrAlreadyPublished}, fakeAuth{})
		wantProblem(t, postPublication(t, h, articleID.String()), http.StatusConflict, "already published")
	})
	t.Run("403 when the database refuses a non-editor at the write", func(t *testing.T) {
		t.Parallel()
		h := editorial.NewHandler(discardLogger(), &approvalStore{err: editorial.ErrNotEditor}, fakeAuth{})
		wantProblem(t, postPublication(t, h, articleID.String()), http.StatusForbidden, "editor role")
	})
	t.Run("400 when the path id is not a uuid", func(t *testing.T) {
		t.Parallel()
		h := editorial.NewHandler(discardLogger(), &approvalStore{err: errUnexpectedCall}, fakeAuth{})
		wantProblem(t, postPublication(t, h, "not-a-uuid"), http.StatusBadRequest, "uuid")
	})
	t.Run("500 on an unrecognised failure", func(t *testing.T) {
		t.Parallel()
		h := editorial.NewHandler(discardLogger(), &approvalStore{err: errors.New("connection torn down")}, fakeAuth{})
		rec := postPublication(t, h, articleID.String())
		wantProblem(t, rec, http.StatusInternalServerError, "")
		if strings.Contains(rec.Body.String(), "torn down") {
			t.Error("internal error detail leaked to the wire")
		}
	})
}

// approvalFixture is a seeded world for the approval and publication
// endpoints, isolated in one transaction: a real editor account, a real
// reader account, and one origin per approval path (each origin can only be
// approved once, so they cannot be shared between cases).
type approvalFixture struct {
	editorID, readerID string
	attribution        string

	titled      string // untranslated origin with a title
	titledAgain string // second titled item, for the publish-now path
	untitled    string // the feed provided no title: approval is a 400
	translation string // a translation awaiting approval
	raced       string // an item that gets an article inserted behind the endpoint
	withdrawnSI string // an item whose only article was withdrawn
	forReader   string // a fresh origin, so the reader case fails on the role
}

func seedApprovalFixture(ctx context.Context, t *testing.T, tx pgx.Tx) approvalFixture {
	t.Helper()
	suffix := randomSuffix(t)
	f := approvalFixture{attribution: "Quelle: Approval Feed " + suffix}

	account := func(role string) string {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx,
			`insert into account (email, display_name, role) values ($1, $2, $3) returning id`,
			role+"-"+suffix+"@example.test", "Approval Test "+role+" "+suffix, role).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", role, err)
		}
		return id
	}
	f.editorID = account("editor")
	f.readerID = account("reader")

	var sourceID string
	if err := tx.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'el', 'GR', $3) returning id`,
		"Approval Feed "+suffix, "https://example.test/approval/"+suffix,
		"Extract and link permitted (approval test)").Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	item := func(name string, title *string) string {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx,
			`insert into source_item (source_id, source_url, original_title, raw_body)
			 values ($1, $2, $3, $4) returning id`,
			sourceID, "https://example.test/approval/"+suffix+"/"+name, title,
			"Σώμα "+name+" "+suffix).Scan(&id); err != nil {
			t.Fatalf("seed source_item %s: %v", name, err)
		}
		return id
	}

	f.titled = item("titled", strptr("Τίτλος "+suffix))
	f.titledAgain = item("titled-again", strptr("Δεύτερος τίτλος "+suffix))
	f.untitled = item("untitled", nil)
	f.raced = item("raced", strptr("Αγώνας "+suffix))
	f.withdrawnSI = item("withdrawn", strptr("Αποσυρμένο "+suffix))
	f.forReader = item("for-reader", strptr("Για αναγνώστη "+suffix))

	translated := item("translated", strptr("Μεταφρασμένο "+suffix))
	if err := tx.QueryRow(ctx,
		`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
		 values ($1, 'de', 'test-model-1', 'prompt-v1', $2, $3, 900) returning id`,
		translated, "Überschrift "+suffix, "Auszug "+suffix).Scan(&f.translation); err != nil {
		t.Fatalf("seed translation: %v", err)
	}
	return f
}

// TestApprovalAgainstSchema exercises the endpoints against the real,
// migrated schema: the article the approval creates, the domain events in
// the same transaction, and the 403 and 409 the database itself decides.
func TestApprovalAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise approvals against Postgres")
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

	f := seedApprovalFixture(ctx, t, tx)
	editorID := uuid.MustParse(f.editorID)
	h := editorial.NewHandler(discardLogger(), editorial.NewPGStore(tx),
		staticAuth{editor: editorial.Editor{ID: editorID, Email: "editor@example.test", DisplayName: "Approval Editor"}})

	// eventCount reports how many events of a type name this article.
	eventCount := func(t *testing.T, eventType, articleID string) int {
		t.Helper()
		var n int
		if err := tx.QueryRow(ctx,
			`select count(*) from domain_event where type = $1 and payload->>'article_id' = $2`,
			eventType, articleID).Scan(&n); err != nil {
			t.Fatalf("counting %s events: %v", eventType, err)
		}
		return n
	}

	var approvedUnpublished string

	t.Run("201 approving an untranslated origin without publishing", func(t *testing.T) {
		rec := postApproval(t, h, `{"source_item_id":"`+f.titled+`","attribution":`+jsonString(t, f.attribution)+`,"publish":false,"places":["munich"]}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
		}
		var body approvalBody
		decodeInto(t, rec, &body)
		approvedUnpublished = body.ArticleID
		if body.ApprovedBy != f.editorID {
			t.Errorf("approved_by = %q, want the seeded editor %q", body.ApprovedBy, f.editorID)
		}
		if body.PublishedAt != nil {
			t.Errorf("published_at = %q, want null", *body.PublishedAt)
		}

		// The row exists, with the origin and attribution the request named
		// and no publication.
		var (
			originID    string
			attribution string
			published   *time.Time
		)
		if err := tx.QueryRow(ctx,
			`select source_item_id, attribution_block, published_at from article where id = $1`,
			body.ArticleID).Scan(&originID, &attribution, &published); err != nil {
			t.Fatalf("reading approved article: %v", err)
		}
		if originID != f.titled || attribution != f.attribution || published != nil {
			t.Errorf("article row = (%s, %q, %v), want (%s, %q, nil)", originID, attribution, published, f.titled, f.attribution)
		}

		// The audit record committed with the approval, and only the
		// approval - nothing was published.
		if got := eventCount(t, "article.approved", body.ArticleID); got != 1 {
			t.Errorf("article.approved events = %d, want 1", got)
		}
		if got := eventCount(t, "article.published", body.ArticleID); got != 0 {
			t.Errorf("article.published events = %d, want none before publication", got)
		}
	})

	t.Run("200 publishing it, 409 the second time", func(t *testing.T) {
		rec := postPublication(t, h, approvedUnpublished)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var body publicationBody
		decodeInto(t, rec, &body)
		if _, err := time.Parse(time.RFC3339Nano, body.PublishedAt); err != nil {
			t.Errorf("published_at %q is not RFC 3339: %v", body.PublishedAt, err)
		}
		if got := eventCount(t, "article.published", approvedUnpublished); got != 1 {
			t.Errorf("article.published events = %d, want 1", got)
		}

		// The database permits published_at to be set exactly once, so the
		// repeat is a conflict and writes no second event.
		wantProblem(t, postPublication(t, h, approvedUnpublished), http.StatusConflict, "already published")
		if got := eventCount(t, "article.published", approvedUnpublished); got != 1 {
			t.Errorf("article.published events after the rejected repeat = %d, want still 1", got)
		}
	})

	t.Run("404 publishing an article that does not exist", func(t *testing.T) {
		wantProblem(t, postPublication(t, h, uuid.NewString()), http.StatusNotFound, "no article")
	})

	t.Run("201 with publish true sets published_at and both events", func(t *testing.T) {
		rec := postApproval(t, h, `{"source_item_id":"`+f.titledAgain+`","attribution":`+jsonString(t, f.attribution)+`,"publish":true,"places":["munich"]}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
		}
		var body approvalBody
		decodeInto(t, rec, &body)
		if body.PublishedAt == nil {
			t.Fatal("published_at = null on the publish: true path")
		}
		if got := eventCount(t, "article.approved", body.ArticleID); got != 1 {
			t.Errorf("article.approved events = %d, want 1", got)
		}
		if got := eventCount(t, "article.published", body.ArticleID); got != 1 {
			t.Errorf("article.published events = %d, want 1", got)
		}
	})

	t.Run("201 approving a translation", func(t *testing.T) {
		rec := postApproval(t, h, `{"translation_id":"`+f.translation+`","attribution":`+jsonString(t, f.attribution)+`,"places":["munich"]}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
		}
		var body approvalBody
		decodeInto(t, rec, &body)
		var originID string
		if err := tx.QueryRow(ctx, `select translation_id from article where id = $1`, body.ArticleID).Scan(&originID); err != nil {
			t.Fatalf("reading approved article: %v", err)
		}
		if originID != f.translation {
			t.Errorf("translation_id = %q, want %q", originID, f.translation)
		}
	})

	t.Run("409 when the origin already has a live article", func(t *testing.T) {
		rec := postApproval(t, h, `{"translation_id":"`+f.translation+`","attribution":`+jsonString(t, f.attribution)+`,"places":["munich"]}`)
		wantProblem(t, rec, http.StatusConflict, "already has")
	})

	// The 409 is the database's, not this package's: an article inserted
	// behind the endpoint's back - exactly what a concurrent approver would
	// do - still loses to the partial unique index. There is no application
	// pre-check that could have caught this one.
	t.Run("409 from the index when an article appears behind the endpoint", func(t *testing.T) {
		var racedArticle string
		if err := tx.QueryRow(ctx,
			`insert into article (source_item_id, approved_by, attribution_block)
			 values ($1, $2, $3) returning id`,
			f.raced, f.editorID, f.attribution).Scan(&racedArticle); err != nil {
			t.Fatalf("inserting the racing article: %v", err)
		}
		rec := postApproval(t, h, `{"source_item_id":"`+f.raced+`","attribution":`+jsonString(t, f.attribution)+`,"places":["munich"]}`)
		wantProblem(t, rec, http.StatusConflict, "already has")

		// The losing approval left nothing behind: no second article, and
		// no orphaned audit event.
		var articles int
		if err := tx.QueryRow(ctx, `select count(*) from article where source_item_id = $1`, f.raced).Scan(&articles); err != nil {
			t.Fatalf("counting articles: %v", err)
		}
		if articles != 1 {
			t.Errorf("articles on the raced origin = %d, want only the one inserted directly", articles)
		}
		if got := eventCount(t, "article.approved", racedArticle); got != 0 {
			t.Errorf("article.approved events for the directly inserted article = %d, want none", got)
		}
	})

	// The one-per-origin indexes are partial on withdrawn_at IS NULL, so a
	// withdrawn origin may be approved again: the documented correction flow.
	t.Run("201 re-approving an origin whose only article was withdrawn", func(t *testing.T) {
		var first string
		if err := tx.QueryRow(ctx,
			`insert into article (source_item_id, approved_by, published_at, attribution_block)
			 values ($1, $2, now(), $3) returning id`,
			f.withdrawnSI, f.editorID, f.attribution).Scan(&first); err != nil {
			t.Fatalf("seeding the article to withdraw: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`update article set withdrawn_at = now(), withdrawn_by = $2, withdrawal_reason = 'correction'
			  where id = $1`, first, f.editorID); err != nil {
			t.Fatalf("withdrawing: %v", err)
		}
		rec := postApproval(t, h, `{"source_item_id":"`+f.withdrawnSI+`","attribution":`+jsonString(t, f.attribution)+`,"publish":true,"places":["greece"]}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 - a withdrawn origin is free for a correction (body %q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("400 approving an untranslated origin with no title", func(t *testing.T) {
		rec := postApproval(t, h, `{"source_item_id":"`+f.untitled+`","attribution":`+jsonString(t, f.attribution)+`,"places":["munich"]}`)
		wantProblem(t, rec, http.StatusBadRequest, "no title")
	})

	t.Run("400 naming an origin that does not exist", func(t *testing.T) {
		unknown := uuid.NewString()
		wantProblem(t, postApproval(t, h,
			`{"source_item_id":"`+unknown+`","attribution":`+jsonString(t, f.attribution)+`,"places":["munich"]}`),
			http.StatusBadRequest, "does not exist")
		wantProblem(t, postApproval(t, h,
			`{"translation_id":"`+unknown+`","attribution":`+jsonString(t, f.attribution)+`,"places":["munich"]}`),
			http.StatusBadRequest, "does not exist")
	})

	// The HTTP gate and the database trigger are two independent checks of
	// the same rule. Here the gate is told the caller is an editor and the
	// account row says otherwise: the trigger refuses, and the endpoint
	// mirrors it as 403 rather than leaking a 500.
	t.Run("403 when the database says the approver is not an editor", func(t *testing.T) {
		readerHandler := editorial.NewHandler(discardLogger(), editorial.NewPGStore(tx),
			staticAuth{editor: editorial.Editor{ID: uuid.MustParse(f.readerID)}})
		rec := doJSON(t, readerHandler, http.MethodPost, "/api/v1/editorial/approvals", editorToken,
			`{"source_item_id":"`+f.forReader+`","attribution":`+jsonString(t, f.attribution)+`,"places":["munich"]}`)
		wantProblem(t, rec, http.StatusForbidden, "editor role")

		// The refusal left no article behind: the guard runs before the row
		// is written, so a reader cannot become an approver of record.
		var articles int
		if err := tx.QueryRow(ctx, `select count(*) from article where source_item_id = $1`, f.forReader).Scan(&articles); err != nil {
			t.Fatalf("counting articles: %v", err)
		}
		if articles != 0 {
			t.Errorf("articles on the origin a reader tried to approve = %d, want none", articles)
		}
	})
}

// TestApprovalWithNoPlaceIsRejected pins the placeless approval to a 400
// answered before any write: the front page is scoped by place, so an
// article tagged to no place is one no reader can ever reach, and the
// database would refuse it at commit anyway (the 0006 trigger). The store
// here fails the test if it is touched at all.
func TestApprovalWithNoPlaceIsRejected(t *testing.T) {
	t.Parallel()
	h := newHandler(t, errStore{err: errUnexpectedCall})

	t.Run("absent places", func(t *testing.T) {
		t.Parallel()
		rec := postApproval(t, h,
			`{"translation_id":"11111111-1111-4111-8111-111111111111","attribution":"Source: Feed"}`)
		wantProblem(t, rec, http.StatusBadRequest, "at least one place")
	})
	t.Run("empty places", func(t *testing.T) {
		t.Parallel()
		rec := postApproval(t, h,
			`{"translation_id":"11111111-1111-4111-8111-111111111111","attribution":"Source: Feed","places":[]}`)
		wantProblem(t, rec, http.StatusBadRequest, "at least one place")
	})
}

// approvalSchemaFixture opens a rolled-back transaction against the real,
// migrated schema and seeds the approval world in it, for the tests below
// that need the database's own verdicts on places.
func approvalSchemaFixture(ctx context.Context, t *testing.T) (pgx.Tx, approvalFixture, http.Handler) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise approvals against Postgres")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}
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

	f := seedApprovalFixture(ctx, t, tx)
	h := editorial.NewHandler(discardLogger(), editorial.NewPGStore(tx),
		staticAuth{editor: editorial.Editor{ID: uuid.MustParse(f.editorID), Email: "editor@example.test", DisplayName: "Approval Editor"}})
	return tx, f, h
}

// TestApprovalWithAnUnknownPlaceIsRejected pins the 400 for a slug the
// place table does not know - named, in the same vocabulary the reader's
// front page uses - and that the failed approval rolled back whole.
func TestApprovalWithAnUnknownPlaceIsRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tx, f, h := approvalSchemaFixture(ctx, t)

	rec := postApproval(t, h,
		`{"source_item_id":"`+f.titled+`","attribution":`+jsonString(t, f.attribution)+`,"places":["munich","atlantis"]}`)
	wantProblem(t, rec, http.StatusBadRequest, `unknown place "atlantis"`)

	// Nothing survived the refusal: no article, and with it no
	// article_place rows for the known slug either - the approval is one
	// transaction, and it rolled back whole.
	var articles int
	if err := tx.QueryRow(ctx,
		`select count(*) from article where source_item_id = $1`, f.titled).Scan(&articles); err != nil {
		t.Fatalf("counting articles: %v", err)
	}
	if articles != 0 {
		t.Errorf("articles left behind by the refused approval = %d, want none", articles)
	}
}

// TestApprovalTagsEveryPlaceItWasGiven pins the write itself: every slug
// the approval names becomes an article_place row in the approving
// transaction, which is what the 0006 trigger demands at commit and what
// the front page's EXISTS reads.
func TestApprovalTagsEveryPlaceItWasGiven(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tx, f, h := approvalSchemaFixture(ctx, t)

	rec := postApproval(t, h,
		`{"source_item_id":"`+f.titled+`","attribution":`+jsonString(t, f.attribution)+`,"publish":true,"places":["munich","greece"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	var body approvalBody
	decodeInto(t, rec, &body)

	rows, err := tx.Query(ctx,
		`select p.slug from article_place ap join place p on p.id = ap.place_id
		  where ap.article_id = $1 order by p.slug`, body.ArticleID)
	if err != nil {
		t.Fatalf("reading article places: %v", err)
	}
	slugs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collecting article places: %v", err)
	}
	if want := []string{"greece", "munich"}; !slices.Equal(slugs, want) {
		t.Errorf("article_place slugs = %v, want %v", slugs, want)
	}
}
