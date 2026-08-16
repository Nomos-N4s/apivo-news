package editorial_test

// The evidence half of the review queue (#87): the approval is permanent
// and article_guard freezes the attribution at the click, so what the
// approval rests on - original text, author, declared publication date,
// content fingerprint, translation lineage and cost - must be on the wire
// BEFORE the click. These tests pin each of those fields against the real,
// migrated schema, inside rolled-back transactions.

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/text"
)

// evidenceFixture seeds one source in a private language with two items:
// one carrying everything a feed can declare plus a translation, one
// declaring as little as a feed can.
type evidenceFixture struct {
	sourceLang, targetLang string
	sourceName             string

	dated       string // publication date, author, markup body, translated
	datedURL    string
	datedAuthor string
	publishedAt time.Time
	rawBody     string
	datedTr     string

	undated string // no publication date, no author
}

// evidenceRawBody is a markup-heavy Greek body well over the extract
// bound: real prose in a 2-bytes-per-letter script, wrapped in tags and
// carrying a script element that must never reach the wire.
func evidenceRawBody(suffix string) string {
	sentence := "Η απόφαση του δημοτικού συμβουλίου εγκρίθηκε μετά από πολύωρη συζήτηση για τον προϋπολογισμό. "
	return `<p>` + strings.Repeat(sentence, 8) + suffix +
		`</p><script>alert("must never reach the wire")</script><p>Ακολουθεί δεύτερη παράγραφος.</p>`
}

func seedEvidenceFixture(ctx context.Context, t *testing.T, tx pgx.Tx) evidenceFixture {
	t.Helper()
	suffix := randomSuffix(t)
	sourceLang, targetLang := languageCodes(t)
	f := evidenceFixture{
		sourceLang:  sourceLang,
		targetLang:  targetLang,
		sourceName:  "Evidence Feed " + suffix,
		datedURL:    "https://example.test/evidence/" + suffix + "/dated",
		datedAuthor: "Katrin Vogel " + suffix,
		publishedAt: time.Date(2026, 8, 13, 5, 58, 0, 0, time.UTC),
		rawBody:     evidenceRawBody(suffix),
	}
	for _, code := range []string{f.sourceLang, f.targetLang} {
		if _, err := tx.Exec(ctx, `insert into language (code) values ($1)`, code); err != nil {
			t.Fatalf("seed language %s: %v", code, err)
		}
	}
	var sourceID string
	if err := tx.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, $3, 'GR', 'Extract and link permitted (evidence test)') returning id`,
		f.sourceName, "https://example.test/evidence/"+suffix, f.sourceLang).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	if err := tx.QueryRow(ctx,
		`insert into source_item (source_id, source_url, original_title, original_author, published_at, raw_body, retrieved_at)
		 values ($1, $2, $3, $4, $5, $6, now())
		 returning id`,
		sourceID, f.datedURL, "Τίτλος με τεκμήρια "+suffix, f.datedAuthor, f.publishedAt, f.rawBody).Scan(&f.dated); err != nil {
		t.Fatalf("seed dated item: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
		 values ($1, $2, 'translate-alpha-1', 'v4', $3, $4, 4100) returning id`,
		f.dated, f.targetLang, "Überschrift "+suffix, "Auszug "+suffix).Scan(&f.datedTr); err != nil {
		t.Fatalf("seed translation: %v", err)
	}

	if err := tx.QueryRow(ctx,
		`insert into source_item (source_id, source_url, original_title, raw_body, retrieved_at)
		 values ($1, $2, $3, $4, now() - interval '1 minute')
		 returning id`,
		sourceID, "https://example.test/evidence/"+suffix+"/undated",
		"Τίτλος χωρίς ημερομηνία "+suffix, "Σύντομο σώμα "+suffix+".").Scan(&f.undated); err != nil {
		t.Fatalf("seed undated item: %v", err)
	}
	return f
}

// evidenceTx opens the migrated, rolled-back transaction the evidence
// tests run in, and hands back the seeded fixture with a handler over it.
func evidenceTx(t *testing.T) (context.Context, pgx.Tx, evidenceFixture, http.Handler) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the queue evidence against Postgres")
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
	f := seedEvidenceFixture(ctx, t, tx)
	return ctx, tx, f, newHandler(t, editorial.NewPGStore(tx))
}

// findItem returns the queue row for the given source_item id, failing the
// test when it is absent.
func findItem(t *testing.T, body queueBody, sourceItemID string) queueItemBody {
	t.Helper()
	for _, candidate := range body.Items {
		if candidate.SourceItemID == sourceItemID {
			return candidate
		}
	}
	t.Fatalf("item %s is absent from the queue page", sourceItemID)
	return queueItemBody{}
}

func TestQueueCarriesThePublicationDateTheFeedDeclared(t *testing.T) {
	t.Parallel()
	ctx, tx, f, h := evidenceTx(t)

	rec, body := getQueue(t, h, "?lang="+f.sourceLang)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	dated := findItem(t, body, f.dated)
	if dated.OriginalPublishedAt == nil {
		t.Fatal("original_published_at = null although the feed declared one - this is the field the attribution freezes")
	}
	if *dated.OriginalPublishedAt != "2026-08-13T05:58:00Z" {
		t.Errorf("original_published_at = %q, want the declared 2026-08-13T05:58:00Z", *dated.OriginalPublishedAt)
	}
	if dated.OriginalAuthor == nil || *dated.OriginalAuthor != f.datedAuthor {
		t.Errorf("original_author = %v, want %q", dated.OriginalAuthor, f.datedAuthor)
	}
	if dated.SourceURL != f.datedURL {
		t.Errorf("source_url = %q, want %q", dated.SourceURL, f.datedURL)
	}
	if dated.SourceLang != f.sourceLang {
		t.Errorf("source_lang = %q, want %q", dated.SourceLang, f.sourceLang)
	}

	// The fingerprint on the wire is the database's own, byte for byte.
	var storedHash string
	if err := tx.QueryRow(ctx, `select content_hash from source_item where id = $1`, f.dated).Scan(&storedHash); err != nil {
		t.Fatalf("reading content_hash: %v", err)
	}
	if dated.ContentHash != storedHash {
		t.Errorf("content_hash = %q, want the database's %q", dated.ContentHash, storedHash)
	}
}

func TestQueueLeavesThePublicationDateNullWhenTheFeedOmittedIt(t *testing.T) {
	t.Parallel()
	_, _, f, h := evidenceTx(t)

	rec, body := getQueue(t, h, "?lang="+f.sourceLang)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	undated := findItem(t, body, f.undated)
	// Null, not the retrieval date: what the feed did not declare stays
	// undeclared (FR-002), and the VISIBLE fallback is the screen's job -
	// a substituted date here would be frozen into the attribution.
	if undated.OriginalPublishedAt != nil {
		t.Errorf("original_published_at = %q, want null when the feed omitted it", *undated.OriginalPublishedAt)
	}
	if undated.OriginalAuthor != nil {
		t.Errorf("original_author = %q, want null when the feed named none", *undated.OriginalAuthor)
	}
}

func TestQueueCarriesTheTranslationLineageAndItsCost(t *testing.T) {
	t.Parallel()
	_, _, f, h := evidenceTx(t)

	rec, body := getQueue(t, h, "?lang="+f.targetLang)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if len(body.Items) != 1 {
		t.Fatalf("queue for %s = %d items, want the one translation", f.targetLang, len(body.Items))
	}
	translated := body.Items[0]
	if translated.TranslationID == nil || *translated.TranslationID != f.datedTr {
		t.Fatalf("translation_id = %v, want %q", translated.TranslationID, f.datedTr)
	}
	if translated.Model == nil || *translated.Model != "translate-alpha-1" {
		t.Errorf("model = %v, want the lineage's translate-alpha-1 (FR-005)", translated.Model)
	}
	if translated.PromptVersion == nil || *translated.PromptVersion != "v4" {
		t.Errorf("prompt_version = %v, want v4", translated.PromptVersion)
	}
	if translated.TargetLang == nil || *translated.TargetLang != f.targetLang {
		t.Errorf("target_lang = %v, want %q", translated.TargetLang, f.targetLang)
	}
	if translated.CostMicroUSD == nil || *translated.CostMicroUSD != 4100 {
		t.Errorf("cost_microusd = %v, want the recorded 4100 (FR-006)", translated.CostMicroUSD)
	}

	// An untranslated origin carries the lineage fields as null together.
	rec, body = getQueue(t, h, "?lang="+f.sourceLang)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	undated := findItem(t, body, f.undated)
	if undated.Model != nil || undated.PromptVersion != nil || undated.CostMicroUSD != nil || undated.TargetLang != nil {
		t.Errorf("lineage on an untranslated origin = (%v, %v, %v, %v), want all null",
			undated.Model, undated.PromptVersion, undated.CostMicroUSD, undated.TargetLang)
	}
}

// TestQueueOriginalExtractIsBoundedProse pins the D9 reduction at the
// database-to-wire hop: markup reduced to prose, the script element gone,
// the bound counted in runes - Greek text is not shorter for taking more
// bytes - and never the unbounded raw body.
func TestQueueOriginalExtractIsBoundedProse(t *testing.T) {
	t.Parallel()
	_, _, f, h := evidenceTx(t)

	rec, body := getQueue(t, h, "?lang="+f.sourceLang)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	extract := findItem(t, body, f.dated).ExtractOriginal
	if extract == "" {
		t.Fatal("extract_original is empty for an item with a body")
	}
	if strings.ContainsAny(extract, "<>") || strings.Contains(extract, "alert(") {
		t.Errorf("extract_original carries markup or script text: %q", extract)
	}
	if runes := utf8.RuneCountInString(extract); runes > text.MaxExtractRunes {
		t.Errorf("extract_original = %d runes, want at most %d", runes, text.MaxExtractRunes)
	}
	// Counted in letters, not bytes: the Greek prose fills the rune bound,
	// which a byte-counted cut could not - 300 bytes of this text is only
	// ~150 letters.
	if runes := utf8.RuneCountInString(extract); runes <= text.MaxExtractRunes/2 {
		t.Errorf("extract_original = %d runes; a byte-counted bound would look like this", runes)
	}
	if len(extract) <= text.MaxExtractRunes {
		t.Errorf("extract_original = %d bytes for %d runes; Greek prose at the rune bound must exceed the byte count",
			len(extract), utf8.RuneCountInString(extract))
	}
	if !strings.HasPrefix(extract, "Η απόφαση του δημοτικού συμβουλίου") {
		t.Errorf("extract_original = %q, want the body's own prose, markup dropped", extract)
	}
	// The D9 sentence rule: what fits ends at a sentence boundary.
	if !strings.HasSuffix(extract, ".") {
		t.Errorf("extract_original ends %q, want a sentence boundary", extract[len(extract)-20:])
	}
}

// TestQueueStillRejectsUnknownQueryParameters pins that widening the
// payload did not loosen the query contract.
func TestQueueStillRejectsUnknownQueryParameters(t *testing.T) {
	t.Parallel()
	h := newHandler(t, errStore{err: errUnexpectedCall})
	rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/queue?evidence=full", editorToken, "")
	wantProblem(t, rec, http.StatusBadRequest, "unknown query parameter")
}

// TestQueueCarriesTheCorrectionHistory pins the wire shape of the two
// fields the client type had been dropping: correction_candidate and the
// withdrawal history that makes a re-queued correction distinguishable
// from a first approval.
func TestQueueCarriesTheCorrectionHistory(t *testing.T) {
	t.Parallel()
	ctx, tx, f, h := evidenceTx(t)

	// Approve and publish the undated origin, then withdraw it: the origin
	// returns to the queue carrying the history.
	var editorID string
	if err := tx.QueryRow(ctx,
		`insert into account (email, display_name, role) values ($1, 'Evidence Editor', 'editor') returning id`,
		"evidence-"+randomSuffix(t)+"@example.test").Scan(&editorID); err != nil {
		t.Fatalf("seed editor: %v", err)
	}
	var articleID string
	if err := tx.QueryRow(ctx,
		`insert into article (source_item_id, approved_by, published_at, attribution_block)
		 values ($1, $2, now(), 'Πηγή: Evidence Feed') returning id`,
		f.undated, editorID).Scan(&articleID); err != nil {
		t.Fatalf("seed article: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`update article set withdrawn_at = now(), withdrawn_by = $2, withdrawal_reason = 'the source corrected the story'
		  where id = $1`, articleID, editorID); err != nil {
		t.Fatalf("withdraw article: %v", err)
	}

	rec, body := getQueue(t, h, "?lang="+f.sourceLang)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	corrected := findItem(t, body, f.undated)
	if !corrected.CorrectionCandidate {
		t.Error("correction_candidate = false on the withdrawn origin")
	}
	if len(corrected.Withdrawals) != 1 {
		t.Fatalf("withdrawals = %d, want the one recorded withdrawal", len(corrected.Withdrawals))
	}
	wd := corrected.Withdrawals[0]
	if wd.ArticleID != articleID || wd.WithdrawnBy != editorID || wd.Reason != "the source corrected the story" {
		t.Errorf("withdrawal = %+v, want the recorded history", wd)
	}
	// And the fresh dated origin still reads as a first approval.
	if dated := findItem(t, body, f.dated); dated.CorrectionCandidate || len(dated.Withdrawals) != 0 {
		t.Error("the fresh origin reads as a correction; the two must stay distinguishable")
	}
}
