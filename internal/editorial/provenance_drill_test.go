package editorial_test

// The I-5 drill (T028 acceptance, US5): against the real, migrated schema,
// seed the full editorial spread - translated and untranslated articles,
// withdrawn ones included - then pull the provenance of randomly picked
// published articles through the real handler and check two things: the
// chain is COMPLETE (source, retrieval-time licence snapshots, lineage
// with cost, named approver, withdrawal history, events), and it arrives
// in a time that makes the five-minute audit promise (FR-010) look like
// the understatement it must be. The timings are logged so the run's
// numbers are quotable, not just green.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/editorial"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// drillBudget is what this test holds each provenance read to. The audit
// promise is five minutes for the whole human task; a single chain read
// that needs more than five seconds of database time would already be a
// regression worth failing on.
const drillBudget = 5 * time.Second

// sha256Hex matches the database-computed content fingerprint.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// drillArticle is one seeded article and what the drill must find in its
// chain.
type drillArticle struct {
	articleID  string
	headline   string
	places     []string
	translated bool
	withdrawn  bool
}

func TestProvenanceI5DrillAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to run the I-5 drill against Postgres")
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

	// --- Seed: an editor, a source, and eight retrieved items. ---
	suffix := randomSuffix(t)
	licence := "Extract and link permitted per feed terms v1 (" + suffix + ")"
	var editorID, sourceID string
	if err := tx.QueryRow(ctx,
		`insert into account (email, display_name, role) values ($1, $2, 'editor') returning id`,
		"drill-editor-"+suffix+"@example.test", "Drill Editor "+suffix).Scan(&editorID); err != nil {
		t.Fatalf("seed editor: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'el', 'GR', $3) returning id`,
		"Drill Feed "+suffix, "https://example.test/drill/"+suffix, licence).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	const itemCount = 8
	itemIDs := make([]string, 0, itemCount)
	for i := range itemCount {
		var id string
		if err := tx.QueryRow(ctx,
			`insert into source_item (source_id, source_url, original_title, original_author, raw_body)
			 values ($1, $2, $3, $4, $5) returning id`,
			sourceID,
			fmt.Sprintf("https://example.test/drill/%s/articles/%d", suffix, i),
			fmt.Sprintf("Πρωτότυπος τίτλος %d (%s)", i, suffix),
			"Δοκιμαστική Συντάκτρια",
			fmt.Sprintf("Δοκιμαστικό περιεχόμενο %d (%s)", i, suffix),
		).Scan(&id); err != nil {
			t.Fatalf("seed source_item %d: %v", i, err)
		}
		itemIDs = append(itemIDs, id)
	}

	// The first half is translated (with an explicit recorded cost, FR-006);
	// the second half publishes untranslated off its original title.
	translationIDs := make(map[string]string, itemCount/2)
	for i := range itemCount / 2 {
		var id string
		if err := tx.QueryRow(ctx,
			`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
			 values ($1, 'de', 'drill-model-1', 'prompt-v7', $2, $3, 4100) returning id`,
			itemIDs[i],
			fmt.Sprintf("Übersetzte Überschrift %d (%s)", i, suffix),
			fmt.Sprintf("Übersetzter Auszug %d (%s)", i, suffix),
		).Scan(&id); err != nil {
			t.Fatalf("seed translation %d: %v", i, err)
		}
		translationIDs[itemIDs[i]] = id
	}

	// --- Approve and publish everything through the real store, so the
	// events the drill asserts on are the ones the flows actually write.
	// Withdraw one translated and one untranslated article afterwards:
	// audit sees full history, so the drill must too. ---
	store := editorial.NewPGStore(tx)
	editor := uuid.MustParse(editorID)
	placeSets := [][]string{{"munich"}, {"bavaria", "munich"}, {"greece"}, {"germany"}}
	seeded := make([]drillArticle, 0, itemCount)
	for i, itemID := range itemIDs {
		approval := editorial.NewApproval{
			Attribution: "Quelle: Drill Feed " + suffix,
			Publish:     true,
			Places:      placeSets[i%len(placeSets)],
			ApprovedBy:  editor,
		}
		expected := drillArticle{translated: i < itemCount/2}
		if expected.translated {
			id := uuid.MustParse(translationIDs[itemID])
			approval.TranslationID = &id
			expected.headline = fmt.Sprintf("Übersetzte Überschrift %d (%s)", i, suffix)
		} else {
			id := uuid.MustParse(itemID)
			approval.SourceItemID = &id
			expected.headline = fmt.Sprintf("Πρωτότυπος τίτλος %d (%s)", i, suffix)
		}
		article, err := store.Approve(ctx, approval)
		if err != nil {
			t.Fatalf("approving article %d: %v", i, err)
		}
		expected.articleID = article.ID.String()
		expected.places = slices.Sorted(slices.Values(approval.Places))
		seeded = append(seeded, expected)
	}
	for _, i := range []int{0, itemCount - 1} { // one translated, one untranslated
		if _, err := store.Withdraw(ctx, uuid.MustParse(seeded[i].articleID), editor,
			"drill withdrawal "+suffix); err != nil {
			t.Fatalf("withdrawing article %d: %v", i, err)
		}
		seeded[i].withdrawn = true
	}

	// --- The drill: random published picks, through the real handler. ---
	h := editorial.NewHandler(discardLogger(), store,
		staticAuth{editor: editorial.Editor{ID: editor, Email: "drill@example.test", DisplayName: "Drill Editor"}})

	// Both withdrawn articles are always drilled - the withdrawal arm of the
	// chain must never dodge the assertion by luck of the draw - and the
	// rest of the picks are random, so repeated CI runs walk different rows.
	picks := []int{0, itemCount - 1}
	for _, i := range rand.Perm(len(seeded)) {
		if len(picks) == 6 {
			break
		}
		if !slices.Contains(picks, i) {
			picks = append(picks, i)
		}
	}

	var total time.Duration
	for _, i := range picks {
		want := seeded[i]

		start := time.Now()
		rec := doJSON(t, h, http.MethodGet, "/api/v1/editorial/articles/"+want.articleID+"/provenance", editorToken, "")
		elapsed := time.Since(start)
		total += elapsed

		if rec.Code != http.StatusOK {
			t.Fatalf("provenance for %s = %d, want 200 (body %q)", want.articleID, rec.Code, rec.Body.String())
		}
		// FR-010 grants the whole human audit five minutes; the chain read
		// itself gets five seconds before this drill calls it a regression.
		if elapsed > drillBudget {
			t.Errorf("provenance for %s took %s, over the drill's %s budget (I-5 audit budget: 5m)", want.articleID, elapsed, drillBudget)
		}
		t.Logf("I-5 drill: provenance for %s (translated=%t, withdrawn=%t) answered complete in %s", want.articleID, want.translated, want.withdrawn, elapsed)

		var got provenanceBody
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshalling provenance for %s: %v", want.articleID, err)
		}
		assertChainComplete(t, got, want, suffix, licence, editorID)
	}
	t.Logf("I-5 drill: %d provenance chains answered complete in %s total (budget: far under 5m)", len(picks), total)
	if total > drillBudget {
		t.Errorf("the whole drill took %s, over the %s budget", total, drillBudget)
	}
}

// assertChainComplete checks every link I-5 names: source, licence
// snapshots as they applied at retrieval, translation lineage with cost,
// named approver, withdrawal history, and the event stream.
func assertChainComplete(t *testing.T, got provenanceBody, want drillArticle, suffix, licence, editorID string) {
	t.Helper()

	if got.ArticleID != want.articleID {
		t.Errorf("article_id = %q, want %q", got.ArticleID, want.articleID)
	}
	if got.Headline != want.headline {
		t.Errorf("headline = %q, want %q", got.Headline, want.headline)
	}
	if !slices.Equal(got.Places, want.places) {
		t.Errorf("places = %v, want %v", got.Places, want.places)
	}

	// Source identity - and identity only; the legal basis is asserted from
	// the snapshots below.
	if got.Source.Name != "Drill Feed "+suffix || got.Source.Jurisdiction != "GR" {
		t.Errorf("source = %+v", got.Source)
	}
	if got.Source.FeedURL != "https://example.test/drill/"+suffix {
		t.Errorf("feed_url = %q", got.Source.FeedURL)
	}

	// The retrieval evidence with its trigger-written snapshots (I-2, I-4).
	if got.SourceItem.LicenceSnapshot != licence {
		t.Errorf("licence_snapshot = %q, want the terms at retrieval %q", got.SourceItem.LicenceSnapshot, licence)
	}
	if got.SourceItem.UsageRuleSnapshot != "extract_and_link" {
		t.Errorf("usage_rule_snapshot = %q, want the trigger-written default", got.SourceItem.UsageRuleSnapshot)
	}
	if !sha256Hex.MatchString(got.SourceItem.ContentHash) {
		t.Errorf("content_hash = %q, want a database-computed sha-256 hex digest", got.SourceItem.ContentHash)
	}
	if got.SourceItem.SourceURL == "" || got.SourceItem.RetrievedAt == "" {
		t.Errorf("source_item lacks retrieval evidence: %+v", got.SourceItem)
	}
	if got.SourceItem.OriginalTitle == nil || got.SourceItem.OriginalAuthor == nil {
		t.Errorf("source_item lacks the feed's title/author: %+v", got.SourceItem)
	}

	// Translation lineage with its recorded cost (FR-005, FR-006).
	if want.translated {
		if got.Translation == nil {
			t.Fatalf("translation = null for a translated article")
		}
		if got.Translation.Model != "drill-model-1" || got.Translation.PromptVersion != "prompt-v7" ||
			got.Translation.TargetLocale != "de" || got.Translation.CostMicroUSD != 4100 {
			t.Errorf("translation = %+v", got.Translation)
		}
	} else if got.Translation != nil {
		t.Errorf("translation = %+v for an untranslated article, want null", got.Translation)
	}

	// The named human approver (I-1).
	if got.Approval.ApproverName != "Drill Editor "+suffix {
		t.Errorf("approver_name = %q", got.Approval.ApproverName)
	}
	if got.Approval.ApproverEmail != "drill-editor-"+suffix+"@example.test" {
		t.Errorf("approver_email = %q", got.Approval.ApproverEmail)
	}
	if got.Approval.ApprovedAt == "" {
		t.Error("approved_at is empty")
	}
	if got.PublishedAt == nil {
		t.Error("published_at = null for a published article")
	}

	// Withdrawal history - audit sees full history (FR-016).
	if want.withdrawn {
		if got.Withdrawal == nil {
			t.Fatalf("withdrawal = null for a withdrawn article")
		}
		if got.Withdrawal.Reason != "drill withdrawal "+suffix || got.Withdrawal.WithdrawnBy != editorID {
			t.Errorf("withdrawal = %+v", got.Withdrawal)
		}
	} else if got.Withdrawal != nil {
		t.Errorf("withdrawal = %+v for an active article, want null", got.Withdrawal)
	}

	// The audit stream: approval and publication are always on record, the
	// withdrawal exactly when it happened, in occurrence order.
	types := make([]string, 0, len(got.Events))
	occurred := make([]time.Time, 0, len(got.Events))
	for _, e := range got.Events {
		types = append(types, e.Type)
		if e.Detail == "" {
			t.Errorf("event %q lacks a detail", e.Type)
		}
		at, err := time.Parse(time.RFC3339Nano, e.OccurredAt)
		if err != nil {
			t.Fatalf("event %q occurred_at %q: %v", e.Type, e.OccurredAt, err)
		}
		occurred = append(occurred, at)
	}
	for _, required := range []string{"article.approved", "article.published"} {
		if !slices.Contains(types, required) {
			t.Errorf("events = %v, want %q on record", types, required)
		}
	}
	if want.withdrawn != slices.Contains(types, "article.withdrawn") {
		t.Errorf("events = %v, withdrawn = %t", types, want.withdrawn)
	}
	if !slices.IsSortedFunc(occurred, time.Time.Compare) {
		t.Errorf("events are not in occurrence order: %v", types)
	}
}
