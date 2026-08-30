// The dedup path: recognising the re-report that changed nothing, and
// letting the rest of the window through (T053, US2 scenario 3).

package networks

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
)

// DedupStore is the conflict-swallowing write the dedup path needs. It is a
// second interface rather than a method on [EvidenceStore] so that a caller
// which only ever writes first reports is handed no way to swallow a
// conflict, and one that only deduplicates is handed no way to raise on one.
type DedupStore interface {
	InsertNetworkTransactionIfNew(ctx context.Context, arg store.InsertNetworkTransactionIfNewParams) (store.InsertNetworkTransactionIfNewRow, error)
}

// Deduplicator records a report unless the network has already reported
// exactly the same facts.
//
// "Exactly the same facts" is the DATABASE's judgement, not this package's.
// A trigger computes a fingerprint from the click reference, both statuses,
// both amounts, the currency and the transaction time, and a unique
// constraint over it makes a repeat of those facts the same row (migration
// 0012). Nothing here recomputes that fingerprint, and the absence is
// deliberate: a second implementation of the formula is the place the two
// would eventually disagree, and the disagreement would show up as either a
// duplicate credit or a status change nobody noticed.
//
// The payload is not part of the fingerprint, which is what makes this
// useful. Networks stamp their own response timestamps and pagination
// metadata into a payload, so a poll that re-reads a trailing window returns
// the same transactions inside different bytes every time (ADR-0003). A
// digest that saw the payload would call every one of those a change.
type Deduplicator struct {
	store DedupStore
}

// NewDeduplicator builds a dedup path over the given store, refusing a nil
// one for the same reason [NewEvidenceWriter] does: a poller that discovered
// it mid-window would already have read a window it cannot persist.
func NewDeduplicator(s DedupStore) (*Deduplicator, error) {
	if s == nil {
		return nil, ErrNoEvidenceStore
	}
	return &Deduplicator{store: s}, nil
}

// RecordIfNew writes one report as a first record of its transaction, unless
// the same facts are already stored.
//
// It answers three ways, and the middle one is the point:
//
//   - (row, true, nil) - the report was new and is now stored.
//   - (zero, false, nil) - the network re-reported facts that have not
//     changed. Nothing was written, nothing is wrong, and the rest of the
//     window can still be persisted in the same transaction.
//   - (zero, false, err) - anything else, including a report whose facts
//     HAVE changed being written as a root. That one is not a duplicate: it
//     wraps [ErrEvidenceNotWritten] carrying the database's own refusal, and
//     the caller's answer to it is to supersede (T054) rather than to
//     discard. Swallowing it would leave a member's confirmed transaction
//     pending forever with nothing logged.
//
// A skipped report comes back as a ZERO [Recorded] rather than the row that
// was already there. The database is asked to write, not to search, and it
// reports nothing about the row it matched; finding that row would mean
// recomputing the fingerprint here, which is the one thing this path will
// not do. A caller that needs the stored report reads the transaction's
// chain, which is what the supersede path does anyway.
//
// Both inputs are validated first, exactly as [EvidenceWriter.Record]
// validates them and for the same reason: the row is immutable, so the last
// gate before permanence is worth paying for twice.
func (d *Deduplicator) RecordIfNew(ctx context.Context, retrieval Retrieval, report Reported) (Recorded, bool, error) {
	if err := retrieval.Validate(); err != nil {
		return Recorded{}, false, err
	}
	if err := report.Validate(); err != nil {
		return Recorded{}, false, err
	}

	clickRef := pgtype.Text{}
	if ref, present := report.ClickRef.Ref(); present {
		clickRef = pgtype.Text{String: ref, Valid: true}
	}

	row, err := d.store.InsertNetworkTransactionIfNew(ctx, store.InsertNetworkTransactionIfNewParams{
		NetworkID:        string(retrieval.Account.Network()),
		NetworkAccountID: pgtype.UUID{Bytes: retrieval.Account.ID(), Valid: true},
		ExternalID:       report.ExternalID,
		ClickRef:         clickRef,
		StatusRaw:        report.StatusRaw,
		Status:           string(report.Status),
		SaleAmountMinor:  report.SaleAmount.Minor,
		CommissionMinor:  report.Commission.Minor,
		Currency:         string(report.SaleAmount.Currency),
		TransactedAt:     pgtype.Timestamptz{Time: report.TransactedAt, Valid: true},
		RetrievedAt:      pgtype.Timestamptz{Time: retrieval.RetrievedAt, Valid: true},
		QueryWindowStart: pgtype.Timestamptz{Time: retrieval.Window.From, Valid: true},
		QueryWindowEnd:   pgtype.Timestamptz{Time: retrieval.Window.To, Valid: true},
		RawPayload:       report.RawPayload,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The conflict the query swallows: no row was written because this
		// exact report is already stored. Not an error, and deliberately not
		// reported as one - a poller that logged every unchanged re-report
		// would log most of every trailing re-read.
		return Recorded{}, false, nil
	case err != nil:
		return Recorded{}, false, fmt.Errorf("%w: %s reporting %s: %w",
			ErrEvidenceNotWritten, retrieval, strconv.Quote(report.ExternalID), err)
	}

	return Recorded{
		ID:            uuid.UUID(row.ID.Bytes),
		ContentDigest: row.ContentDigest,
		RetrievedAt:   row.RetrievedAt.Time,
	}, true, nil
}
