package ingestion

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Beginner is the narrow slice of database access the store needs: the
// ability to open a transaction. It is defined here per the boundary rules
// (the consumer names its dependency); both *pgxpool.Pool and pgx.Tx
// satisfy it, and the composition root in cmd decides which is wired in.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Store is the provenance write path: it turns a NormalizedItem into a
// source_item row plus its item.retrieved domain event, in one transaction
// (I-2). It deliberately supplies neither the content hash nor the
// licence/usage/permission snapshots - the database computes the hash from
// the stored body and the snapshot trigger copies the terms from the source
// row (I-4), so the caller cannot record false evidence.
type Store struct {
	db Beginner
}

// NewStore builds a Store on any transaction opener.
func NewStore(db Beginner) *Store {
	return &Store{db: db}
}

// RetrievalResult reports what one RecordRetrieval call did.
type RetrievalResult struct {
	// ItemID is the stored source_item row id; zero when Duplicate.
	ItemID uuid.UUID
	// Duplicate reports that identical content from this source was
	// already on record, so nothing was written (FR-014). It is a clean
	// outcome, not an error: the poll loop keeps going.
	Duplicate bool
}

// RecordRetrieval stores one retrieved feed item with its provenance, and
// the item.retrieved domain event, atomically: both commit together or
// neither exists (I-2). Fields the feed omitted are stored as NULL, never
// invented (FR-002). Re-retrieving identical content from the same source
// is a no-op reported as Duplicate - no new row, no second event.
func (s *Store) RecordRetrieval(ctx context.Context, sourceID uuid.UUID, item NormalizedItem) (RetrievalResult, error) {
	// Checked before any database work: an item with no origin link or no
	// text would otherwise fail deep inside the insert as a NOT NULL or
	// CHECK violation, which reports that a constraint was hit but not
	// which item was unusable or why.
	if err := item.Validate(); err != nil {
		return RetrievalResult{}, fmt.Errorf("ingestion: record retrieval: %w", err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return RetrievalResult{}, fmt.Errorf("ingestion: record retrieval: begin: %w", err)
	}
	// A no-op after commit; the guarantee that a failed write stores
	// nothing on any earlier return path.
	defer func() { _ = tx.Rollback(ctx) }()

	// content_hash is a generated column and the licence/usage/permission
	// snapshots are written by the source_item_snapshot trigger from the
	// source row, inside this same transaction. Supplying any of them here
	// would bypass the database as the authority on the evidence.
	var (
		itemID      string
		contentHash string
		retrievedAt time.Time
	)
	err = tx.QueryRow(ctx,
		`insert into source_item (source_id, source_url, original_title, original_author, published_at, raw_body)
		 values ($1, $2, $3, $4, $5, $6)
		 returning id, content_hash, retrieved_at`,
		sourceID.String(), item.Link, nullIfEmpty(item.Title), nullIfEmpty(item.Author), item.Published, item.Body,
	).Scan(&itemID, &contentHash, &retrievedAt)
	if isDuplicateItem(err) {
		return RetrievalResult{Duplicate: true}, nil
	}
	if err != nil {
		return RetrievalResult{}, fmt.Errorf("ingestion: record retrieval: insert source_item: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`insert into domain_event (type, payload) values ('item.retrieved', $1)`,
		map[string]any{
			"source_item_id": itemID,
			"source_id":      sourceID.String(),
			"source_url":     item.Link,
			"content_hash":   contentHash,
			"retrieved_at":   retrievedAt,
		},
	); err != nil {
		return RetrievalResult{}, fmt.Errorf("ingestion: record retrieval: insert domain event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return RetrievalResult{}, fmt.Errorf("ingestion: record retrieval: commit: %w", err)
	}
	id, err := uuid.Parse(itemID)
	if err != nil {
		// The committed row is fine; only the reported id is unusable.
		return RetrievalResult{}, fmt.Errorf("ingestion: record retrieval: database returned malformed id %q: %w", itemID, err)
	}
	return RetrievalResult{ItemID: id}, nil
}

// isDuplicateItem recognises the unique violation on (source_id,
// content_hash): identical content from the same source is already on
// record. Deduplication is per source by design - the same text from two
// sources is two pieces of evidence.
func isDuplicateItem(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == "23505" &&
		pgErr.ConstraintName == "source_item_unique_per_source"
}

// nullIfEmpty maps an absent feed field to NULL: what the feed did not
// provide is stored as nothing, never as an invented empty value (FR-002).
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
