package account

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"

	"github.com/google/uuid"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// maxBodyBytes bounds every request body here. The largest legal one is a
// single short cursor, so this is orders of magnitude of headroom and
// exists only so a hostile body cannot be read into memory.
const maxBodyBytes = 4 << 10

// MaxToursPerAccount bounds how many tours one account may have progress
// for.
//
// The tour ids come from the front end, not from a table, which means an
// authenticated caller can PUT any id they invent. Without a ceiling the
// document grows until the row does, and the person paying for that is
// whoever next reads the account — the row is loaded on ordinary requests.
// A cap turns an unbounded write into a 409 that names the problem.
const MaxToursPerAccount = 32

// tourIDPattern is what may become a key in the tour_progress document.
// Narrow on purpose: these are identifiers from this product's own front
// end, and anything outside the shape is either a typo or somebody probing
// what the column will hold.
var tourIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// cursorPattern is what may become a value. The front end owns what a
// cursor MEANS — a step index into a list defined there — and this only
// refuses shapes the column should never hold. Four digits is a tour of
// ten thousand steps, which is not a tour.
var cursorPattern = regexp.MustCompile(`^(done|[0-9]{1,4})$`)

// TourStore reads and writes one account's tour progress.
type TourStore interface {
	// Tours returns the account's whole progress document. A missing
	// account reports ErrNoAccount.
	Tours(ctx context.Context, accountID uuid.UUID) (map[string]string, error)
	// SetTour records a cursor for one tour, returning false when the
	// account already holds MaxToursPerAccount other tours. A missing
	// account reports ErrNoAccount.
	SetTour(ctx context.Context, accountID uuid.UUID, tourID, cursor string) (bool, error)
}

// ErrNoAccount reports that the token resolved to an account id with no
// row behind it — deleted between authentication and this query. Distinct
// from a failed query, which leaves the question unanswered.
var ErrNoAccount = errors.New("account: no such account")

// Handler is this module's route table.
type Handler struct {
	log   *slog.Logger
	store TourStore
	auth  Authenticator
}

// NewHandler builds the account route table. The composition root mounts
// it under the prefix every pattern below shares.
func NewHandler(log *slog.Logger, store TourStore, auth Authenticator) http.Handler {
	h := &Handler{log: log, store: store, auth: auth}
	mux := http.NewServeMux()
	for pattern, handler := range h.routes() {
		mux.HandleFunc(pattern, handler)
	}
	// Every error under this prefix is problem+json, including paths
	// nobody wrote a handler for — the same convention the reader and
	// editorial modules hold, so the API has one error shape rather than
	// one per module plus ServeMux's text/plain in the corners.
	mux.HandleFunc("/api/v1/account/", h.handleUnrouted)
	return h.requireAccount(mux)
}

func (h *Handler) routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /api/v1/account/tours":        h.readTours,
		"PUT /api/v1/account/tours/{tour}": h.writeTour,
	}
}

// Patterns lists this module's ServeMux patterns ("METHOD /path"), sorted,
// for the contract test in cmd. The catch-all is deliberately absent: it
// is the error convention for paths nobody serves, not an endpoint.
func Patterns() []string {
	h := &Handler{}
	patterns := make([]string, 0, len(h.routes()))
	for pattern := range h.routes() {
		patterns = append(patterns, pattern)
	}
	slices.Sort(patterns)
	return patterns
}

// handleUnrouted answers anything under the prefix that no route claims.
func (h *Handler) handleUnrouted(w http.ResponseWriter, r *http.Request) {
	platformhttp.Problem(w, http.StatusNotFound, "no such account endpoint: "+r.Method+" "+r.URL.Path)
}

// toursResponse is the body of GET /api/v1/account/tours.
type toursResponse struct {
	// Tours maps a tour id to its cursor. Always present, `{}` when the
	// account has started none — an absent field and an empty object would
	// mean the same thing and the client would have to handle both.
	Tours map[string]string `json:"tours"`
}

func (h *Handler) readTours(w http.ResponseWriter, r *http.Request) {
	tours, err := h.store.Tours(r.Context(), accountFrom(r.Context()).ID)
	switch {
	case errors.Is(err, ErrNoAccount):
		platformhttp.Problem(w, http.StatusNotFound, "this token authenticates an account that no longer exists")
		return
	case err != nil:
		h.internalError(w, r, "reading tour progress", err)
		return
	}
	if tours == nil {
		tours = map[string]string{}
	}
	h.writeJSON(w, r, http.StatusOK, toursResponse{Tours: tours})
}

// tourWrite is the body of PUT /api/v1/account/tours/{tour}.
type tourWrite struct {
	Cursor string `json:"cursor"`
}

func (h *Handler) writeTour(w http.ResponseWriter, r *http.Request) {
	tourID := r.PathValue("tour")
	if !tourIDPattern.MatchString(tourID) {
		platformhttp.Problem(w, http.StatusBadRequest, "a tour id is lower-case letters, digits and hyphens, starting with a letter")
		return
	}
	var body tourWrite
	if !decodeJSON(w, r, &body) {
		return
	}
	if !cursorPattern.MatchString(body.Cursor) {
		platformhttp.Problem(w, http.StatusBadRequest, `cursor must be "done" or a step number`)
		return
	}

	stored, err := h.store.SetTour(r.Context(), accountFrom(r.Context()).ID, tourID, body.Cursor)
	switch {
	case errors.Is(err, ErrNoAccount):
		platformhttp.Problem(w, http.StatusNotFound, "this token authenticates an account that no longer exists")
		return
	case err != nil:
		h.internalError(w, r, "recording tour progress", err)
		return
	}
	if !stored {
		platformhttp.Problem(w, http.StatusConflict, "this account already tracks the maximum number of tours")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeJSON decodes the request body into dst, answering the 400 itself
// when the body is not the expected shape. Unknown fields are rejected: a
// misspelled field silently ignored would read as acceptance.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, "request body is not valid JSON for this endpoint: "+err.Error())
		return false
	}
	// Requiring io.EOF rather than !dec.More() rejects both a second
	// document and trailing syntax errors; More() answers the narrower
	// question "is another VALUE coming?", so a stray `}` reads as no.
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		platformhttp.Problem(w, http.StatusBadRequest, "request body must contain a single JSON document")
		return false
	}
	return true
}

func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.log.WarnContext(r.Context(), "writing account response", "error", err)
	}
}

// internalError logs the failure and answers an opaque 500: internals are
// for the log, never the wire.
func (h *Handler) internalError(w http.ResponseWriter, r *http.Request, doing string, err error) {
	h.log.ErrorContext(r.Context(), doing, "error", err)
	platformhttp.Problem(w, http.StatusInternalServerError, "")
}
