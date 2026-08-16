package main

// The alpha's definition of done (issue #36, T033, spec section 10 / SC-001
// and SC-002), walked as ONE journey against the real schema and the real
// modules, composed the way serve() composes them: a licensed Greek feed is
// registered through the editorial handler, polled with the real fetcher
// and provenance write path, translated by the real pipeline over a fake
// Translator, reviewed and approved through the real endpoints, read
// through the real content handler place by place, withdrawn with a
// recorded reason - and then the I-5 drill times the full provenance chain
// of the translated, the untranslated and the withdrawn article against
// the five-minute audit promise.
//
// The modules meet only here, in the composition root, exactly as with the
// editorial wiring, front-page-flow and cycle-counts tests: the arch test
// forbids them importing each other, tests included.
//
// Isolation: the whole journey runs in ONE transaction that is rolled
// back, never a throwaway database - the point of T033 is the journey
// against THE schema and data the founder actually runs, and a rollback
// leaves that database untouched (the honest-record rule cuts both ways:
// no test articles may outlive the test). The expected refusals - the
// duplicate item, the non-editor approval, the second approval of an
// origin - raise inside the stores' own nested transactions, which pgx
// runs as savepoints, so the refusal aborts the savepoint and never
// poisons the outer transaction (the 25P02 containment the Withdraw
// docstring describes). Determinism beside concurrently running suites
// comes from three choices: every row the journey writes is invisible to
// other suites and found again BY ID, never by position; the poll-dedupe
// delta is counted on a source only this transaction can see; and the
// FR-006 ledger delta is made exact by taking the shared month row's lock
// up front (the lockedMonth pattern from the pipeline integration tests),
// so no concurrent spend can land between the before and after readings.
// REPEATABLE READ is deliberately NOT used here, unlike the cycle-counts
// test: the translation trigger updates the shared translation_spend row,
// and under a frozen snapshot a concurrent committed update to that row
// would abort the whole journey with a serialization failure.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/content"
	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/ingestion"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/text"
	"github.com/Nomos-N4s/apivo-news/internal/translation"
)

// journeyChainBudget is what the drill holds each provenance read to, and
// the drill's total as well - the same stance as the T028 drill: the audit
// promise is five minutes for the whole human task (FR-010, SC-002), so a
// chain read needing more than five seconds of database time is already a
// regression worth failing on.
const journeyChainBudget = 5 * time.Second

// journeySHA256 matches the database-computed content fingerprint.
var journeySHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// The feed fixture's items, by stable index. The values mirror
// testdata/journey_feed.xml - change one and change the other.
const (
	journeyTramTitle     = "Νέα γραμμή τραμ συνδέει το κέντρο του Μονάχου με τα βόρεια προάστια"
	journeyTramLink      = "https://news.example.gr/journey/tram-monacho"
	journeyTramAuthor    = "Ελένη Παπαδοπούλου"
	journeyWaterTitle    = "Αναβάθμιση του δικτύου ύδρευσης στη Θεσσαλονίκη"
	journeyWaterLink     = "https://news.example.gr/journey/ydreysi-thessaloniki"
	journeyFestivalTitle = "Φεστιβάλ ελληνικής μουσικής επιστρέφει στη Βαυαρία τον Σεπτέμβριο"
	journeyFestivalLink  = "https://news.example.gr/journey/festival-bavaria"
	// journeyTramPublished is the fixture's pubDate instant rendered the
	// way the contract renders every timestamp: UTC, whatever zone the feed
	// - or the server - stood in. The fixture says 06:30 +0300.
	journeyTramPublished = "2025-08-04T03:30:00Z"
)

// journeyGate authenticates every request as one fixed account. It stands
// where serve() wires the JWT verifier: the editorial module's consumer-
// defined seam, satisfied here by a fake so the journey exercises the
// handlers and the database's own role checks rather than token mechanics
// (the editorial wiring test owns those).
type journeyGate struct{ editor editorial.Editor }

func (g journeyGate) AuthenticateEditor(context.Context, string) (editorial.Editor, error) {
	return g.editor, nil
}

// journeyTranslator is the fake Translator: no network, deterministic
// output, scripted per source title. A title it does not know belongs to
// another suite's committed backlog; answering ErrInvalidRequest makes the
// pipeline step over it without spending anything, so the ledger delta the
// journey asserts stays exactly its own items' costs.
//
// The title alone is not identity enough: the work list is database-wide,
// and a committed item that happens to share a fixture title would be
// translated and billed, breaking the exact delta on a healthy system. So
// each answer also demands a fragment of its own item's body in the
// request text - a look-alike falls into the same step-over path.
type journeyTranslator struct {
	results map[string]translation.Result
	texts   map[string]string
}

func (j journeyTranslator) Translate(_ context.Context, req translation.Request) (translation.Result, error) {
	res, ok := j.results[req.SourceTitle]
	if !ok || !strings.Contains(req.SourceText, j.texts[req.SourceTitle]) {
		return translation.Result{}, fmt.Errorf("%w: the journey translates only its own items, not %q", translation.ErrInvalidRequest, req.SourceTitle)
	}
	res.PromptVersion = req.PromptVersion
	return res, nil
}

// journeyDo performs one request against a mounted handler, the way the
// real server would deliver it.
func journeyDo(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.Header.Set("Authorization", "Bearer journey")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// journeyUTCStamp parses a wire timestamp and requires it to READ as UTC,
// not merely to denote the right instant: the journey runs under
// TZ=Europe/Athens as well, and a "+03:00" rendering that parses to the
// same moment is exactly the zone dependence the run exists to catch.
func journeyUTCStamp(t *testing.T, field, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("%s = %q is not an RFC 3339 timestamp: %v", field, value, err)
	}
	if !strings.HasSuffix(value, "Z") {
		t.Errorf("%s = %q renders in a non-UTC zone; the wire and the audit record read Z wherever the server stands", field, value)
	}
	return at
}

// Wire shapes the journey reads back, mirroring the contract.
type journeyQueueItem struct {
	SourceItemID        string  `json:"source_item_id"`
	TranslationID       *string `json:"translation_id"`
	SourceName          string  `json:"source_name"`
	HeadlineOriginal    *string `json:"headline_original"`
	HeadlineTranslated  *string `json:"headline_translated"`
	ExtractTranslated   *string `json:"extract_translated"`
	RetrievedAt         string  `json:"retrieved_at"`
	LicenceSnapshot     string  `json:"licence_snapshot"`
	SourceURL           string  `json:"source_url"`
	OriginalAuthor      *string `json:"original_author"`
	OriginalPublishedAt *string `json:"original_published_at"`
	ContentHash         string  `json:"content_hash"`
	ExtractOriginal     string  `json:"extract_original"`
	SourceLang          string  `json:"source_lang"`
	TargetLang          *string `json:"target_lang"`
	Model               *string `json:"model"`
	PromptVersion       *string `json:"prompt_version"`
	CostMicroUSD        *int64  `json:"cost_microusd"`
}

type journeyQueueBody struct {
	Items      []journeyQueueItem `json:"items"`
	NextCursor *string            `json:"next_cursor"`
}

type journeyFrontItem struct {
	ID          string   `json:"id"`
	Headline    string   `json:"headline"`
	Extract     string   `json:"extract"`
	Lang        string   `json:"lang"`
	Places      []string `json:"places"`
	Attribution string   `json:"attribution"`
	SourceURL   string   `json:"source_url"`
	PublishedAt string   `json:"published_at"`
	ApprovedAt  string   `json:"approved_at"` // article page only
}

type journeyFrontBody struct {
	Items      []journeyFrontItem `json:"items"`
	NextCursor *string            `json:"next_cursor"`
}

type journeyProvenance struct {
	ArticleID string   `json:"article_id"`
	Headline  string   `json:"headline"`
	Places    []string `json:"places"`
	Source    struct {
		Name         string `json:"name"`
		FeedURL      string `json:"feed_url"`
		Jurisdiction string `json:"jurisdiction"`
	} `json:"source"`
	SourceItem struct {
		SourceURL                  string  `json:"source_url"`
		OriginalTitle              *string `json:"original_title"`
		RetrievedAt                string  `json:"retrieved_at"`
		ContentHash                string  `json:"content_hash"`
		LicenceSnapshot            string  `json:"licence_snapshot"`
		UsageRuleSnapshot          string  `json:"usage_rule_snapshot"`
		PermissionEvidenceSnapshot *string `json:"permission_evidence_snapshot"`
		OriginalAuthor             *string `json:"original_author"`
	} `json:"source_item"`
	Translation *struct {
		Model         string `json:"model"`
		PromptVersion string `json:"prompt_version"`
		TargetLocale  string `json:"target_locale"`
		GeneratedAt   string `json:"generated_at"`
		CostMicroUSD  int64  `json:"cost_microusd"`
	} `json:"translation"`
	Approval struct {
		ApproverName  string `json:"approver_name"`
		ApproverEmail string `json:"approver_email"`
		ApprovedAt    string `json:"approved_at"`
	} `json:"approval"`
	PublishedAt *string `json:"published_at"`
	Withdrawal  *struct {
		WithdrawnAt string `json:"withdrawn_at"`
		WithdrawnBy string `json:"withdrawn_by"`
		Reason      string `json:"reason"`
	} `json:"withdrawal"`
	Events []struct {
		Type       string `json:"type"`
		OccurredAt string `json:"occurred_at"`
		Detail     string `json:"detail"`
	} `json:"events"`
}

// journeyChain is one article the drill walks and everything its chain
// must contain.
type journeyChain struct {
	label            string
	articleID        string
	headline         string
	places           []string
	sourceURL        string
	author           *string
	translated       bool
	costMicroUSD     int64
	withdrawn        bool
	withdrawalReason string
}

func TestAlphaDefinitionOfDoneJourney(t *testing.T) {
	t.Parallel()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to walk the alpha's definition of done")
	}
	if err := db.Migrate(dbURL); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	// --- The people: a named editor the articles will record (I-1), and a
	// reader account the database must refuse as an approver. ---
	suffix := randomHex(t)
	editorName := "Journey Editor " + suffix
	editorEmail := "journey-editor-" + suffix + "@example.test"
	var editorIDText, readerIDText string
	if err := tx.QueryRow(ctx,
		`insert into account (email, display_name, role) values ($1, $2, 'editor') returning id`,
		editorEmail, editorName).Scan(&editorIDText); err != nil {
		t.Fatalf("seed editor: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`insert into account (email, display_name, role) values ($1, $2, 'reader') returning id`,
		"journey-reader-"+suffix+"@example.test", "Journey Reader "+suffix).Scan(&readerIDText); err != nil {
		t.Fatalf("seed reader: %v", err)
	}
	editorID, readerID := uuid.MustParse(editorIDText), uuid.MustParse(readerIDText)

	// Both editorial handlers are the real route table over the real store;
	// only the auth seam is faked, exactly where serve() wires the verifier.
	editorHandler := editorial.NewHandler(discardLogger(), editorial.NewPGStore(tx),
		journeyGate{editor: editorial.Editor{ID: editorID, Email: editorEmail, DisplayName: editorName}})
	// The reader smuggled past the HTTP gate on purpose: the journey's I-1
	// claim is that the DATABASE refuses a non-editor approver even when
	// the application fails to.
	readerHandler := editorial.NewHandler(discardLogger(), editorial.NewPGStore(tx),
		journeyGate{editor: editorial.Editor{ID: readerID, Email: "journey-reader-" + suffix + "@example.test", DisplayName: "Journey Reader " + suffix}})

	// ================= 1. FEED -> RETRIEVAL =================

	feedXML, err := os.ReadFile("testdata/journey_feed.xml")
	if err != nil {
		t.Fatalf("reading the feed fixture: %v", err)
	}
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write(feedXML)
	}))
	t.Cleanup(feed.Close)
	feedURL := feed.URL + "/journey-" + suffix + ".xml"

	// Register the source through the real HTTP handler.
	licence := "Extract and link permitted per feed terms v1 (journey " + suffix + ")"
	sourceName := "Journey Feed " + suffix
	sourceBody, _ := json.Marshal(map[string]any{
		"name":          sourceName,
		"url":           feedURL,
		"language":      "el",
		"jurisdiction":  "GR",
		"licence_terms": licence,
	})
	rec := journeyDo(t, editorHandler, http.MethodPost, "/api/v1/editorial/sources", string(sourceBody))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST sources = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	var createdSource struct {
		ID        string `json:"id"`
		UsageRule string `json:"usage_rule"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &createdSource); err != nil {
		t.Fatalf("unmarshalling the created source: %v", err)
	}
	if createdSource.UsageRule != "extract_and_link" {
		t.Fatalf("usage_rule = %q, want the only value a registration can produce, extract_and_link (FR-004)", createdSource.UsageRule)
	}
	journeyUTCStamp(t, "source.created_at", createdSource.CreatedAt)
	sourceID := uuid.MustParse(createdSource.ID)

	// Poll it: the real fetcher against the fixture server, the real
	// provenance write path, the real poll-state write - the same three
	// steps the poll loop performs.
	fetchCfg := ingestion.FetchConfig{Timeout: 10 * time.Second, MaxAttempts: 1}
	firstPoll, err := fetchCfg.Fetch(ctx, feedURL, ingestion.Validators{})
	if err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if len(firstPoll.Items) != 3 {
		t.Fatalf("the fixture parsed to %d items, want 3", len(firstPoll.Items))
	}
	itemStore := ingestion.NewStore(tx)
	itemIDs := make(map[string]uuid.UUID, 3)
	for _, item := range firstPoll.Items {
		stored, err := itemStore.RecordRetrieval(ctx, sourceID, item)
		if err != nil {
			t.Fatalf("storing %q: %v", item.Title, err)
		}
		if stored.Duplicate {
			t.Fatalf("first retrieval of %q reported Duplicate", item.Title)
		}
		itemIDs[item.Title] = stored.ItemID
	}
	sourceStore := ingestion.NewSourceStore(tx)
	if err := sourceStore.RecordPollOutcome(ctx, sourceID, ingestion.PollOutcome{
		Validators: ingestion.Validators{ETag: firstPoll.ETag, LastModified: firstPoll.LastModified},
		Retrieved:  3,
	}); err != nil {
		t.Fatalf("recording the first poll outcome: %v", err)
	}

	// The evidence: every row carries the trigger-written licence/usage
	// snapshots and the database-computed fingerprint (I-2, I-4) - the
	// caller supplied none of them, so none of them can be false.
	contentHashes := make(map[string]string, 3)
	for title, itemID := range itemIDs {
		var (
			gotLicence, gotUsage, gotHash, gotURL string
			gotAuthor                             *string
			gotPublished                          *time.Time
			gotRetrieved                          time.Time
		)
		if err := tx.QueryRow(ctx,
			`select licence_snapshot, usage_rule_snapshot, content_hash, source_url, original_author, published_at, retrieved_at
			   from source_item where id = $1`, itemID.String(),
		).Scan(&gotLicence, &gotUsage, &gotHash, &gotURL, &gotAuthor, &gotPublished, &gotRetrieved); err != nil {
			t.Fatalf("reading the evidence row for %q: %v", title, err)
		}
		if gotLicence != licence {
			t.Errorf("%q licence_snapshot = %q, want the terms at retrieval %q (I-4)", title, gotLicence, licence)
		}
		if gotUsage != "extract_and_link" {
			t.Errorf("%q usage_rule_snapshot = %q, want the trigger-written default", title, gotUsage)
		}
		if !journeySHA256.MatchString(gotHash) {
			t.Errorf("%q content_hash = %q, want a database-computed sha-256 hex digest (I-2)", title, gotHash)
		}
		if gotRetrieved.IsZero() {
			t.Errorf("%q retrieved_at is zero", title)
		}
		contentHashes[title] = gotHash

		// The item.retrieved event is the render frozen into the append-only
		// stream, and it is scoped by source_item_id rather than article_id -
		// the provenance assertions below never read it. It gets the same
		// Z discipline as every article payload: what the record froze must
		// not depend on where the server stood.
		var eventRetrievedAt string
		if err := tx.QueryRow(ctx,
			`select payload->>'retrieved_at' from domain_event
			  where type = 'item.retrieved' and payload->>'source_item_id' = $1`,
			itemID.String(),
		).Scan(&eventRetrievedAt); err != nil {
			t.Fatalf("reading the item.retrieved payload for %q: %v", title, err)
		}
		journeyUTCStamp(t, "item.retrieved payload retrieved_at", eventRetrievedAt)
		switch title {
		case journeyTramTitle:
			if gotURL != journeyTramLink {
				t.Errorf("tram source_url = %q, want %q", gotURL, journeyTramLink)
			}
			if gotAuthor == nil || *gotAuthor != journeyTramAuthor {
				t.Errorf("tram original_author = %v, want the feed's %q", gotAuthor, journeyTramAuthor)
			}
			if want, _ := time.Parse(time.RFC3339, journeyTramPublished); gotPublished == nil || !gotPublished.Equal(want) {
				t.Errorf("tram published_at = %v, want the feed's declared %s", gotPublished, journeyTramPublished)
			}
		case journeyWaterTitle:
			// The feed omitted author and date: absent stays absent, never
			// invented (FR-002).
			if gotAuthor != nil || gotPublished != nil {
				t.Errorf("water author/published = %v/%v, want both NULL - the feed stated neither", gotAuthor, gotPublished)
			}
		}
	}

	// Poll AGAIN: identical content, so every item dedupes on the
	// database-computed fingerprint (FR-014) - counted, and no new rows.
	secondPoll, err := fetchCfg.Fetch(ctx, feedURL, ingestion.Validators{})
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	duplicates := 0
	for _, item := range secondPoll.Items {
		stored, err := itemStore.RecordRetrieval(ctx, sourceID, item)
		if err != nil {
			t.Fatalf("re-storing %q: %v", item.Title, err)
		}
		if !stored.Duplicate {
			t.Fatalf("re-retrieval of %q was stored as new evidence; FR-014 dedupes identical content per source", item.Title)
		}
		duplicates++
	}
	if err := sourceStore.RecordPollOutcome(ctx, sourceID, ingestion.PollOutcome{
		Validators: ingestion.Validators{ETag: secondPoll.ETag, LastModified: secondPoll.LastModified},
		Duplicates: duplicates,
	}); err != nil {
		t.Fatalf("recording the second poll outcome: %v", err)
	}
	var rowCount int
	if err := tx.QueryRow(ctx, `select count(*) from source_item where source_id = $1`, sourceID.String()).Scan(&rowCount); err != nil {
		t.Fatalf("counting evidence rows: %v", err)
	}
	if rowCount != 3 {
		t.Fatalf("the source holds %d evidence rows after the second poll, want the same 3", rowCount)
	}
	var lastRetrieved, lastDuplicates int
	if err := tx.QueryRow(ctx, `select last_poll_retrieved, last_poll_duplicates from source where id = $1`, sourceID.String()).Scan(&lastRetrieved, &lastDuplicates); err != nil {
		t.Fatalf("reading the poll state: %v", err)
	}
	if lastRetrieved != 0 || lastDuplicates != 3 {
		t.Errorf("poll state after the second poll = (%d retrieved, %d duplicates), want (0, 3)", lastRetrieved, lastDuplicates)
	}

	// ================= 2. TRANSLATION =================

	// Take the shared month row's lock first, so the before/after ledger
	// readings bracket exactly this journey's spend (FR-006) even while
	// other suites commit spend of their own.
	ledger := translation.NewLedger(tx)
	if _, err := ledger.RecordUnbilledSpend(ctx, translation.Spend{}); err != nil {
		t.Fatalf("locking the month row: %v", err)
	}
	monthBefore, err := ledger.ThisMonth(ctx)
	if err != nil {
		t.Fatalf("reading the ledger before translation: %v", err)
	}
	if monthBefore.Halted() {
		t.Fatalf("the current month is halted in this database (halted_at %s); the journey cannot exercise translation - clear translation_spend.halted_at or run against a fresh month", monthBefore.HaltedAt)
	}

	costs := map[string]int64{
		journeyTramTitle:     1500,
		journeyWaterTitle:    1600,
		journeyFestivalTitle: 1700,
	}
	var totalCost int64
	for _, c := range costs {
		totalCost += c
	}
	fake := journeyTranslator{results: map[string]translation.Result{
		journeyTramTitle: {
			Headline: "Neue Tramlinie verbindet Münchens Zentrum mit den nördlichen Vororten",
			Extract:  "Die neue Tramlinie hat heute ihren Betrieb aufgenommen und verbindet das Zentrum Münchens in unter zwanzig Minuten mit den nördlichen Vororten.",
			Model:    "journey-model-1",
			Spend:    translation.Spend{CostMicroUSD: costs[journeyTramTitle]},
		},
		journeyWaterTitle: {
			Headline: "Modernisierung des Wassernetzes in Thessaloniki",
			Extract:  "Im kommenden Monat beginnen die Arbeiten zur Modernisierung des Wassernetzes in Thessaloniki, mit dem Austausch von Leitungen in vierzehn Stadtbezirken.",
			Model:    "journey-model-1",
			Spend:    translation.Spend{CostMicroUSD: costs[journeyWaterTitle]},
		},
		journeyFestivalTitle: {
			Headline: "Festival griechischer Musik kehrt im September nach Bayern zurück",
			Extract:  "Das jährliche Festival griechischer Musik kehrt im September mit Konzerten in drei Städten nach Bayern zurück.",
			Model:    "journey-model-1",
			Spend:    translation.Spend{CostMicroUSD: costs[journeyFestivalTitle]},
		},
	}, texts: map[string]string{
		// One distinctive fragment of each item's own body, so a foreign
		// item sharing a fixture title is stepped over, never billed.
		journeyTramTitle:     "λιγότερο από είκοσι λεπτά",
		journeyWaterTitle:    "δεκατέσσερις δήμους",
		journeyFestivalTitle: "χιλιάδες επισκέπτες",
	}}
	pipeline, err := translation.NewPipeline(discardLogger(), tx, fake, translation.PipelineConfig{
		Interval: time.Hour,
		// Far above any realistic backlog: the work list is database-wide
		// and the bound counts provider calls, so foreign eligible items -
		// each stepped over unbilled by the fake - must never exhaust the
		// cycle before the journey's own three are reached. Other suites
		// seed future-dated rows, which sort ahead of these in the
		// newest-first list, so "newest wins" is not a defence here.
		Limit:         10_000,
		ReaderLocales: translation.AlphaReaderLocales,
		Caps: translation.Caps{
			PerArticleMicroUSD: 20_000,
			MonthlyMicroUSD:    monthBefore.SpentMicroUSD + 10_000_000,
		},
	})
	if err != nil {
		t.Fatalf("building the pipeline: %v", err)
	}
	if err := pipeline.RunOnce(ctx); err != nil {
		t.Fatalf("running the translation cycle: %v", err)
	}

	// Each translation row carries its lineage and cost (FR-005, FR-006)...
	translationIDs := make(map[string]uuid.UUID, 3)
	for title, itemID := range itemIDs {
		var trID, model, promptVersion, locale, headline string
		var cost int64
		if err := tx.QueryRow(ctx,
			`select id, model, prompt_version, target_locale, cost_microusd, headline
			   from translation where source_item_id = $1`, itemID.String(),
		).Scan(&trID, &model, &promptVersion, &locale, &cost, &headline); err != nil {
			t.Fatalf("reading the translation of %q: %v", title, err)
		}
		if model != "journey-model-1" {
			t.Errorf("%q translation model = %q, want journey-model-1", title, model)
		}
		if promptVersion != translation.CurrentPromptVersion {
			t.Errorf("%q prompt_version = %q, want %q", title, promptVersion, translation.CurrentPromptVersion)
		}
		if locale != "de" {
			t.Errorf("%q target_locale = %q, want de - the one reader locale that is not the item's own", title, locale)
		}
		if cost != costs[title] {
			t.Errorf("%q cost_microusd = %d, want the provider-reported %d", title, cost, costs[title])
		}
		if headline != fake.results[title].Headline {
			t.Errorf("%q translated headline = %q, want %q", title, headline, fake.results[title].Headline)
		}
		translationIDs[title] = uuid.MustParse(trID)
	}

	// ...and the ledger moved by exactly their sum, in this same
	// transaction's view: the 0005 trigger books the cost with the insert,
	// so a translation whose cost is off the ledger cannot exist (FR-006).
	monthAfter, err := ledger.ThisMonth(ctx)
	if err != nil {
		t.Fatalf("reading the ledger after translation: %v", err)
	}
	if got := monthAfter.SpentMicroUSD - monthBefore.SpentMicroUSD; got != totalCost {
		t.Errorf("the ledger moved by %d micro-USD, want exactly the journey's %d", got, totalCost)
	}

	// ================= 3. QUEUE -> APPROVAL =================

	// The review queue through the real handler: the tram translation's row
	// must carry the evidence the approval rests on (#87).
	tramTranslation := translationIDs[journeyTramTitle].String()
	tramRow, found := journeyFindQueueItem(t, editorHandler, tramTranslation)
	if !found {
		t.Fatal("the tram translation never appeared in the review queue")
	}
	if tramRow.OriginalPublishedAt == nil || *tramRow.OriginalPublishedAt != journeyTramPublished {
		t.Errorf("queue original_published_at = %v, want the feed's declared %q rendered in UTC", tramRow.OriginalPublishedAt, journeyTramPublished)
	}
	if tramRow.ContentHash != contentHashes[journeyTramTitle] {
		t.Errorf("queue content_hash = %q, want the evidence row's %q", tramRow.ContentHash, contentHashes[journeyTramTitle])
	}
	if tramRow.ExtractOriginal == "" {
		t.Error("queue extract_original is empty; the editor approves evidence they cannot see")
	}
	if n := utf8.RuneCountInString(tramRow.ExtractOriginal); n > text.MaxExtractRunes {
		t.Errorf("queue extract_original is %d runes, over the D9 bound of %d", n, text.MaxExtractRunes)
	}
	if tramRow.LicenceSnapshot != licence {
		t.Errorf("queue licence_snapshot = %q, want %q", tramRow.LicenceSnapshot, licence)
	}
	if tramRow.SourceURL != journeyTramLink {
		t.Errorf("queue source_url = %q, want %q", tramRow.SourceURL, journeyTramLink)
	}
	if tramRow.OriginalAuthor == nil || *tramRow.OriginalAuthor != journeyTramAuthor {
		t.Errorf("queue original_author = %v, want %q", tramRow.OriginalAuthor, journeyTramAuthor)
	}
	if tramRow.Model == nil || *tramRow.Model != "journey-model-1" ||
		tramRow.PromptVersion == nil || *tramRow.PromptVersion != translation.CurrentPromptVersion ||
		tramRow.CostMicroUSD == nil || *tramRow.CostMicroUSD != costs[journeyTramTitle] {
		t.Errorf("queue lineage = (%v, %v, %v), want (journey-model-1, %s, %d)",
			tramRow.Model, tramRow.PromptVersion, tramRow.CostMicroUSD, translation.CurrentPromptVersion, costs[journeyTramTitle])
	}

	attribution := "Πηγή: " + sourceName + " — " + journeyTramLink

	// An approval naming no places is refused before anything is written:
	// an article tagged to no place is unreachable by every reader.
	noPlaces, _ := json.Marshal(map[string]any{
		"translation_id": tramTranslation,
		"attribution":    attribution,
		"publish":        true,
	})
	if rec := journeyDo(t, editorHandler, http.MethodPost, "/api/v1/editorial/approvals", string(noPlaces)); rec.Code != http.StatusBadRequest {
		t.Fatalf("approval without places = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}

	// The tram translation publishes to Munich and Greece - article A.
	articleA := journeyApprove(t, editorHandler, map[string]any{
		"translation_id": tramTranslation,
		"attribution":    attribution,
		"publish":        true,
		"places":         []string{"munich", "greece"},
	})

	// I-1: the reader passed the HTTP gate (the fake let them through on
	// purpose), and the DATABASE still refuses the approval.
	readerApproval, _ := json.Marshal(map[string]any{
		"source_item_id": itemIDs[journeyWaterTitle].String(),
		"attribution":    "Πηγή: " + sourceName,
		"publish":        true,
		"places":         []string{"munich"},
	})
	if rec := journeyDo(t, readerHandler, http.MethodPost, "/api/v1/editorial/approvals", string(readerApproval)); rec.Code != http.StatusForbidden {
		t.Fatalf("approval by a reader account = %d, want 403 - the database is the authority on I-1 (body %q)", rec.Code, rec.Body.String())
	}

	// The water item publishes untranslated off its original title -
	// article B, the untranslated chain of the drill.
	articleB := journeyApprove(t, editorHandler, map[string]any{
		"source_item_id": itemIDs[journeyWaterTitle].String(),
		"attribution":    "Πηγή: " + sourceName + " — " + journeyWaterLink,
		"publish":        true,
		"places":         []string{"munich"},
	})

	// A second approval of an already-approved origin is a conflict: the
	// one-per-origin partial index answers, not an application pre-check.
	repeat, _ := json.Marshal(map[string]any{
		"translation_id": tramTranslation,
		"attribution":    attribution,
		"publish":        true,
		"places":         []string{"munich"},
	})
	if rec := journeyDo(t, editorHandler, http.MethodPost, "/api/v1/editorial/approvals", string(repeat)); rec.Code != http.StatusConflict {
		t.Fatalf("second approval of the same origin = %d, want 409 (body %q)", rec.Code, rec.Body.String())
	}

	// The festival translation publishes to Munich - article C, which the
	// journey later withdraws.
	articleC := journeyApprove(t, editorHandler, map[string]any{
		"translation_id": translationIDs[journeyFestivalTitle].String(),
		"attribution":    "Πηγή: " + sourceName + " — " + journeyFestivalLink,
		"publish":        true,
		"places":         []string{"munich"},
	})

	// ================= 4. READER =================

	readerHTTP := content.NewHandler(discardLogger(), tx)

	// The front page answers each article for its places...
	frontA := journeyFindFront(t, readerHTTP, "de", []string{"munich"}, articleA)
	if frontA == nil {
		t.Fatal("article A is missing from the de/munich front page")
	}
	if frontA.Headline != fake.results[journeyTramTitle].Headline {
		t.Errorf("front headline = %q, want the translation's %q", frontA.Headline, fake.results[journeyTramTitle].Headline)
	}
	if frontA.Attribution != attribution {
		t.Errorf("front attribution = %q, want the approval's %q (FR-008)", frontA.Attribution, attribution)
	}
	if frontA.SourceURL != journeyTramLink {
		t.Errorf("front source_url = %q, want the original at the publisher %q (FR-008)", frontA.SourceURL, journeyTramLink)
	}
	if want := []string{"greece", "munich"}; !slices.Equal(frontA.Places, want) {
		t.Errorf("front places = %v, want %v", frontA.Places, want)
	}
	if journeyFindFront(t, readerHTTP, "de", []string{"greece"}, articleA) == nil {
		t.Error("article A is missing from the de/greece front page although the approval named greece")
	}
	if journeyFindFront(t, readerHTTP, "de", []string{"munich"}, articleC) == nil {
		t.Error("article C is missing from the de/munich front page before its withdrawal")
	}
	frontB := journeyFindFront(t, readerHTTP, "el", []string{"munich"}, articleB)
	if frontB == nil {
		t.Fatal("article B is missing from the el/munich front page")
	}
	if frontB.Headline != journeyWaterTitle {
		t.Errorf("untranslated front headline = %q, want the feed's own title", frontB.Headline)
	}
	if n := utf8.RuneCountInString(frontB.Extract); frontB.Extract == "" || n > text.MaxExtractRunes {
		t.Errorf("untranslated front extract is %d runes (%q), want derived, non-empty and within %d", n, frontB.Extract, text.MaxExtractRunes)
	}

	// ...and a place the approval did not name does NOT see it (FR-009).
	if journeyFindFront(t, readerHTTP, "de", []string{"bavaria"}, articleA) != nil {
		t.Error("article A appears on the bavaria front page although the approval never named bavaria")
	}
	if journeyFindFront(t, readerHTTP, "el", []string{"greece"}, articleB) != nil {
		t.Error("article B appears on the greece front page although the approval named only munich")
	}

	// The article page answers.
	rec = journeyDo(t, readerHTTP, http.MethodGet, "/api/v1/articles/"+articleA, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET article A = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var pageA journeyFrontItem
	if err := json.Unmarshal(rec.Body.Bytes(), &pageA); err != nil {
		t.Fatalf("unmarshalling article A: %v", err)
	}
	if pageA.SourceURL != journeyTramLink || pageA.Attribution != attribution || pageA.Lang != "de" {
		t.Errorf("article page A = (%q, %q, %q), want the tram link, the approval's attribution and de", pageA.SourceURL, pageA.Attribution, pageA.Lang)
	}
	if pageA.ApprovedAt == "" {
		t.Error("article page A lacks approved_at")
	}

	// ================= 5. WITHDRAWAL =================

	withdrawalReason := "Ο εκδότης ζήτησε διόρθωση της μετάφρασης (journey " + suffix + ")"
	withdrawBody, _ := json.Marshal(map[string]string{"reason": withdrawalReason})
	rec = journeyDo(t, editorHandler, http.MethodPost, "/api/v1/editorial/articles/"+articleC+"/withdrawal", string(withdrawBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("withdrawal of article C = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	// The banner shape: the confirmation IS the record of what happened, so
	// it names the article, the moment, the person and the frozen reason.
	var withdrawal struct {
		ArticleID   string `json:"article_id"`
		WithdrawnAt string `json:"withdrawn_at"`
		WithdrawnBy string `json:"withdrawn_by"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &withdrawal); err != nil {
		t.Fatalf("unmarshalling the withdrawal: %v", err)
	}
	if withdrawal.ArticleID != articleC || withdrawal.WithdrawnBy != editorIDText || withdrawal.Reason != withdrawalReason {
		t.Errorf("withdrawal = %+v, want article C, the editor, and the recorded reason", withdrawal)
	}
	journeyUTCStamp(t, "withdrawal.withdrawn_at", withdrawal.WithdrawnAt)

	// Withdrawn means gone from the reader's site...
	if journeyFindFront(t, readerHTTP, "de", []string{"munich"}, articleC) != nil {
		t.Error("article C still appears on the front page after its withdrawal")
	}
	if rec := journeyDo(t, readerHTTP, http.MethodGet, "/api/v1/articles/"+articleC, ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET withdrawn article C = %d, want the indistinguishable 404", rec.Code)
	}
	// ...and NOT gone from the audit: the drill below reads its full chain.

	// ================= 6. THE I-5 DRILL =================

	tramAuthor := journeyTramAuthor
	chains := []journeyChain{
		{
			label: "translated", articleID: articleA,
			headline: fake.results[journeyTramTitle].Headline,
			places:   []string{"greece", "munich"}, sourceURL: journeyTramLink,
			author: &tramAuthor, translated: true, costMicroUSD: costs[journeyTramTitle],
		},
		{
			label: "untranslated", articleID: articleB,
			headline: journeyWaterTitle,
			places:   []string{"munich"}, sourceURL: journeyWaterLink,
		},
		{
			label: "withdrawn", articleID: articleC,
			headline: fake.results[journeyFestivalTitle].Headline,
			places:   []string{"munich"}, sourceURL: journeyFestivalLink,
			translated: true, costMicroUSD: costs[journeyFestivalTitle],
			withdrawn: true, withdrawalReason: withdrawalReason,
		},
	}
	var total time.Duration
	for _, chain := range chains {
		start := time.Now()
		rec := journeyDo(t, editorHandler, http.MethodGet, "/api/v1/editorial/articles/"+chain.articleID+"/provenance", "")
		elapsed := time.Since(start)
		total += elapsed
		if rec.Code != http.StatusOK {
			t.Fatalf("provenance for the %s article = %d, want 200 (body %q)", chain.label, rec.Code, rec.Body.String())
		}
		if elapsed > journeyChainBudget {
			t.Errorf("the %s chain took %s, over the drill's %s budget (I-5 audit budget: 5m)", chain.label, elapsed, journeyChainBudget)
		}
		t.Logf("I-5 drill: the %s article's full chain answered in %s", chain.label, elapsed)

		var got journeyProvenance
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshalling the %s chain: %v", chain.label, err)
		}
		journeyAssertChain(t, got, chain, licence, sourceName, feedURL, editorName, editorEmail, editorIDText)
	}
	t.Logf("I-5 drill: all %d chains answered complete in %s total, against the five-minute audit budget", len(chains), total)
	if total > journeyChainBudget {
		t.Errorf("the whole drill took %s, over the %s budget", total, journeyChainBudget)
	}
}

// journeyApprove posts one approval and returns the created article id.
func journeyApprove(t *testing.T, h http.Handler, body map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshalling the approval: %v", err)
	}
	rec := journeyDo(t, h, http.MethodPost, "/api/v1/editorial/approvals", string(payload))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST approvals = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	var created struct {
		ArticleID   string  `json:"article_id"`
		ApprovedBy  string  `json:"approved_by"`
		ApprovedAt  string  `json:"approved_at"`
		PublishedAt *string `json:"published_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshalling the approval: %v", err)
	}
	journeyUTCStamp(t, "approval.approved_at", created.ApprovedAt)
	if created.PublishedAt == nil {
		t.Fatal("published_at = null for a publish:true approval")
	}
	journeyUTCStamp(t, "approval.published_at", *created.PublishedAt)
	return created.ArticleID
}

// journeyFindQueueItem walks the review queue page by page through the real
// handler until it finds the row whose translation id matches, or the queue
// is exhausted. By ID, never by position: the queue is shared with whatever
// the founder's database and concurrent suites hold.
func journeyFindQueueItem(t *testing.T, h http.Handler, translationID string) (journeyQueueItem, bool) {
	t.Helper()
	cursor := ""
	for range 50 {
		target := "/api/v1/editorial/queue?limit=100"
		if cursor != "" {
			target += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := journeyDo(t, h, http.MethodGet, target, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET queue = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var page journeyQueueBody
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("unmarshalling the queue: %v", err)
		}
		for _, item := range page.Items {
			if item.TranslationID != nil && *item.TranslationID == translationID {
				return item, true
			}
		}
		if page.NextCursor == nil {
			return journeyQueueItem{}, false
		}
		cursor = *page.NextCursor
	}
	t.Fatal("the queue walk never exhausted after 50 pages")
	return journeyQueueItem{}, false
}

// journeyFindFront walks a front page through the real content handler and
// returns the article's row, or nil once the feed is exhausted without it.
func journeyFindFront(t *testing.T, h http.Handler, lang string, places []string, articleID string) *journeyFrontItem {
	t.Helper()
	query := url.Values{"lang": {lang}, "limit": {"100"}}
	for _, place := range places {
		query.Add("place", place)
	}
	cursor := ""
	for range 50 {
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		rec := journeyDo(t, h, http.MethodGet, "/api/v1/front?"+query.Encode(), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET front %s/%v = %d, want 200 (body %q)", lang, places, rec.Code, rec.Body.String())
		}
		var page journeyFrontBody
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("unmarshalling the front page: %v", err)
		}
		for i := range page.Items {
			if page.Items[i].ID == articleID {
				return &page.Items[i]
			}
		}
		if page.NextCursor == nil {
			return nil
		}
		cursor = *page.NextCursor
	}
	t.Fatalf("the %s/%v front page never exhausted after 50 pages", lang, places)
	return nil
}

// journeyAssertChain checks every link I-5 names on one drilled article:
// source identity, the retrieval-time snapshots, the lineage with its cost,
// the named approver, the withdrawal where present, and the audit stream in
// lifecycle order - every timestamp rendered in UTC.
func journeyAssertChain(t *testing.T, got journeyProvenance, want journeyChain, licence, sourceName, feedURL, editorName, editorEmail, editorID string) {
	t.Helper()

	if got.ArticleID != want.articleID {
		t.Errorf("%s: article_id = %q, want %q", want.label, got.ArticleID, want.articleID)
	}
	if got.Headline != want.headline {
		t.Errorf("%s: headline = %q, want %q", want.label, got.Headline, want.headline)
	}
	if !slices.Equal(got.Places, want.places) {
		t.Errorf("%s: places = %v, want %v", want.label, got.Places, want.places)
	}

	// The source: identity only - the legal basis is the snapshots below.
	if got.Source.Name != sourceName || got.Source.FeedURL != feedURL || got.Source.Jurisdiction != "GR" {
		t.Errorf("%s: source = %+v, want (%q, %q, GR)", want.label, got.Source, sourceName, feedURL)
	}

	// The retrieval evidence with its trigger-written snapshots (I-2, I-4).
	if got.SourceItem.SourceURL != want.sourceURL {
		t.Errorf("%s: source_url = %q, want %q", want.label, got.SourceItem.SourceURL, want.sourceURL)
	}
	if got.SourceItem.LicenceSnapshot != licence {
		t.Errorf("%s: licence_snapshot = %q, want the terms at retrieval %q", want.label, got.SourceItem.LicenceSnapshot, licence)
	}
	if got.SourceItem.UsageRuleSnapshot != "extract_and_link" {
		t.Errorf("%s: usage_rule_snapshot = %q, want extract_and_link", want.label, got.SourceItem.UsageRuleSnapshot)
	}
	if !journeySHA256.MatchString(got.SourceItem.ContentHash) {
		t.Errorf("%s: content_hash = %q, want a sha-256 hex digest", want.label, got.SourceItem.ContentHash)
	}
	if got.SourceItem.OriginalTitle == nil {
		t.Errorf("%s: original_title is null", want.label)
	}
	switch {
	case want.author == nil && got.SourceItem.OriginalAuthor != nil:
		t.Errorf("%s: original_author = %q, want null - the feed named nobody", want.label, *got.SourceItem.OriginalAuthor)
	case want.author != nil && (got.SourceItem.OriginalAuthor == nil || *got.SourceItem.OriginalAuthor != *want.author):
		t.Errorf("%s: original_author = %v, want %q", want.label, got.SourceItem.OriginalAuthor, *want.author)
	}
	journeyUTCStamp(t, want.label+" retrieved_at", got.SourceItem.RetrievedAt)

	// The translation lineage and its recorded cost (FR-005, FR-006).
	if want.translated {
		if got.Translation == nil {
			t.Fatalf("%s: translation = null for a translated article", want.label)
		}
		if got.Translation.Model != "journey-model-1" ||
			got.Translation.PromptVersion != translation.CurrentPromptVersion ||
			got.Translation.TargetLocale != "de" ||
			got.Translation.CostMicroUSD != want.costMicroUSD {
			t.Errorf("%s: translation = %+v, want (journey-model-1, %s, de, %d)",
				want.label, got.Translation, translation.CurrentPromptVersion, want.costMicroUSD)
		}
		journeyUTCStamp(t, want.label+" generated_at", got.Translation.GeneratedAt)
	} else if got.Translation != nil {
		t.Errorf("%s: translation = %+v for an untranslated article, want null", want.label, got.Translation)
	}

	// The named human approver (I-1).
	if got.Approval.ApproverName != editorName || got.Approval.ApproverEmail != editorEmail {
		t.Errorf("%s: approver = (%q, %q), want (%q, %q)", want.label, got.Approval.ApproverName, got.Approval.ApproverEmail, editorName, editorEmail)
	}
	journeyUTCStamp(t, want.label+" approved_at", got.Approval.ApprovedAt)
	if got.PublishedAt == nil {
		t.Errorf("%s: published_at = null for a published article", want.label)
	} else {
		journeyUTCStamp(t, want.label+" published_at", *got.PublishedAt)
	}

	// The withdrawal, where publication ended (FR-016).
	if want.withdrawn {
		if got.Withdrawal == nil {
			t.Fatalf("%s: withdrawal = null for a withdrawn article", want.label)
		}
		if got.Withdrawal.Reason != want.withdrawalReason || got.Withdrawal.WithdrawnBy != editorID {
			t.Errorf("%s: withdrawal = %+v, want the recorded reason and the editor", want.label, got.Withdrawal)
		}
		journeyUTCStamp(t, want.label+" withdrawn_at", got.Withdrawal.WithdrawnAt)
	} else if got.Withdrawal != nil {
		t.Errorf("%s: withdrawal = %+v for an active article, want null", want.label, got.Withdrawal)
	}

	// The audit stream, in lifecycle order (FR-012). The order is asserted
	// by position, as in the T028 drill: the whole journey shares one
	// transaction timestamp, so occurred_at cannot order the ties.
	types := make([]string, 0, len(got.Events))
	for _, e := range got.Events {
		types = append(types, e.Type)
		journeyUTCStamp(t, want.label+" event "+e.Type+" occurred_at", e.OccurredAt)
		if e.Detail == "" {
			t.Errorf("%s: event %q lacks a detail", want.label, e.Type)
		}
		// The frozen audit payload itself renders its timestamps in UTC:
		// what lands in the append-only stream must not depend on the
		// server's zone.
		if e.Type == "article.approved" {
			var payload struct {
				ApprovedAt string `json:"approved_at"`
			}
			if err := json.Unmarshal([]byte(e.Detail), &payload); err == nil && payload.ApprovedAt != "" {
				journeyUTCStamp(t, want.label+" audit payload approved_at", payload.ApprovedAt)
			}
		}
	}
	for _, required := range []string{"article.approved", "article.published"} {
		if !slices.Contains(types, required) {
			t.Errorf("%s: events = %v, want %q on record", want.label, types, required)
		}
	}
	if want.withdrawn != slices.Contains(types, "article.withdrawn") {
		t.Errorf("%s: events = %v, withdrawn = %t", want.label, types, want.withdrawn)
	}
	positions := make([]int, 0, 3)
	for _, step := range []string{"article.approved", "article.published", "article.withdrawn"} {
		if at := slices.Index(types, step); at >= 0 {
			positions = append(positions, at)
		}
	}
	if !slices.IsSorted(positions) {
		t.Errorf("%s: events %v do not follow the lifecycle order", want.label, types)
	}
}

// Interface satisfaction pinned at compile time: the fakes stand exactly
// where serve() wires the real implementations.
var (
	_ editorial.EditorAuthenticator = journeyGate{}
	_ translation.Translator        = journeyTranslator{}
)
