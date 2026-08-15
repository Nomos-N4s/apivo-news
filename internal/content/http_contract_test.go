package content_test

// Contract tests for the public reader endpoints (T024): statuses, shapes
// and error bodies exactly as contracts/http-api.md promises, exercised via
// httptest against the real, migrated schema.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/content"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// itemJSON mirrors the contract's reader item; approved_at is present only
// on the article payload.
type itemJSON struct {
	ID          string     `json:"id"`
	Headline    string     `json:"headline"`
	Extract     string     `json:"extract"`
	Lang        string     `json:"lang"`
	Places      []string   `json:"places"`
	Attribution string     `json:"attribution"`
	SourceURL   string     `json:"source_url"`
	PublishedAt time.Time  `json:"published_at"`
	ApprovedAt  *time.Time `json:"approved_at"`
}

type frontJSON struct {
	Items      []itemJSON `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

type problemJSON struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding response %q: %v", rec.Body.String(), err)
	}
	return v
}

func itemIDs(items []itemJSON) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

func uuidString(u pgtype.UUID) string { return uuid.UUID(u.Bytes).String() }

// wantProblem asserts an RFC 9457 problem+json response of the given status.
func wantProblem(t *testing.T, rec *httptest.ResponseRecorder, status int) problemJSON {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), status)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	p := decodeBody[problemJSON](t, rec)
	if p.Type == "" || p.Title == "" || p.Detail == "" || p.Status != status {
		t.Errorf("problem body %+v lacks the RFC 9457 fields for status %d", p, status)
	}
	return p
}

func TestReaderEndpoints(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the reader endpoints")
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

	f := seedReaderFixture(ctx, t, tx)
	h := content.NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), tx)

	midFirst, midSecond := f.deMidA, f.deMidB
	if uuidString(f.deMidB) > uuidString(f.deMidA) {
		midFirst, midSecond = f.deMidB, f.deMidA
	}

	t.Run("front serves the translated feed newest first", func(t *testing.T) {
		rec := get(t, h, "/api/v1/front?lang=de&place="+f.alphaSlug)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		resp := decodeBody[frontJSON](t, rec)
		want := []string{uuidString(f.deNew), uuidString(midFirst), uuidString(midSecond), uuidString(f.deOld)}
		if got := itemIDs(resp.Items); !slices.Equal(got, want) {
			t.Fatalf("item ids = %v, want %v", got, want)
		}
		if resp.NextCursor != nil {
			t.Errorf("next_cursor = %q, want null on the final page", *resp.NextCursor)
		}
		top := resp.Items[0]
		if top.Headline != f.deNewHeadline {
			t.Errorf("headline = %q, want the translation headline %q", top.Headline, f.deNewHeadline)
		}
		if top.Extract != f.deNewExtract {
			t.Errorf("extract = %q, want the translation extract %q", top.Extract, f.deNewExtract)
		}
		if top.Lang != "de" {
			t.Errorf("lang = %q, want de", top.Lang)
		}
		if !slices.Equal(top.Places, []string{f.alphaSlug}) {
			t.Errorf("places = %v, want [%s]", top.Places, f.alphaSlug)
		}
		if top.Attribution != f.deAttribution {
			t.Errorf("attribution = %q, want %q", top.Attribution, f.deAttribution)
		}
		if !strings.HasSuffix(top.SourceURL, "/de-new") {
			t.Errorf("source_url = %q, want the retrieved item's url", top.SourceURL)
		}
		if !top.PublishedAt.Equal(f.deNewPublished) {
			t.Errorf("published_at = %v, want %v", top.PublishedAt, f.deNewPublished)
		}
		if top.ApprovedAt != nil {
			t.Errorf("front item carries approved_at %v; that field belongs to the article payload", top.ApprovedAt)
		}
	})

	t.Run("front serves the untranslated shape with the derived extract", func(t *testing.T) {
		rec := get(t, h, "/api/v1/front?lang=el&place="+f.alphaSlug+"&place="+f.betaSlug)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
		}
		resp := decodeBody[frontJSON](t, rec)
		if len(resp.Items) != 1 {
			t.Fatalf("items = %v, want exactly the one untranslated article", itemIDs(resp.Items))
		}
		got := resp.Items[0]
		if got.ID != uuidString(f.elBoth) {
			t.Errorf("id = %s, want %s", got.ID, uuidString(f.elBoth))
		}
		if got.Headline != f.elTitle {
			t.Errorf("headline = %q, want original_title %q", got.Headline, f.elTitle)
		}
		if want := content.DeriveExtract(f.elRawBody); got.Extract != want {
			t.Errorf("extract = %q, want the D9 derivation %q", got.Extract, want)
		}
		if got.Lang != "el" {
			t.Errorf("lang = %q, want el (the source language)", got.Lang)
		}
		if !slices.Equal(got.Places, []string{f.alphaSlug, f.betaSlug}) {
			t.Errorf("places = %v, want both followed places", got.Places)
		}
	})

	t.Run("front pages by opaque keyset cursor", func(t *testing.T) {
		rec := get(t, h, "/api/v1/front?lang=de&place="+f.alphaSlug+"&limit=2")
		page1 := decodeBody[frontJSON](t, rec)
		if want := []string{uuidString(f.deNew), uuidString(midFirst)}; !slices.Equal(itemIDs(page1.Items), want) {
			t.Fatalf("page 1 = %v, want %v", itemIDs(page1.Items), want)
		}
		if page1.NextCursor == nil {
			t.Fatal("page 1 next_cursor is null with more items available")
		}
		rec = get(t, h, "/api/v1/front?lang=de&place="+f.alphaSlug+"&limit=2&cursor="+*page1.NextCursor)
		page2 := decodeBody[frontJSON](t, rec)
		if want := []string{uuidString(midSecond), uuidString(f.deOld)}; !slices.Equal(itemIDs(page2.Items), want) {
			t.Fatalf("page 2 = %v, want %v", itemIDs(page2.Items), want)
		}
		if page2.NextCursor != nil {
			t.Errorf("page 2 next_cursor = %q, want null on the exhausted feed", *page2.NextCursor)
		}
	})

	t.Run("front answers empty results with an empty list, never 500", func(t *testing.T) {
		rec := get(t, h, "/api/v1/front?lang=de&place="+f.betaSlug)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, `"items":[]`) {
			t.Errorf("body = %s, want a literal empty items array", body)
		}
		resp := decodeBody[frontJSON](t, rec)
		if resp.NextCursor != nil {
			t.Errorf("next_cursor = %q, want null", *resp.NextCursor)
		}
	})

	t.Run("front rejects bad requests with problem+json", func(t *testing.T) {
		valid := "lang=de&place=" + f.alphaSlug
		tests := []struct {
			name   string
			query  string
			detail string
		}{
			{name: "missing lang", query: "place=" + f.alphaSlug, detail: "unknown lang"},
			{name: "unknown lang", query: "lang=fr&place=" + f.alphaSlug, detail: "unknown lang"},
			{name: "seeded but non-reader lang", query: "lang=en&place=" + f.alphaSlug, detail: "unknown lang"},
			{name: "missing place", query: "lang=de", detail: "place is required"},
			{name: "unknown place", query: "lang=de&place=atlantis", detail: `unknown place "atlantis"`},
			{name: "one good and one unknown place", query: valid + "&place=atlantis", detail: `unknown place "atlantis"`},
			{name: "limit zero", query: valid + "&limit=0", detail: "invalid limit"},
			{name: "limit above the cap", query: valid + "&limit=101", detail: "invalid limit"},
			{name: "limit not a number", query: valid + "&limit=twenty", detail: "invalid limit"},
			{name: "cursor not base64", query: valid + "&cursor=%25%25", detail: "invalid cursor"},
			{name: "cursor with garbage payload", query: valid + "&cursor=Z2FyYmFnZQ", detail: "invalid cursor"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				p := wantProblem(t, get(t, h, "/api/v1/front?"+tt.query), http.StatusBadRequest)
				if !strings.Contains(p.Detail, tt.detail) {
					t.Errorf("detail = %q, want it to mention %q", p.Detail, tt.detail)
				}
			})
		}
	})

	t.Run("article serves both origin shapes with approved_at", func(t *testing.T) {
		rec := get(t, h, "/api/v1/articles/"+uuidString(f.deNew))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
		}
		translated := decodeBody[itemJSON](t, rec)
		if translated.Headline != f.deNewHeadline || translated.Extract != f.deNewExtract || translated.Lang != "de" {
			t.Errorf("translated article = %+v, want the translation columns in de", translated)
		}
		if translated.ApprovedAt == nil {
			t.Error("article payload lacks approved_at")
		} else if !translated.ApprovedAt.Equal(f.deNewPublished) {
			// The fixture approves and publishes at the same instant.
			t.Errorf("approved_at = %v, want %v", translated.ApprovedAt, f.deNewPublished)
		}

		rec = get(t, h, "/api/v1/articles/"+uuidString(f.elBoth))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
		}
		untranslated := decodeBody[itemJSON](t, rec)
		if untranslated.Headline != f.elTitle {
			t.Errorf("headline = %q, want original_title %q", untranslated.Headline, f.elTitle)
		}
		if want := content.DeriveExtract(f.elRawBody); untranslated.Extract != want {
			t.Errorf("extract = %q, want the D9 derivation %q", untranslated.Extract, want)
		}
		if !slices.Equal(untranslated.Places, []string{f.alphaSlug, f.betaSlug}) {
			t.Errorf("places = %v, want both tagged places", untranslated.Places)
		}
	})

	t.Run("article hides withdrawn, unpublished and unknown alike", func(t *testing.T) {
		for name, id := range map[string]string{
			"withdrawn":   uuidString(f.withdrawn),
			"unpublished": uuidString(f.unpublished),
			"unknown":     uuidString(mustRandomUUID(t)),
			"not a uuid":  "not-a-uuid",
		} {
			p := wantProblem(t, get(t, h, "/api/v1/articles/"+id), http.StatusNotFound)
			if p.Detail != "no such article" {
				t.Errorf("%s: detail = %q; the 404 must not distinguish why", name, p.Detail)
			}
		}
	})

	t.Run("front rejects non-GET methods", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/front?lang=de&place="+f.alphaSlug, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST /api/v1/front = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("front page latency sanity", func(t *testing.T) {
		// Local p95 sanity check (plan: API p95 < 200 ms at alpha scale).
		// The bound here is deliberately loose - CI machines vary - the
		// logged figure is the datum.
		const n = 60
		durations := make([]time.Duration, 0, n)
		for range n {
			start := time.Now()
			rec := get(t, h, "/api/v1/front?lang=de&place="+f.alphaSlug)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			durations = append(durations, time.Since(start))
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		p95 := durations[n*95/100-1]
		t.Logf("front page p95 over %d requests: %v", n, p95)
		if p95 > 2*time.Second {
			t.Errorf("front page p95 = %v; even a loose sanity bound of 2s is blown", p95)
		}
	})
}
