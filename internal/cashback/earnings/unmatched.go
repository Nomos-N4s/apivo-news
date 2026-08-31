// Recording that a network named a reference nothing answers to (T067,
// FR-034's other half).
//
// The queue this writes to is the one the networks module writes to, and
// that is deliberate: an operator looking at unattributed money should see
// one list, not two, because the two kinds are the same problem seen from
// different sides. What differs is who can ask the question. The networks
// module can see that a report carried no reference at all, because that is
// a column of its own; only this path can see that a reference matched no
// CLICK, because cashback.click belongs here.

package earnings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
)

var (
	// ErrNoUnmatchedStore reports a matcher built with nowhere to record what
	// it found. Refused at construction rather than at the first report: a
	// matcher that discovered it mid-window has already read reports it
	// cannot queue, and those are transactions nobody will look for again.
	ErrNoUnmatchedStore = errors.New("earnings: recording an unmatched reference needs a store to write to")
	// ErrNotQueued reports an observation that could not be written. It wraps
	// the database's refusal unchanged, because the observation and whatever
	// advanced past it commit together: a caller that swallowed this would
	// move a cursor past money that is in no queue at all.
	ErrNotQueued = errors.New("earnings: the report could not be queued as unattributed")
)

// UnmatchedStore is the one write this file needs, named here rather than
// taken as *store.Queries so the caller keeps the transaction boundary. That
// is not a convenience: the observation and the cursor that follows it are
// one commit or none.
type UnmatchedStore interface {
	RecordUnmatchedReference(ctx context.Context, networkTransactionID pgtype.UUID) (store.RecordUnmatchedReferenceRow, error)
}

// Unmatched is what the DATABASE decided about one observation: the queue row
// it wrote and the report that row names.
//
// The row is read back rather than assumed for the reason the detection
// instant is: the statement decides whether there was anything to record at
// all, and a caller echoing its own inputs would report a row that does not
// exist.
type Unmatched struct {
	// ID is the queue row.
	ID uuid.UUID
	// ReportID is the evidence row it was recorded about.
	ReportID uuid.UUID
	// DetectedAt is when the row says it was noticed. Read back rather than
	// stamped here, so what is announced about the observation and what an
	// operator sees in the queue name one moment.
	DetectedAt time.Time
}

// queueUnmatched records that this report's reference matched no click,
// reporting whether it wrote a row.
//
// A function over the store rather than a method on the matcher, so that
// what this file describes - the write, and the three different silences it
// treats alike - can be read and changed without the resolving half.
//
// Nothing here reads a click. The statement's own WHERE clause decides, for
// the reason the content digest is computed by a trigger rather than
// supplied: the stored columns are the authority, and they are the same ones
// entry_evidence_guard reads when it later decides whether a credit may omit
// its click. A second implementation in Go is the place the two would
// eventually disagree - as a member never paid, or as money in no queue.
//
// False and no error covers three outcomes that mean the same thing here:
// the report carried no reference at all, its reference matched a click, or
// the observation was already recorded. The last is the ordinary path after
// a crash, when a window is re-read.
func queueUnmatched(ctx context.Context, unmatched UnmatchedStore, reportID uuid.UUID) (Unmatched, bool, error) {
	row, err := unmatched.RecordUnmatchedReference(ctx, pgtype.UUID{Bytes: reportID, Valid: true})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Unmatched{}, false, nil
	case err != nil:
		return Unmatched{}, false, fmt.Errorf("%w: report %s: %w", ErrNotQueued, reportID, err)
	}
	return Unmatched{
		ID:         uuid.UUID(row.ID.Bytes),
		ReportID:   uuid.UUID(row.NetworkTransactionID.Bytes),
		DetectedAt: row.DetectedAt.Time,
	}, true, nil
}
