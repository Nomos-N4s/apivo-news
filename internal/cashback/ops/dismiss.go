// Closing a queue row: the operator decision this surface exists for
// (T059, FR-034, FR-061).
//
// A dismissal is the whole of what an operator may do to an unattributed
// report today. Attributing one - creating an entry with no click behind it
// - waits on the earnings module, which owns what a member's share is and
// how an entry reaches the ledger; there is no shortcut to it here, because
// an entry written without a share computation and a posting is a credit
// nobody can reconcile.

package ops

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// maxReasonRunes bounds the recorded reason. It is generous - a paragraph,
// not a form field - because the reason is the audit record and an operator
// cut off mid-sentence writes a worse one. The schema's own rule is only
// that it is not blank.
const maxReasonRunes = 2000

// maxBodyBytes bounds every request body here. The largest legal one is a
// reason, so this is orders of magnitude of headroom and exists only so a
// hostile body cannot be read into memory.
const maxBodyBytes = 64 << 10

var (
	// ErrNoSuchQueueRow reports an id that names no queue row. A queue row
	// is never deleted (0024), so this is an id that was never issued - a
	// client's mistake rather than a race, and the endpoint answers 404
	// rather than 409 so the two stay distinguishable to whoever is
	// debugging an operator interface.
	ErrNoSuchQueueRow = errors.New("ops: no such unattributed queue row")
	// ErrNotDismissed reports a dismissal that could not be recorded. It
	// wraps the failure unchanged: the resolution and the event it
	// publishes commit together, so a caller that swallowed this would
	// report a decision the database does not hold.
	ErrNotDismissed = errors.New("ops: the dismissal could not be recorded")
)

// Dismissal is one operator's decision to close a queue row: the row, the
// named human closing it, and why.
//
// The reason travels with the action rather than after it. FR-061 asks for
// a named human and a reason on every operator action, and an audit record
// written in a second step is a record of what somebody remembered.
type Dismissal struct {
	// ID is the queue row being closed.
	ID uuid.UUID
	// Operator is the authenticated caller, whose account id the row
	// records as resolved_by.
	Operator Operator
	// Reason is why. Never blank - the schema refuses one, and so does the
	// endpoint, earlier and with a better message.
	Reason string
}

// Dismissed is what the database recorded, read back rather than echoed.
// resolved_at has no supplied value to echo: the statement stamps it with
// the row's own clock, which is the instant an auditor reads.
type Dismissed struct {
	ID         uuid.UUID
	ReportID   uuid.UUID
	DetectedAt time.Time
	ResolvedBy uuid.UUID
	Reason     string
	ResolvedAt time.Time
}

// ClosedError reports a queue row that exists but is not open work, and
// says why. It wraps ErrNoLongerOpen, so a caller that only cares about the
// verdict matches that and a caller rendering an answer reads the detail.
type ClosedError struct {
	// ID is the row asked about.
	ID uuid.UUID
	// Why is what the database says about it now.
	Why ClosedReason
}

func (e ClosedError) Error() string {
	return "ops: unattributed queue row " + e.ID.String() + " is no longer open work: " + e.Why.detail()
}

// Unwrap makes the openness verdict matchable without knowing this type.
func (e ClosedError) Unwrap() error { return networks.ErrNoLongerOpen }

// ClosedReason says why a row a caller asked about is not open work. It is
// the difference between "somebody beat you to it" and "the network moved
// underneath you", which is the difference between an operator retrying and
// an operator reloading.
type ClosedReason struct {
	// Resolved reports that somebody has already closed it, and Reason is
	// what they wrote.
	Resolved bool
	Reason   string
	// Credited reports that an entry now cites the report this row names,
	// so the money has been decided.
	Credited bool
	// Superseded reports that the network has replaced the report this row
	// names; the successor is either attributed or queued in its own right.
	Superseded bool
}

// dismissRequest is the body of POST .../dismiss.
type dismissRequest struct {
	Reason string `json:"reason"`
}

// dismissResponse is the recorded resolution.
type dismissResponse struct {
	ID                   string `json:"id"`
	NetworkTransactionID string `json:"network_transaction_id"`
	DetectedAt           string `json:"detected_at"`
	ResolvedBy           string `json:"resolved_by"`
	Reason               string `json:"reason"`
	ResolvedAt           string `json:"resolved_at"`
}

// dismissUnattributed implements
// POST /api/v1/cashback/ops/unattributed/{id}/dismiss.
func (h *Handler) dismissUnattributed(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		// Not a 404: the address is well formed as a route, the id in it is
		// not a queue row's id, and saying so is more useful than pretending
		// to have looked.
		platformhttp.Problem(w, http.StatusBadRequest, "the queue row id is not a UUID")
		return
	}

	var req dismissRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	reason := strings.TrimSpace(req.Reason)
	switch {
	case reason == "":
		platformhttp.Problem(w, http.StatusBadRequest,
			"a dismissal records why (FR-061): supply a non-blank reason")
		return
	case len([]rune(reason)) > maxReasonRunes:
		platformhttp.Problem(w, http.StatusBadRequest,
			"the reason is longer than "+strconv.Itoa(maxReasonRunes)+" characters")
		return
	}

	dismissed, err := h.store.Dismiss(r.Context(), Dismissal{
		ID:       id,
		Operator: operatorFrom(r.Context()),
		Reason:   reason,
	})
	var closed ClosedError
	switch {
	case errors.Is(err, ErrNoSuchQueueRow):
		platformhttp.Problem(w, http.StatusNotFound, "no such unattributed queue row")
		return
	case errors.As(err, &closed):
		platformhttp.Problem(w, http.StatusConflict, closed.Why.detail())
		return
	// The openness verdict without a classification behind it. Reached only
	// if the row vanished between the two reads, which 0024 makes
	// impossible - so this is the answer that stays correct if that ever
	// stops being true.
	case errors.Is(err, networks.ErrNoLongerOpen):
		platformhttp.Problem(w, http.StatusConflict,
			"this row is no longer open work; reload the queue before deciding it")
		return
	case err != nil:
		h.internalError(w, r, "dismissing unattributed work", err)
		return
	}

	h.writeJSON(w, r, dismissResponse{
		ID:                   dismissed.ID.String(),
		NetworkTransactionID: dismissed.ReportID.String(),
		DetectedAt:           stamp(dismissed.DetectedAt),
		ResolvedBy:           dismissed.ResolvedBy.String(),
		Reason:               dismissed.Reason,
		ResolvedAt:           stamp(dismissed.ResolvedAt),
	})
}

// detail renders the 409's explanation. Every flag that is set is named:
// two of them can be true at once, and an operator told only the first
// would reload and find the row still unexplained.
func (c ClosedReason) detail() string {
	var why []string
	if c.Resolved {
		reason := "somebody has already closed it"
		if c.Reason != "" {
			reason += ": " + c.Reason
		}
		why = append(why, reason)
	}
	if c.Credited {
		why = append(why, "an entry now cites the report it names, so the money has been decided")
	}
	if c.Superseded {
		why = append(why, "the network has replaced the report it names")
	}
	if len(why) == 0 {
		// The three flags are the openness read's three conditions, so this
		// is unreachable while the two statements agree. Answering rather
		// than falling through keeps a future divergence a visible 409
		// instead of a silent success.
		why = append(why, "it is no longer open work")
	}
	return "this row cannot be dismissed: " + strings.Join(why, "; ") + ". Reload the queue before deciding it"
}
