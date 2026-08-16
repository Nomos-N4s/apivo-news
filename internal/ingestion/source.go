package ingestion

// The poll loop's view of the source table: which feeds to walk, and where
// each poll's outcome is written back. Like store.go this is hand-written
// pgx rather than sqlc - ingestion owns its own SQL - and like there the
// consumer names the narrow slice of database access it needs.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SourceQuerier is the slice of database access the source store needs:
// plain queries and statements, no transaction - reading the source list
// and writing one poll outcome are each a single statement. *pgxpool.Pool
// satisfies it, and the composition root in cmd wires it in.
type SourceQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// PolledSource is one active source as the poll loop sees it: where to ask,
// and the validators the source last handed out to ask conditionally with.
type PolledSource struct {
	ID  uuid.UUID
	URL string
	// Validators are the stored conditional-GET tokens; zero-valued for a
	// source that has never answered with any.
	Validators Validators
}

// PollOutcome is what one completed poll attempt on one source writes back.
// It is the last poll's outcome only, overwritten each cycle - history
// lives in domain_event and source_item, not on the source row.
type PollOutcome struct {
	// Validators are stored for the next conditional GET: the response's
	// own tokens after a successful exchange, the previously stored ones
	// after a failure (a failed exchange refreshes nothing).
	Validators Validators
	// Error is why the poll failed, or empty on success - and empty is what
	// clears last_poll_error, so a recovered source stops looking broken.
	Error string
	// Retrieved counts the items this poll stored as new evidence.
	Retrieved int
	// Duplicates counts the items already on record (FR-014 no-ops).
	Duplicates int
}

// SourceStore reads the pollable sources and records poll outcomes.
type SourceStore struct {
	db SourceQuerier
}

// NewSourceStore builds a SourceStore on any plain querier.
func NewSourceStore(db SourceQuerier) *SourceStore {
	return &SourceStore{db: db}
}

// ListActiveSources returns every source the poll loop should walk, with
// the validators stored from each one's last answer. Paused sources
// (active = false) are simply absent. The order is by URL: stable across
// cycles, so the spacing between two given sources stays roughly constant
// rather than shuffling every cycle.
func (s *SourceStore) ListActiveSources(ctx context.Context) ([]PolledSource, error) {
	rows, err := s.db.Query(ctx,
		`select id, url, etag, last_modified from source where active order by url`)
	if err != nil {
		return nil, fmt.Errorf("ingestion: list active sources: %w", err)
	}
	defer rows.Close()

	var sources []PolledSource
	for rows.Next() {
		var (
			id  string
			src PolledSource
		)
		if err := rows.Scan(&id, &src.URL, &src.Validators.ETag, &src.Validators.LastModified); err != nil {
			return nil, fmt.Errorf("ingestion: list active sources: scan: %w", err)
		}
		src.ID, err = uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("ingestion: list active sources: database returned malformed id %q: %w", id, err)
		}
		sources = append(sources, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ingestion: list active sources: %w", err)
	}
	return sources, nil
}

// RecordPollOutcome writes one poll's outcome onto the source row in a
// single UPDATE: validators for the next conditional GET, the attempt time,
// and the counters. A success (empty outcome.Error) clears last_poll_error;
// a failure stores the error string, which the fetcher has already redacted
// URLs from.
func (s *SourceStore) RecordPollOutcome(ctx context.Context, sourceID uuid.UUID, outcome PollOutcome) error {
	tag, err := s.db.Exec(ctx,
		`update source
		    set etag = $2,
		        last_modified = $3,
		        last_polled_at = now(),
		        last_poll_error = $4,
		        last_poll_retrieved = $5,
		        last_poll_duplicates = $6
		  where id = $1`,
		sourceID.String(), outcome.Validators.ETag, outcome.Validators.LastModified,
		nullIfEmpty(outcome.Error), outcome.Retrieved, outcome.Duplicates,
	)
	if err != nil {
		return fmt.Errorf("ingestion: record poll outcome: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// The row this cycle listed is gone. Nothing to store the outcome
		// on, and pretending otherwise would be a success state for a
		// record that was not written.
		return fmt.Errorf("ingestion: record poll outcome: source %s no longer exists", sourceID)
	}
	return nil
}
