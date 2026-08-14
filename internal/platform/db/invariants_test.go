package db_test

// These tests assert that the DATABASE enforces the licensing invariants.
// Application code is not trusted with legal guarantees; every test here
// attempts an illegal state and requires Postgres itself to reject it.
//
//	I-1  an article cannot exist without a named human approver
//	I-2  provenance is captured at retrieval, with the content
//	I-3  source_item and domain_event are immutable / append only
//	I-4  licence terms are snapshotted at retrieval
//	I-5  full provenance of any article is one query away
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

	err := tx.QueryRow(ctx,
		`insert into account (email, display_name) values ($1, $2) returning id`,
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

	err = tx.QueryRow(ctx,
		`insert into translation (source_item_id, target_locale, model, prompt_version, body)
		 values ($1, 'de', 'test-model-1', 'prompt-v1', $2) returning id`,
		f.sourceItemID, "Testinhalt "+suffix,
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
		{name: "update body", stmt: `update translation set body = 'rewritten' where id = $1`},
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
// permissive by default.
func TestSourceDefaultsToExtractAndLink(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	suffix := randomSuffix(t)

	var usageRule string
	err := tx.QueryRow(ctx,
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'de', 'DE', 'terms') returning usage_rule`,
		"Default Feed "+suffix, "https://example.test/default/"+suffix).Scan(&usageRule)
	if err != nil {
		t.Fatalf("insert source: %v", err)
	}
	if usageRule != "extract_and_link" {
		t.Fatalf("default usage_rule = %q, want extract_and_link", usageRule)
	}
}

// TestIsEntitled asserts the single entitlement gate answers for existing
// accounts and refuses unknown ones.
func TestIsEntitled(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seed(t, tx)

	tests := []struct {
		name  string
		query string
		args  []any
		want  bool
	}{
		{name: "existing account is entitled", query: `select is_entitled($1, 'read')`, args: []any{f.accountID}, want: true},
		{name: "unknown account is not", query: `select is_entitled(gen_random_uuid(), 'read')`, want: false},
		{name: "null action is not an entitlement", query: `select is_entitled($1, null)`, args: []any{f.accountID}, want: false},
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
