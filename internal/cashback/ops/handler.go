package ops

import (
	"log/slog"
	"maps"
	"net/http"
	"slices"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// Prefix is where this module's route table is mounted, and the prefix
// every pattern below shares. It is exported so the composition root
// mounts exactly the string the catch-all claims: two copies of it would
// be one deployment away from a subtree that answers in text/plain because
// nothing under it was ever reached.
//
// It is a subtree of the cashback base path rather than the whole of it:
// the member endpoints under /api/v1/cashback/ are a different surface
// with a different gate, and this module must not claim their 404s.
const Prefix = "/api/v1/cashback/ops/"

// Handler serves the cashback operator endpoints. Build it with NewHandler.
type Handler struct {
	log   *slog.Logger
	store UnattributedStore
	// approvals releases withdrawals. A second dependency rather than a
	// method on store, because the two answer to different modules: the
	// queue is the networks module's evidence and an approval is the payout
	// module's money.
	approvals WithdrawalApprover
	// refusals is the other answer to the same question; see
	// [WithdrawalRefuser] for why it is a separate dependency.
	refusals WithdrawalRefuser
	// settlements records a payment an operator made by hand. May be nil:
	// see [Handler.settleWithdrawal] for why a deployment without one
	// answers 503 on that route alone rather than not serving the queue.
	settlements WithdrawalSettler
	// reconciliation imports statements, derives and lists their
	// differences, and records decisions on them (US6). One dependency for
	// the four because one type answers them all, and because the import
	// endpoint needs two of them in sequence.
	reconciliation ReconciliationStore
	auth           OperatorAuthenticator
	// allow is the 405 classifier, derived from routes() in NewHandler so
	// it cannot drift from what is actually registered.
	allow platformhttp.AllowTable
}

// NewHandler builds the operator route table as an http.Handler for the
// composition root to mount. Every route sits behind the requireOperator
// gate - authentication wraps the whole table, so a future route cannot be
// added unauthenticated by omission.
func NewHandler(log *slog.Logger, store UnattributedStore, approvals WithdrawalApprover, refusals WithdrawalRefuser, settlements WithdrawalSettler, reconciliation ReconciliationStore, auth OperatorAuthenticator) http.Handler {
	h := &Handler{
		log: log, store: store, approvals: approvals, refusals: refusals,
		settlements: settlements, reconciliation: reconciliation, auth: auth,
	}
	h.allow = platformhttp.NewAllowTable(slices.Collect(maps.Keys(h.routes())))
	mux := http.NewServeMux()
	for pattern, handler := range h.routes() {
		mux.HandleFunc(pattern, handler)
	}
	// Every error under this prefix is problem+json, including the ones
	// nobody wrote a handler for - the same convention the reader,
	// editorial and account modules hold, so the API has one error shape
	// rather than one per module plus ServeMux's text/plain in the corners.
	mux.HandleFunc(Prefix, h.handleUnrouted)
	return h.requireOperator(mux)
}

// routes maps every operator route to its handler. NewHandler registers
// exactly this map and Patterns reports exactly its keys, so a route cannot
// exist without being listed - which is what lets the OpenAPI document be
// checked against the routes rather than against someone's memory of them.
func (h *Handler) routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET " + Prefix + "unattributed":               h.listUnattributed,
		"POST " + Prefix + "unattributed/{id}/dismiss": h.dismissUnattributed,
		"POST " + Prefix + "withdrawals/{id}/approve":  h.approveWithdrawal,
		"POST " + Prefix + "withdrawals/{id}/reject":   h.rejectWithdrawal,
		"POST " + Prefix + "withdrawals/{id}/settle":   h.settleWithdrawal,

		"POST " + Prefix + "reconciliation/runs":                     h.importStatement,
		"GET " + Prefix + "reconciliation/runs/{id}/differences":     h.listDifferences,
		"POST " + Prefix + "reconciliation/differences/{id}/resolve": h.resolveDifference,

		"GET " + Prefix + "exports/ledger":         h.exportLedger,
		"GET " + Prefix + "exports/reconciliation": h.exportReconciliation,
	}
}

// Patterns lists this module's ServeMux patterns ("METHOD /path"), sorted.
// Every one of them sits under Prefix, and behind the requireOperator gate.
// The catch-all is deliberately absent: it is the error convention for
// paths nobody serves, not an endpoint a client can call.
func Patterns() []string {
	var h Handler
	return slices.Sorted(maps.Keys(h.routes()))
}

// handleUnrouted answers anything under the prefix that no route claimed -
// still behind the auth gate, so an unauthenticated probe of an unknown
// path answers 401 before this runs. A known path reached with the wrong
// method is 405 (with Allow, as HTTP requires); anything else is 404.
func (h *Handler) handleUnrouted(w http.ResponseWriter, r *http.Request) {
	if allow := h.allow.MethodsFor(r.URL.Path); allow != "" {
		w.Header().Set("Allow", allow)
		platformhttp.Problem(w, http.StatusMethodNotAllowed,
			r.Method+" is not allowed on this endpoint; use "+allow)
		return
	}
	platformhttp.Problem(w, http.StatusNotFound, "no such endpoint")
}
