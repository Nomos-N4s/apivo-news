package db_test

// These tests assert that the DATABASE enforces the licensing invariants.
// Application code is not trusted with legal guarantees; every test here
// attempts an illegal state and requires Postgres itself to reject it.
//
//	I-1  an article cannot exist without a named human approver
//	     (tightened in 0002: the approver must hold the editor role)
//	I-2  provenance is captured at retrieval, with the content
//	I-3  source_item and domain_event are immutable / append only
//	I-4  licence terms are snapshotted at retrieval
//	I-5  full provenance of any article is one query away
//	     (extended in 0002: withdrawal ends publication, keeps the record)
//
// They run against a real Postgres, keyed on DATABASE_URL. Locally:
// `docker compose up -d postgres` and export DATABASE_URL. In CI the
// database is a service container; these tests are never skipped there.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// SQLSTATE codes the assertions expect.
const (
	codeNotNullViolation    = "23502"
	codeForeignKeyViolation = "23503"
	codeUniqueViolation     = "23505"
	codeCheckViolation      = "23514"
	codeGeneratedAlways     = "428C9"
	codeRaiseException      = "P0001"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	url := os.Getenv("DATABASE_URL")
	if url != "" {
		if err := db.Migrate(url); err != nil {
			fmt.Fprintln(os.Stderr, "migrating test database:", err)
			os.Exit(1)
		}
		pool, err := pgxpool.New(context.Background(), url)
		if err != nil {
			fmt.Fprintln(os.Stderr, "connecting test database:", err)
			os.Exit(1)
		}
		testPool = pool
	}
	code := m.Run()
	if testPool != nil {
		testPool.Close()
	}
	os.Exit(code)
}

// beginTx opens a transaction that is always rolled back, keeping tests
// independent and the database clean.
func beginTx(t *testing.T) pgx.Tx {
	t.Helper()
	if testPool == nil {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set DATABASE_URL to exercise schema invariants")
	}
	tx, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func wantPgCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want SQLSTATE %s, but the database accepted the write", code)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("want *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != code {
		t.Fatalf("want SQLSTATE %s, got %s: %s", code, pgErr.Code, pgErr.Message)
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

func sha256Hex(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// fixtures is a fully valid chain: source -> source_item -> translation,
// plus an approver account. Illegal-write tests mutate one aspect at a time.
type fixtures struct {
	accountID     string
	sourceID      string
	sourceItemID  string
	translationID string
	rawBody       string
	licence       string
}

func seed(t *testing.T, tx pgx.Tx) fixtures {
	t.Helper()
	ctx := context.Background()
	suffix := randomSuffix(t)
	f := fixtures{
		rawBody: "Δοκιμαστικό περιεχόμενο " + suffix,
		licence: "Extract and link permitted per feed terms v1 (" + suffix + ")",
	}

	// Approvers must hold the editor role (0002); the default is reader.
	err := tx.QueryRow(ctx,
		`insert into account (email, display_name, role) values ($1, $2, 'editor') returning id`,
		"editor-"+suffix+"@example.test", "Test Editor "+suffix,
	).Scan(&f.accountID)
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}

	err = tx.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'el', 'GR', $3) returning id`,
		"Test Feed "+suffix, "https://example.test/feed/"+suffix, f.licence,
	).Scan(&f.sourceID)
	if err != nil {
		t.Fatalf("seed source: %v", err)
	}

	// content_hash and the licence/usage/evidence snapshots are written by
	// the database itself; callers never supply them.
	err = tx.QueryRow(ctx,
		`insert into source_item (source_id, source_url, original_author, published_at, raw_body)
		 values ($1, $2, $3, now(), $4) returning id`,
		f.sourceID, "https://example.test/articles/"+suffix, "Α. Συντάκτης", f.rawBody,
	).Scan(&f.sourceItemID)
	if err != nil {
		t.Fatalf("seed source_item: %v", err)
	}

	// cost_microusd has no default (0002): the cost is always explicit.
	err = tx.QueryRow(ctx,
		`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
		 values ($1, 'de', 'test-model-1', 'prompt-v1', $2, $3, 1500) returning id`,
		f.sourceItemID, "Testüberschrift "+suffix, "Testauszug "+suffix,
	).Scan(&f.translationID)
	if err != nil {
		t.Fatalf("seed translation: %v", err)
	}
	return f
}

// TestDatabaseRejectsIllegalWrites is the core invariant table: every case
// is a write that must fail at the database layer with a specific SQLSTATE.
func TestDatabaseRejectsIllegalWrites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		invariant string
		write     func(ctx context.Context, tx pgx.Tx, f fixtures) error
		wantCode  string
	}{
		{
			name:      "article without approver",
			invariant: "I-1",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into article (translation_id, approved_by, attribution_block)
					 values ($1, null, 'Quelle: Test Feed')`, f.translationID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name:      "article approved by nonexistent account",
			invariant: "I-1",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into article (translation_id, approved_by, attribution_block)
					 values ($1, gen_random_uuid(), 'Quelle: Test Feed')`, f.translationID)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name:      "source_item without raw body",
			invariant: "I-2",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into source_item (source_id, source_url, raw_body)
					 values ($1, 'https://example.test/a', null)`,
					f.sourceID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name:      "source_item without source url",
			invariant: "I-2",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into source_item (source_id, source_url, raw_body)
					 values ($1, null, $2)`,
					f.sourceID, "no url "+f.rawBody)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name:      "source_item with blank source url",
			invariant: "I-2",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into source_item (source_id, source_url, raw_body)
					 values ($1, '  ', $2)`,
					f.sourceID, "blank url "+f.rawBody)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "source_item with caller-supplied content hash",
			invariant: "I-2",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				// The fingerprint is computed by the database; a caller
				// attempting to write one - even a correct one - is refused.
				body := "supplied hash " + f.rawBody
				_, err := tx.Exec(ctx,
					`insert into source_item (source_id, source_url, raw_body, content_hash)
					 values ($1, 'https://example.test/a', $2, $3)`,
					f.sourceID, body, sha256Hex(body))
				return err
			},
			wantCode: codeGeneratedAlways,
		},
		{
			name:      "source_item for a source with no licence terms on record",
			invariant: "I-4",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures) error {
				// Blank licence terms are rejected on source itself, so the
				// snapshot chain can never start from nothing.
				_, err := tx.Exec(ctx,
					`insert into source (name, url, language_code, jurisdiction, licence_terms)
					 values ('Empty Terms Feed', 'https://example.test/empty-terms', 'el', 'GR', '   ')`)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "article with two origins",
			invariant: "schema",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into article (translation_id, source_item_id, approved_by, attribution_block)
					 values ($1, $2, $3, 'Quelle: Test Feed')`,
					f.translationID, f.sourceItemID, f.accountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "article with no origin",
			invariant: "schema",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into article (approved_by, attribution_block)
					 values ($1, 'Quelle: Test Feed')`, f.accountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "article published before approval",
			invariant: "schema",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into article (translation_id, approved_by, approved_at, published_at, attribution_block)
					 values ($1, $2, now(), now() - interval '1 hour', 'Quelle: Test Feed')`,
					f.translationID, f.accountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "article with blank attribution",
			invariant: "schema",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into article (translation_id, approved_by, attribution_block)
					 values ($1, $2, '   ')`, f.translationID, f.accountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "source claiming full text without permission evidence",
			invariant: "usage-rule",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into source (name, url, language_code, jurisdiction, licence_terms, usage_rule)
					 values ('Greedy Feed', 'https://example.test/greedy', 'el', 'GR', 'terms', 'full_text')`)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "source claiming full text with blank permission evidence",
			invariant: "usage-rule",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into source (name, url, language_code, jurisdiction, licence_terms, usage_rule, permission_evidence)
					 values ('Greedy Feed', 'https://example.test/greedy', 'el', 'GR', 'terms', 'full_text', '   ')`)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "source with unknown usage rule",
			invariant: "usage-rule",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into source (name, url, language_code, jurisdiction, licence_terms, usage_rule)
					 values ('Odd Feed', 'https://example.test/odd', 'el', 'GR', 'terms', 'scrape_everything')`)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "combined locale tag rejected as language",
			invariant: "locale model",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures) error {
				_, err := tx.Exec(ctx, `insert into language (code) values ('el-DE')`)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "place with invalid country code",
			invariant: "locale model",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into place (name, country) values ('Nowhere', 'Germany')`)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "translation with blank headline",
			invariant: "extract-and-link",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
					 values ($1, 'de', 'm', 'p', '   ', 'extract', 0)`, f.sourceItemID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "translation with blank extract",
			invariant: "extract-and-link",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
					 values ($1, 'de', 'm', 'p', 'headline', '', 0)`, f.sourceItemID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "second article from the same translation",
			invariant: "one-per-origin",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				if _, err := tx.Exec(ctx,
					`insert into article (translation_id, approved_by, attribution_block)
					 values ($1, $2, 'Quelle: Test Feed')`, f.translationID, f.accountID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into article (translation_id, approved_by, attribution_block)
					 values ($1, $2, 'Quelle: Test Feed again')`, f.translationID, f.accountID)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name:      "second article from the same retrieved item",
			invariant: "one-per-origin",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				if _, err := tx.Exec(ctx,
					`insert into article (source_item_id, approved_by, attribution_block)
					 values ($1, $2, 'Πηγή: Test Feed')`, f.sourceItemID, f.accountID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into article (source_item_id, approved_by, attribution_block)
					 values ($1, $2, 'Πηγή: Test Feed again')`, f.sourceItemID, f.accountID)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name:      "second active consent for same purpose",
			invariant: "consent",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				if _, err := tx.Exec(ctx,
					`insert into consent (account_id, purpose) values ($1, 'marketing')`,
					f.accountID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into consent (account_id, purpose) values ($1, 'marketing')`,
					f.accountID)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name:      "consent revoked before granted",
			invariant: "consent",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into consent (account_id, purpose, granted_at, revoked_at)
					 values ($1, 'marketing', now(), now() - interval '1 day')`, f.accountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "duplicate account email is rejected case-insensitively",
			invariant: "identity",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures) error {
				if _, err := tx.Exec(ctx,
					`insert into account (email, display_name) values ('Dup@Example.Test', 'One')`); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into account (email, display_name) values ('dup@example.test', 'Two')`)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name:      "account with unknown role",
			invariant: "I-1",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into account (email, display_name, role) values ('admin@example.test', 'Admin', 'admin')`)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "article approved by a reader-role account",
			invariant: "I-1",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				// The default role is reader; approval authority belongs
				// to editors only.
				var readerID string
				if err := tx.QueryRow(ctx,
					`insert into account (email, display_name) values ('reader-approver@example.test', 'A Reader') returning id`).
					Scan(&readerID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into article (translation_id, approved_by, attribution_block)
					 values ($1, $2, 'Quelle: Test Feed')`, f.translationID, readerID)
				return err
			},
			wantCode: codeRaiseException,
		},
		{
			name:      "partial withdrawal write",
			invariant: "I-5",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				var articleID string
				if err := tx.QueryRow(ctx,
					`insert into article (translation_id, approved_by, published_at, attribution_block)
					 values ($1, $2, now(), 'Quelle: Test Feed') returning id`,
					f.translationID, f.accountID).Scan(&articleID); err != nil {
					return err
				}
				// Withdrawal is who, when and why together; a timestamp
				// alone is a partial, unrepresentable withdrawal.
				_, err := tx.Exec(ctx,
					`update article set withdrawn_at = now() where id = $1`, articleID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "withdrawal of a never-published article",
			invariant: "I-5",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				var articleID string
				if err := tx.QueryRow(ctx,
					`insert into article (translation_id, approved_by, attribution_block)
					 values ($1, $2, 'Quelle: Test Feed') returning id`,
					f.translationID, f.accountID).Scan(&articleID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`update article set withdrawn_at = now(), withdrawn_by = $2, withdrawal_reason = 'never ran'
					 where id = $1`, articleID, f.accountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "withdrawal timestamped before publication",
			invariant: "I-5",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				var articleID string
				if err := tx.QueryRow(ctx,
					`insert into article (translation_id, approved_by, approved_at, published_at, attribution_block)
					 values ($1, $2, now() - interval '2 hours', now() - interval '1 hour', 'Quelle: Test Feed') returning id`,
					f.translationID, f.accountID).Scan(&articleID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`update article set withdrawn_at = now() - interval '90 minutes', withdrawn_by = $2, withdrawal_reason = 'time travel'
					 where id = $1`, articleID, f.accountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "article born withdrawn",
			invariant: "I-5",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				// Insert-time withdrawal would skip the guarded transition
				// and the active-only one-per-origin indexes.
				_, err := tx.Exec(ctx,
					`insert into article (translation_id, approved_by, published_at, attribution_block, withdrawn_at, withdrawn_by, withdrawal_reason)
					 values ($1, $2, now(), 'Quelle: Test Feed', now(), $2, 'smuggled in')`,
					f.translationID, f.accountID)
				return err
			},
			wantCode: codeRaiseException,
		},
		{
			name:      "withdrawal by a reader-role account",
			invariant: "I-5",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				var articleID, readerID string
				if err := tx.QueryRow(ctx,
					`insert into article (translation_id, approved_by, published_at, attribution_block)
					 values ($1, $2, now(), 'Quelle: Test Feed') returning id`,
					f.translationID, f.accountID).Scan(&articleID); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx,
					`insert into account (email, display_name) values ('reader-withdrawer@example.test', 'A Reader') returning id`).
					Scan(&readerID); err != nil {
					return err
				}
				// Withdrawal is an editorial decision, symmetric with approval.
				_, err := tx.Exec(ctx,
					`update article set withdrawn_at = now(), withdrawn_by = $2, withdrawal_reason = 'not my call'
					 where id = $1`, articleID, readerID)
				return err
			},
			wantCode: codeRaiseException,
		},
		{
			name:      "withdrawal with a blank reason",
			invariant: "I-5",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				var articleID string
				if err := tx.QueryRow(ctx,
					`insert into article (translation_id, approved_by, published_at, attribution_block)
					 values ($1, $2, now(), 'Quelle: Test Feed') returning id`,
					f.translationID, f.accountID).Scan(&articleID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`update article set withdrawn_at = now(), withdrawn_by = $2, withdrawal_reason = '   '
					 where id = $1`, articleID, f.accountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "translation omitting the cost",
			invariant: "cost lineage",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				// cost_microusd has no default: an unrecorded cost is a
				// rejected insert, never a silent zero.
				_, err := tx.Exec(ctx,
					`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract)
					 values ($1, 'de', 'm', 'p', 'headline', 'extract')`, f.sourceItemID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name:      "translation with a negative cost",
			invariant: "cost lineage",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
					 values ($1, 'de', 'm', 'p', 'headline', 'extract', -1)`, f.sourceItemID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "second spend ledger row for the same month",
			invariant: "cost lineage",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures) error {
				if _, err := tx.Exec(ctx,
					`insert into translation_spend (month, spent_microusd) values (date '2026-08-01', 100)`); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into translation_spend (month, spent_microusd) values (date '2026-08-01', 200)`)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name:      "negative monthly spend",
			invariant: "cost lineage",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into translation_spend (month, spent_microusd) values (date '2026-08-01', -1)`)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "spend ledger keyed on a mid-month date",
			invariant: "cost lineage",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures) error {
				// The key is the first day of the month; otherwise two rows
				// could silently describe the same month.
				_, err := tx.Exec(ctx,
					`insert into translation_spend (month, spent_microusd) values (date '2026-08-15', 100)`)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "place with a blank slug",
			invariant: "locale model",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures) error {
				_, err := tx.Exec(ctx,
					`insert into place (name, country, slug) values ('Blankville', 'DE', '   ')`)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name:      "place with a duplicate slug",
			invariant: "locale model",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures) error {
				// Collides with the seeded Munich row: slugs are addresses
				// and addresses are unique.
				_, err := tx.Exec(ctx,
					`insert into place (name, country, slug) values ('Munich Clone', 'DE', 'munich')`)
				return err
			},
			wantCode: codeUniqueViolation,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seed(t, tx)
			err := tt.write(context.Background(), tx, f)
			wantPgCode(t, err, tt.wantCode)
		})
	}
}

// TestProvenanceSnapshotsAreAuthoritative asserts that the database, not
// the caller, writes the retrieval-time legal basis (I-2, I-4): whatever a
// caller supplies for the snapshot columns is replaced with the source
// row's actual terms in the same transaction.
func TestProvenanceSnapshotsAreAuthoritative(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seed(t, tx)

	var licence, usageRule, hash string
	var evidence *string
	err := tx.QueryRow(ctx,
		`insert into source_item (source_id, source_url, raw_body, licence_snapshot, usage_rule_snapshot, permission_evidence_snapshot)
		 values ($1, 'https://example.test/forged', $2, 'forged friendly terms', 'full_text', 'forged evidence')
		 returning licence_snapshot, usage_rule_snapshot, permission_evidence_snapshot, content_hash`,
		f.sourceID, "forgery attempt "+f.rawBody).
		Scan(&licence, &usageRule, &evidence, &hash)
	if err != nil {
		t.Fatalf("insert with forged snapshots: %v", err)
	}
	if licence != f.licence {
		t.Errorf("licence_snapshot = %q, want the source's real terms %q (I-4)", licence, f.licence)
	}
	if usageRule != "extract_and_link" {
		t.Errorf("usage_rule_snapshot = %q, want the source's real rule extract_and_link", usageRule)
	}
	if evidence != nil {
		t.Errorf("permission_evidence_snapshot = %v, want nil (source has none on record)", *evidence)
	}
	if hash != sha256Hex("forgery attempt "+f.rawBody) {
		t.Error("content_hash does not fingerprint the stored body")
	}
}

// TestArticleFrozenAfterApproval asserts that a published article's
// provenance can never be silently reassigned: identity, origin, approval
// and attribution are frozen, and publication is a one-way transition.
func TestArticleFrozenAfterApproval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		write func(ctx context.Context, tx pgx.Tx, f fixtures, articleID, otherAccountID string) error
	}{
		{
			name: "reassign translation origin",
			write: func(ctx context.Context, tx pgx.Tx, f fixtures, articleID, _ string) error {
				_, err := tx.Exec(ctx,
					`update article set translation_id = null, source_item_id = $2 where id = $1`,
					articleID, f.sourceItemID)
				return err
			},
		},
		{
			name: "reassign approver",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures, articleID, otherAccountID string) error {
				_, err := tx.Exec(ctx,
					`update article set approved_by = $2 where id = $1`, articleID, otherAccountID)
				return err
			},
		},
		{
			name: "rewrite approval time",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures, articleID, _ string) error {
				_, err := tx.Exec(ctx,
					`update article set approved_at = now() - interval '1 year' where id = $1`, articleID)
				return err
			},
		},
		{
			name: "rewrite attribution",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures, articleID, _ string) error {
				_, err := tx.Exec(ctx,
					`update article set attribution_block = 'someone else entirely' where id = $1`, articleID)
				return err
			},
		},
		{
			name: "unpublish by clearing published_at",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures, articleID, _ string) error {
				_, err := tx.Exec(ctx,
					`update article set published_at = null where id = $1`, articleID)
				return err
			},
		},
		{
			name: "shift publication time",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures, articleID, _ string) error {
				_, err := tx.Exec(ctx,
					`update article set published_at = published_at + interval '1 day' where id = $1`, articleID)
				return err
			},
		},
		{
			name: "delete the approval record",
			write: func(ctx context.Context, tx pgx.Tx, _ fixtures, articleID, _ string) error {
				_, err := tx.Exec(ctx, `delete from article where id = $1`, articleID)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			ctx := context.Background()
			f := seed(t, tx)

			var articleID, otherAccountID string
			err := tx.QueryRow(ctx,
				`insert into article (translation_id, approved_by, published_at, attribution_block)
				 values ($1, $2, now(), 'Quelle: Test Feed') returning id`,
				f.translationID, f.accountID).Scan(&articleID)
			if err != nil {
				t.Fatalf("insert article: %v", err)
			}
			err = tx.QueryRow(ctx,
				`insert into account (email, display_name) values ($1, 'Another Editor') returning id`,
				"other-"+randomSuffix(t)+"@example.test").Scan(&otherAccountID)
			if err != nil {
				t.Fatalf("insert second account: %v", err)
			}

			err = tt.write(ctx, tx, f, articleID, otherAccountID)
			wantPgCode(t, err, codeRaiseException)
		})
	}
}

// TestArticlePublishTransition asserts the one legal article update: an
// approved-but-unpublished article may be published exactly once.
func TestArticlePublishTransition(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seed(t, tx)

	var articleID string
	err := tx.QueryRow(ctx,
		`insert into article (translation_id, approved_by, attribution_block)
		 values ($1, $2, 'Quelle: Test Feed') returning id`,
		f.translationID, f.accountID).Scan(&articleID)
	if err != nil {
		t.Fatalf("insert unpublished article: %v", err)
	}

	if _, err := tx.Exec(ctx,
		`update article set published_at = now() where id = $1`, articleID); err != nil {
		t.Fatalf("publishing an approved article must be allowed: %v", err)
	}
	_, err = tx.Exec(ctx,
		`update article set published_at = now() + interval '1 hour' where id = $1`, articleID)
	wantPgCode(t, err, codeRaiseException)
}

// TestArticleWithdrawalTransition asserts the second legal article update
// (0002): a published article may be withdrawn - who, when and why, all at
// once - exactly once. The record then stays frozen: no edits, no
// un-withdrawal, and the provenance view carries the full history (I-5).
func TestArticleWithdrawalTransition(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seed(t, tx)

	var articleID string
	err := tx.QueryRow(ctx,
		`insert into article (translation_id, approved_by, published_at, attribution_block)
		 values ($1, $2, now(), 'Quelle: Test Feed') returning id`,
		f.translationID, f.accountID).Scan(&articleID)
	if err != nil {
		t.Fatalf("insert published article: %v", err)
	}

	if _, err := tx.Exec(ctx,
		`update article set withdrawn_at = now(), withdrawn_by = $2, withdrawal_reason = 'source retraction'
		 where id = $1`, articleID, f.accountID); err != nil {
		t.Fatalf("withdrawing a published article must be allowed: %v", err)
	}

	// The audit event is written by trigger in the same transaction as
	// the withdrawal itself, never left to application discipline.
	var eventCount int
	var evBy, evReason string
	err = tx.QueryRow(ctx,
		`select count(*), min(payload->>'withdrawn_by'), min(payload->>'reason')
		 from domain_event
		 where type = 'article.withdrawn' and payload->>'article_id' = $1`, articleID).
		Scan(&eventCount, &evBy, &evReason)
	if err != nil {
		t.Fatalf("querying article.withdrawn domain event: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("article.withdrawn domain events = %d, want exactly 1 in the withdrawing transaction", eventCount)
	}
	if evBy != f.accountID || evReason != "source retraction" {
		t.Errorf("article.withdrawn payload = by %q reason %q, want by %q reason %q",
			evBy, evReason, f.accountID, "source retraction")
	}

	var otherEditorID string
	if err := tx.QueryRow(ctx,
		`insert into account (email, display_name, role) values ($1, 'Another Editor', 'editor') returning id`,
		"other-"+randomSuffix(t)+"@example.test").Scan(&otherEditorID); err != nil {
		t.Fatalf("insert second editor: %v", err)
	}

	// The record is terminal: every further touch of the withdrawal
	// columns is refused by the guard. Each attempt runs in a savepoint
	// (nested transaction) so the raised exception does not abort the
	// enclosing test transaction.
	frozen := []struct {
		name string
		stmt string
		args []any
	}{
		{name: "second withdrawal", stmt: `update article set withdrawn_at = now() + interval '1 hour' where id = $1`, args: []any{articleID}},
		{name: "rewrite the reason", stmt: `update article set withdrawal_reason = 'a friendlier story' where id = $1`, args: []any{articleID}},
		{name: "reassign who withdrew", stmt: `update article set withdrawn_by = $2 where id = $1`, args: []any{articleID, otherEditorID}},
		{name: "un-withdraw", stmt: `update article set withdrawn_at = null, withdrawn_by = null, withdrawal_reason = null where id = $1`, args: []any{articleID}},
	}
	for _, tt := range frozen {
		nested, err := tx.Begin(ctx)
		if err != nil {
			t.Fatalf("%s: begin savepoint: %v", tt.name, err)
		}
		_, execErr := nested.Exec(ctx, tt.stmt, tt.args...)
		if err := nested.Rollback(ctx); err != nil {
			t.Fatalf("%s: rollback savepoint: %v", tt.name, err)
		}
		if execErr == nil {
			t.Fatalf("%s: want rejection, got success", tt.name)
		}
		wantPgCode(t, execErr, codeRaiseException)
	}

	// Audit reads the full history from the provenance view in one query.
	var withdrawnBy, reason string
	var withdrawnAt *string
	err = tx.QueryRow(ctx,
		`select withdrawn_at::text, withdrawn_by::text, withdrawal_reason
		 from article_provenance where article_id = $1`, articleID).
		Scan(&withdrawnAt, &withdrawnBy, &reason)
	if err != nil {
		t.Fatalf("querying withdrawal from article_provenance: %v", err)
	}
	if withdrawnAt == nil || withdrawnBy != f.accountID || reason != "source retraction" {
		t.Errorf("provenance view lost the withdrawal record: at=%v by=%q reason=%q (I-5)",
			withdrawnAt, withdrawnBy, reason)
	}
}

// TestWithdrawnOriginCanBeReapproved asserts the correction flow: a
// withdrawn article frees its origin for a fresh approval, while a second
// ACTIVE article from the same origin remains impossible.
func TestWithdrawnOriginCanBeReapproved(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		origin string // column name; the fixture supplies the value
	}{
		{name: "translation origin", origin: "translation_id"},
		{name: "source item origin", origin: "source_item_id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			ctx := context.Background()
			f := seed(t, tx)

			originID := f.translationID
			if tt.origin == "source_item_id" {
				originID = f.sourceItemID
			}
			insert := `insert into article (` + tt.origin + `, approved_by, published_at, attribution_block)
			 values ($1, $2, now(), 'Quelle: Test Feed') returning id`

			var firstID string
			if err := tx.QueryRow(ctx, insert, originID, f.accountID).Scan(&firstID); err != nil {
				t.Fatalf("insert first article: %v", err)
			}
			if _, err := tx.Exec(ctx,
				`update article set withdrawn_at = now(), withdrawn_by = $2, withdrawal_reason = 'correction'
				 where id = $1`, firstID, f.accountID); err != nil {
				t.Fatalf("withdraw first article: %v", err)
			}

			// The origin is free again: the corrected article may be approved.
			var secondID string
			if err := tx.QueryRow(ctx, insert, originID, f.accountID).Scan(&secondID); err != nil {
				t.Fatalf("re-approval after withdrawal must be allowed: %v", err)
			}

			// But never two ACTIVE articles from one origin.
			var thirdID string
			err := tx.QueryRow(ctx, insert, originID, f.accountID).Scan(&thirdID)
			wantPgCode(t, err, codeUniqueViolation)
		})
	}
}

// TestEditorRoleFrozenWhileReferenced asserts that I-1 stays provable over
// history: an editor whose approvals or withdrawals are on record cannot be
// demoted, so every recorded editorial decision remains attributable to a
// verifiable editor. An unreferenced editor can still be demoted.
func TestEditorRoleFrozenWhileReferenced(t *testing.T) {
	t.Parallel()

	t.Run("demote an approver on record", func(t *testing.T) {
		t.Parallel()
		tx := beginTx(t)
		ctx := context.Background()
		f := seed(t, tx)

		if _, err := tx.Exec(ctx,
			`insert into article (translation_id, approved_by, attribution_block)
			 values ($1, $2, 'Quelle: Test Feed')`, f.translationID, f.accountID); err != nil {
			t.Fatalf("insert article: %v", err)
		}
		_, err := tx.Exec(ctx,
			`update account set role = 'reader' where id = $1`, f.accountID)
		wantPgCode(t, err, codeRaiseException)
	})

	t.Run("demote a withdrawer on record", func(t *testing.T) {
		t.Parallel()
		tx := beginTx(t)
		ctx := context.Background()
		f := seed(t, tx)

		var withdrawerID, articleID string
		if err := tx.QueryRow(ctx,
			`insert into account (email, display_name, role) values ($1, 'Withdrawing Editor', 'editor') returning id`,
			"withdrawer-"+randomSuffix(t)+"@example.test").Scan(&withdrawerID); err != nil {
			t.Fatalf("insert withdrawing editor: %v", err)
		}
		if err := tx.QueryRow(ctx,
			`insert into article (translation_id, approved_by, published_at, attribution_block)
			 values ($1, $2, now(), 'Quelle: Test Feed') returning id`,
			f.translationID, f.accountID).Scan(&articleID); err != nil {
			t.Fatalf("insert article: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`update article set withdrawn_at = now(), withdrawn_by = $2, withdrawal_reason = 'errata'
			 where id = $1`, articleID, withdrawerID); err != nil {
			t.Fatalf("withdraw article: %v", err)
		}
		_, err := tx.Exec(ctx,
			`update account set role = 'reader' where id = $1`, withdrawerID)
		wantPgCode(t, err, codeRaiseException)
	})

	t.Run("unreferenced editor can be demoted", func(t *testing.T) {
		t.Parallel()
		tx := beginTx(t)
		ctx := context.Background()

		var editorID, role string
		if err := tx.QueryRow(ctx,
			`insert into account (email, display_name, role) values ($1, 'Idle Editor', 'editor') returning id`,
			"idle-"+randomSuffix(t)+"@example.test").Scan(&editorID); err != nil {
			t.Fatalf("insert editor: %v", err)
		}
		if err := tx.QueryRow(ctx,
			`update account set role = 'reader' where id = $1 returning role`, editorID).Scan(&role); err != nil {
			t.Fatalf("demoting an unreferenced editor must be allowed: %v", err)
		}
		if role != "reader" {
			t.Fatalf("role after demotion = %q, want reader", role)
		}
	})
}

// TestEditorDemotionRaceIsSerialized asserts the concurrency story behind
// the role checks: the article triggers read the actor's role with a row
// lock (FOR SHARE), so an in-flight approval and a concurrent demotion of
// the same editor serialize instead of racing - whichever transaction
// commits second sees the other's write and raises. Without the locking
// read, both sides could pass their own trigger against the prior state
// and both commit, recording a reader as the editorial actor.
//
// The race needs two real sessions and real commits; row locks are
// invisible inside a single rolled-back transaction. The committed rows
// stay in the test database on purpose: source_item and article are
// immutable by design, and random suffixes keep runs independent.
func TestEditorDemotionRaceIsSerialized(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set DATABASE_URL to exercise schema invariants")
	}
	ctx := context.Background()

	connA, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect session A: %v", err)
	}
	defer func() { _ = connA.Close(ctx) }()
	connB, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect session B: %v", err)
	}
	defer func() { _ = connB.Close(ctx) }()

	// seedCommitted writes (and commits) an editor and a retrievable origin.
	seedCommitted := func(t *testing.T) (editorID, sourceItemID string) {
		t.Helper()
		suffix := randomSuffix(t)
		if err := connA.QueryRow(ctx,
			`insert into account (email, display_name, role) values ($1, 'Race Editor', 'editor') returning id`,
			"race-"+suffix+"@example.test").Scan(&editorID); err != nil {
			t.Fatalf("seed editor: %v", err)
		}
		var sourceID string
		if err := connA.QueryRow(ctx,
			`insert into source (name, url, language_code, jurisdiction, licence_terms)
			 values ($1, $2, 'el', 'GR', 'terms') returning id`,
			"Race Feed "+suffix, "https://example.test/race/"+suffix).Scan(&sourceID); err != nil {
			t.Fatalf("seed source: %v", err)
		}
		if err := connA.QueryRow(ctx,
			`insert into source_item (source_id, source_url, raw_body)
			 values ($1, $2, $3) returning id`,
			sourceID, "https://example.test/race/"+suffix+"/a", "race body "+suffix).Scan(&sourceItemID); err != nil {
			t.Fatalf("seed source_item: %v", err)
		}
		return editorID, sourceItemID
	}

	t.Run("demotion waits for an in-flight approval, then raises", func(t *testing.T) {
		editorID, sourceItemID := seedCommitted(t)

		txA, err := connA.Begin(ctx)
		if err != nil {
			t.Fatalf("begin approval tx: %v", err)
		}
		defer func() { _ = txA.Rollback(ctx) }()
		// The insert trigger takes FOR SHARE on the editor's account row.
		var articleID string
		if err := txA.QueryRow(ctx,
			`insert into article (source_item_id, approved_by, attribution_block)
			 values ($1, $2, 'Quelle: Race Feed') returning id`, sourceItemID, editorID).Scan(&articleID); err != nil {
			t.Fatalf("approval insert: %v", err)
		}
		// This transaction commits for real, so the 0006 rule applies: the
		// article must carry a place row by COMMIT or the commit raises.
		if _, err := txA.Exec(ctx,
			`insert into article_place (article_id, place_id)
			 select $1, id from place where slug = 'munich'`, articleID); err != nil {
			t.Fatalf("tagging the approval's place: %v", err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := connB.Exec(ctx,
				`update account set role = 'reader' where id = $1`, editorID)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("demotion finished during the uncommitted approval (err=%v): the role read did not serialize", err)
		case <-time.After(300 * time.Millisecond):
			// Blocked on the share lock, as required.
		}
		if err := txA.Commit(ctx); err != nil {
			t.Fatalf("commit approval: %v", err)
		}
		select {
		case err := <-done:
			// Resuming after the commit, account_role_guard re-checks
			// against a fresh snapshot, sees the approval, and raises.
			wantPgCode(t, err, codeRaiseException)
		case <-time.After(10 * time.Second):
			t.Fatal("demotion still blocked after the approval committed")
		}
	})

	t.Run("approval waits for an in-flight demotion, then raises", func(t *testing.T) {
		editorID, sourceItemID := seedCommitted(t)

		txB, err := connB.Begin(ctx)
		if err != nil {
			t.Fatalf("begin demotion tx: %v", err)
		}
		defer func() { _ = txB.Rollback(ctx) }()
		// No approvals on record yet: the demotion passes its guard and
		// holds the account row lock uncommitted.
		if _, err := txB.Exec(ctx,
			`update account set role = 'reader' where id = $1`, editorID); err != nil {
			t.Fatalf("demotion update: %v", err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := connA.Exec(ctx,
				`insert into article (source_item_id, approved_by, attribution_block)
				 values ($1, $2, 'Quelle: Race Feed')`, sourceItemID, editorID)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("approval finished during the uncommitted demotion (err=%v): the role read did not serialize", err)
		case <-time.After(300 * time.Millisecond):
			// Blocked on the demotion's row lock, as required.
		}
		if err := txB.Commit(ctx); err != nil {
			t.Fatalf("commit demotion: %v", err)
		}
		select {
		case err := <-done:
			// The FOR SHARE read resumes on the latest committed row,
			// sees role = reader, and the insert trigger raises.
			wantPgCode(t, err, codeRaiseException)
		case <-time.After(10 * time.Second):
			t.Fatal("approval still blocked after the demotion committed")
		}
	})
}

// TestAnArticleWithNoPlaceIsRejectedAtCommit asserts the 0006 rule: the
// front page is scoped by place, so an article with no article_place row
// can appear on none of them - and the database refuses to let such an
// article exist. The trigger is DEFERRED, so the raise happens at COMMIT,
// not at the insert: the assertion is on tx.Commit, and the savepoint
// pattern the other guard tests use does not apply - releasing a savepoint
// runs no deferred checks. A failed commit leaves nothing behind.
func TestAnArticleWithNoPlaceIsRejectedAtCommit(t *testing.T) {
	t.Parallel()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set DATABASE_URL to exercise schema invariants")
	}
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	f := seed(t, tx)

	// The insert itself succeeds: the trigger's whole point is to let the
	// article and its place rows arrive in either order within the
	// transaction, so nothing can be judged until COMMIT.
	if _, err := tx.Exec(ctx,
		`insert into article (translation_id, approved_by, published_at, attribution_block)
		 values ($1, $2, now(), 'Quelle: Test Feed')`, f.translationID, f.accountID); err != nil {
		t.Fatalf("placeless insert must succeed until commit: %v", err)
	}
	wantPgCode(t, tx.Commit(ctx), codeRaiseException)
}

// TestTranslationZeroCostIsAccepted is the positive control for the cost
// rule: an explicit zero (provider genuinely charged nothing) is legal;
// only an OMITTED cost is rejected.
func TestTranslationZeroCostIsAccepted(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seed(t, tx)

	var cost int64
	err := tx.QueryRow(ctx,
		`insert into translation (source_item_id, target_locale, model, prompt_version, headline, extract, cost_microusd)
		 values ($1, 'el', 'test-model-1', 'prompt-v1', 'Δωρεάν', 'Απόσπασμα', 0) returning cost_microusd`,
		f.sourceItemID).Scan(&cost)
	if err != nil {
		t.Fatalf("translation with explicit zero cost rejected: %v", err)
	}
	if cost != 0 {
		t.Fatalf("cost_microusd = %d, want 0", cost)
	}
}

// TestPlaceSeeds asserts the alpha reference places shipped by 0002: the
// Munich -> Bavaria -> Germany hierarchy and the national Greece row, each
// addressable by slug.
func TestPlaceSeeds(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()

	var munich, bavaria, germany struct {
		name    string
		country string
	}
	var germanyParent *string
	err := tx.QueryRow(ctx,
		`select m.name, m.country, b.name, b.country, g.name, g.country, g.parent_id::text
		 from place m
		 join place b on b.id = m.parent_id
		 join place g on g.id = b.parent_id
		 where m.slug = 'munich' and b.slug = 'bavaria' and g.slug = 'germany'`).
		Scan(&munich.name, &munich.country, &bavaria.name, &bavaria.country,
			&germany.name, &germany.country, &germanyParent)
	if err != nil {
		t.Fatalf("seeded Munich -> Bavaria -> Germany chain missing: %v", err)
	}
	if munich.name != "Munich" || munich.country != "DE" {
		t.Errorf("munich seed = %q/%q, want Munich/DE", munich.name, munich.country)
	}
	if bavaria.name != "Bavaria" || bavaria.country != "DE" {
		t.Errorf("bavaria seed = %q/%q, want Bavaria/DE", bavaria.name, bavaria.country)
	}
	if germany.name != "Germany" || germany.country != "DE" || germanyParent != nil {
		t.Errorf("germany seed = %q/%q parent=%v, want Germany/DE at the top of the chain",
			germany.name, germany.country, germanyParent)
	}

	var greece struct {
		name    string
		country string
	}
	var greeceParent *string
	err = tx.QueryRow(ctx,
		`select name, country, parent_id::text from place where slug = 'greece'`).
		Scan(&greece.name, &greece.country, &greeceParent)
	if err != nil {
		t.Fatalf("seeded Greece missing: %v", err)
	}
	if greece.name != "Greece" || greece.country != "GR" || greeceParent != nil {
		t.Errorf("greece seed = %q/%q parent=%v, want national Greece/GR",
			greece.name, greece.country, greeceParent)
	}
}

// TestConsentHistoryIsProtected asserts consent rows can be closed but
// never erased or rewritten: identity and grant are frozen, revocation is
// one-way, deletion is impossible.
func TestConsentHistoryIsProtected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		revoked  bool
		otherArg bool
		stmt     string
	}{
		{name: "delete a consent row", stmt: `delete from consent where id = $1`},
		{name: "rewrite the purpose", stmt: `update consent set purpose = 'something-else' where id = $1`},
		{name: "rewrite the grant time", stmt: `update consent set granted_at = now() - interval '1 year' where id = $1`},
		{name: "move it to another account", otherArg: true, stmt: `update consent set account_id = $2 where id = $1`},
		{name: "un-revoke", revoked: true, stmt: `update consent set revoked_at = null where id = $1`},
		{name: "shift the revocation time", revoked: true, stmt: `update consent set revoked_at = revoked_at + interval '1 day' where id = $1`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			ctx := context.Background()
			f := seed(t, tx)

			var consentID string
			err := tx.QueryRow(ctx,
				`insert into consent (account_id, purpose) values ($1, 'analytics') returning id`,
				f.accountID).Scan(&consentID)
			if err != nil {
				t.Fatalf("grant consent: %v", err)
			}
			if tt.revoked {
				if _, err := tx.Exec(ctx,
					`update consent set revoked_at = now() where id = $1`, consentID); err != nil {
					t.Fatalf("revoke consent: %v", err)
				}
			}
			args := []any{consentID}
			if tt.otherArg {
				var otherAccountID string
				err := tx.QueryRow(ctx,
					`insert into account (email, display_name) values ($1, 'Another Person') returning id`,
					"other-"+randomSuffix(t)+"@example.test").Scan(&otherAccountID)
				if err != nil {
					t.Fatalf("insert second account: %v", err)
				}
				args = append(args, otherAccountID)
			}
			_, err = tx.Exec(ctx, tt.stmt, args...)
			wantPgCode(t, err, codeRaiseException)
		})
	}
}

// TestSourceItemIsImmutable asserts I-3: retrieved content is legal evidence
// and can never be altered or removed, no matter how the write is phrased.
func TestSourceItemIsImmutable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		stmt string
	}{
		{name: "update raw body", stmt: `update source_item set raw_body = 'tampered' where id = $1`},
		{name: "update licence snapshot", stmt: `update source_item set licence_snapshot = 'friendlier terms' where id = $1`},
		{name: "update retrieved timestamp", stmt: `update source_item set retrieved_at = now() where id = $1`},
		{name: "delete row", stmt: `delete from source_item where id = $1`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seed(t, tx)
			_, err := tx.Exec(context.Background(), tt.stmt, f.sourceItemID)
			wantPgCode(t, err, codeRaiseException)
		})
	}
}

// TestTranslationIsImmutable asserts the middle link of the provenance chain
// cannot be rewritten: a mutable translation would silently falsify which
// model, prompt and retrieved item an approved article was built on (I-5).
func TestTranslationIsImmutable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		stmt string
	}{
		{name: "update model", stmt: `update translation set model = 'other-model' where id = $1`},
		{name: "update prompt version", stmt: `update translation set prompt_version = 'prompt-v999' where id = $1`},
		{name: "update headline", stmt: `update translation set headline = 'rewritten' where id = $1`},
		{name: "update extract", stmt: `update translation set extract = 'rewritten' where id = $1`},
		{name: "delete row", stmt: `delete from translation where id = $1`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seed(t, tx)
			_, err := tx.Exec(context.Background(), tt.stmt, f.translationID)
			wantPgCode(t, err, codeRaiseException)
		})
	}
}

// TestImmutableTablesRejectTruncate closes the bulk route: row-level triggers
// do not fire on TRUNCATE, so the statement-level triggers must. Deliberately
// not parallel - TRUNCATE takes ACCESS EXCLUSIVE locks that would contend
// with the parallel subtests' open transactions.
func TestImmutableTablesRejectTruncate(t *testing.T) {
	tests := []struct {
		name string
		// source_item and translation are referenced by foreign keys, so a
		// plain TRUNCATE would fail on the FK (SQLSTATE 0A000) before the
		// trigger fires; CASCADE expands the closure so the immutability
		// trigger itself is what raises.
		stmt string
	}{
		{name: "source_item", stmt: `truncate source_item cascade`},
		{name: "translation", stmt: `truncate translation cascade`},
		{name: "article", stmt: `truncate article cascade`},
		{name: "consent", stmt: `truncate consent`},
		{name: "domain_event", stmt: `truncate domain_event`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := beginTx(t)
			ctx := context.Background()
			if _, err := tx.Exec(ctx, `set local lock_timeout = '10s'`); err != nil {
				t.Fatalf("set lock_timeout: %v", err)
			}
			_, err := tx.Exec(ctx, tt.stmt)
			wantPgCode(t, err, codeRaiseException)
		})
	}
}

// TestDomainEventIsAppendOnly asserts the audit stream cannot be rewritten.
func TestDomainEventIsAppendOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		stmt string
	}{
		{name: "update event", stmt: `update domain_event set payload = '{}'::jsonb where id = $1`},
		{name: "delete event", stmt: `delete from domain_event where id = $1`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			ctx := context.Background()
			var eventID string
			err := tx.QueryRow(ctx,
				`insert into domain_event (type, payload)
				 values ('test.event', '{"k":"v"}'::jsonb) returning id`).Scan(&eventID)
			if err != nil {
				t.Fatalf("seed domain_event: %v", err)
			}
			_, err = tx.Exec(ctx, tt.stmt, eventID)
			wantPgCode(t, err, codeRaiseException)
		})
	}
}

// TestValidApprovedArticleIsAccepted is the positive control: the schema
// must accept the legal path, or the invariant tests above prove nothing.
func TestValidApprovedArticleIsAccepted(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seed(t, tx)

	var articleID string
	err := tx.QueryRow(ctx,
		`insert into article (translation_id, approved_by, published_at, attribution_block)
		 values ($1, $2, now(), 'Quelle: Test Feed - https://example.test') returning id`,
		f.translationID, f.accountID).Scan(&articleID)
	if err != nil {
		t.Fatalf("valid article rejected: %v", err)
	}

	// An untranslated article straight from a retrieved item is also legal.
	err = tx.QueryRow(ctx,
		`insert into article (source_item_id, approved_by, attribution_block)
		 values ($1, $2, 'Πηγή: Test Feed') returning id`,
		f.sourceItemID, f.accountID).Scan(&articleID)
	if err != nil {
		t.Fatalf("valid untranslated article rejected: %v", err)
	}
}

// TestArticleProvenanceView asserts I-5: source, licence snapshot, model,
// prompt version and named approver for any article, in one query.
func TestArticleProvenanceView(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seed(t, tx)

	var articleID string
	err := tx.QueryRow(ctx,
		`insert into article (translation_id, approved_by, published_at, attribution_block)
		 values ($1, $2, now(), 'Quelle: Test Feed') returning id`,
		f.translationID, f.accountID).Scan(&articleID)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}

	var (
		approverName, approverEmail         string
		model, promptVersion, targetLocale  string
		sourceURL, contentHash, licenceSnap string
		sourceName, feedURL, jurisdiction   string
		usageRule                           string
	)
	err = tx.QueryRow(ctx,
		`select approver_name, approver_email, model, prompt_version, target_locale,
		        source_url, content_hash, licence_snapshot, source_name, source_feed_url,
		        jurisdiction, usage_rule
		 from article_provenance where article_id = $1`, articleID).
		Scan(&approverName, &approverEmail, &model, &promptVersion, &targetLocale,
			&sourceURL, &contentHash, &licenceSnap, &sourceName, &feedURL,
			&jurisdiction, &usageRule)
	if err != nil {
		t.Fatalf("querying article_provenance: %v", err)
	}

	if approverName == "" || approverEmail == "" {
		t.Error("provenance lacks the named human approver (I-1, I-5)")
	}
	if model != "test-model-1" || promptVersion != "prompt-v1" || targetLocale != "de" {
		t.Errorf("provenance lacks translation lineage: model=%q prompt=%q locale=%q", model, promptVersion, targetLocale)
	}
	if licenceSnap != f.licence {
		t.Errorf("licence snapshot = %q, want the terms at retrieval %q (I-4)", licenceSnap, f.licence)
	}
	if contentHash != sha256Hex(f.rawBody) {
		t.Error("provenance content hash does not fingerprint the retrieved body")
	}
	if sourceURL == "" || sourceName == "" || feedURL == "" || jurisdiction == "" {
		t.Error("provenance lacks source identification (I-5)")
	}
	if usageRule != "extract_and_link" {
		t.Errorf("usage_rule = %q, want the default extract_and_link", usageRule)
	}

	// The untranslated shape: an article straight from the retrieved item
	// must surface in the view with source provenance intact and no
	// translation lineage.
	var article2ID string
	err = tx.QueryRow(ctx,
		`insert into article (source_item_id, approved_by, attribution_block)
		 values ($1, $2, 'Πηγή: Test Feed') returning id`,
		f.sourceItemID, f.accountID).Scan(&article2ID)
	if err != nil {
		t.Fatalf("insert untranslated article: %v", err)
	}

	var (
		model2, prompt2, locale2 *string
		licence2, hash2, name2   string
	)
	err = tx.QueryRow(ctx,
		`select model, prompt_version, target_locale, licence_snapshot, content_hash, approver_name
		 from article_provenance where article_id = $1`, article2ID).
		Scan(&model2, &prompt2, &locale2, &licence2, &hash2, &name2)
	if err != nil {
		t.Fatalf("querying article_provenance for untranslated article: %v", err)
	}
	if model2 != nil || prompt2 != nil || locale2 != nil {
		t.Error("untranslated article must carry no translation lineage in provenance")
	}
	if licence2 != f.licence || hash2 != sha256Hex(f.rawBody) || name2 == "" {
		t.Error("untranslated article lost source provenance or approver in the view (I-5)")
	}
}

// TestSourceDefaultsToExtractAndLink asserts §9.4: a new source is never
// permissive by default - and it is born active (0002), so pausing is an
// explicit operator decision, never an accidental initial state.
func TestSourceDefaultsToExtractAndLink(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	suffix := randomSuffix(t)

	var usageRule string
	var active bool
	err := tx.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'de', 'DE', 'terms') returning usage_rule, active`,
		"Default Feed "+suffix, "https://example.test/default/"+suffix).Scan(&usageRule, &active)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	if usageRule != "extract_and_link" {
		t.Fatalf("default usage_rule = %q, want extract_and_link", usageRule)
	}
	if !active {
		t.Fatal("default source.active = false, want true (a new source polls until explicitly paused)")
	}
}

// TestIsEntitled asserts the single entitlement gate answers for existing
// accounts, refuses unknown ones, and (0002) grants editorial.* actions to
// editors only.
func TestIsEntitled(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seed(t, tx) // f.accountID holds the editor role

	var readerID string
	err := tx.QueryRow(ctx,
		`insert into account (email, display_name) values ($1, 'A Reader') returning id`,
		"reader-"+randomSuffix(t)+"@example.test").Scan(&readerID)
	if err != nil {
		t.Fatalf("insert reader account: %v", err)
	}

	tests := []struct {
		name  string
		query string
		args  []any
		want  bool
	}{
		{name: "existing account is entitled", query: `select is_entitled($1, 'read')`, args: []any{f.accountID}, want: true},
		{name: "unknown account is not", query: `select is_entitled(gen_random_uuid(), 'read')`, want: false},
		{name: "null action is not an entitlement", query: `select is_entitled($1, null)`, args: []any{f.accountID}, want: false},
		{name: "editor may take editorial actions", query: `select is_entitled($1, 'editorial.approve')`, args: []any{f.accountID}, want: true},
		{name: "reader may not take editorial actions", query: `select is_entitled($1, 'editorial.approve')`, args: []any{readerID}, want: false},
		{name: "reader keeps non-editorial entitlements", query: `select is_entitled($1, 'read')`, args: []any{readerID}, want: true},
		{name: "unknown account gets no editorial entitlement", query: `select is_entitled(gen_random_uuid(), 'editorial.approve')`, want: false},
	}

	for _, tt := range tests {
		var got bool
		if err := tx.QueryRow(ctx, tt.query, tt.args...).Scan(&got); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("%s: is_entitled = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestConsentRevokeAndRegrant asserts the per-purpose consent model keeps
// history: revocation closes a row, a fresh grant opens a new one.
func TestConsentRevokeAndRegrant(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seed(t, tx)

	if _, err := tx.Exec(ctx,
		`insert into consent (account_id, purpose) values ($1, 'analytics')`, f.accountID); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`update consent set revoked_at = now() where account_id = $1 and purpose = 'analytics' and revoked_at is null`,
		f.accountID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`insert into consent (account_id, purpose) values ($1, 'analytics')`, f.accountID); err != nil {
		t.Fatalf("re-grant after revoke rejected: %v", err)
	}

	var rows int
	if err := tx.QueryRow(ctx,
		`select count(*) from consent where account_id = $1 and purpose = 'analytics'`,
		f.accountID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 2 {
		t.Fatalf("consent history rows = %d, want 2 (history preserved)", rows)
	}
}
