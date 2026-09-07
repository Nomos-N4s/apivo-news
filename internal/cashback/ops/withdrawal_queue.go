// The queue the three decisions act on (FR-060, C-4).
//
// approve, reject and settle have existed since T092 and T147, each keyed by
// a request id, and nothing ever answered "which requests?". An operator had
// to know an id already, which no screen could supply - so the queue was a
// screen served from fixtures and the money it releases could not be found.
//
// The read is here rather than in payout for the reason
// queries/withdrawals.sql sets out: every statement over this table in that
// module is narrowed on the account, and an operator has no account to narrow
// on. This file is also where the ONE money figure gets its name - see
// awaitingItem.ReservedAmount, which is not a synonym for what the member
// asked for.

package ops

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// stateAwaitingApproval is the only state this queue serves.
//
// Accepted as a query parameter rather than ignored, because the client the
// contract was written for sends it and a parameter silently dropped is a
// filter somebody believes is applied. Any other value is refused rather than
// widened: `withdrawal_request_awaiting_idx` is this predicate exactly, and a
// queue that quietly scanned every decided request as well would be a
// different query wearing this one's name.
const stateAwaitingApproval = "awaiting_approval"

// AwaitingWithdrawal is one request nobody has decided yet, with the
// destination it would pay.
type AwaitingWithdrawal struct {
	ID        uuid.UUID
	AccountID uuid.UUID
	// AccountEmail is how an operator reaches the member. Releasing money
	// is sometimes a conversation first, and an account id is not one.
	AccountEmail string
	// AmountMinor and Currency are what is RESERVED, which is what a payout
	// will pay. See awaitingItem.ReservedAmount.
	AmountMinor int64
	Currency    string
	RequestedAt time.Time
	// The destination, joined rather than fetched separately: deciding
	// whether to release money means seeing where it goes, and an
	// unverified destination must not be paid at all (FR-051).
	DestinationID             uuid.UUID
	DestinationKind           string
	DestinationDetailsRef     string
	DestinationVerifiedAt     time.Time
	DestinationVerifiedMethod string
	DestinationCreatedAt      time.Time
}

// WithdrawalLister is the read behind the queue, named here per the boundary
// rules. *PGStore satisfies it.
//
// Separate from WithdrawalApprover and WithdrawalRefuser beside it because
// those two reach into the payout module and this does not: it is this
// module's own query, over a predicate no member-scoped statement can
// express.
type WithdrawalLister interface {
	// WithdrawalsAwaitingApproval returns one page of the queue, oldest
	// first, starting after the given position.
	WithdrawalsAwaitingApproval(ctx context.Context, after WithdrawalAfter, limit int) ([]AwaitingWithdrawal, error)
}

// WithdrawalAfter is a position in the queue: everything ordered after this
// row. The zero value starts at the beginning.
//
// Both ordering columns, for the reason every other queue's position carries
// both: requested_at defaults to now(), so two requests made in one
// transaction share an instant and the instant alone is not a total order.
type WithdrawalAfter struct {
	RequestedAt time.Time
	ID          uuid.UUID
}

// awaitingDestination is where one queued request would send the money.
type awaitingDestination struct {
	DestinationID string `json:"destination_id"`
	Kind          string `json:"kind"`
	// DetailsRef is where the details are, never what they are. It is on
	// this row because it is the operator's next action: the manual rail
	// pays by a person opening this reference in the vault (ADR-0006), and
	// nothing in this api can resolve it for them.
	DetailsRef string `json:"details_ref"`
	// VerifiedAt and VerifiedMethod are null only where nobody has proved
	// this destination belongs to its member. A withdrawal naming an
	// unverified destination is refused when it is made (FR-051), so a null
	// here should be unreachable - and it is rendered rather than assumed
	// away, because the reader of this row is about to release money and a
	// silent assumption is not something they can check.
	VerifiedAt     *string `json:"verified_at"`
	VerifiedMethod *string `json:"verified_method"`
	CreatedAt      string  `json:"created_at"`
}

// awaitingItem is one queue row on the wire.
type awaitingItem struct {
	RequestID string `json:"request_id"`
	AccountID string `json:"account_id"`
	// AccountEmail, for the reason the destination queue carries one.
	AccountEmail string `json:"account_email"`
	// ReservedAmount is what will be paid, and the name is the point.
	//
	// It is NOT what the member asked for, and the difference is real:
	// entries are reserved whole, so covering a request for €18.40 may
	// reserve €19.10 and that larger figure is what leaves the ledger and
	// what the rail pays (D9, and payout.Withdrawal.Amount says the same).
	// What was asked is not stored anywhere - only the reservation is - so
	// this endpoint serves the one figure that exists under the one name
	// that is true of it. Calling it `amount` would invite an operator to
	// compare it against a number this system does not keep.
	ReservedAmount amountJSON          `json:"reserved_amount"`
	RequestedAt    string              `json:"requested_at"`
	Destination    awaitingDestination `json:"destination"`
}

// awaitingPage is one page of the queue.
type awaitingPage struct {
	Items      []awaitingItem `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}

// listWithdrawalsAwaitingApproval implements
// GET /api/v1/cashback/ops/withdrawals.
func (h *Handler) listWithdrawalsAwaitingApproval(w http.ResponseWriter, r *http.Request) {
	values, detail, ok := takeState(r.URL.Query())
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}
	at, rowID, limit, detail, ok := parsePage(values, withdrawalCursors)
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}

	// One more than the page, so "is there another page?" is answered by
	// what came back rather than by guessing from a full one.
	rows, err := h.awaiting.WithdrawalsAwaitingApproval(r.Context(),
		WithdrawalAfter{RequestedAt: at, ID: rowID}, limit+1)
	if err != nil {
		h.internalError(w, r, "listing withdrawals awaiting approval", err)
		return
	}

	page := awaitingPage{Items: make([]awaitingItem, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		page.Items = append(page.Items, awaitingItem{
			RequestID:      row.ID.String(),
			AccountID:      row.AccountID.String(),
			AccountEmail:   row.AccountEmail,
			ReservedAmount: amountJSON{Minor: row.AmountMinor, Currency: row.Currency},
			RequestedAt:    stamp(row.RequestedAt),
			Destination: awaitingDestination{
				DestinationID:  row.DestinationID.String(),
				Kind:           row.DestinationKind,
				DetailsRef:     row.DestinationDetailsRef,
				VerifiedAt:     stampOrNil(row.DestinationVerifiedAt),
				VerifiedMethod: optional(row.DestinationVerifiedMethod),
				CreatedAt:      stamp(row.DestinationCreatedAt),
			},
		})
	}
	if len(rows) > limit {
		last := rows[limit-1]
		next := encodeCursor(withdrawalCursors, last.RequestedAt, last.ID)
		page.NextCursor = &next
	}
	h.writeJSON(w, r, page)
}

// takeState validates and removes the one filter this endpoint accepts,
// leaving the rest for parsePage.
//
// Done here rather than by widening parsePage, so the four queues that take
// only limit and cursor keep refusing everything else. The values are copied
// rather than mutated in place: r.URL.Query() builds a fresh map today, and a
// handler that quietly depended on that would break the day it stopped.
func takeState(values url.Values) (url.Values, string, bool) {
	supplied, present := values["state"]
	if !present {
		return values, "", true
	}
	if len(supplied) > 1 {
		return nil, "query parameter \"state\" was supplied " + strconv.Itoa(len(supplied)) + " times; supply it at most once", false
	}
	if supplied[0] != stateAwaitingApproval {
		return nil, "state must be " + strconv.Quote(stateAwaitingApproval) + "; this queue lists the requests nobody has decided yet, and a decided one is read through the ledger export rather than here", false
	}
	rest := url.Values{}
	for name, value := range values {
		if name != "state" {
			rest[name] = value
		}
	}
	return rest, "", true
}

// stampOrNil renders an instant, or null where there is none.
func stampOrNil(at time.Time) *string {
	if at.IsZero() {
		return nil
	}
	rendered := stamp(at)
	return &rendered
}
