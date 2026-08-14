package editorial

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// maxBodyBytes bounds every editorial request body. The largest legal
// payload is a source registration - licence terms included, far below
// this - so the bound only cuts off abuse, never intent.
const maxBodyBytes = 1 << 20

// Handler serves the editorial endpoints. Build it with NewHandler.
type Handler struct {
	log   *slog.Logger
	store Store
	auth  EditorAuthenticator
}

// NewHandler builds the editorial route table as an http.Handler for the
// composition root to mount. Every route sits behind the requireEditor
// gate - authentication wraps the whole table, so a future route cannot be
// added unauthenticated by omission.
func NewHandler(log *slog.Logger, store Store, auth EditorAuthenticator) http.Handler {
	h := &Handler{log: log, store: store, auth: auth}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/editorial/sources", h.createSource)
	return h.requireEditor(mux)
}

// sourceRequest is the source-registration payload. UsageRule is decoded
// only to be rejected: the contract does not accept it as input, and a
// caller supplying one (even null) must hear an explicit 400, not have it
// silently ignored.
type sourceRequest struct {
	Name         string          `json:"name"`
	URL          string          `json:"url"`
	Language     string          `json:"language"`
	Jurisdiction string          `json:"jurisdiction"`
	LicenceTerms string          `json:"licence_terms"`
	UsageRule    json.RawMessage `json:"usage_rule"`
}

// sourceResponse is the created source, as the database recorded it.
type sourceResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	URL          string `json:"url"`
	Language     string `json:"language"`
	Jurisdiction string `json:"jurisdiction"`
	LicenceTerms string `json:"licence_terms"`
	UsageRule    string `json:"usage_rule"`
	CreatedAt    string `json:"created_at"`
}

// createSource implements POST /api/v1/editorial/sources.
func (h *Handler) createSource(w http.ResponseWriter, r *http.Request) {
	var req sourceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.UsageRule != nil {
		platformhttp.Problem(w, http.StatusBadRequest,
			"usage_rule is not accepted: new sources are always extract_and_link; upgrades are a separate founder-gated flow")
		return
	}
	src := NewSource{
		Name:         req.Name,
		URL:          req.URL,
		Language:     req.Language,
		Jurisdiction: req.Jurisdiction,
		LicenceTerms: req.LicenceTerms,
	}
	if detail, ok := validateNewSource(src); !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}

	created, err := h.store.CreateSource(r.Context(), src)
	switch {
	case errors.Is(err, ErrDuplicateSourceURL):
		platformhttp.Problem(w, http.StatusConflict, "a source with this feed URL is already registered")
		return
	case errors.Is(err, ErrUnknownLanguage):
		platformhttp.Problem(w, http.StatusBadRequest, "language must be a known language code")
		return
	case err != nil:
		h.internalError(w, r, "creating source", err)
		return
	}

	h.writeJSON(w, r, http.StatusCreated, sourceResponse{
		ID:           created.ID.String(),
		Name:         created.Name,
		URL:          created.URL,
		Language:     created.Language,
		Jurisdiction: created.Jurisdiction,
		LicenceTerms: created.LicenceTerms,
		UsageRule:    created.UsageRule,
		CreatedAt:    created.CreatedAt.Format(timeFormat),
	})
}

// validateNewSource checks a registration for blank fields. The database
// enforces the same non-blank rules again; this is the polite 400 with a
// nameable field, not the guarantee.
func validateNewSource(src NewSource) (detail string, ok bool) {
	switch {
	case blank(src.Name):
		return "name is required and must not be blank", false
	case blank(src.URL):
		return "url is required and must not be blank", false
	case blank(src.Language):
		return "language is required and must not be blank", false
	case blank(src.Jurisdiction):
		return "jurisdiction is required and must not be blank", false
	case blank(src.LicenceTerms):
		return "licence_terms is required and must not be blank", false
	}
	return "", true
}
