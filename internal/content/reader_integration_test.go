package content_test

// Exercises the reader queries (T023) against the real, migrated schema:
// published-and-visible semantics, both origin shapes, language and place
// scoping, and keyset pagination - the behaviours the public endpoints
// build on.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/content/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// readerFixture is a seeded world for the reader queries, isolated in one
// transaction: an editor, two places, one Greek source, and articles in
// every visibility and origin shape the queries must distinguish.
type readerFixture struct {
	tx pgx.Tx
	q  *store.Queries

	alphaSlug string // place followed by every seeded article
	betaSlug  string // second place; only the untranslated article carries it

	// Translated (el->de) articles tagged alpha, all published:
	// deNew (newest) > deMidA == deMidB (same instant) > deOld.
	deNew, deMidA, deMidB, deOld pgtype.UUID

	// Untranslated Greek article tagged BOTH alpha and beta.
	elBoth         pgtype.UUID
	elTitle        string
	elRawBody      string
	deNewHeadline  string
	deNewExtract   string
	deAttribution  string
	withdrawn      pgtype.UUID // published then withdrawn - never visible
	unpublished    pgtype.UUID // approved, never published - never visible
	deNewPublished time.Time
}

func seedReaderFixture(ctx context.Context, t *testing.T, tx pgx.Tx) readerFixture {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	suffix := hex.EncodeToString(b)

	f := readerFixture{
		tx:            tx,
		q:             store.New(tx),
		alphaSlug:     "alpha-" + suffix,
		betaSlug:      "beta-" + suffix,
		elTitle:       "Πρωτότυπος τίτλος " + suffix,
		elRawBody:     "Πρώτη πρόταση του άρθρου. Δεύτερη πρόταση με λεπτομέρειες. " + suffix,
		deNewHeadline: "Neueste Überschrift " + suffix,
		deNewExtract:  "Neuester Auszug " + suffix,
		deAttribution: "Quelle: Test Feed " + suffix,
	}

	var editorID, sourceID, alphaID, betaID string
	if err := tx.QueryRow(ctx,
		`insert into account (email, display_name, role) values ($1, $2, 'editor') returning id`,
		"editor-"+suffix+"@example.test", "Reader Test Editor "+suffix).Scan(&editorID); err != nil {
		t.Fatalf("seed editor: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`insert into place (name, country, slug) values ($1, 'DE', $2) returning id`,
		"Alphastadt "+suffix, f.alphaSlug).Scan(&alphaID); err != nil {
		t.Fatalf("seed place alpha: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`insert into place (name, country, slug) values ($1, 'GR', $2) returning id`,
		"Betapolis "+suffix, f.betaSlug).Scan(&betaID); err != nil {
		t.Fatalf("seed place beta: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'el', 'GR', 'Extract and link permitted (test)') returning id`,
		"Reader Test Feed "+suffix, "https://example.test/reader/"+suffix).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	sourceItem := func(name string, title *string, rawBody string) string {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx,
			`insert into source_item (source_id, source_url, original_title, raw_body)
			 values ($1, $2, $3, $4) returning id`,
			sourceID, "https://example.test/reader/"+suffix+"/"+name, title, rawBody).Scan(&id); err != nil {
			t.Fatalf("seed source_item %s: %v", name, err)
		}
		return id
	}
	translation := func(itemID, headline, extract string) string {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx,
			`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
			 values ($1, 'de', 'test-model-1', 'prompt-v1', $2, $3, 1000) returning id`,
			itemID, headline, extract).Scan(&id); err != nil {
			t.Fatalf("seed translation: %v", err)
		}
		return id
	}
	article := func(translationID, sourceItemID *string, publishedAt *time.Time) pgtype.UUID {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx,
			`insert into article (translation_id, source_item_id, approved_by, approved_at, published_at, attribution_block)
			 values ($1, $2, $3, coalesce($4::timestamptz, now()), $4, $5) returning id`,
			translationID, sourceItemID, editorID, publishedAt, f.deAttribution).Scan(&id); err != nil {
			t.Fatalf("seed article: %v", err)
		}
		var u pgtype.UUID
		if err := u.Scan(id); err != nil {
			t.Fatalf("parse article id: %v", err)
		}
		return u
	}
	tag := func(articleID pgtype.UUID, placeID string) {
		t.Helper()
		if _, err := tx.Exec(ctx,
			`insert into article_place (article_id, place_id) values ($1, $2)`,
			articleID, placeID); err != nil {
			t.Fatalf("tag article: %v", err)
		}
	}
	translated := func(name, headline, extract string, publishedAt *time.Time) pgtype.UUID {
		t.Helper()
		trID := translation(sourceItem(name, nil, "Σώμα "+name+" "+suffix), headline, extract)
		return article(&trID, nil, publishedAt)
	}

	// Timestamps are truncated to microseconds - Postgres precision - so
	// what goes in equals what comes out (keyset cursors round-trip).
	base := time.Now().UTC().Truncate(time.Microsecond)
	at := func(d time.Duration) *time.Time { ts := base.Add(d); return &ts }

	f.deNewPublished = *at(-1 * time.Hour)
	f.deNew = translated("de-new", f.deNewHeadline, f.deNewExtract, &f.deNewPublished)
	f.deMidA = translated("de-mid-a", "Mittlere Überschrift A "+suffix, "Auszug A "+suffix, at(-2*time.Hour))
	f.deMidB = translated("de-mid-b", "Mittlere Überschrift B "+suffix, "Auszug B "+suffix, at(-2*time.Hour))
	f.deOld = translated("de-old", "Alte Überschrift "+suffix, "Alter Auszug "+suffix, at(-3*time.Hour))
	for _, id := range []pgtype.UUID{f.deNew, f.deMidA, f.deMidB, f.deOld} {
		tag(id, alphaID)
	}

	elItem := sourceItem("el-both", &f.elTitle, f.elRawBody)
	f.elBoth = article(nil, &elItem, at(-90*time.Minute))
	tag(f.elBoth, alphaID)
	tag(f.elBoth, betaID)

	// Withdrawn: was the newest of all - its absence proves filtering, not
	// pagination luck.
	f.withdrawn = translated("de-withdrawn", "Zurückgezogen "+suffix, "Auszug "+suffix, at(-30*time.Minute))
	tag(f.withdrawn, alphaID)
	if _, err := tx.Exec(ctx,
		`update article set withdrawn_at = now(), withdrawn_by = $2, withdrawal_reason = 'reader query test'
		 where id = $1`, f.withdrawn, editorID); err != nil {
		t.Fatalf("withdraw article: %v", err)
	}

	f.unpublished = translated("de-unpublished", "Unveröffentlicht "+suffix, "Auszug "+suffix, nil)
	tag(f.unpublished, alphaID)

	return f
}

func frontPage(ctx context.Context, t *testing.T, f readerFixture, lang string, places []string, limit int32, cursor *store.ListFrontPageRow) []store.ListFrontPageRow {
	t.Helper()
	params := store.ListFrontPageParams{Lang: lang, Places: places, RowLimit: limit}
	if cursor != nil {
		params.CursorPublishedAt = cursor.PublishedAt
		params.CursorID = cursor.ID
	}
	rows, err := f.q.ListFrontPage(ctx, params)
	if err != nil {
		t.Fatalf("ListFrontPage(%s, %v): %v", lang, places, err)
	}
	return rows
}

func ids(rows []store.ListFrontPageRow) []pgtype.UUID {
	out := make([]pgtype.UUID, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func TestReaderQueries(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the reader queries")
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

	// The two same-instant articles order by id descending (the keyset
	// tie-break); establish which comes first.
	midFirst, midSecond := f.deMidA, f.deMidB
	if bytes.Compare(f.deMidB.Bytes[:], f.deMidA.Bytes[:]) > 0 {
		midFirst, midSecond = f.deMidB, f.deMidA
	}

	t.Run("translated feed is newest-first and excludes withdrawn and unpublished", func(t *testing.T) {
		rows := frontPage(ctx, t, f, "de", []string{f.alphaSlug}, 10, nil)
		want := []pgtype.UUID{f.deNew, midFirst, midSecond, f.deOld}
		if got := ids(rows); !slices.Equal(got, want) {
			t.Fatalf("feed ids = %v, want %v (withdrawn %v and unpublished %v must be absent)",
				got, want, f.withdrawn, f.unpublished)
		}
		top := rows[0]
		if !top.TranslationHeadline.Valid || top.TranslationHeadline.String != f.deNewHeadline {
			t.Errorf("TranslationHeadline = %+v, want %q", top.TranslationHeadline, f.deNewHeadline)
		}
		if !top.TranslationExtract.Valid || top.TranslationExtract.String != f.deNewExtract {
			t.Errorf("TranslationExtract = %+v, want %q", top.TranslationExtract, f.deNewExtract)
		}
		if !top.TargetLocale.Valid || top.TargetLocale.String != "de" {
			t.Errorf("TargetLocale = %+v, want de", top.TargetLocale)
		}
		if top.AttributionBlock != f.deAttribution {
			t.Errorf("AttributionBlock = %q, want %q", top.AttributionBlock, f.deAttribution)
		}
		// A translated row renders from the translation columns, so the
		// evidence blob is deliberately not fetched for it.
		if top.RawBody.Valid {
			t.Errorf("RawBody = %+v on a translated row; the feed must not overfetch the evidence blob", top.RawBody)
		}
		if !top.PublishedAt.Time.Equal(f.deNewPublished) {
			t.Errorf("PublishedAt = %v, want %v", top.PublishedAt.Time, f.deNewPublished)
		}
		if !slices.Equal(top.PlaceSlugs, []string{f.alphaSlug}) {
			t.Errorf("PlaceSlugs = %v, want [%s]", top.PlaceSlugs, f.alphaSlug)
		}
	})

	t.Run("untranslated feed matches the source language and carries raw fields", func(t *testing.T) {
		rows := frontPage(ctx, t, f, "el", []string{f.alphaSlug}, 10, nil)
		if len(rows) != 1 || rows[0].ID != f.elBoth {
			t.Fatalf("el feed = %v, want exactly [%v]", ids(rows), f.elBoth)
		}
		got := rows[0]
		if !got.OriginalTitle.Valid || got.OriginalTitle.String != f.elTitle {
			t.Errorf("OriginalTitle = %+v, want %q", got.OriginalTitle, f.elTitle)
		}
		if !got.RawBody.Valid || got.RawBody.String != f.elRawBody {
			t.Errorf("RawBody = %+v, want %q", got.RawBody, f.elRawBody)
		}
		if got.SourceLanguage != "el" {
			t.Errorf("SourceLanguage = %q, want el", got.SourceLanguage)
		}
		if got.TranslationHeadline.Valid || got.TranslationExtract.Valid {
			t.Errorf("untranslated row carries translation columns: %+v / %+v",
				got.TranslationHeadline, got.TranslationExtract)
		}
		if !slices.Equal(got.PlaceSlugs, []string{f.alphaSlug, f.betaSlug}) {
			t.Errorf("PlaceSlugs = %v, want [%s %s]", got.PlaceSlugs, f.alphaSlug, f.betaSlug)
		}
	})

	t.Run("article tagged to two requested places appears exactly once", func(t *testing.T) {
		rows := frontPage(ctx, t, f, "el", []string{f.alphaSlug, f.betaSlug}, 10, nil)
		if len(rows) != 1 || rows[0].ID != f.elBoth {
			t.Fatalf("el feed for both places = %v, want exactly [%v] once", ids(rows), f.elBoth)
		}
	})

	t.Run("place scoping is exact", func(t *testing.T) {
		if rows := frontPage(ctx, t, f, "el", []string{f.betaSlug}, 10, nil); len(rows) != 1 || rows[0].ID != f.elBoth {
			t.Errorf("el feed for beta = %v, want [%v]", ids(rows), f.elBoth)
		}
		// The translated articles are tagged alpha only: beta sees none.
		if rows := frontPage(ctx, t, f, "de", []string{f.betaSlug}, 10, nil); len(rows) != 0 {
			t.Errorf("de feed for beta = %v, want empty", ids(rows))
		}
	})

	t.Run("keyset pagination pages across a same-timestamp boundary", func(t *testing.T) {
		page1 := frontPage(ctx, t, f, "de", []string{f.alphaSlug}, 2, nil)
		if want := []pgtype.UUID{f.deNew, midFirst}; !slices.Equal(ids(page1), want) {
			t.Fatalf("page 1 = %v, want %v", ids(page1), want)
		}
		// The cursor is the last row of page 1 - mid-pair, so page 2 must
		// resume at the tie-break, not skip or repeat the same instant.
		page2 := frontPage(ctx, t, f, "de", []string{f.alphaSlug}, 2, &page1[1])
		if want := []pgtype.UUID{midSecond, f.deOld}; !slices.Equal(ids(page2), want) {
			t.Fatalf("page 2 = %v, want %v", ids(page2), want)
		}
		if page3 := frontPage(ctx, t, f, "de", []string{f.alphaSlug}, 2, &page2[1]); len(page3) != 0 {
			t.Fatalf("page 3 = %v, want empty (feed exhausted)", ids(page3))
		}
	})

	// A cursor is a (published_at, id) pair and the query treats it
	// atomically: a half-supplied cursor is no cursor. Without that arm the
	// row comparison against a NULL would be UNKNOWN for every row and the
	// reader would get a silently empty feed instead of a first page.
	t.Run("half-supplied cursor is treated as no cursor, never an empty page", func(t *testing.T) {
		full := frontPage(ctx, t, f, "de", []string{f.alphaSlug}, 10, nil)
		last := full[len(full)-1]

		tests := []struct {
			name   string
			params store.ListFrontPageParams
		}{
			{
				name: "timestamp without id",
				params: store.ListFrontPageParams{
					Lang: "de", Places: []string{f.alphaSlug}, RowLimit: 10,
					CursorPublishedAt: last.PublishedAt,
				},
			},
			{
				name: "id without timestamp",
				params: store.ListFrontPageParams{
					Lang: "de", Places: []string{f.alphaSlug}, RowLimit: 10,
					CursorID: last.ID,
				},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				rows, err := f.q.ListFrontPage(ctx, tt.params)
				if err != nil {
					t.Fatalf("ListFrontPage: %v", err)
				}
				if !slices.Equal(ids(rows), ids(full)) {
					t.Errorf("feed = %v, want the unpaged first page %v", ids(rows), ids(full))
				}
			})
		}

		// The complete pair still pages: the atomicity arm did not disable
		// the keyset comparison.
		paged := frontPage(ctx, t, f, "de", []string{f.alphaSlug}, 10, &full[0])
		if !slices.Equal(ids(paged), ids(full[1:])) {
			t.Errorf("paged feed = %v, want %v", ids(paged), ids(full[1:]))
		}
	})

	t.Run("detail returns both origin shapes", func(t *testing.T) {
		translated, err := f.q.GetPublishedArticle(ctx, f.deNew)
		if err != nil {
			t.Fatalf("GetPublishedArticle(translated): %v", err)
		}
		if !translated.TranslationHeadline.Valid || translated.TranslationHeadline.String != f.deNewHeadline {
			t.Errorf("detail TranslationHeadline = %+v, want %q", translated.TranslationHeadline, f.deNewHeadline)
		}
		if !translated.ApprovedAt.Valid {
			t.Error("detail lacks approved_at")
		}

		untranslated, err := f.q.GetPublishedArticle(ctx, f.elBoth)
		if err != nil {
			t.Fatalf("GetPublishedArticle(untranslated): %v", err)
		}
		if !untranslated.OriginalTitle.Valid || untranslated.OriginalTitle.String != f.elTitle {
			t.Errorf("detail OriginalTitle = %+v, want %q", untranslated.OriginalTitle, f.elTitle)
		}
		if !untranslated.RawBody.Valid || untranslated.RawBody.String != f.elRawBody {
			t.Errorf("detail RawBody = %+v, want %q", untranslated.RawBody, f.elRawBody)
		}
		if translated.RawBody.Valid {
			t.Errorf("detail RawBody = %+v on a translated article; it renders from the translation columns", translated.RawBody)
		}
		if !slices.Equal(untranslated.PlaceSlugs, []string{f.alphaSlug, f.betaSlug}) {
			t.Errorf("detail PlaceSlugs = %v, want [%s %s]", untranslated.PlaceSlugs, f.alphaSlug, f.betaSlug)
		}
	})

	t.Run("detail hides withdrawn, unpublished and unknown articles alike", func(t *testing.T) {
		for name, id := range map[string]pgtype.UUID{
			"withdrawn":   f.withdrawn,
			"unpublished": f.unpublished,
			"unknown":     mustRandomUUID(t),
		} {
			if _, err := f.q.GetPublishedArticle(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
				t.Errorf("GetPublishedArticle(%s) error = %v, want pgx.ErrNoRows", name, err)
			}
		}
	})

	t.Run("place slugs resolve only to existing places", func(t *testing.T) {
		got, err := f.q.ListPlacesBySlugs(ctx, []string{f.alphaSlug, f.betaSlug, "no-such-place"})
		if err != nil {
			t.Fatalf("ListPlacesBySlugs: %v", err)
		}
		slices.Sort(got)
		if want := []string{f.alphaSlug, f.betaSlug}; !slices.Equal(got, want) {
			t.Errorf("ListPlacesBySlugs = %v, want %v", got, want)
		}
	})
}

// mustRandomUUID builds a random v4-shaped uuid that matches no seeded row.
func mustRandomUUID(t *testing.T) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if _, err := rand.Read(u.Bytes[:]); err != nil {
		t.Fatalf("random uuid: %v", err)
	}
	u.Bytes[6] = (u.Bytes[6] & 0x0f) | 0x40
	u.Bytes[8] = (u.Bytes[8] & 0x3f) | 0x80
	u.Valid = true
	return u
}
