// The reconciliation endpoints (T113, US6): importing a statement, reading
// the differences it produced, and deciding one.
//
// This file is the surface and only the surface. What an import is, how the
// differences are derived and what a verdict means live in statement.go,
// differences.go and resolve.go; here is the shape on the wire, the statuses
// the store's answers map to, and the two refusals the endpoint owes on its
// own: a body that is not the endpoint's shape, and an id that is not one.

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// maxStatementBytes bounds an import body. A statement carries a line per
// transaction the network paid, so it is the one operator body that can be
// large; four MiB is tens of thousands of lines, and anything past it is not
// a month's statement.
const maxStatementBytes = 4 << 20

// ReconciliationStore is what these endpoints need of a store, named here
// per the boundary rules. One interface for the four, because one type
// answers them all - *PGStore - and because the import endpoint needs two of
// them in sequence.
type ReconciliationStore interface {
	StatementImporter
	DifferenceDetector
	DifferenceResolver
	// ListDifferences returns one page of a run's differences, oldest
	// first, starting after the given position; ErrNoSuchRun for a run
	// nobody imported.
	ListDifferences(ctx context.Context, run uuid.UUID, after DifferenceAfter, limit int) ([]ListedDifference, error)
}

// periodJSON is a statement period on the wire, both ends RFC 3339.
type periodJSON struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// importRequest is the body of POST .../reconciliation/runs.
type importRequest struct {
	NetworkAccountID string     `json:"network_account_id"`
	Period           periodJSON `json:"period"`
	// Statement is the network's document, verbatim; its shape is
	// [Statement]'s to judge, not this file's.
	Statement json.RawMessage `json:"statement"`
}

// detectionJSON is what the import's detection pass found and recorded.
type detectionJSON struct {
	Found    int `json:"found"`
	Recorded int `json:"recorded"`
}

// importResponse is the run, as recorded, and what detection made of it.
type importResponse struct {
	RunID            string        `json:"run_id"`
	NetworkAccountID string        `json:"network_account_id"`
	NetworkID        string        `json:"network_id"`
	Period           periodJSON    `json:"period"`
	Lines            int           `json:"lines"`
	StatementDigest  string        `json:"statement_digest"`
	ImportedBy       string        `json:"imported_by"`
	ImportedAt       string        `json:"imported_at"`
	AlreadyImported  bool          `json:"already_imported"`
	Differences      detectionJSON `json:"differences"`
}

// importStatement implements POST /api/v1/cashback/ops/reconciliation/runs.
//
// Two transactions, deliberately: the import commits, then detection runs
// over what was committed. A detection that fails leaves the run in place -
// it is the counterparty's statement and there is nothing wrong with it -
// and a retry of the same request finds the run already imported and runs
// detection again, which records only what the first pass did not.
func (h *Handler) importStatement(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if !decodeJSONUpTo(w, r, &req, maxStatementBytes) {
		return
	}
	account, err := uuid.Parse(req.NetworkAccountID)
	if err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, "network_account_id is not a UUID")
		return
	}
	period, detail, ok := parsePeriod(req.Period)
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}

	imported, err := h.reconciliation.ImportStatement(r.Context(), Statement{
		Account:  account,
		Period:   period,
		Raw:      req.Statement,
		Operator: operatorFrom(r.Context()),
	})
	switch {
	case errors.Is(err, ErrInvalidStatement):
		platformhttp.Problem(w, http.StatusBadRequest, detailOf(err, ErrInvalidStatement))
		return
	case errors.Is(err, ErrNoSuchNetworkAccount):
		platformhttp.Problem(w, http.StatusNotFound, "no such publisher account")
		return
	case err != nil:
		h.internalError(w, r, "importing a statement", err)
		return
	}

	detection, err := h.reconciliation.DetectDifferences(r.Context(), imported.ID)
	if err != nil {
		// The run is committed and says so in the log; the client is told
		// the request failed, and a retry of the same statement is the
		// same run with detection run again.
		h.log.ErrorContext(r.Context(), "a statement was imported and its differences could not be detected",
			"run", imported.ID, "error", err)
		platformhttp.Problem(w, http.StatusInternalServerError, "")
		return
	}

	status := http.StatusCreated
	if imported.AlreadyImported {
		status = http.StatusOK
	}
	h.writeJSONStatus(w, r, status, importResponse{
		RunID:            imported.ID.String(),
		NetworkAccountID: imported.Account.String(),
		NetworkID:        imported.Network.String(),
		Period:           periodJSON{Start: stamp(imported.Period.Start), End: stamp(imported.Period.End)},
		Lines:            imported.Lines,
		StatementDigest:  imported.Digest,
		ImportedBy:       imported.ImportedBy.String(),
		ImportedAt:       stamp(imported.ImportedAt),
		AlreadyImported:  imported.AlreadyImported,
		Differences:      detectionJSON{Found: len(detection.Found), Recorded: detection.Recorded},
	})
}

// parsePeriod reads the period, answering the 400's detail when an end is
// missing or is not a timestamp. Ordering is [Statement.Validate]'s to
// refuse, with the rest of what a statement must be.
func parsePeriod(p periodJSON) (Period, string, bool) {
	start, err := time.Parse(time.RFC3339Nano, p.Start)
	if err != nil {
		return Period{}, "period.start is not an RFC 3339 timestamp", false
	}
	end, err := time.Parse(time.RFC3339Nano, p.End)
	if err != nil {
		return Period{}, "period.end is not an RFC 3339 timestamp", false
	}
	return Period{Start: start, End: end}, "", true
}

// resolutionJSON is a recorded decision on a listed row.
type resolutionJSON struct {
	Resolution string `json:"resolution"`
	ResolvedBy string `json:"resolved_by"`
	Reason     string `json:"reason"`
	ResolvedAt string `json:"resolved_at"`
}

// differenceItem is one row of the queue on the wire.
type differenceItem struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// NetworkTransactionID is the report named, null for money matching
	// no report; TransactionID is the network's own id either way.
	NetworkTransactionID *string `json:"network_transaction_id"`
	TransactionID        string  `json:"transaction_id"`
	// Expected and Actual are absent by kind; Delta is paid less owed. All
	// three marshal as {minor, currency} (C-6).
	Expected   *money.Amount   `json:"expected"`
	Actual     *money.Amount   `json:"actual"`
	Delta      money.Amount    `json:"delta"`
	DetectedAt string          `json:"detected_at"`
	Superseded bool            `json:"superseded"`
	Resolution *resolutionJSON `json:"resolution"`
}

// differencePage is one page of a run's queue: the contract's list shape.
type differencePage struct {
	Items      []differenceItem `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

// listDifferences implements
// GET /api/v1/cashback/ops/reconciliation/runs/{id}/differences.
func (h *Handler) listDifferences(w http.ResponseWriter, r *http.Request) {
	run, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, "the run id is not a UUID")
		return
	}
	at, id, limit, detail, ok := parsePage(r.URL.Query(), differenceCursors)
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}
	// One more than the page, so "is there another page?" is answered by
	// what came back rather than by a second query.
	rows, err := h.reconciliation.ListDifferences(r.Context(), run, DifferenceAfter{DetectedAt: at, ID: id}, limit+1)
	switch {
	case errors.Is(err, ErrNoSuchRun):
		platformhttp.Problem(w, http.StatusNotFound, "no such reconciliation run")
		return
	case err != nil:
		h.internalError(w, r, "listing a run's differences", err)
		return
	}
	page := differencePage{Items: make([]differenceItem, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		page.Items = append(page.Items, differenceItemOf(row))
	}
	if len(rows) > limit {
		last := rows[limit-1].After()
		next := encodeCursor(differenceCursors, last.DetectedAt, last.ID)
		page.NextCursor = &next
	}
	h.writeJSON(w, r, page)
}

// differenceItemOf renders one listed row for the wire.
func differenceItemOf(row ListedDifference) differenceItem {
	item := differenceItem{
		ID:            row.ID.String(),
		Kind:          string(row.Kind),
		TransactionID: row.TransactionID,
		Expected:      row.Expected,
		Actual:        row.Actual,
		Delta:         row.Delta,
		DetectedAt:    stamp(row.DetectedAt),
		Superseded:    row.Superseded,
	}
	if row.Report != uuid.Nil {
		report := row.Report.String()
		item.NetworkTransactionID = &report
	}
	if row.Resolution != nil {
		item.Resolution = &resolutionJSON{
			Resolution: string(row.Resolution.Verdict),
			ResolvedBy: row.Resolution.ResolvedBy.String(),
			Reason:     row.Resolution.Reason,
			ResolvedAt: stamp(row.Resolution.ResolvedAt),
		}
	}
	return item
}

// resolveRequest is the body of POST .../differences/{id}/resolve.
type resolveRequest struct {
	Resolution string `json:"resolution"`
	Reason     string `json:"reason"`
}

// resolveResponse is the recorded decision.
type resolveResponse struct {
	ID         string `json:"id"`
	RunID      string `json:"run_id"`
	Kind       string `json:"kind"`
	Resolution string `json:"resolution"`
	ResolvedBy string `json:"resolved_by"`
	Reason     string `json:"reason"`
	ResolvedAt string `json:"resolved_at"`
}

// resolveDifference implements
// POST /api/v1/cashback/ops/reconciliation/differences/{id}/resolve.
func (h *Handler) resolveDifference(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, "the difference id is not a UUID")
		return
	}
	var req resolveRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	resolution := Resolution{
		ID:       id,
		Verdict:  Verdict(req.Resolution),
		Reason:   req.Reason,
		Operator: operatorFrom(r.Context()),
	}
	// Refused here, before the store, so the 400 names the field and the
	// audit record is never half-written (FR-061).
	if err := resolution.Validate(); err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, detailOf(err, ErrInvalidResolution))
		return
	}

	resolved, err := h.reconciliation.ResolveDifference(r.Context(), resolution)
	var already AlreadyResolvedError
	switch {
	case errors.Is(err, ErrInvalidResolution):
		platformhttp.Problem(w, http.StatusBadRequest, detailOf(err, ErrInvalidResolution))
		return
	case errors.Is(err, ErrNoSuchDifference):
		platformhttp.Problem(w, http.StatusNotFound, "no such difference; rows are never deleted, so this id was never issued")
		return
	case errors.As(err, &already):
		platformhttp.Problem(w, http.StatusConflict,
			"this difference was already resolved as "+string(already.Verdict)+" by "+already.By.String()+
				" at "+stamp(already.At)+" ("+already.Reason+"); reload the queue before deciding it")
		return
	case err != nil:
		h.internalError(w, r, "resolving a difference", err)
		return
	}

	h.writeJSON(w, r, resolveResponse{
		ID:         resolved.ID.String(),
		RunID:      resolved.Run.String(),
		Kind:       string(resolved.Kind),
		Resolution: string(resolved.Verdict),
		ResolvedBy: resolved.ResolvedBy.String(),
		Reason:     resolved.Reason,
		ResolvedAt: stamp(resolved.ResolvedAt),
	})
}

// detailOf is the part of a refusal that is about the request: the text
// after the sentinel it wraps. The sentinel names this module; the rest
// names the field.
func detailOf(err error, sentinel error) string {
	return strings.TrimPrefix(err.Error(), sentinel.Error()+": ")
}
