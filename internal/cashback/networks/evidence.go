// Evidence persistence: turning what a network reported into the row a
// member's money later rests on (T052, FR-032).

package networks

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
)

var (
	// ErrNoEvidenceStore reports an evidence writer built with nothing to
	// write to. Refused at construction rather than at the first report,
	// because a poller that discovered it mid-window would already have read
	// a window it cannot persist and would have to decide what to do with
	// the half of it still in memory.
	ErrNoEvidenceStore = errors.New("networks: an evidence writer needs a store to write to")
	// ErrEvidenceNotWritten reports a report that could not be stored. It
	// wraps whatever the database said, unchanged: the ingestion path reads
	// the cause to tell an unchanged re-report (T053) and a superseding one
	// (T054) apart from a genuine failure, and a writer that flattened them
	// into one message would make that impossible.
	ErrEvidenceNotWritten = errors.New("networks: the report could not be written as evidence")
)

// EvidenceStore is the write this package needs from the generated store,
// named here rather than taken as *store.Queries so that the caller decides
// the transaction boundary.
//
// That is the whole of what "in one transaction" means for this task. The
// normalised record and the verbatim payload are ONE ROW, so writing them
// together is atomic by construction rather than by discipline - the schema
// makes a normalised record with no evidence behind it unrepresentable, and
// a second write to add the payload would be an UPDATE the table refuses
// (C-3). The transaction that does matter is a wider one: a poller persists
// a whole window and only then advances its durable cursor (FR-031), and it
// can only do that if this writer composes into a transaction it opened.
// Both *pgxpool.Pool and pgx.Tx satisfy the generated store, so both satisfy
// this.
type EvidenceStore interface {
	InsertNetworkTransaction(ctx context.Context, arg store.InsertNetworkTransactionParams) (store.InsertNetworkTransactionRow, error)
}

// Recorded is what the DATABASE decided about one stored report: the row's
// identity, the fingerprint it computed from the reported facts, and the
// retrieval instant it stored.
//
// It carries the database's answers rather than the caller's inputs, and
// that is the point. The digest is computed by a trigger from the facts and
// a caller-supplied one is discarded (migration 0012), so a writer that
// echoed its own inputs back would report success for a row the database had
// rewritten underneath it.
type Recorded struct {
	// ID is the evidence row. Every later reference to this report - a
	// superseding row's predecessor, an earning's citation - is this value.
	ID uuid.UUID
	// ContentDigest is the database's fingerprint of the reported facts. It
	// is what makes an unchanged re-report a no-op and a changed one a new
	// row, and it is returned so a caller can compare two reports without
	// recomputing what only the database is the authority on.
	ContentDigest string
	// RetrievedAt is the instant the row carries, read back rather than
	// assumed: a caller that trusted its own value would not notice a column
	// default having overridden it.
	RetrievedAt time.Time
}

// EvidenceWriter records what a network reported, exactly as it reported it.
//
// It writes and does not decide. Whether the click reference matches a
// click, whether this report supersedes one already stored, whether a member
// is owed anything - none of those are asked here. They are the ingestion
// path's questions and they are answered from stored rows, after the fact,
// which is what keeps a mis-translation from being able to move money on its
// own: a wrong mapping produces a wrong row an operator can see and
// re-derive, never a credit nobody authorised.
type EvidenceWriter struct {
	store EvidenceStore
}

// NewEvidenceWriter builds a writer over the given store, refusing a nil one.
func NewEvidenceWriter(s EvidenceStore) (*EvidenceWriter, error) {
	if s == nil {
		return nil, ErrNoEvidenceStore
	}
	return &EvidenceWriter{store: s}, nil
}

// Record writes one report as a first record of its transaction and returns
// what the database decided about it.
//
// Both inputs are validated before anything is written, and neither check is
// redundant with one somewhere else. Contract rule 7 has every adapter call
// [Reported.Validate] before yielding, but this row is IMMUTABLE: a bad one
// cannot be corrected, only superseded, and it would sit in the evidence a
// member's money rests on forever. The last gate before permanence is worth
// paying for twice.
//
// It records a ROOT: the row names no predecessor. A report that supersedes
// one already stored is T054's, and it is a separate operation rather than a
// flag on this one, because the two have different preconditions - a
// superseding row must name a predecessor of the same transaction at the
// same network, which the guard trigger enforces and only the ingestion path
// can establish.
//
// The digest is never sent. The parameters this writer builds have no field
// for one, because the query names no such column: the database computes it
// from the reported facts and discards anything a caller supplied, and a
// writer that appeared to supply it would be claiming an authority it does
// not have over the fingerprint of its own evidence.
func (w *EvidenceWriter) Record(ctx context.Context, retrieval Retrieval, report Reported) (Recorded, error) {
	if err := retrieval.Validate(); err != nil {
		return Recorded{}, err
	}
	if err := report.Validate(); err != nil {
		return Recorded{}, err
	}

	row, err := w.store.InsertNetworkTransaction(ctx, evidenceRow(retrieval, report, pgtype.UUID{}))
	if err != nil {
		return Recorded{}, fmt.Errorf("%w: %s reporting %s: %w",
			ErrEvidenceNotWritten, retrieval, strconv.Quote(report.ExternalID), err)
	}

	return Recorded{
		ID:            uuid.UUID(row.ID.Bytes),
		ContentDigest: row.ContentDigest,
		RetrievedAt:   row.RetrievedAt.Time,
	}, nil
}

// evidenceRow is the row one report and one retrieval describe, naming
// supersedes as its predecessor - the zero value for a first report.
//
// One builder for three write paths (this file's, the dedup path's and the
// supersede path's), because "what a report becomes in the table" is one
// answer and three copies of it would be three places to forget a column the
// day one is added. The digest is absent from all of them, as it is from the
// queries: the database computes it.
func evidenceRow(retrieval Retrieval, report Reported, supersedes pgtype.UUID) store.InsertNetworkTransactionParams {
	clickRef := pgtype.Text{}
	if ref, present := report.ClickRef.Ref(); present {
		clickRef = pgtype.Text{String: ref, Valid: true}
	}
	return store.InsertNetworkTransactionParams{
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
		SupersedesID:     supersedes,
	}
}

// evidenceRowIfNew is the same row for the conflict-swallowing insert. The
// conversion is deliberate rather than a second builder: sqlc generates the
// two parameter structs independently, so if a column ever reached one query
// and not the other this line would stop compiling - which is the tripwire
// worth having, because the alternative is two builders that drift silently.
func evidenceRowIfNew(retrieval Retrieval, report Reported, supersedes pgtype.UUID) store.InsertNetworkTransactionIfNewParams {
	return store.InsertNetworkTransactionIfNewParams(evidenceRow(retrieval, report, supersedes))
}
