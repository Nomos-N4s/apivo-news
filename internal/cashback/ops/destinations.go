package ops

// Verifying that a payout destination belongs to the member who named it
// (B6, #548, FR-051 with FR-061, ADR-0006).
//
// FR-051 says money never moves to a destination nobody proved, and until
// now nothing could perform that proof: the service existed, unrouted, and
// every deployment's members were stuck one step short of being paid.
//
// ADR-0006 settled what proving means here. The details live in OpenBao and
// this api cannot read them back, so verification is not something this
// process can compute - it is a person confirming ownership out of band and
// recording that they did. That makes it an operator action, and it is
// shaped like every other one on this surface: a named human, a reason in
// their own words, and an event, all in one transaction.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// The sentinels verification is refused with.
var (
	// ErrNoSuchDestination reports an id naming no destination. Distinct
	// from a destination that is already verified, which is not a failure.
	ErrNoSuchDestination = errors.New("ops: no payout destination has that id")
	// ErrNotVerified reports a verification that could not be recorded.
	ErrNotVerified = errors.New("ops: the destination could not be verified")
)

// Verification is one operator's proof that a destination is its member's.
type Verification struct {
	// ID is the destination.
	ID uuid.UUID
	// Operator is the named human performing it (FR-061).
	Operator Operator
	// Method is how they satisfied themselves, in their own words. The
	// schema refuses a blank one: "verified, method unknown" is not a
	// verification anybody could defend later.
	Method string
}

// Verified is what the database recorded.
type Verified struct {
	ID         uuid.UUID
	AccountID  uuid.UUID
	Kind       string
	Method     string
	VerifiedBy uuid.UUID
	VerifiedAt time.Time
}

// UnverifiedDestination is one row of the queue: a member who cannot be
// paid until somebody looks.
//
// It carries no details and cannot. The column holds a reference and the
// details are in the vault, which is where an operator opens them
// (ADR-0006) - so the reference travels, and what it points at does not.
type UnverifiedDestination struct {
	ID           uuid.UUID
	AccountID    uuid.UUID
	AccountEmail string
	Kind         string
	DetailsRef   string
	CreatedAt    time.Time
}

// DestinationVerifier is the operator's half of FR-051, named here per the
// boundary rules.
type DestinationVerifier interface {
	// UnverifiedDestinations returns one page of the queue, oldest first,
	// starting after the given position.
	UnverifiedDestinations(ctx context.Context, after DestinationAfter, limit int) ([]UnverifiedDestination, error)
	// Verify records the proof, and announces it, in one transaction.
	//
	// Idempotent: a destination somebody already verified answers the
	// verification that stands rather than replacing it, because the
	// table's guard makes verification one-way and an operator repeating
	// themselves has done nothing wrong.
	Verify(ctx context.Context, v Verification) (Verified, error)
}

// DestinationAfter is a position in the queue: everything ordered after
// this row. The zero value starts at the beginning.
//
// Both ordering columns, for the reason the unattributed queue's position
// carries both: created_at defaults to now(), so two destinations recorded
// in one transaction share an instant and the instant alone is not a total
// order.
type DestinationAfter struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// destinationItem is one queue row on the wire.
type destinationItem struct {
	DestinationID string `json:"destination_id"`
	AccountID     string `json:"account_id"`
	// AccountEmail is how an operator reaches the member whose destination
	// this is. Verification is a conversation with a person, so the queue
	// carries the way to have it.
	AccountEmail string `json:"account_email"`
	Kind         string `json:"kind"`
	// DetailsRef is where the details are, never what they are. An
	// operator opens this in the vault; nothing in this api can.
	DetailsRef string `json:"details_ref"`
	CreatedAt  string `json:"created_at"`
}

// destinationPage is one page of the queue.
type destinationPage struct {
	Items      []destinationItem `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

// verificationRequestBody is what an operator sends.
type verificationRequestBody struct {
	Method string `json:"method"`
}

// verifiedResponse is what they get back.
type verifiedResponse struct {
	DestinationID  string `json:"destination_id"`
	AccountID      string `json:"account_id"`
	Kind           string `json:"kind"`
	VerifiedMethod string `json:"verified_method"`
	VerifiedBy     string `json:"verified_by"`
	VerifiedAt     string `json:"verified_at"`
}

// listUnverifiedDestinations implements
// GET /api/v1/cashback/ops/payout-destinations.
func (h *Handler) listUnverifiedDestinations(w http.ResponseWriter, r *http.Request) {
	at, rowID, limit, detail, ok := parsePage(r.URL.Query(), destinationCursors)
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}

	// One more than the page, so "is there another page?" is answered by
	// what came back rather than by guessing from a full one.
	rows, err := h.destinations.UnverifiedDestinations(r.Context(),
		DestinationAfter{CreatedAt: at, ID: rowID}, limit+1)
	if err != nil {
		h.internalError(w, r, "listing unverified payout destinations", err)
		return
	}

	page := destinationPage{Items: make([]destinationItem, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		page.Items = append(page.Items, destinationItem{
			DestinationID: row.ID.String(),
			AccountID:     row.AccountID.String(),
			AccountEmail:  row.AccountEmail,
			Kind:          row.Kind,
			DetailsRef:    row.DetailsRef,
			CreatedAt:     stamp(row.CreatedAt),
		})
	}
	if len(rows) > limit {
		last := rows[limit-1]
		next := encodeCursor(destinationCursors, last.CreatedAt, last.ID)
		page.NextCursor = &next
	}
	h.writeJSON(w, r, page)
}

// verifyDestination implements
// POST /api/v1/cashback/ops/payout-destinations/{id}/verify.
func (h *Handler) verifyDestination(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, "the destination id is not a UUID")
		return
	}
	var body verificationRequestBody
	if !decodeJSON(w, r, &body) {
		return
	}
	method := strings.TrimSpace(body.Method)
	if method == "" {
		platformhttp.Problem(w, http.StatusBadRequest,
			"method is required: a verification says how it was done, or it is not one")
		return
	}

	verified, err := h.destinations.Verify(r.Context(), Verification{
		ID: id, Operator: operatorFrom(r.Context()), Method: method,
	})
	switch { //nolint:gocritic // the sentinel arm reads as a sibling of the failure arm.
	case errors.Is(err, ErrNoSuchDestination):
		platformhttp.Problem(w, http.StatusNotFound, "no payout destination has that id")
		return
	case err != nil:
		h.internalError(w, r, "verifying a payout destination", err)
		return
	}
	h.writeJSON(w, r, verifiedResponse{
		DestinationID:  verified.ID.String(),
		AccountID:      verified.AccountID.String(),
		Kind:           verified.Kind,
		VerifiedMethod: verified.Method,
		VerifiedBy:     verified.VerifiedBy.String(),
		VerifiedAt:     stamp(verified.VerifiedAt),
	})
}
