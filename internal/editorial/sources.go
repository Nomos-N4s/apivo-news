package editorial

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/editorial/store"
)

// SourceCursor is a position in the source list: the (created_at, id) of a
// row, which the next page starts strictly after. It is the internal form
// of the opaque cursor the contract puts on the wire, sharing the queue's
// codec because it is the same timestamp-and-tiebreak pair.
type SourceCursor struct {
	// CreatedAt is when the row's source was registered.
	CreatedAt time.Time
	// ID is the row's source id, breaking ties between registrations that
	// share a created_at.
	ID uuid.UUID
}

// SourcesQuery is one request for a page of the source list.
type SourcesQuery struct {
	// Active filters to polled (true) or paused (false) feeds; nil means no
	// filter. A pointer, because "unfiltered" and "false" are different
	// requests.
	Active *bool
	// Limit is the maximum number of items in the page, within the
	// contract's 1..100; the store checks rather than trusts it.
	Limit int32
	// Cursor positions the page; nil starts at the newest registration.
	Cursor *SourceCursor
}

// ListedSource is one registered source as the list shows it: identity,
// the CURRENT licensing terms - the mutable row, deliberately, because the
// legal basis of anything already retrieved is the snapshot on source_item
// (I-4) and lives behind the provenance endpoint - and the operational
// poll state 0007 keeps on the row.
type ListedSource struct {
	ID           uuid.UUID
	Name         string
	URL          string
	Language     string
	Jurisdiction string
	LicenceTerms string
	UsageRule    string
	// PermissionEvidence is the recorded written permission behind a
	// full_text rule, nil when none is on record. It is served behind the
	// editor gate: the screen exists to make the licensing basis visible.
	PermissionEvidence *string
	// Active is the operator's pause switch (0002); this list is its one
	// read path.
	Active bool
	// LastPolledAt is when the poll loop last completed an attempt, nil for
	// a source never polled.
	LastPolledAt *time.Time
	CreatedAt    time.Time
	// Cursor is this row's keyset position, from which the endpoint builds
	// the next page's cursor.
	Cursor SourceCursor
}

// SourcesPage is one page of the source list.
type SourcesPage struct {
	// Items are the page's rows, newest registration first.
	Items []ListedSource
	// NextCursor positions the following page, nil when this page is the
	// last one - an absent cursor always means the list is exhausted.
	NextCursor *SourceCursor
}

// PollCycle is the last poll cycle as the poll state records it: the sums
// of each active source's last-poll counters, and the names of the feeds
// whose last poll failed. Real readings of 0007's columns, never invented
// figures.
type PollCycle struct {
	Retrieved  int64
	Duplicates int64
	// Failures is sorted by source name and empty (never nil) when every
	// active feed's last poll succeeded.
	Failures []string
}

// ListSources returns one page of registered sources, newest registration
// first.
//
// Like the queue, it reads one row more than asked for and trims it: the
// extra row is the only honest way to answer "is there a next page?"
// without a second count query, and it keeps NextCursor nil exactly when
// the list is exhausted.
func (s *PGStore) ListSources(ctx context.Context, q SourcesQuery) (SourcesPage, error) {
	// Re-checked here for the queue's reason: Store is a seam other callers
	// may reach directly, and the one-row overfetch is int32 arithmetic.
	if q.Limit < 1 || q.Limit > maxQueueLimit {
		return SourcesPage{}, fmt.Errorf("editorial: source list limit %d is outside 1..%d", q.Limit, maxQueueLimit)
	}
	params := store.ListSourcesParams{RowLimit: q.Limit + 1}
	if q.Active != nil {
		params.Active = pgtype.Bool{Bool: *q.Active, Valid: true}
	}
	if q.Cursor != nil {
		params.CursorCreatedAt = pgtype.Timestamptz{Time: q.Cursor.CreatedAt, Valid: true}
		params.CursorRowID = pgtype.UUID{Bytes: q.Cursor.ID, Valid: true}
	}

	rows, err := s.q.ListSources(ctx, params)
	if err != nil {
		return SourcesPage{}, fmt.Errorf("editorial: listing sources: %w", err)
	}

	var page SourcesPage
	if len(rows) > int(q.Limit) {
		rows = rows[:q.Limit]
		last := rows[len(rows)-1]
		page.NextCursor = &SourceCursor{
			CreatedAt: last.CreatedAt.Time,
			ID:        uuid.UUID(last.ID.Bytes),
		}
	}

	page.Items = make([]ListedSource, 0, len(rows))
	for _, row := range rows {
		item := ListedSource{
			ID:                 uuid.UUID(row.ID.Bytes),
			Name:               row.Name,
			URL:                row.Url,
			Language:           row.LanguageCode,
			Jurisdiction:       row.Jurisdiction,
			LicenceTerms:       row.LicenceTerms,
			UsageRule:          row.UsageRule,
			PermissionEvidence: textPtr(row.PermissionEvidence),
			Active:             row.Active,
			CreatedAt:          row.CreatedAt.Time,
			Cursor: SourceCursor{
				CreatedAt: row.CreatedAt.Time,
				ID:        uuid.UUID(row.ID.Bytes),
			},
		}
		if row.LastPolledAt.Valid {
			at := row.LastPolledAt.Time
			item.LastPolledAt = &at
		}
		page.Items = append(page.Items, item)
	}
	return page, nil
}

// LastPollCycle reads the last poll cycle from the poll state on the
// active source rows.
func (s *PGStore) LastPollCycle(ctx context.Context) (PollCycle, error) {
	row, err := s.q.LastPollCycle(ctx)
	if err != nil {
		return PollCycle{}, fmt.Errorf("editorial: reading last poll cycle: %w", err)
	}
	cycle := PollCycle{
		Retrieved:  row.Retrieved,
		Duplicates: row.Duplicates,
		// Empty renders as [], never null: "no failures" is a finding, not
		// an absence of one.
		Failures: make([]string, 0, len(row.Failures)),
	}
	cycle.Failures = append(cycle.Failures, row.Failures...)
	return cycle, nil
}
