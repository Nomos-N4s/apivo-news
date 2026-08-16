package editorial_test

// Contract tests for PATCH and DELETE /api/v1/editorial/sources/{id}
// (#118): edits as licensing events with the source.updated audit line,
// and deletion decided by the database - the source_item FK's 23503
// mapped to the 409 that names the evidence count.
//
// The database-backed tests run inside one transaction that is rolled
// back at the end: the source table is the founder's live one, and these
// tests write update events and deletions that must never outlive the
// test.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// manageStore answers source updates and deletes with canned results and
// records what the handler asked for.
type manageStore struct {
	updated   editorial.ListedSource
	updateErr error
	deleteErr error

	gotID       uuid.UUID
	gotEditorID uuid.UUID
	gotPatch    editorial.SourcePatch
	deletedID   uuid.UUID
}

func (s *manageStore) UpdateSource(_ context.Context, id, editorID uuid.UUID, patch editorial.SourcePatch) (editorial.ListedSource, error) {
	s.gotID, s.gotEditorID, s.gotPatch = id, editorID, patch
	return s.updated, s.updateErr
}

func (s *manageStore) DeleteSource(_ context.Context, id uuid.UUID) error {
	s.deletedID = id
	return s.deleteErr
}

func (s *manageStore) CreateSource(context.Context, editorial.NewSource) (editorial.Source, error) {
	return editorial.Source{}, errUnexpectedCall
}

func (s *manageStore) ListSources(context.Context, editorial.SourcesQuery) (editorial.SourcesPage, error) {
	return editorial.SourcesPage{}, errUnexpectedCall
}

func (s *manageStore) LastPollCycle(context.Context) (editorial.PollCycle, error) {
	return editorial.PollCycle{}, errUnexpectedCall
}

func (s *manageStore) ReviewQueue(context.Context, editorial.QueueQuery) (editorial.QueuePage, error) {
	return editorial.QueuePage{}, errUnexpectedCall
}

func (s *manageStore) Approve(context.Context, editorial.NewApproval) (editorial.Article, error) {
	return editorial.Article{}, errUnexpectedCall
}

func (s *manageStore) Publish(context.Context, uuid.UUID, uuid.UUID) (editorial.Article, error) {
	return editorial.Article{}, errUnexpectedCall
}

func (s *manageStore) Withdraw(context.Context, uuid.UUID, uuid.UUID, string) (editorial.Withdrawal, error) {
	return editorial.Withdrawal{}, errUnexpectedCall
}

func (s *manageStore) Provenance(context.Context, uuid.UUID) (editorial.Provenance, error) {
	return editorial.Provenance{}, errUnexpectedCall
}

const manageID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

func TestSourceManagementAuth(t *testing.T) {
	t.Parallel()
	h := newHandler(t, errStore{err: errUnexpectedCall})
	path := "/api/v1/editorial/sources/" + manageID

	cases := []struct{ name, method, body string }{
		{name: "PATCH", method: http.MethodPatch, body: `{"active":false}`},
		{name: "DELETE", method: http.MethodDelete, body: ""},
	}
	for _, tc := range cases {
		t.Run("401 on "+tc.name+" without a token", func(t *testing.T) {
			t.Parallel()
			rec := doJSON(t, h, tc.method, path, "", tc.body)
			wantProblem(t, rec, http.StatusUnauthorized, "bearer token is required")
		})
		t.Run("403 on "+tc.name+" for a non-editor", func(t *testing.T) {
			t.Parallel()
			rec := doJSON(t, h, tc.method, path, readerToken, tc.body)
			wantProblem(t, rec, http.StatusForbidden, "editor role")
		})
	}
}

func TestPatchSourceValidation(t *testing.T) {
	t.Parallel()
	// The store must never be reached by a payload that was not accepted.
	h := newHandler(t, errStore{err: errUnexpectedCall})
	path := "/api/v1/editorial/sources/" + manageID

	cases := []struct{ name, path, body, detail string }{
		{
			name: "a non-uuid id", path: "/api/v1/editorial/sources/not-a-uuid",
			body: `{"active":false}`, detail: "source id in the path must be a uuid",
		},
		{
			name: "an unknown field", path: path,
			body: `{"jurisdiction":"DE"}`, detail: "not valid JSON for this endpoint",
		},
		{
			// usage_rule stays a founder-gated flow, so the PATCH does not
			// even name the field: it is unknown here, per the decoder.
			name: "usage_rule", path: path,
			body: `{"usage_rule":"full_text"}`, detail: "not valid JSON for this endpoint",
		},
		{
			name: "an empty patch", path: path,
			body: `{}`, detail: "supply at least one of name, url, active or licence_terms",
		},
		{
			name: "a blank name", path: path,
			body: `{"name":"   "}`, detail: "name is required and must not be blank",
		},
		{
			name: "a blank licence", path: path,
			body: `{"licence_terms":""}`, detail: "licence_terms is required and must not be blank",
		},
		// The url cases carry createSource's own words: the PATCH runs the
		// SAME validation function, and these details prove no second,
		// laxer copy exists.
		{
			name: "a blank url", path: path,
			body: `{"url":"  "}`, detail: "url is required and must not be blank",
		},
		{
			name: "a relative url", path: path,
			body: `{"url":"/feed.xml"}`, detail: "url must be an absolute http or https URL with a host",
		},
		{
			name: "a non-http url", path: path,
			body: `{"url":"ftp://example.org/feed.xml"}`, detail: "url must be an absolute http or https URL with a host",
		},
		{
			name: "a hostless url", path: path,
			body: `{"url":"http://"}`, detail: "url must be an absolute http or https URL with a host",
		},
	}
	for _, tc := range cases {
		t.Run("400 on "+tc.name, func(t *testing.T) {
			t.Parallel()
			rec := doJSON(t, h, http.MethodPatch, tc.path, editorToken, tc.body)
			wantProblem(t, rec, http.StatusBadRequest, tc.detail)
		})
	}
}

// TestPatchSourceCarriesEachField pins what reaches the store: each
// supplied field as a pointer, absent fields as nil, the path id and the
// authenticated editor - and the url trimmed exactly as on registration.
func TestPatchSourceCarriesEachField(t *testing.T) {
	t.Parallel()
	store := &manageStore{updated: editorial.ListedSource{ID: uuid.MustParse(manageID)}}
	h := editorial.NewHandler(discardLogger(), store, fakeAuth{})

	body := `{"name":"Renamed Feed","url":"  https://example.test/feed/renamed  ","active":false,"licence_terms":"New terms"}`
	rec := doJSON(t, h, http.MethodPatch, "/api/v1/editorial/sources/"+manageID, editorToken, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if store.gotID != uuid.MustParse(manageID) {
		t.Errorf("id reaching the store = %s, want %s", store.gotID, manageID)
	}
	if store.gotEditorID != testEditor.ID {
		t.Errorf("editor id reaching the store = %s, want the authenticated editor %s", store.gotEditorID, testEditor.ID)
	}
	p := store.gotPatch
	if p.Name == nil || *p.Name != "Renamed Feed" {
		t.Errorf("patch name = %v, want Renamed Feed", p.Name)
	}
	if p.URL == nil || *p.URL != "https://example.test/feed/renamed" {
		t.Errorf("patch url = %v, want the trimmed feed URL", p.URL)
	}
	if p.Active == nil || *p.Active {
		t.Errorf("patch active = %v, want false", p.Active)
	}
	if p.LicenceTerms == nil || *p.LicenceTerms != "New terms" {
		t.Errorf("patch licence_terms = %v, want New terms", p.LicenceTerms)
	}
}

// TestPatchSourceSubset pins that one supplied field leaves the other
// three nil: "absent" must reach the store as absent, never as a zero
// value that would blank a column.
func TestPatchSourceSubset(t *testing.T) {
	t.Parallel()
	store := &manageStore{updated: editorial.ListedSource{ID: uuid.MustParse(manageID)}}
	h := editorial.NewHandler(discardLogger(), store, fakeAuth{})

	rec := doJSON(t, h, http.MethodPatch, "/api/v1/editorial/sources/"+manageID, editorToken, `{"active":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	p := store.gotPatch
	if p.Active == nil || !*p.Active {
		t.Errorf("patch active = %v, want true", p.Active)
	}
	if p.Name != nil || p.URL != nil || p.LicenceTerms != nil {
		t.Errorf("unsupplied fields reached the store non-nil: name %v, url %v, licence_terms %v", p.Name, p.URL, p.LicenceTerms)
	}
}

func TestPatchSourceStoreVerdicts(t *testing.T) {
	t.Parallel()

	t.Run("404 for an unknown id", func(t *testing.T) {
		t.Parallel()
		h := newHandler(t, &manageStore{updateErr: editorial.ErrSourceNotFound})
		rec := doJSON(t, h, http.MethodPatch, "/api/v1/editorial/sources/"+manageID, editorToken, `{"active":false}`)
		wantProblem(t, rec, http.StatusNotFound, "no source with this id")
	})
	t.Run("409 for a url already registered", func(t *testing.T) {
		t.Parallel()
		h := newHandler(t, &manageStore{updateErr: editorial.ErrDuplicateSourceURL})
		rec := doJSON(t, h, http.MethodPatch, "/api/v1/editorial/sources/"+manageID, editorToken,
			`{"url":"https://example.test/feed/taken"}`)
		wantProblem(t, rec, http.StatusConflict, "already registered")
	})
	t.Run("500 for a store failure", func(t *testing.T) {
		t.Parallel()
		h := newHandler(t, errStore{err: errUnexpectedCall})
		rec := doJSON(t, h, http.MethodPatch, "/api/v1/editorial/sources/"+manageID, editorToken, `{"active":false}`)
		wantProblem(t, rec, http.StatusInternalServerError, "")
	})
}

func TestDeleteSourceVerdicts(t *testing.T) {
	t.Parallel()
	path := "/api/v1/editorial/sources/" + manageID

	t.Run("400 for a non-uuid id", func(t *testing.T) {
		t.Parallel()
		h := newHandler(t, errStore{err: errUnexpectedCall})
		rec := doJSON(t, h, http.MethodDelete, "/api/v1/editorial/sources/not-a-uuid", editorToken, "")
		wantProblem(t, rec, http.StatusBadRequest, "source id in the path must be a uuid")
	})
	t.Run("404 for an unknown id", func(t *testing.T) {
		t.Parallel()
		h := newHandler(t, &manageStore{deleteErr: editorial.ErrSourceNotFound})
		rec := doJSON(t, h, http.MethodDelete, path, editorToken, "")
		wantProblem(t, rec, http.StatusNotFound, "no source with this id")
	})
	t.Run("409 naming the evidence count", func(t *testing.T) {
		t.Parallel()
		h := newHandler(t, &manageStore{deleteErr: editorial.SourceEvidenceError{Items: 12}})
		rec := doJSON(t, h, http.MethodDelete, path, editorToken, "")
		wantProblem(t, rec, http.StatusConflict, "12 retrieved item(s)")
		// The refusal points at the everyday remove; it never performs it.
		if !strings.Contains(rec.Body.String(), "deactivate") {
			t.Errorf("the refusal %q does not point at deactivation", rec.Body.String())
		}
	})
	t.Run("204 with an empty body", func(t *testing.T) {
		t.Parallel()
		store := &manageStore{}
		h := editorial.NewHandler(discardLogger(), store, fakeAuth{})
		rec := doJSON(t, h, http.MethodDelete, path, editorToken, "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 (body %q)", rec.Code, rec.Body.String())
		}
		if rec.Body.Len() != 0 {
			t.Errorf("204 carried a body: %q", rec.Body.String())
		}
		if store.deletedID != uuid.MustParse(manageID) {
			t.Errorf("deleted id = %s, want %s", store.deletedID, manageID)
		}
	})
	t.Run("500 for a store failure", func(t *testing.T) {
		t.Parallel()
		h := newHandler(t, errStore{err: errUnexpectedCall})
		rec := doJSON(t, h, http.MethodDelete, path, editorToken, "")
		wantProblem(t, rec, http.StatusInternalServerError, "")
	})
}

// TestSourceManagementAgainstSchema exercises the full handler-to-database
// path inside one rolled-back transaction: the PATCH's row and its
// source.updated event with old and new values, the FK's refusal of a
// delete over evidence as the 409 naming the count, and the clean delete
// of a source that never yielded an item.
func TestSourceManagementAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the contract against Postgres")
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
	defer func() { _ = tx.Rollback(ctx) }()

	editorID := uuid.New()
	h := editorial.NewHandler(discardLogger(), editorial.NewPGStore(tx),
		staticAuth{editor: editorial.Editor{ID: editorID, Email: "sources@example.test", DisplayName: "Sources Editor"}})

	seedSource := func(t *testing.T, name string) string {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx,
			`insert into source (name, url, language_code, jurisdiction, licence_terms)
			 values ($1, $2, 'el', 'GR', 'Original terms v1') returning id`,
			name, "https://example.test/feed/manage-"+randomSuffix(t)).Scan(&id); err != nil {
			t.Fatalf("seeding source: %v", err)
		}
		return id
	}

	t.Run("a licence edit lands and appends source.updated with old and new", func(t *testing.T) {
		id := seedSource(t, "Manage Feed "+randomSuffix(t))

		rec := doJSON(t, h, http.MethodPatch, "/api/v1/editorial/sources/"+id, editorToken,
			`{"licence_terms":"Renegotiated terms v2"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var body struct {
			ID           string `json:"id"`
			LicenceTerms string `json:"licence_terms"`
			UsageRule    string `json:"usage_rule"`
			Active       bool   `json:"active"`
			CreatedAt    string `json:"created_at"`
		}
		decodeInto(t, rec, &body)
		if body.ID != id || body.LicenceTerms != "Renegotiated terms v2" {
			t.Errorf("response = %+v, want the updated row", body)
		}
		if body.UsageRule != "extract_and_link" || !body.Active {
			t.Errorf("untouched columns moved: usage_rule %q, active %v", body.UsageRule, body.Active)
		}
		if _, err := time.Parse(time.RFC3339Nano, body.CreatedAt); err != nil {
			t.Errorf("created_at %q is not RFC 3339: %v", body.CreatedAt, err)
		}

		var stored string
		if err := tx.QueryRow(ctx, `select licence_terms from source where id = $1`, id).Scan(&stored); err != nil {
			t.Fatalf("reading the row back: %v", err)
		}
		if stored != "Renegotiated terms v2" {
			t.Errorf("licence_terms = %q, want the new terms", stored)
		}

		// The audit line: one source.updated event, old and new terms both
		// on record - "what did we believe the terms were, when" answered.
		var payload []byte
		if err := tx.QueryRow(ctx,
			`select payload from domain_event where type = 'source.updated' and payload->>'source_id' = $1`, id).
			Scan(&payload); err != nil {
			t.Fatalf("reading the source.updated event: %v", err)
		}
		var event struct {
			SourceID     string `json:"source_id"`
			UpdatedBy    string `json:"updated_by"`
			LicenceTerms *struct {
				Old string `json:"old"`
				New string `json:"new"`
			} `json:"licence_terms"`
			URL *json.RawMessage `json:"url"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatalf("unmarshalling event payload %q: %v", payload, err)
		}
		if event.UpdatedBy != editorID.String() {
			t.Errorf("updated_by = %q, want the authenticated editor %s", event.UpdatedBy, editorID)
		}
		if event.LicenceTerms == nil {
			t.Fatalf("event payload %s carries no licence_terms change", payload)
		}
		if event.LicenceTerms.Old != "Original terms v1" || event.LicenceTerms.New != "Renegotiated terms v2" {
			t.Errorf("licence_terms change = %+v, want old and new on record", event.LicenceTerms)
		}
		if event.URL != nil {
			t.Errorf("event payload %s records a url change no patch made", payload)
		}
	})

	t.Run("a url edit records old and new", func(t *testing.T) {
		id := seedSource(t, "URL Feed "+randomSuffix(t))
		newURL := "https://example.test/feed/moved-" + randomSuffix(t)

		rec := doJSON(t, h, http.MethodPatch, "/api/v1/editorial/sources/"+id, editorToken,
			`{"url":`+jsonString(t, newURL)+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var oldURL, gotNew string
		if err := tx.QueryRow(ctx,
			`select payload->'url'->>'old', payload->'url'->>'new'
			   from domain_event where type = 'source.updated' and payload->>'source_id' = $1`, id).
			Scan(&oldURL, &gotNew); err != nil {
			t.Fatalf("reading the url change: %v", err)
		}
		if !strings.HasPrefix(oldURL, "https://example.test/feed/manage-") || gotNew != newURL {
			t.Errorf("url change = (%q, %q), want the old and the new feed URL", oldURL, gotNew)
		}
	})

	t.Run("a patch restating the row appends no event", func(t *testing.T) {
		id := seedSource(t, "Restated Feed "+randomSuffix(t))

		rec := doJSON(t, h, http.MethodPatch, "/api/v1/editorial/sources/"+id, editorToken,
			`{"licence_terms":"Original terms v1"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var events int
		if err := tx.QueryRow(ctx,
			`select count(*) from domain_event where type = 'source.updated' and payload->>'source_id' = $1`, id).
			Scan(&events); err != nil {
			t.Fatalf("counting events: %v", err)
		}
		if events != 0 {
			t.Errorf("source.updated events = %d, want 0 - the stream records edits, not re-statements", events)
		}
	})

	t.Run("patching the url to another source's is a 409", func(t *testing.T) {
		first := seedSource(t, "Holder Feed "+randomSuffix(t))
		second := seedSource(t, "Claimant Feed "+randomSuffix(t))
		var taken string
		if err := tx.QueryRow(ctx, `select url from source where id = $1`, first).Scan(&taken); err != nil {
			t.Fatalf("reading the held url: %v", err)
		}
		rec := doJSON(t, h, http.MethodPatch, "/api/v1/editorial/sources/"+second, editorToken,
			`{"url":`+jsonString(t, taken)+`}`)
		wantProblem(t, rec, http.StatusConflict, "already registered")
	})

	t.Run("an unknown id is a 404", func(t *testing.T) {
		rec := doJSON(t, h, http.MethodPatch, "/api/v1/editorial/sources/"+uuid.NewString(), editorToken,
			`{"active":false}`)
		wantProblem(t, rec, http.StatusNotFound, "no source with this id")
	})

	t.Run("a source with evidence cannot be deleted, and the refusal names the count", func(t *testing.T) {
		id := seedSource(t, "Evidence Feed "+randomSuffix(t))
		for i := 0; i < 2; i++ {
			if _, err := tx.Exec(ctx,
				`insert into source_item (source_id, source_url, original_title, raw_body)
				 values ($1, $2, 'Retrieved item', $3)`,
				id, "https://example.test/articles/"+randomSuffix(t), "body "+randomSuffix(t)); err != nil {
				t.Fatalf("seeding evidence: %v", err)
			}
		}

		rec := doJSON(t, h, http.MethodDelete, "/api/v1/editorial/sources/"+id, editorToken, "")
		wantProblem(t, rec, http.StatusConflict, "2 retrieved item(s)")

		// The FK's verdict rolled the delete back whole: row and evidence
		// both stand.
		var sources, items int
		if err := tx.QueryRow(ctx, `select count(*) from source where id = $1`, id).Scan(&sources); err != nil {
			t.Fatalf("counting the source: %v", err)
		}
		if err := tx.QueryRow(ctx, `select count(*) from source_item where source_id = $1`, id).Scan(&items); err != nil {
			t.Fatalf("counting the evidence: %v", err)
		}
		if sources != 1 || items != 2 {
			t.Errorf("after the refused delete: %d source rows and %d items, want 1 and 2", sources, items)
		}
	})

	t.Run("a source without evidence deletes with 204", func(t *testing.T) {
		id := seedSource(t, "Deletable Feed "+randomSuffix(t))

		rec := doJSON(t, h, http.MethodDelete, "/api/v1/editorial/sources/"+id, editorToken, "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 (body %q)", rec.Code, rec.Body.String())
		}
		var count int
		if err := tx.QueryRow(ctx, `select count(*) from source where id = $1`, id).Scan(&count); err != nil {
			t.Fatalf("counting the source: %v", err)
		}
		if count != 0 {
			t.Errorf("source rows after the delete = %d, want 0", count)
		}
	})

	t.Run("deleting an unknown id is a 404", func(t *testing.T) {
		rec := doJSON(t, h, http.MethodDelete, "/api/v1/editorial/sources/"+uuid.NewString(), editorToken, "")
		wantProblem(t, rec, http.StatusNotFound, "no source with this id")
	})
}
