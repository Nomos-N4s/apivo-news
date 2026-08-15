package ingestion_test

// Integration tests for the provenance write path, against the real,
// migrated schema. The invariants under test are I-2 and I-4: item and
// event exist together or not at all, the database computes the content
// fingerprint, and the licence terms are snapshotted by trigger in the
// same transaction. Rows written here are immutable by design (I-3), so
// every test isolates itself with random content instead of cleaning up.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/ingestion"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
)

// storePool connects to the migrated test database, skipping when none is
// configured.
func storePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the provenance write path")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedSource commits a source row and returns its id and licence terms.
// Source rows are mutable but referenced by immutable items, so they stay;
// the random suffix keeps runs independent.
func seedSource(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, string) {
	t.Helper()
	suffix := uuid.NewString()
	licence := "Extract and link permitted per feed terms (" + suffix + ")"
	var id string
	err := pool.QueryRow(context.Background(),
		`insert into source (name, url, language_code, jurisdiction, licence_terms)
		 values ($1, $2, 'el', 'GR', $3) returning id`,
		"Ingestion Test Feed "+suffix, "https://ingest.example.test/feed/"+suffix, licence,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seeding source: %v", err)
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("parsing source id: %v", err)
	}
	return parsed, licence
}

// countItems returns how many source_item rows exist for a source with the
// given raw body, and countEvents how many item.retrieved events reference
// the given source item.
func countItems(t *testing.T, pool *pgxpool.Pool, sourceID uuid.UUID, body string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`select count(*) from source_item where source_id = $1 and raw_body = $2`,
		sourceID.String(), body).Scan(&n)
	if err != nil {
		t.Fatalf("counting items: %v", err)
	}
	return n
}

func countEvents(t *testing.T, pool *pgxpool.Pool, itemID string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`select count(*) from domain_event where type = 'item.retrieved' and payload->>'source_item_id' = $1`,
		itemID).Scan(&n)
	if err != nil {
		t.Fatalf("counting events: %v", err)
	}
	return n
}

func TestRecordRetrievalCapturesProvenanceAtomically(t *testing.T) {
	t.Parallel()
	pool := storePool(t)
	sourceID, licence := seedSource(t, pool)
	store := ingestion.NewStore(pool)

	published := time.Date(2025, 6, 2, 9, 30, 0, 0, time.FixedZone("EEST", 3*60*60))
	body := "Το πλήρες κείμενο όπως ανακτήθηκε (" + uuid.NewString() + ")"
	item := ingestion.NormalizedItem{
		Title:     "Νέο πάρκο στο κέντρο",
		Link:      "https://ingest.example.test/articles/" + uuid.NewString(),
		Author:    "Μαρία Παπαδοπούλου",
		Published: &published,
		Body:      body,
	}

	res, err := store.RecordRetrieval(context.Background(), sourceID, item)
	if err != nil {
		t.Fatalf("RecordRetrieval() error: %v", err)
	}
	if res.Duplicate {
		t.Fatal("RecordRetrieval() reported Duplicate for fresh content")
	}
	if res.ItemID == uuid.Nil {
		t.Fatal("RecordRetrieval() returned a zero item id")
	}

	var (
		sourceURL, rawBody, contentHash, licenceSnap, usageSnap string
		title, author                                           *string
		publishedAt                                             *time.Time
		retrievedAt                                             time.Time
		permissionSnap                                          *string
	)
	err = pool.QueryRow(context.Background(),
		`select source_url, raw_body, content_hash, licence_snapshot, usage_rule_snapshot,
		        permission_evidence_snapshot, original_title, original_author, published_at, retrieved_at
		   from source_item where id = $1`,
		res.ItemID.String(),
	).Scan(&sourceURL, &rawBody, &contentHash, &licenceSnap, &usageSnap,
		&permissionSnap, &title, &author, &publishedAt, &retrievedAt)
	if err != nil {
		t.Fatalf("reading stored item: %v", err)
	}

	if sourceURL != item.Link {
		t.Errorf("source_url = %q, want %q", sourceURL, item.Link)
	}
	if rawBody != body {
		t.Errorf("raw_body = %q, want %q", rawBody, body)
	}
	sum := sha256.Sum256([]byte(body))
	if want := hex.EncodeToString(sum[:]); contentHash != want {
		t.Errorf("content_hash = %q, want the database-computed %q", contentHash, want)
	}
	// The snapshots come from the trigger, not from the caller: they must
	// equal the source row's terms at retrieval (I-4).
	if licenceSnap != licence {
		t.Errorf("licence_snapshot = %q, want %q", licenceSnap, licence)
	}
	if usageSnap != "extract_and_link" {
		t.Errorf("usage_rule_snapshot = %q, want %q", usageSnap, "extract_and_link")
	}
	if permissionSnap != nil {
		t.Errorf("permission_evidence_snapshot = %q, want NULL", *permissionSnap)
	}
	if title == nil || *title != item.Title {
		t.Errorf("original_title = %v, want %q", title, item.Title)
	}
	if author == nil || *author != item.Author {
		t.Errorf("original_author = %v, want %q", author, item.Author)
	}
	if publishedAt == nil || !publishedAt.Equal(published) {
		t.Errorf("published_at = %v, want %v", publishedAt, published)
	}
	if retrievedAt.IsZero() {
		t.Error("retrieved_at is zero")
	}

	// The audit event committed with the item, carrying the fingerprint.
	var payloadHash, payloadSourceID string
	err = pool.QueryRow(context.Background(),
		`select payload->>'content_hash', payload->>'source_id'
		   from domain_event
		  where type = 'item.retrieved' and payload->>'source_item_id' = $1`,
		res.ItemID.String()).Scan(&payloadHash, &payloadSourceID)
	if err != nil {
		t.Fatalf("reading item.retrieved event: %v", err)
	}
	if payloadHash != contentHash {
		t.Errorf("event content_hash = %q, want %q", payloadHash, contentHash)
	}
	if payloadSourceID != sourceID.String() {
		t.Errorf("event source_id = %q, want %q", payloadSourceID, sourceID)
	}
	if n := countEvents(t, pool, res.ItemID.String()); n != 1 {
		t.Errorf("item.retrieved events = %d, want exactly 1", n)
	}
}

func TestRecordRetrievalStoresAbsentFieldsAsNull(t *testing.T) {
	t.Parallel()
	pool := storePool(t)
	sourceID, _ := seedSource(t, pool)
	store := ingestion.NewStore(pool)

	item := ingestion.NormalizedItem{
		Link: "https://ingest.example.test/articles/" + uuid.NewString(),
		Body: "Body without title, author or date (" + uuid.NewString() + ")",
	}
	res, err := store.RecordRetrieval(context.Background(), sourceID, item)
	if err != nil {
		t.Fatalf("RecordRetrieval() error: %v", err)
	}

	var title, author *string
	var publishedAt *time.Time
	err = pool.QueryRow(context.Background(),
		`select original_title, original_author, published_at from source_item where id = $1`,
		res.ItemID.String()).Scan(&title, &author, &publishedAt)
	if err != nil {
		t.Fatalf("reading stored item: %v", err)
	}
	if title != nil {
		t.Errorf("original_title = %q, want NULL: an absent field is never invented (FR-002)", *title)
	}
	if author != nil {
		t.Errorf("original_author = %q, want NULL", *author)
	}
	if publishedAt != nil {
		t.Errorf("published_at = %v, want NULL", *publishedAt)
	}
}

func TestRecordRetrievalDeduplicatesPerSource(t *testing.T) {
	t.Parallel()
	pool := storePool(t)
	sourceID, _ := seedSource(t, pool)
	otherSourceID, _ := seedSource(t, pool)
	store := ingestion.NewStore(pool)

	body := "Ίδιο περιεχόμενο σε κάθε ανάκτηση (" + uuid.NewString() + ")"
	item := ingestion.NormalizedItem{
		Link: "https://ingest.example.test/articles/" + uuid.NewString(),
		Body: body,
	}

	first, err := store.RecordRetrieval(context.Background(), sourceID, item)
	if err != nil {
		t.Fatalf("first RecordRetrieval() error: %v", err)
	}
	if first.Duplicate {
		t.Fatal("first RecordRetrieval() reported Duplicate")
	}

	// Same content re-polled from the same source: clean no-op (FR-014).
	second, err := store.RecordRetrieval(context.Background(), sourceID, item)
	if err != nil {
		t.Fatalf("re-retrieval must be a clean no-op, got error: %v", err)
	}
	if !second.Duplicate {
		t.Fatal("re-retrieval did not report Duplicate")
	}
	if second.ItemID != uuid.Nil {
		t.Errorf("duplicate result carries item id %s, want zero", second.ItemID)
	}
	if n := countItems(t, pool, sourceID, body); n != 1 {
		t.Errorf("source_item rows = %d, want 1", n)
	}
	if n := countEvents(t, pool, first.ItemID.String()); n != 1 {
		t.Errorf("item.retrieved events = %d, want 1: a dedupe no-op must not emit a second event", n)
	}

	// The same content from a DIFFERENT source is separate evidence:
	// deduplication applies within a source only.
	other, err := store.RecordRetrieval(context.Background(), otherSourceID, item)
	if err != nil {
		t.Fatalf("RecordRetrieval() on second source error: %v", err)
	}
	if other.Duplicate {
		t.Error("identical content from a different source reported Duplicate; dedupe is per source")
	}
}

// failingEventTx passes the source_item insert through and fails the
// domain_event write, simulating a mid-transaction failure.
type failingEventTx struct {
	pgx.Tx
}

func (f failingEventTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "domain_event") {
		return pgconn.CommandTag{}, errors.New("injected failure before the event write")
	}
	return f.Tx.Exec(ctx, sql, args...)
}

type failingBeginner struct {
	pool *pgxpool.Pool
}

func (b failingBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return failingEventTx{Tx: tx}, nil
}

func TestRecordRetrievalFailureStoresNothing(t *testing.T) {
	t.Parallel()
	pool := storePool(t)
	sourceID, _ := seedSource(t, pool)

	body := "Περιεχόμενο που δεν πρέπει να αποθηκευτεί (" + uuid.NewString() + ")"
	item := ingestion.NormalizedItem{
		Link: "https://ingest.example.test/articles/" + uuid.NewString(),
		Body: body,
	}

	// Failure injected between the item insert and the event insert: the
	// transaction must roll back both (I-2).
	store := ingestion.NewStore(failingBeginner{pool: pool})
	if _, err := store.RecordRetrieval(context.Background(), sourceID, item); err == nil {
		t.Fatal("RecordRetrieval() with injected event failure: want error, got nil")
	}
	if n := countItems(t, pool, sourceID, body); n != 0 {
		t.Errorf("source_item rows after failed write = %d, want 0: a failed provenance write stores nothing", n)
	}
	var events int
	err := pool.QueryRow(context.Background(),
		`select count(*) from domain_event where type = 'item.retrieved' and payload->>'source_url' = $1`,
		item.Link).Scan(&events)
	if err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if events != 0 {
		t.Errorf("item.retrieved events after failed write = %d, want 0", events)
	}

	// A context cancelled mid-transaction ends the same way: nothing.
	direct := ingestion.NewStore(pool)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := direct.RecordRetrieval(ctx, sourceID, item); err == nil {
		t.Fatal("RecordRetrieval() with cancelled context: want error, got nil")
	}
	if n := countItems(t, pool, sourceID, body); n != 0 {
		t.Errorf("source_item rows after cancelled write = %d, want 0", n)
	}
}
