package editorial

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// SourcePatch is one PATCH of a registered source: the fields an editor
// may change, each nil when the request did not supply it - "unchanged"
// and "set to the same value" are different requests, and only the
// supplied ones belong in the audit record. The usage rule is deliberately
// absent, exactly as on NewSource: upgrades are a founder-gated flow
// outside this module.
type SourcePatch struct {
	Name         *string
	URL          *string
	Active       *bool
	LicenceTerms *string
}

// Empty reports a patch that supplies nothing - the handler refuses it
// before any write, because accepting it would confirm an edit that never
// was.
func (p SourcePatch) Empty() bool {
	return p.Name == nil && p.URL == nil && p.Active == nil && p.LicenceTerms == nil
}

// sourceFieldChange is one edited field in a source.updated payload: what
// the row said before, and what it says now. Old and new travel together
// because "what did we believe the terms were, when" is the question the
// audit stream exists to answer (I-4's question, asked of the mutable row).
type sourceFieldChange struct {
	Old any `json:"old"`
	New any `json:"new"`
}

// sourceUpdatedPayload is the source.updated event: who edited which
// source, and each field that actually changed, old and new. Fields whose
// supplied value equalled the row are absent - the stream records edits,
// not re-statements.
type sourceUpdatedPayload struct {
	SourceID     string             `json:"source_id"`
	UpdatedBy    string             `json:"updated_by"`
	Name         *sourceFieldChange `json:"name,omitempty"`
	URL          *sourceFieldChange `json:"url,omitempty"`
	Active       *sourceFieldChange `json:"active,omitempty"`
	LicenceTerms *sourceFieldChange `json:"licence_terms,omitempty"`
}

// UpdateSource applies a patch to a registered source and, when anything
// actually changed, appends the source.updated domain event in the same
// transaction - an edit to licence terms is a licensing event, and a
// record of it that could commit without its audit line would be a record
// with a hole in it.
//
// An unknown id reports ErrSourceNotFound; a url already registered to
// another source reports ErrDuplicateSourceURL, exactly as on creation.
func (s *PGStore) UpdateSource(ctx context.Context, id, editorID uuid.UUID, patch SourcePatch) (ListedSource, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return ListedSource{}, fmt.Errorf("editorial: beginning source update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	params := store.UpdateSourceParams{ID: pgtype.UUID{Bytes: id, Valid: true}}
	if patch.Name != nil {
		params.Name = pgtype.Text{String: *patch.Name, Valid: true}
	}
	if patch.URL != nil {
		params.Url = pgtype.Text{String: *patch.URL, Valid: true}
	}
	if patch.Active != nil {
		params.Active = pgtype.Bool{Bool: *patch.Active, Valid: true}
	}
	if patch.LicenceTerms != nil {
		params.LicenceTerms = pgtype.Text{String: *patch.LicenceTerms, Valid: true}
	}

	row, err := q.UpdateSource(ctx, params)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ListedSource{}, fmt.Errorf("%w: %s", ErrSourceNotFound, id)
	case err != nil:
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) &&
			pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == "source_url_key" {
			return ListedSource{}, fmt.Errorf("%w: %s", ErrDuplicateSourceURL, *patch.URL)
		}
		return ListedSource{}, fmt.Errorf("editorial: updating source: %w", err)
	}

	if payload, changed := sourceUpdate(id, editorID, patch, row); changed {
		if err := recordEvent(ctx, q, eventSourceUpdated, payload); err != nil {
			return ListedSource{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ListedSource{}, fmt.Errorf("editorial: committing source update: %w", err)
	}

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
	}
	if row.LastPolledAt.Valid {
		at := row.LastPolledAt.Time
		item.LastPolledAt = &at
	}
	return item, nil
}

// sourceUpdate builds the source.updated payload from the statement's own
// before-and-after reading, reporting whether anything changed at all. A
// patch that restated the current values changes nothing, and appending an
// event over it would put a non-edit into the audit stream.
//
// Both halves of every pair come from the one statement, and its "old" is
// the row the write actually replaced because the query takes it under
// FOR UPDATE - a concurrent edit that commits mid-flight is waited for and
// then read, never skipped over.
func sourceUpdate(id, editorID uuid.UUID, patch SourcePatch, row store.UpdateSourceRow) (sourceUpdatedPayload, bool) {
	payload := sourceUpdatedPayload{SourceID: id.String(), UpdatedBy: editorID.String()}
	changed := false
	if patch.Name != nil && row.OldName != row.Name {
		payload.Name = &sourceFieldChange{Old: row.OldName, New: row.Name}
		changed = true
	}
	if patch.URL != nil && row.OldUrl != row.Url {
		payload.URL = &sourceFieldChange{Old: row.OldUrl, New: row.Url}
		changed = true
	}
	if patch.Active != nil && row.OldActive != row.Active {
		payload.Active = &sourceFieldChange{Old: row.OldActive, New: row.Active}
		changed = true
	}
	if patch.LicenceTerms != nil && row.OldLicenceTerms != row.LicenceTerms {
		payload.LicenceTerms = &sourceFieldChange{Old: row.OldLicenceTerms, New: row.LicenceTerms}
		changed = true
	}
	return payload, changed
}

// DeleteSource removes a source that no evidence references. The database
// decides: the source_item FK carries no ON DELETE clause, so a source
// with retrieved items raises 23503 and the refusal comes back as
// SourceEvidenceError naming the count. Deactivation, not deletion, is the
// everyday "remove" - this succeeds only for a source that never yielded
// anything.
func (s *PGStore) DeleteSource(ctx context.Context, id uuid.UUID) error {
	// The delete runs in its own transaction (a savepoint when the caller
	// is already inside one), so the FK's refusal aborts the delete alone:
	// the evidence count that names the refusal still has a live
	// connection state to be read on.
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("editorial: beginning source delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := s.q.WithTx(tx).DeleteSource(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		_ = tx.Rollback(ctx)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) &&
			pgErr.Code == pgerrcode.ForeignKeyViolation && pgErr.ConstraintName == "source_item_source_id_fkey" {
			items, countErr := s.q.CountSourceEvidence(ctx, pgtype.UUID{Bytes: id, Valid: true})
			if countErr != nil {
				return fmt.Errorf("editorial: counting the evidence that refused a source delete: %w", countErr)
			}
			return SourceEvidenceError{Items: items}
		}
		return fmt.Errorf("editorial: deleting source: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("%w: %s", ErrSourceNotFound, id)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("editorial: committing source delete: %w", err)
	}
	return nil
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
