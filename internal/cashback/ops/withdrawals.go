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
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// WithdrawalApprover is the one call this module makes into the payout
// module, named here per the boundary rules. *payout.Approvals satisfies it.
type WithdrawalApprover interface {
	Approve(ctx context.Context, approval payout.Approval) (payout.Payout, error)
}

// WithdrawalRefuser is the other answer to the same question.
// *payout.Rejections satisfies it.
//
// A second interface rather than a second method on the first, because the
// two are implemented by different types for a reason: approving reaches a
// payout rail and refusing does not, so one commits before it submits and
// the other is a single transaction. Folding them together would put a rail
// in the type that has no use for one.
type WithdrawalRefuser interface {
	Reject(ctx context.Context, rejection payout.Rejection) (payout.Rejected, error)
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
	h.writeJSON(w, r, body)
}

// rejectRequest is what an operator sends: the reason, and nothing else. Who
// refuses comes from the token, for the reason approving takes no body at
// all (C-4) - the difference is that a refusal genuinely has something for a
// client to say.
type rejectRequest struct {
	Reason string `json:"reason"`
}

// rejectResponse is what a refusal answers.
type rejectResponse struct {
	RequestID string `json:"request_id"`
	State     string `json:"state"`
	// DecidedBy is the operator this refusal rests on, echoed so they see
	// which account the server recorded.
	DecidedBy      string     `json:"decided_by"`
	DecidedAt      string     `json:"decided_at"`
	DecisionReason string     `json:"decision_reason"`
	ReleasedAmount amountJSON `json:"released_amount"`
	// ReleaseTransfer is the ledger movement that carried the money back,
	// distinct from the reservation that carried it out. Both are in the
	// response because a refusal is two facts - a decision and a movement -
	// and an operator checking one against the ledger needs the reference.
	ReleaseTransfer string `json:"release_transfer"`
}

// rejectWithdrawal implements POST /ops/withdrawals/{id}/reject.
func (h *Handler) rejectWithdrawal(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, "the withdrawal id is not a UUID")
		return
	}
	var req rejectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	reason := strings.TrimSpace(req.Reason)
	switch {
	case reason == "":
		platformhttp.Problem(w, http.StatusBadRequest,
			"a refusal records why (FR-061): supply a non-blank reason")
		return
	case len([]rune(reason)) > maxReasonRunes:
		platformhttp.Problem(w, http.StatusBadRequest,
			"the reason is longer than "+strconv.Itoa(maxReasonRunes)+" characters")
		return
	}

	refused, err := h.refusals.Reject(r.Context(), payout.Rejection{
		Request:  id,
		Operator: operatorFrom(r.Context()).ID,
		Reason:   reason,
	})
	switch {
	case errors.Is(err, payout.ErrNoSuchWithdrawal):
		platformhttp.Problem(w, http.StatusNotFound, "no such withdrawal request")
		return
	case errors.Is(err, payout.ErrNotAwaitingApproval):
		platformhttp.Problem(w, http.StatusConflict,
			"this withdrawal is no longer awaiting approval; reload the queue before deciding it")
		return
	case errors.Is(err, payout.ErrNothingReserved):
		// 409, and it deserves a look: reserved_transfer_ref is NOT NULL so
		// that every request has entries behind it, and one with none has a
		// balance nobody can put back.
		h.log.ErrorContext(r.Context(), "a withdrawal was refused and its reservation moved no entries",
			"withdrawal", id, "error", err)
		platformhttp.Problem(w, http.StatusConflict,
			"this withdrawal's reservation moved no entries, so there is nothing to release; it needs looking at rather than deciding")
		return
	case errors.Is(err, payout.ErrNoDecisionReason):
		platformhttp.Problem(w, http.StatusBadRequest,
			"a refusal records why (FR-061): supply a non-blank reason")
		return
	case errors.Is(err, payout.ErrNoReceivable):
		h.log.ErrorContext(r.Context(), "a withdrawal was refused and this deployment has not named the receivable",
			"withdrawal", id, "keys", "HOUSE_ACCOUNT_NETWORK_RECEIVABLE")
		platformhttp.Problem(w, http.StatusServiceUnavailable,
			"this deployment cannot release a reservation yet")
		return
	case errors.Is(err, payout.ErrNotRejected):
		h.log.WarnContext(r.Context(), "a withdrawal refusal was refused", "withdrawal", id, "error", err)
		platformhttp.Problem(w, http.StatusConflict,
			"this withdrawal cannot be refused as it stands")
		return
	case err != nil:
		h.internalError(w, r, "refusing a withdrawal", err)
		return
	}

	h.writeJSON(w, r, rejectResponse{
		RequestID:      refused.Request.ID.String(),
		State:          refused.Request.State.String(),
		DecidedBy:      refused.Request.DecidedBy.String(),
		DecidedAt:      stamp(refused.Request.DecidedAt),
		DecisionReason: refused.Request.DecisionReason,
		ReleasedAmount: amountJSON{
			Minor:    refused.Released.Minor,
			Currency: string(refused.Released.Currency),
		},
		ReleaseTransfer: refused.ReleaseTransfer,
	})
}

// WithdrawalSettler records a payment an operator made by hand.
// *payout.Settlements satisfies it.
//
// A third interface for the same reason there are two already: this one
// answers a question the other two cannot be asked. Approving and refusing
// DECIDE a request; this reports what happened to money afterwards, which is
// not a decision and does not belong on a type that makes them.
type WithdrawalSettler interface {
	Record(ctx context.Context, recording payout.Recording) (payout.Settlement, error)
}

// settleRequest is what an operator sends: what their bank called the
// transfer, and nothing else.
//
// Not the operator, for the reason no operator action on this surface takes
// one (C-4, FR-061): the acting human is the token subject, so "settle as
// somebody else" is not a request this endpoint can express. Not the amount
// either - the payout froze that at approval, and a settlement that could
// restate it would be a settlement that could pay a different sum.
type settleRequest struct {
	RailReference string `json:"rail_reference"`
}

// settleResponse is the payment as it now stands.
type settleResponse struct {
	PayoutID  string `json:"payout_id"`
	RequestID string `json:"request_id"`
	// RailReference is what the operator recorded, replacing the manual:
	// placeholder that meant nobody had made the transfer yet.
	RailReference string     `json:"rail_reference"`
	Amount        amountJSON `json:"amount"`
	State         string     `json:"state"`
	// SettledAt is never null here: a settled payout has an instant, tied
	// to the state by payout_settled_iff_settlement_time.
	SettledAt string `json:"settled_at"`
	// RequestState follows the payout, and is echoed because an operator
	// reading one screen should not have to reload another to learn that
	// the member's request is now paid.
	RequestState string `json:"request_state"`
}

// maxReferenceRunes bounds what a bank may be quoted as calling a transfer.
// Generous, because reference formats vary by rail and by country; bounded,
// because this reaches a member's screen and a log.
const maxReferenceRunes = 200

// settleWithdrawal implements POST /ops/withdrawals/{id}/settle.
//
// This is the manual rail's whole settlement path, and there is no automatic
// alternative to it: payout.Settlements sweeps the rails on a schedule, and
// the manual rail always answers "submitted" because a rail that guessed
// otherwise would report money as delivered on the strength of a clock. A
// person went to a bank; a person says so.
func (h *Handler) settleWithdrawal(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, "the withdrawal id is not a UUID")
		return
	}
	if h.settlements == nil {
		// 503 on this route alone. A deployment that cannot record a
		// settlement can still run its approval queue, and taking the
		// whole operator surface down over it would be the larger failure.
		h.log.ErrorContext(r.Context(), "a settlement was recorded and this deployment has nowhere to record it",
			"withdrawal", id)
		platformhttp.Problem(w, http.StatusServiceUnavailable,
			"this deployment cannot record settlements yet")
		return
	}
	var req settleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	reference := strings.TrimSpace(req.RailReference)
	switch {
	case reference == "":
		platformhttp.Problem(w, http.StatusBadRequest,
			"a settlement records what the bank called the transfer: supply a non-blank rail_reference")
		return
	case len([]rune(reference)) > maxReferenceRunes:
		platformhttp.Problem(w, http.StatusBadRequest,
			"the rail reference is longer than "+strconv.Itoa(maxReferenceRunes)+" characters")
		return
	}

	settled, err := h.settlements.Record(r.Context(), payout.Recording{
		Request:   id,
		Operator:  operatorFrom(r.Context()).ID,
		Reference: reference,
	})
	switch {
	case errors.Is(err, payout.ErrNoPayout):
		// No payment exists, so there is nothing that could have landed.
		platformhttp.Problem(w, http.StatusNotFound,
			"this withdrawal has no payout; it has not been approved")
		return
	case errors.Is(err, payout.ErrNoSuchWithdrawal):
		platformhttp.Problem(w, http.StatusNotFound, "no such withdrawal request")
		return
	case errors.Is(err, payout.ErrNotSubmitted):
		platformhttp.Problem(w, http.StatusConflict,
			"this payment is not waiting on a rail; reload before recording it")
		return
	case errors.Is(err, payout.ErrNotSettled):
		h.log.WarnContext(r.Context(), "a settlement was refused", "withdrawal", id, "error", err)
		platformhttp.Problem(w, http.StatusConflict,
			"this payment cannot be settled as it stands")
		return
	case err != nil:
		h.internalError(w, r, "recording a settlement", err)
		return
	}

	h.writeJSON(w, r, settleResponse{
		PayoutID:      settled.Payout.ID.String(),
		RequestID:     settled.Payout.Request.String(),
		RailReference: settled.Payout.RailReference,
		Amount: amountJSON{
			Minor:    settled.Payout.Amount.Minor,
			Currency: string(settled.Payout.Amount.Currency),
		},
		State:        settled.Payout.State.String(),
		SettledAt:    stamp(settled.Payout.SettledAt),
		RequestState: string(payout.StatePaid),
	})
}
