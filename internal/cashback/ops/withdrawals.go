// The approval queue's one decision: releasing a withdrawal (T092, C-4, C-5).
//
// This file is the endpoint. What an approval DOES - the two commits, the
// claimed idempotency key, the rail - belongs to the payout module and stays
// there; this module owns the operator surface and the decision, exactly as
// it owns the dismissal without owning the unattributed queue.
//
// C-4 is why the operator is never in the body. The approver is the token
// subject and nothing else, so "approve as somebody else" is not a request
// this endpoint can express - and the database checks the role behind that
// subject in payout_insert_guard, with a locking read, so a demotion racing
// an approval cannot slip past.

package ops

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// WithdrawalApprover is the one call this module makes into the payout
// module, named here per the boundary rules. *payout.Approvals satisfies it.
type WithdrawalApprover interface {
	Approve(ctx context.Context, approval payout.Approval) (payout.Payout, error)
}

// approveResponse is what an operator reads back: the payment that now
// exists, and the evidence it rests on.
type approveResponse struct {
	PayoutID  string `json:"payout_id"`
	RequestID string `json:"request_id"`
	// ApprovedBy is the operator this payment rests on (C-4). Echoed rather
	// than left implicit, so an operator sees which account the server
	// recorded rather than which one they believe they are.
	ApprovedBy string `json:"approved_by"`
	// IdempotencyKey is the database's own derivation from the request
	// (C-5, D8). It is in the response because it is what a retry must
	// reuse, and an operator chasing a payment on the rail's side searches
	// by it.
	IdempotencyKey string     `json:"idempotency_key"`
	Amount         amountJSON `json:"amount"`
	Rail           string     `json:"rail"`
	// RailReference is null until the rail has answered. A submitted payout
	// without one is not broken: it is the state a retry picks up (FR-053).
	RailReference *string `json:"rail_reference"`
	State         string  `json:"state"`
	SubmittedAt   string  `json:"submitted_at"`
}

// amountJSON is one figure as the contract spells it, the shape C-6 mandates
// everywhere.
type amountJSON struct {
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
}

// approveWithdrawal implements POST /ops/withdrawals/{id}/approve.
func (h *Handler) approveWithdrawal(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		// Not a 404, for the reason dismissUnattributed gives: the address
		// is well formed as a route and the id in it is not a request's.
		platformhttp.Problem(w, http.StatusBadRequest, "the withdrawal id is not a UUID")
		return
	}
	// No body. An approval records who and when, and both come from the
	// token and the clock; there is nothing for a client to say. Refusing
	// one that arrives anyway keeps a future field from being silently
	// ignored on the endpoint where that would matter most.
	if !emptyBody(w, r) {
		return
	}

	paid, err := h.approvals.Approve(r.Context(), payout.Approval{
		Request:  id,
		Operator: operatorFrom(r.Context()).ID,
	})
	switch {
	case errors.Is(err, payout.ErrNoSuchWithdrawal):
		platformhttp.Problem(w, http.StatusNotFound, "no such withdrawal request")
		return
	case errors.Is(err, payout.ErrNotAwaitingApproval), errors.Is(err, payout.ErrAlreadyApproved):
		platformhttp.Problem(w, http.StatusConflict,
			"this withdrawal is no longer awaiting approval; reload the queue before deciding it")
		return
	case errors.Is(err, payout.ErrBrandUnresolved):
		// 409 rather than 500: nothing is broken in this request, and the
		// request is not payable as it stands. An operator can act on it -
		// by splitting it - which is what makes it a conflict.
		platformhttp.Problem(w, http.StatusConflict,
			"the entries this withdrawal reserved were not all earned under one brand, so it cannot be paid as one payout")
		return
	case errors.Is(err, payout.ErrRailRetryable), errors.Is(err, payout.ErrRailTerminal):
		// 502, and the APPROVAL STANDS. The payout row is committed and its
		// key is fixed, so this says the rail did not take it - not that
		// the decision did not happen. An operator told otherwise would
		// approve it again.
		h.log.ErrorContext(r.Context(), "a withdrawal was approved and the rail refused it",
			"withdrawal", id, "error", err)
		platformhttp.Problem(w, http.StatusBadGateway,
			"the approval is recorded and the payout exists, but the rail did not accept it; it will be retried under the same key")
		return
	case errors.Is(err, payout.ErrNotApproved):
		// Everything the database refused, C-4's role check among it. 409
		// rather than 500: these are answers about this request, not
		// failures of the system.
		h.log.WarnContext(r.Context(), "a withdrawal approval was refused", "withdrawal", id, "error", err)
		platformhttp.Problem(w, http.StatusConflict,
			"this withdrawal cannot be approved as it stands; check that the destination is verified and matches this deployment's rail, and that you hold the operator role")
		return
	case err != nil:
		h.internalError(w, r, "approving a withdrawal", err)
		return
	}

	body := approveResponse{
		PayoutID:       paid.ID.String(),
		RequestID:      paid.Request.String(),
		ApprovedBy:     paid.ApprovedBy.String(),
		IdempotencyKey: paid.IdempotencyKey,
		Amount:         amountJSON{Minor: paid.Amount.Minor, Currency: string(paid.Amount.Currency)},
		Rail:           paid.Rail.String(),
		State:          paid.State.String(),
		SubmittedAt:    stamp(paid.SubmittedAt),
	}
	if paid.RailReference != "" {
		reference := paid.RailReference
		body.RailReference = &reference
	}
	h.writeJSON(w, r, http.StatusOK, body)
}
