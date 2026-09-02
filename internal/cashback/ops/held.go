// The held queue and its two decisions (T119, US7 scenario 3, FR-061).
//
// This file is the surface and only the surface. What a hold is, what a
// release and a rejection do to the money, and what each records live in
// the earnings module (holdrules.go, review.go); here is the shape on the
// wire, the statuses the module's answers map to, and the refusals the
// endpoint owes on its own: an id that is not one, a body that is not the
// endpoint's shape, and a blank reason - refused here, before anything is
// read, so the 400 names the field and the audit record is never
// half-written.
//
// C-4 is why the operator is never in the body. Who decided is the token
// subject and nothing else, so "release as somebody else" is not a request
// this endpoint can express.

package ops

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// HeldReviewer is what these endpoints need of the earnings module, named
// here per the boundary rules. *earnings.Reviews satisfies it.
type HeldReviewer interface {
	// Held returns one page of the queue, oldest first, starting after
	// the given position.
	Held(ctx context.Context, after earnings.HeldAfter, limit int) ([]earnings.HeldCredit, error)
	// Release moves a held credit to pending, recording who and why.
	Release(ctx context.Context, review earnings.Review) (earnings.Released, error)
	// Reject undoes a held credit with a reversing entry, recording who
	// and why.
	Reject(ctx context.Context, review earnings.Review) (earnings.Rejected, error)
}

// heldItem is one row of the queue on the wire.
type heldItem struct {
	ID                   string `json:"id"`
	AccountID            string `json:"account_id"`
	BrandID              string `json:"brand_id"`
	NetworkTransactionID string `json:"network_transaction_id"`
	// ClickID is null where an operator attributed the purchase by hand.
	ClickID *string `json:"click_id"`
	// HoldRule is the rule holding it and HoldReason what the rule said.
	HoldRule   string `json:"hold_rule"`
	HoldReason string `json:"hold_reason"`
	// Amount is the member's share, sitting in the held stage. Every
	// figure marshals as {minor, currency} (C-6).
	Amount    money.Amount `json:"amount"`
	HeldSince string       `json:"held_since"`
	// The report, in the network's own terms.
	NetworkID    string       `json:"network_id"`
	ExternalID   string       `json:"external_id"`
	ReportStatus string       `json:"report_status"`
	Sale         money.Amount `json:"sale"`
	Commission   money.Amount `json:"commission"`
	TransactedAt string       `json:"transacted_at"`
}

// heldPage is one page of the queue: the contract's list shape.
type heldPage struct {
	Items      []heldItem `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

// listHeld implements GET /api/v1/cashback/ops/held.
func (h *Handler) listHeld(w http.ResponseWriter, r *http.Request) {
	at, id, limit, detail, ok := parsePage(r.URL.Query(), heldCursors)
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}
	// One more than the page, so "is there another page?" is answered by
	// what came back rather than by a second query.
	rows, err := h.held.Held(r.Context(), earnings.HeldAfter{HeldSince: at, ID: id}, limit+1)
	if err != nil {
		h.internalError(w, r, "listing held credits", err)
		return
	}
	page := heldPage{Items: make([]heldItem, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		page.Items = append(page.Items, heldItemOf(row))
	}
	if len(rows) > limit {
		last := rows[limit-1].After()
		next := encodeCursor(heldCursors, last.HeldSince, last.ID)
		page.NextCursor = &next
	}
	h.writeJSON(w, r, page)
}

// heldItemOf renders one queue row for the wire.
func heldItemOf(row earnings.HeldCredit) heldItem {
	item := heldItem{
		ID:                   row.ID.String(),
		AccountID:            row.Member.String(),
		BrandID:              row.Brand,
		NetworkTransactionID: row.Report.String(),
		HoldRule:             row.Rule,
		HoldReason:           row.Reason,
		Amount:               row.Amount,
		HeldSince:            stamp(row.HeldSince),
		NetworkID:            string(row.Network),
		ExternalID:           row.ExternalID,
		ReportStatus:         string(row.ReportStatus),
		Sale:                 row.Sale,
		Commission:           row.Commission,
		TransactedAt:         stamp(row.TransactedAt),
	}
	if row.Click != uuid.Nil {
		click := row.Click.String()
		item.ClickID = &click
	}
	return item
}

// heldDecisionRequest is the body of POST .../held/{id}/release and
// .../reject: the reason, and nothing else. The operator is the token.
type heldDecisionRequest struct {
	Reason string `json:"reason"`
}

// heldReleaseResponse is the recorded release, read back rather than echoed.
type heldReleaseResponse struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	// State is where the credit now sits: pending. HoldRule is the rule it
	// was held under, which the row no longer carries.
	State             string       `json:"state"`
	HoldRule          string       `json:"hold_rule"`
	Amount            money.Amount `json:"amount"`
	ReleasedBy        string       `json:"released_by"`
	Reason            string       `json:"reason"`
	LedgerTransferRef string       `json:"ledger_transfer_ref"`
	ReleasedAt        string       `json:"released_at"`
}

// heldRejectResponse is the recorded rejection: the credit, left as it was,
// and the reversing entry that undoes it.
type heldRejectResponse struct {
	ID              string       `json:"id"`
	ReversalEntryID string       `json:"reversal_entry_id"`
	AccountID       string       `json:"account_id"`
	HoldRule        string       `json:"hold_rule"`
	Amount          money.Amount `json:"amount"`
	RejectedBy      string       `json:"rejected_by"`
	Reason          string       `json:"reason"`
	RejectedAt      string       `json:"rejected_at"`
}

// releaseHeld implements POST /api/v1/cashback/ops/held/{id}/release.
func (h *Handler) releaseHeld(w http.ResponseWriter, r *http.Request) {
	review, ok := heldReview(w, r, "a release")
	if !ok {
		return
	}
	released, err := h.held.Release(r.Context(), review)
	if h.heldRefused(w, r, "releasing a held credit", err) {
		return
	}
	h.writeJSON(w, r, heldReleaseResponse{
		ID:                released.Entry.ID.String(),
		AccountID:         released.Entry.Member.String(),
		State:             string(released.Entry.State),
		HoldRule:          released.Rule,
		Amount:            released.Entry.Amount,
		ReleasedBy:        released.ReleasedBy.String(),
		Reason:            released.Reason,
		LedgerTransferRef: string(released.Transfer),
		ReleasedAt:        stamp(released.At),
	})
}

// rejectHeld implements POST /api/v1/cashback/ops/held/{id}/reject.
func (h *Handler) rejectHeld(w http.ResponseWriter, r *http.Request) {
	review, ok := heldReview(w, r, "a rejection")
	if !ok {
		return
	}
	rejected, err := h.held.Reject(r.Context(), review)
	if h.heldRefused(w, r, "rejecting a held credit", err) {
		return
	}
	h.writeJSON(w, r, heldRejectResponse{
		ID:              rejected.Credit.ID.String(),
		ReversalEntryID: rejected.Reversal.ID.String(),
		AccountID:       rejected.Credit.Member.String(),
		HoldRule:        rejected.Rule,
		Amount:          rejected.Credit.Amount,
		RejectedBy:      rejected.RejectedBy.String(),
		Reason:          rejected.Reason,
		RejectedAt:      stamp(rejected.At),
	})
}

// heldReview reads the decision a request carries - the id in the path,
// the reason in the body, the operator from the token - answering the 400
// itself for anything it refuses. what names the action in the refusal.
func heldReview(w http.ResponseWriter, r *http.Request, what string) (earnings.Review, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, "the entry id is not a UUID")
		return earnings.Review{}, false
	}
	var req heldDecisionRequest
	if !decodeJSON(w, r, &req) {
		return earnings.Review{}, false
	}
	reason := strings.TrimSpace(req.Reason)
	switch {
	case reason == "":
		platformhttp.Problem(w, http.StatusBadRequest,
			what+" records why (FR-061): supply a non-blank reason")
		return earnings.Review{}, false
	case len([]rune(reason)) > maxReasonRunes:
		platformhttp.Problem(w, http.StatusBadRequest,
			"the reason is longer than "+strconv.Itoa(maxReasonRunes)+" characters")
		return earnings.Review{}, false
	}
	return earnings.Review{Entry: id, Operator: operatorFrom(r.Context()).ID, Reason: reason}, true
}

// heldRefused maps the earnings module's answers to statuses, answering the
// problem itself and reporting whether it did.
func (h *Handler) heldRefused(w http.ResponseWriter, r *http.Request, doing string, err error) bool {
	var notHeld earnings.NotHeldError
	switch {
	case err == nil:
		return false
	case errors.Is(err, earnings.ErrInvalidReview):
		platformhttp.Problem(w, http.StatusBadRequest, detailOf(err, earnings.ErrInvalidReview))
	case errors.Is(err, earnings.ErrNoSuchEntry):
		platformhttp.Problem(w, http.StatusNotFound, "no such entry; entries are never deleted, so this id was never issued")
	case errors.As(err, &notHeld):
		platformhttp.Problem(w, http.StatusConflict,
			"this credit is "+string(notHeld.State)+", not held; reload the queue before deciding it")
	case errors.Is(err, earnings.ErrAlreadyRejected):
		platformhttp.Problem(w, http.StatusConflict,
			"this credit was already rejected: a reversing entry undoes it and the money has gone back; reload the queue")
	case errors.Is(err, earnings.ErrEntryMoved):
		platformhttp.Problem(w, http.StatusConflict,
			"somebody moved this credit between the read and the decision; reload the queue before deciding it")
	case errors.Is(err, earnings.ErrNoReceivable):
		// The one 503 on this surface: the deployment cannot move money
		// yet, and the queue stays readable so an operator can see what
		// is waiting on it.
		platformhttp.Problem(w, http.StatusServiceUnavailable,
			"this deployment has not named HOUSE_ACCOUNT_NETWORK_RECEIVABLE, so no credit can be released or rejected")
	default:
		h.internalError(w, r, doing, err)
	}
	return true
}
