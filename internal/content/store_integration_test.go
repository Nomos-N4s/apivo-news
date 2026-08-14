package content_test

// Exercises the sqlc-generated store against the real, migrated schema. The
// generated types must stay in lockstep with the migrations; this test (plus
// the sqlc drift check in CI) is what makes that claim true rather than
// asserted.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/content/store"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

func TestGeneratedStoreAgainstSchema(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the generated store")
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

	// Seed a full provenance chain with plain SQL: writing these rows is
	// ingestion's and editorial's job, not the read-side store's.
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	suffix := hex.EncodeToString(b)
	rawBody := "Δοκιμαστικό περιεχόμενο " + suffix
	licence := "Extract and link permitted per feed terms v1 (" + suffix + ")"
	hashSum := sha256.Sum256([]byte(rawBody))
	contentHash := hex.EncodeToString(hashSum[:])

	var accountID, sourceID, sourceItemID, translationID, articleID string
	// Approvers must hold the editor role (0002); the default is reader.
	if err := tx.QueryRow(ctx,
		`insert into account (email, display_name, role) values ($1, $2, 'editor') returning id`,
		"editor-"+suffix+"@example.test", "Test Editor "+suffix).Scan(&accountID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'el', 'GR', $3) returning id`,
		"Test Feed "+suffix, "https://example.test/feed/"+suffix, licence).Scan(&sourceID); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	// content_hash and the licence snapshot are written by the database.
	if err := tx.QueryRow(ctx,
		`insert into source_item (source_id, source_url, raw_body)
		 values ($1, $2, $3) returning id`,
		sourceID, "https://example.test/articles/"+suffix, rawBody).Scan(&sourceItemID); err != nil {
		t.Fatalf("seed source_item: %v", err)
	}
	// cost_microusd has no default (0002): the cost is always explicit.
	if err := tx.QueryRow(ctx,
		`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
		 values ($1, 'de', 'test-model-1', 'prompt-v1', $2, $3, 1500) returning id`,
		sourceItemID, "Testüberschrift "+suffix, "Testauszug "+suffix).Scan(&translationID); err != nil {
		t.Fatalf("seed translation: %v", err)
	}
	if err := tx.QueryRow(ctx,
		`insert into article (translation_id, approved_by, published_at, attribution_block)
		 values ($1, $2, now(), 'Quelle: Test Feed') returning id`,
		translationID, accountID).Scan(&articleID); err != nil {
		t.Fatalf("seed article: %v", err)
	}

	var articleUUID pgtype.UUID
	if err := articleUUID.Scan(articleID); err != nil {
		t.Fatalf("parsing article id: %v", err)
	}

	q := store.New(tx)

	prov, err := q.GetArticleProvenance(ctx, articleUUID)
	if err != nil {
		t.Fatalf("GetArticleProvenance: %v", err)
	}
	if prov.LicenceSnapshot != licence {
		t.Errorf("LicenceSnapshot = %q, want %q", prov.LicenceSnapshot, licence)
	}
	if prov.ContentHash != contentHash {
		t.Errorf("ContentHash = %q, want %q", prov.ContentHash, contentHash)
	}
	if !prov.Model.Valid || prov.Model.String != "test-model-1" {
		t.Errorf("Model = %+v, want test-model-1", prov.Model)
	}
	if !prov.PromptVersion.Valid || prov.PromptVersion.String != "prompt-v1" {
		t.Errorf("PromptVersion = %+v, want prompt-v1", prov.PromptVersion)
	}
	if prov.ApproverName == "" || prov.ApproverEmail == "" {
		t.Error("provenance lacks the named human approver")
	}

	articles, err := q.ListPublishedArticles(ctx, 10)
	if err != nil {
		t.Fatalf("ListPublishedArticles: %v", err)
	}
	found := false
	for _, a := range articles {
		if a.ID == articleUUID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("published article %s missing from ListPublishedArticles", articleID)
	}

	// Withdrawal ends publication (FR-016): the article must vanish from
	// the reader-facing listing while its row and provenance remain.
	if _, err := tx.Exec(ctx,
		`update article set withdrawn_at = now(), withdrawn_by = $2, withdrawal_reason = 'integration withdrawal'
		 where id = $1`, articleID, accountID); err != nil {
		t.Fatalf("withdraw article: %v", err)
	}
	articles, err = q.ListPublishedArticles(ctx, 10)
	if err != nil {
		t.Fatalf("ListPublishedArticles after withdrawal: %v", err)
	}
	for _, a := range articles {
		if a.ID == articleUUID {
			t.Errorf("withdrawn article %s still listed by ListPublishedArticles", articleID)
		}
	}
	if _, err := q.GetArticleProvenance(ctx, articleUUID); err != nil {
		t.Errorf("withdrawn article lost its provenance record: %v", err)
	}
}
