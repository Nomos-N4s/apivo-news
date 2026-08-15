package editorial

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// maxBodyBytes bounds every editorial request body. The largest legal
// payload is a source registration - licence terms included, far below
// this - so the bound only cuts off abuse, never intent.
const maxBodyBytes = 1 << 20

// Pagination bounds from the contract's conventions: limit defaults to 20
// and never exceeds 100, so no caller can ask for the whole queue at once.
const (
	defaultQueueLimit = 20
	maxQueueLimit     = 100
)

// languageCode matches the shape the database accepts for a language code
// (`language_code_is_bcp47_subtag`): a BCP-47 primary subtag, never a
// combined locale tag - language and place are independent axes.
var languageCode = regexp.MustCompile(`^[a-z]{2,3}$`)

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
	mux.HandleFunc("GET /api/v1/editorial/queue", h.reviewQueue)
	mux.HandleFunc("POST /api/v1/editorial/approvals", h.createApproval)
	mux.HandleFunc("POST /api/v1/editorial/articles/{id}/publication", h.publishArticle)
	mux.HandleFunc("POST /api/v1/editorial/sources", h.createSource)
	return h.requireEditor(mux)
}

// approvalRequest is the approval payload: exactly one origin, the
// attribution, and whether to publish immediately. The two origin fields
// are pointers so an explicit null is "not supplied" rather than the zero
// uuid, which would be a different - and wrong - answer.
type approvalRequest struct {
	TranslationID *string `json:"translation_id"`
	SourceItemID  *string `json:"source_item_id"`
	Attribution   string  `json:"attribution"`
	Publish       bool    `json:"publish"`
}

// approvalResponse is the created article. published_at is null for the
// publish: false path - an approved record that has not been released.
type approvalResponse struct {
	ArticleID   string  `json:"article_id"`
	ApprovedBy  string  `json:"approved_by"`
	ApprovedAt  string  `json:"approved_at"`
	PublishedAt *string `json:"published_at"`
}

// publicationResponse is the released article.
type publicationResponse struct {
	ArticleID   string `json:"article_id"`
	PublishedAt string `json:"published_at"`
}

// createApproval implements POST /api/v1/editorial/approvals.
func (h *Handler) createApproval(w http.ResponseWriter, r *http.Request) {
	var req approvalRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	approval, detail, ok := newApproval(req, editorFrom(r.Context()))
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}

	article, err := h.store.Approve(r.Context(), approval)
	switch {
	case errors.Is(err, ErrOriginAlreadyApproved):
		platformhttp.Problem(w, http.StatusConflict,
			"this origin already has a published or approved article; withdraw it before approving a correction")
		return
	case errors.Is(err, ErrUnknownOrigin):
		platformhttp.Problem(w, http.StatusBadRequest, "the named origin does not exist")
		return
	case errors.Is(err, ErrUntitledOrigin):
		platformhttp.Problem(w, http.StatusBadRequest,
			"this item's feed provided no title, so an untranslated approval would have no headline; approve a translation of it instead")
		return
	// The database checks the approver's role again on insert. Reaching
	// this arm means the caller passed the HTTP gate and the trigger still
	// refused - the role changed under the request, and the database wins.
	case errors.Is(err, ErrNotEditor):
		platformhttp.Problem(w, http.StatusForbidden, "the editor role is required")
		return
	case err != nil:
		h.internalError(w, r, "approving article", err)
		return
	}

	body := approvalResponse{
		ArticleID:  article.ID.String(),
		ApprovedBy: article.ApprovedBy.String(),
		ApprovedAt: article.ApprovedAt.Format(timeFormat),
	}
	if article.PublishedAt != nil {
		published := article.PublishedAt.Format(timeFormat)
		body.PublishedAt = &published
	}
	h.writeJSON(w, r, http.StatusCreated, body)
}

// publishArticle implements POST /api/v1/editorial/articles/{id}/publication.
func (h *Handler) publishArticle(w http.ResponseWriter, r *http.Request) {
	id, ok := pathArticleID(w, r)
	if !ok {
		return
	}
	article, err := h.store.Publish(r.Context(), id, editorFrom(r.Context()).ID)
	switch {
	case errors.Is(err, ErrArticleNotFound):
		platformhttp.Problem(w, http.StatusNotFound, "no article with this id")
		return
	case errors.Is(err, ErrAlreadyPublished):
		platformhttp.Problem(w, http.StatusConflict,
			"this article is already published; publication happens once and is never repeated")
		return
	case err != nil:
		h.internalError(w, r, "publishing article", err)
		return
	}
	h.writeJSON(w, r, http.StatusOK, publicationResponse{
		ArticleID:   article.ID.String(),
		PublishedAt: article.PublishedAt.Format(timeFormat),
	})
}

// newApproval validates the payload and builds the store command, reporting
// the 400 detail when it cannot.
func newApproval(req approvalRequest, editor Editor) (approval NewApproval, detail string, ok bool) {
	switch {
	// Exactly one origin, mirroring article_exactly_one_origin: an article
	// is born from a translation or from the retrieved item, never both and
	// never neither.
	case req.TranslationID != nil && req.SourceItemID != nil:
		return NewApproval{}, "supply translation_id or source_item_id, not both: an article has exactly one origin", false
	case req.TranslationID == nil && req.SourceItemID == nil:
		return NewApproval{}, "supply translation_id or source_item_id: an article has exactly one origin", false
	case blank(req.Attribution):
		return NewApproval{}, "attribution is required and must not be blank", false
	}

	origin, field := req.TranslationID, "translation_id"
	if req.SourceItemID != nil {
		origin, field = req.SourceItemID, "source_item_id"
	}
	id, err := uuid.Parse(*origin)
	if err != nil {
		return NewApproval{}, field + " must be a uuid", false
	}

	approval = NewApproval{
		Attribution: strings.TrimSpace(req.Attribution),
		Publish:     req.Publish,
		ApprovedBy:  editor.ID,
	}
	if req.SourceItemID != nil {
		approval.SourceItemID = &id
	} else {
		approval.TranslationID = &id
	}
	return approval, "", true
}

// pathArticleID parses the {id} path segment, answering the 400 itself when
// it is not a uuid: a malformed id is a client mistake worth naming, not the
// 404 that would suggest the article merely does not exist.
func pathArticleID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		platformhttp.Problem(w, http.StatusBadRequest, "the article id in the path must be a uuid")
		return uuid.UUID{}, false
	}
	return id, true
}

// queueResponse is one page of the review queue.
type queueResponse struct {
	Items []queueItemResponse `json:"items"`
	// NextCursor is null on the last page: an absent cursor always means
	// the queue is exhausted, never "ask again and find out".
	NextCursor *string `json:"next_cursor"`
}

// queueItemResponse is one origin awaiting a decision. translation_id and
// the translated columns are null together, for an untranslated origin.
type queueItemResponse struct {
	SourceItemID       string  `json:"source_item_id"`
	TranslationID      *string `json:"translation_id"`
	SourceName         string  `json:"source_name"`
	HeadlineOriginal   *string `json:"headline_original"`
	HeadlineTranslated *string `json:"headline_translated"`
	ExtractTranslated  *string `json:"extract_translated"`
	RetrievedAt        string  `json:"retrieved_at"`
	LicenceSnapshot    string  `json:"licence_snapshot"`
	// CorrectionCandidate marks an origin whose only articles were
	// withdrawn: it is back in the queue on purpose, and the editor is
	// looking at a correction rather than a first approval.
	CorrectionCandidate bool `json:"correction_candidate"`
	// Withdrawals is the origin's withdrawal history, newest first, and
	// empty (never null) for a fresh candidate.
	Withdrawals []withdrawalResponse `json:"withdrawals"`
}

// withdrawalResponse is one ended publication in an origin's history.
type withdrawalResponse struct {
	ArticleID   string `json:"article_id"`
	WithdrawnAt string `json:"withdrawn_at"`
	WithdrawnBy string `json:"withdrawn_by"`
	Reason      string `json:"reason"`
}

// reviewQueue implements GET /api/v1/editorial/queue.
func (h *Handler) reviewQueue(w http.ResponseWriter, r *http.Request) {
	query, detail, ok := parseQueueQuery(r.URL.Query())
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}

	page, err := h.store.ReviewQueue(r.Context(), query)
	if err != nil {
		h.internalError(w, r, "listing review queue", err)
		return
	}

	body := queueResponse{Items: make([]queueItemResponse, 0, len(page.Items))}
	for _, item := range page.Items {
		body.Items = append(body.Items, queueItem(item))
	}
	if page.NextCursor != nil {
		next := encodeCursor(*page.NextCursor)
		body.NextCursor = &next
	}
	h.writeJSON(w, r, http.StatusOK, body)
}

// queueItem renders one queue row for the wire.
func queueItem(item QueueItem) queueItemResponse {
	out := queueItemResponse{
		SourceItemID:        item.SourceItemID.String(),
		SourceName:          item.SourceName,
		HeadlineOriginal:    item.HeadlineOriginal,
		HeadlineTranslated:  item.HeadlineTranslated,
		ExtractTranslated:   item.ExtractTranslated,
		RetrievedAt:         item.RetrievedAt.Format(timeFormat),
		LicenceSnapshot:     item.LicenceSnapshot,
		CorrectionCandidate: item.CorrectionCandidate(),
		Withdrawals:         make([]withdrawalResponse, 0, len(item.Withdrawals)),
	}
	if item.TranslationID != nil {
		id := item.TranslationID.String()
		out.TranslationID = &id
	}
	for _, wd := range item.Withdrawals {
		out.Withdrawals = append(out.Withdrawals, withdrawalResponse{
			ArticleID:   wd.ArticleID.String(),
			WithdrawnAt: wd.WithdrawnAt.Format(timeFormat),
			WithdrawnBy: wd.WithdrawnBy.String(),
			Reason:      wd.Reason,
		})
	}
	return out
}

// parseQueueQuery validates the query string and builds the store query,
// reporting the 400 detail when it cannot.
//
// Unknown parameters are rejected for the same reason the JSON decoder
// rejects unknown fields: a misspelled `language=de` that silently returned
// the whole unfiltered queue would read as acceptance of the filter. A
// repeated known parameter is rejected for that same reason - url.Values
// keeps every value but Get returns only the first, so `?limit=10&limit=20`
// would silently answer one of two contradictory requests.
func parseQueueQuery(values url.Values) (query QueueQuery, detail string, ok bool) {
	for name, supplied := range values {
		switch name {
		case "lang", "limit", "cursor":
			if len(supplied) > 1 {
				return QueueQuery{}, "query parameter " + strconv.Quote(name) + " was supplied " + strconv.Itoa(len(supplied)) + " times; supply it at most once", false
			}
		default:
			return QueueQuery{}, "unknown query parameter " + strconv.Quote(name) + "; this endpoint accepts lang, limit and cursor", false
		}
	}

	query.Limit = defaultQueueLimit
	// Presence, not a non-empty value: `?limit=` is a supplied limit that
	// happens to be unparseable, and answering it with the default page
	// would read as acceptance of whatever the caller meant to send.
	if values.Has("limit") {
		// ParseInt with a 32-bit size, not Atoi: it refuses anything that
		// would not fit the column type in the first place, so the bound
		// check below is about the contract rather than about overflow.
		limit, err := strconv.ParseInt(values.Get("limit"), 10, 32)
		if err != nil || limit < 1 || limit > maxQueueLimit {
			return QueueQuery{}, "limit must be a whole number between 1 and " + strconv.Itoa(maxQueueLimit), false
		}
		query.Limit = int32(limit)
	}

	if values.Has("lang") {
		lang := values.Get("lang")
		if !languageCode.MatchString(lang) {
			return QueueQuery{}, "lang must be a language code such as el or de", false
		}
		query.Lang = lang
	}

	if values.Has("cursor") {
		cursor, err := decodeCursor(values.Get("cursor"))
		if err != nil {
			return QueueQuery{}, "cursor is not one this endpoint issued; pass back the next_cursor from the previous page", false
		}
		query.Cursor = &cursor
	}
	return query, "", true
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
		URL:          strings.TrimSpace(req.URL),
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

// validateNewSource checks a registration for blank fields and a usable
// feed URL. The database enforces the non-blank rules again; this is the
// polite 400 with a nameable field, not the guarantee.
func validateNewSource(src NewSource) (detail string, ok bool) {
	switch {
	case blank(src.Name):
		return "name is required and must not be blank", false
	case blank(src.URL):
		return "url is required and must not be blank", false
	case !feedURL(src.URL):
		return "url must be an absolute http or https URL with a host, e.g. https://example.org/feed.xml", false
	case blank(src.Language):
		return "language is required and must not be blank", false
	case blank(src.Jurisdiction):
		return "jurisdiction is required and must not be blank", false
	case blank(src.LicenceTerms):
		return "licence_terms is required and must not be blank", false
	}
	return "", true
}

// feedURL reports whether raw is a URL the crawler could actually poll:
// absolute, http or https, and carrying a host. source.url is the sole
// ingestion origin, so a syntactically accepted but unfetchable value
// (`not-a-url`, `/feed.xml`, `ftp://…`) would register a source no poller
// can ever read - the database has no opinion on the matter, so this is
// the only place it can be caught.
func feedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	// Parse accepts "http://" with an empty host; a feed needs somewhere
	// to point at. Hostname() strips any port, so ":8080" alone fails too.
	return u.Hostname() != ""
}
