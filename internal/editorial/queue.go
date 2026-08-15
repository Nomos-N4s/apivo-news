package editorial

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/editorial/store"
)

// QueueCursor is a position in the review queue: the (retrieved_at, row_id)
// of a row, which the next page starts strictly after. It is the internal
// form of the opaque cursor the contract puts on the wire.
type QueueCursor struct {
	// RetrievedAt is the retrieval time of the row's source item.
	RetrievedAt time.Time
	// RowID is the row's origin id: the translation id for a translated
	// row, the source item id for an untranslated one.
	RowID uuid.UUID
}

// QueueQuery is one request for a page of the review queue.
type QueueQuery struct {
	// Lang filters to one language - a translation's target locale or, for
	// an untranslated origin, its source's language. Empty means no filter.
	Lang string
	// Limit is the maximum number of items in the page. It must be within
	// the contract's 1..100; the store checks rather than trusts it.
	Limit int32
	// Cursor positions the page; nil starts at the newest retrieval.
	Cursor *QueueCursor
}

// Withdrawal is one ended publication in an origin's history: who ended it,
// when and why. Its presence is what makes a queue item a correction
// candidate rather than a fresh one.
type Withdrawal struct {
	// ArticleID is the withdrawn article.
	ArticleID uuid.UUID
	// WithdrawnAt is when publication ended.
	WithdrawnAt time.Time
	// WithdrawnBy is the account id of the editor who withdrew it.
	WithdrawnBy uuid.UUID
	// Reason is the recorded justification; never blank.
	Reason string
}

// QueueItem is one origin awaiting an editorial decision. Exactly one of
// the two origin shapes applies: a translated item carries TranslationID
// and the translated columns, an untranslated one carries neither.
type QueueItem struct {
	// SourceItemID is the retrieved item this origin comes from - set for
	// both shapes, because a translation is always of some retrieved item.
	SourceItemID uuid.UUID
	// TranslationID is the translation awaiting review, nil when the origin
	// is the retrieved item itself.
	TranslationID *uuid.UUID
	// SourceName is the licensed source's name.
	SourceName string
	// HeadlineOriginal is source_item.original_title, nil when the feed
	// provided no title.
	HeadlineOriginal *string
	// HeadlineTranslated is translation.headline, nil for an untranslated
	// origin.
	HeadlineTranslated *string
	// ExtractTranslated is translation.extract, nil for an untranslated
	// origin.
	ExtractTranslated *string
	// RetrievedAt is when the item was retrieved.
	RetrievedAt time.Time
	// LicenceSnapshot is the licence terms that applied at retrieval (I-4).
	LicenceSnapshot string
	// Withdrawals is this origin's withdrawal history, newest first, empty
	// for an origin that has never been published.
	Withdrawals []Withdrawal
	// Cursor is this row's keyset position, from which the endpoint builds
	// the next page's cursor.
	Cursor QueueCursor
}

// CorrectionCandidate reports whether this origin is back in the queue
// because its only articles were withdrawn - the documented correction
// flow, as opposed to an origin that has never been approved.
func (i QueueItem) CorrectionCandidate() bool { return len(i.Withdrawals) > 0 }

// QueuePage is one page of the review queue.
type QueuePage struct {
	// Items are the page's rows, newest retrieval first.
	Items []QueueItem
	// NextCursor positions the following page, nil when this page is the
	// last one - so an empty next cursor always means "nothing further",
	// never "try and see".
	NextCursor *QueueCursor
}

// ReviewQueue returns one page of origins awaiting an editorial decision:
// retrieved items and translations with no non-withdrawn article, newest
// retrieval first.
//
// It reads one row more than asked for and trims it: that extra row is the
// only honest way to answer "is there a next page?" without a second count
// query, and it keeps NextCursor nil exactly when the queue is exhausted
// rather than handing out a cursor that leads to an empty page.
func (s *PGStore) ReviewQueue(ctx context.Context, q QueueQuery) (QueuePage, error) {
	// The bound is re-checked here, not merely at the endpoint: Store is a
	// seam other callers may reach directly, and the one-row overfetch below
	// is int32 arithmetic - an unbounded Limit would overflow it into a
	// negative LIMIT and turn a programming mistake into a database error.
	if q.Limit < 1 || q.Limit > maxQueueLimit {
		return QueuePage{}, fmt.Errorf("editorial: review queue limit %d is outside 1..%d", q.Limit, maxQueueLimit)
	}
	params := store.ListReviewQueueParams{RowLimit: q.Limit + 1}
	if q.Lang != "" {
		params.Lang = pgtype.Text{String: q.Lang, Valid: true}
	}
	if q.Cursor != nil {
		params.CursorRetrievedAt = pgtype.Timestamptz{Time: q.Cursor.RetrievedAt, Valid: true}
		params.CursorRowID = pgtype.UUID{Bytes: q.Cursor.RowID, Valid: true}
	}

	rows, err := s.q.ListReviewQueue(ctx, params)
	if err != nil {
		return QueuePage{}, fmt.Errorf("editorial: listing review queue: %w", err)
	}

	var page QueuePage
	if len(rows) > int(q.Limit) {
		rows = rows[:q.Limit]
		last := rows[len(rows)-1]
		page.NextCursor = &QueueCursor{
			RetrievedAt: last.RetrievedAt.Time,
			RowID:       uuid.UUID(last.RowID.Bytes),
		}
	}

	page.Items = make([]QueueItem, 0, len(rows))
	for _, row := range rows {
		item := QueueItem{
			SourceItemID:       uuid.UUID(row.SourceItemID.Bytes),
			SourceName:         row.SourceName,
			HeadlineOriginal:   textPtr(row.OriginalTitle),
			HeadlineTranslated: textPtr(row.TranslationHeadline),
			ExtractTranslated:  textPtr(row.TranslationExtract),
			RetrievedAt:        row.RetrievedAt.Time,
			LicenceSnapshot:    row.LicenceSnapshot,
			Cursor: QueueCursor{
				RetrievedAt: row.RetrievedAt.Time,
				RowID:       uuid.UUID(row.RowID.Bytes),
			},
		}
		if row.TranslationID.Valid {
			id := uuid.UUID(row.TranslationID.Bytes)
			item.TranslationID = &id
		}
		page.Items = append(page.Items, item)
	}

	if err := s.attachWithdrawals(ctx, page.Items); err != nil {
		return QueuePage{}, err
	}
	return page, nil
}

// attachWithdrawals fills in the withdrawal history of the page's origins in
// one round trip, so a page of correction candidates costs the same as a
// page of fresh ones.
func (s *PGStore) attachWithdrawals(ctx context.Context, items []QueueItem) error {
	if len(items) == 0 {
		return nil
	}
	// Each origin is looked up under the shape it was listed as: a
	// translated row by its translation id, an untranslated one by its
	// source item id. Mixing the two would attribute a withdrawn
	// translation's history to the untranslated origin behind it, which is
	// a different approval target entirely.
	params := store.ListQueueWithdrawalsParams{
		TranslationIds: []pgtype.UUID{},
		SourceItemIds:  []pgtype.UUID{},
	}
	for _, item := range items {
		if item.TranslationID != nil {
			params.TranslationIds = append(params.TranslationIds,
				pgtype.UUID{Bytes: *item.TranslationID, Valid: true})
			continue
		}
		params.SourceItemIds = append(params.SourceItemIds,
			pgtype.UUID{Bytes: item.SourceItemID, Valid: true})
	}

	rows, err := s.q.ListQueueWithdrawals(ctx, params)
	if err != nil {
		return fmt.Errorf("editorial: listing review queue withdrawals: %w", err)
	}

	byOrigin := make(map[uuid.UUID][]Withdrawal, len(rows))
	for _, row := range rows {
		origin := uuid.UUID(row.SourceItemID.Bytes)
		if row.TranslationID.Valid {
			origin = uuid.UUID(row.TranslationID.Bytes)
		}
		byOrigin[origin] = append(byOrigin[origin], Withdrawal{
			ArticleID:   uuid.UUID(row.ArticleID.Bytes),
			WithdrawnAt: row.WithdrawnAt.Time,
			WithdrawnBy: uuid.UUID(row.WithdrawnBy.Bytes),
			Reason:      row.WithdrawalReason.String,
		})
	}
	for i := range items {
		items[i].Withdrawals = byOrigin[items[i].Cursor.RowID]
	}
	return nil
}

// textPtr converts a nullable database text column to the pointer form the
// JSON layer renders as null.
func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}
