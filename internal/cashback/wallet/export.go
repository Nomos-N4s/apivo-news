// A member's whole cashback history, in one answer (T081, FR-003).
//
// The right to leave and the right to take your record with you are one
// requirement in FR-003, and they are one requirement for a reason: a member
// deciding whether to go should not have to choose between keeping their
// history and ending their participation.
//
// So this is deliberately NOT a page. GET /wallet/entries pages because a
// member reads their history a screen at a time; an export is read by a
// spreadsheet, an accountant or a subject-access request, and a partial one
// is worse than none - it is a document that looks complete and is not.
// Everything below follows from that: the whole history is assembled before
// a byte of it is written, and a history too large to assemble is refused
// rather than cut short.

package wallet

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// MaxExportEntries bounds one export.
//
// It exists so that a history too large to hold is REFUSED rather than
// truncated, and the number is chosen to be one no member reaches: fifty
// thousand entries is a purchase every day for a hundred and thirty years.
// If it is ever hit, the answer is an asynchronous export a member is
// notified about, not a larger number here - and the refusal is what will
// say so, where a silently shortened file would say nothing at all.
const MaxExportEntries = 50_000

// exportPageSize is how much of the history is read at a time. It is
// MaxPageSize because that is the largest page the query is built and
// budgeted for; the page size is invisible in the answer, which is one
// document either way.
const exportPageSize = MaxPageSize

var (
	// ErrNoHistoryToExport reports an exporter built with nothing to read
	// from.
	ErrNoHistoryToExport = errors.New("wallet: an export needs a history to read")
	// ErrExportTooLarge reports a history above MaxExportEntries.
	//
	// A refusal rather than a truncation, and that is the whole design of
	// this file. A CSV cut short is indistinguishable from a complete
	// shorter one - there is no closing bracket to be missing, no length to
	// disagree with - so a member handed one would have a file they believe
	// is their record and is not. Refusing says what happened.
	ErrExportTooLarge = errors.New("wallet: this history is too large to export in one document")
)

// ExportRequest is one ask for a member's whole history.
//
// There is deliberately no state filter and no limit. An export is the
// complete record or it is not an export, and a filtered one is a document
// a member may later rely on as complete. The handler refuses the
// parameters outright rather than ignoring them, so a client asking for
// something narrower learns that it did not get it.
type ExportRequest struct {
	// Member is whose history it is. Taken from the token by the handler
	// and never from the request.
	Member uuid.UUID
	// Language is the BCP-47 primary subtag to name merchants in. Empty
	// asks for none, and every name then comes back labelled as a fallback.
	Language string
}

// Exports assembles a member's complete history.
type Exports struct {
	history *History
}

// NewExports builds the exporter, refusing one with nothing to read.
func NewExports(history *History) (*Exports, error) {
	if history == nil {
		return nil, ErrNoHistoryToExport
	}
	return &Exports{history: history}, nil
}

// All reads the member's entire history, newest first.
//
// It walks the same keyset pages GET /wallet/entries serves, which is what
// keeps one query shape behind both endpoints: a second unbounded query
// would be a second copy of a six-table join, and the copy is the one that
// would drift.
//
// Keyset paging is also what makes the walk correct while the history
// GROWS. Entries are inserted as a member reads - a poll runs, a network
// confirms - and each page continues from the last row's own position
// rather than from an offset, so a row arriving mid-walk cannot push
// another out of the export or into it twice. What such a row CAN do is
// miss the export: it is newer than the page already written, and the walk
// only ever moves backwards. That is the honest boundary for a document
// stamped with the moment it was taken, and it is why the answer carries
// that moment.
// A member of uuid.Nil is refused, by [History.Page] rather than here.
// There is no second check for it: two guards for one condition is one that
// can be changed while the other keeps the test green, and the page's is the
// one every caller already goes through.
func (e *Exports) All(ctx context.Context, req ExportRequest) ([]Entry, error) {
	var (
		history []Entry
		cursor  string
	)
	for {
		page, err := e.history.Page(ctx, PageRequest{
			Member:   req.Member,
			Language: req.Language,
			Limit:    exportPageSize,
			Cursor:   cursor,
		})
		if err != nil {
			return nil, err
		}
		history = append(history, page.Entries...)
		if len(history) > MaxExportEntries {
			return nil, fmt.Errorf("%w: %s has more than %d entries",
				ErrExportTooLarge, req.Member, MaxExportEntries)
		}
		if page.NextCursor == "" {
			return history, nil
		}
		cursor = page.NextCursor
	}
}
