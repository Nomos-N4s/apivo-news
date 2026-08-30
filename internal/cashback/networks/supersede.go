// The supersede path: a report whose facts have changed becomes a NEW row
// naming the one it replaces (T054, US2 scenario 4).

package networks

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
)

var (
	// ErrSupersededConcurrently reports a race two pollers can lose: both
	// read the same current row and both offered a successor for it, and the
	// chain allows one. It is separate from [ErrEvidenceNotWritten] because
	// the answer is different - re-read the tip and try again, rather than
	// investigate - and because a caller that retried every write failure
	// would keep retrying an expired credential forever.
	ErrSupersededConcurrently = errors.New("networks: another report superseded the same row first")
)

// supersededConcurrently reports whether err is the chain's one-successor
// rule refusing a second claim on the same predecessor. It reads the
// constraint name rather than the SQLSTATE, because three different rules on
// this table refuse with 23505 and they mean different things: the same
// report twice, two roots for one transaction, and this.
func supersededConcurrently(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.ConstraintName == "network_transaction_superseded_once"
}

// Outcome is what became of one report. Three states rather than a boolean,
// because a caller acts differently on each: a first report and a
// superseding one both change what a member is owed and both deserve an
// event (T062), while an unchanged re-report is most of every trailing
// re-read and deserves silence.
type Outcome string

const (
	// OutcomeFirstReport means the network had never reported this
	// transaction and a root row was written.
	OutcomeFirstReport Outcome = "first-report"
	// OutcomeSuperseded means the facts had changed and a new row was
	// written naming the previous one. The old row is untouched: it is
	// evidence of what the network said then, and it stays readable.
	OutcomeSuperseded Outcome = "superseded"
	// OutcomeUnchanged means the network re-reported facts that had not
	// changed. Nothing was written and nothing is wrong.
	OutcomeUnchanged Outcome = "unchanged"
)

// ChainStore is the read the supersede path needs: what the network last
// said about one transaction.
type ChainStore interface {
	GetCurrentNetworkTransaction(ctx context.Context, arg store.GetCurrentNetworkTransactionParams) (store.CashbackNetworkTransaction, error)
}

// Superseder records what a network reported, whatever it turns out to be:
// the first word on a transaction, a changed one, or the same one again.
//
// It never edits and never deletes, because it cannot: the table refuses
// both (C-3). A status change is a NEW row that names the one it replaces,
// so what the network said last month stays exactly as readable as what it
// said today - which is what an operator needs on the day a member asks why
// a confirmed purchase was reversed.
type Superseder struct {
	chain ChainStore
	write DedupStore
}

// NewSuperseder builds the path over the two stores it needs, refusing a nil
// either. Both are the same generated Queries in practice; they are named
// separately because a read and a write into an immutable table are
// different permissions and it is worth being able to see which a caller was
// given.
func NewSuperseder(chain ChainStore, write DedupStore) (*Superseder, error) {
	if chain == nil || write == nil {
		return nil, ErrNoEvidenceStore
	}
	return &Superseder{chain: chain, write: write}, nil
}

// Record stores one report as whatever it is.
//
// The shape is one read and one write, and the decision between them is the
// DATABASE's both times. The read finds the transaction's current row - the
// tip of its chain, derived rather than stamped, because an immutable table
// cannot mark a row superseded. The write then offers the report with that
// tip as its predecessor, and the unique constraint over the content digest
// decides whether it is a change: an unchanged report collides with the row
// already in the chain and is swallowed, a changed one is written.
//
// Nothing here compares two reports field by field, and that absence is the
// design. A comparison in Go would be a second implementation of what the
// digest already decides, and the two would eventually disagree - as either
// a duplicate credit or a status change nobody noticed. What Go decides is
// only which row to name as predecessor.
//
// The three outcomes:
//
//   - [OutcomeFirstReport] - the network had never reported this
//     transaction; a root was written.
//   - [OutcomeSuperseded] - the facts changed; a new row names the old one,
//     which stays exactly where it was.
//   - [OutcomeUnchanged] - the same facts again; nothing was written, and
//     the rest of the window can still be persisted in the same transaction.
//
// A concurrent poller that superseded the same tip first is refused by
// [ErrSupersededConcurrently] rather than by a duplicate row: the chain
// allows one successor, so the loser of that race re-reads and tries again
// rather than forking the transaction's history.
func (s *Superseder) Record(ctx context.Context, retrieval Retrieval, report Reported) (Recorded, Outcome, error) {
	if err := retrieval.Validate(); err != nil {
		return Recorded{}, "", err
	}
	if err := report.Validate(); err != nil {
		return Recorded{}, "", err
	}

	predecessor, found, err := s.current(ctx, retrieval, report)
	if err != nil {
		return Recorded{}, "", err
	}

	row, err := s.write.InsertNetworkTransactionIfNew(ctx, evidenceRowIfNew(retrieval, report, predecessor))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The digest matched a row already in this transaction's chain.
		return Recorded{}, OutcomeUnchanged, nil
	case err != nil && supersededConcurrently(err):
		return Recorded{}, "", fmt.Errorf("%w: %s reporting %s: %w",
			ErrSupersededConcurrently, retrieval, strconv.Quote(report.ExternalID), err)
	case err != nil:
		return Recorded{}, "", fmt.Errorf("%w: %s reporting %s: %w",
			ErrEvidenceNotWritten, retrieval, strconv.Quote(report.ExternalID), err)
	}

	outcome := OutcomeFirstReport
	if found {
		outcome = OutcomeSuperseded
	}
	return Recorded{
		ID:            uuid.UUID(row.ID.Bytes),
		ContentDigest: row.ContentDigest,
		RetrievedAt:   row.RetrievedAt.Time,
	}, outcome, nil
}

// current finds the row a new report would supersede, answering a zero uuid
// and false when the network has never reported this transaction. No rows is
// a first report rather than a failure, which is why it is not an error here.
func (s *Superseder) current(ctx context.Context, retrieval Retrieval, report Reported) (pgtype.UUID, bool, error) {
	row, err := s.chain.GetCurrentNetworkTransaction(ctx, store.GetCurrentNetworkTransactionParams{
		NetworkID:  string(retrieval.Account.Network()),
		ExternalID: report.ExternalID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return pgtype.UUID{}, false, nil
	case err != nil:
		return pgtype.UUID{}, false, fmt.Errorf("%w: %s reading what was last said about %s: %w",
			ErrEvidenceNotWritten, retrieval, strconv.Quote(report.ExternalID), err)
	}
	return row.ID, true, nil
}
