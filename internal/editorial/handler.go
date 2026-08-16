package editorial

import (
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
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
	// allow is the 405 classifier, derived from routes() in NewHandler so
	// it cannot drift from what is actually registered.
	allow allowTable
}

// NewHandler builds the editorial route table as an http.Handler for the
// composition root to mount. Every route sits behind the requireEditor
// gate - authentication wraps the whole table, so a future route cannot be
// added unauthenticated by omission.
func NewHandler(log *slog.Logger, store Store, auth EditorAuthenticator) http.Handler {
	h := &Handler{log: log, store: store, auth: auth}
	h.allow = buildAllowTable(h.routes())
	mux := http.NewServeMux()
	for pattern, handler := range h.routes() {
		mux.HandleFunc(pattern, handler)
	}
	// Every error under this prefix is problem+json, including the ones
	// nobody wrote a handler for - mirroring the content module, which owns
	// the same convention for the rest of /api/v1. Without this catch-all
	// the ServeMux answers an unknown path or a wrong method with a
	// text/plain body, which was the one corner where the API's single
	// error convention did not hold.
	mux.HandleFunc("/api/v1/editorial/", h.handleUnrouted)
	return h.requireEditor(mux)
}

// routes maps every editorial route to its handler. NewHandler registers
// exactly this map and Patterns reports exactly its keys, so a route cannot
// exist without being listed - which is what lets the OpenAPI document be
// checked against the routes rather than against someone's memory of them.
func (h *Handler) routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /api/v1/editorial/queue":                      h.reviewQueue,
		"POST /api/v1/editorial/approvals":                 h.createApproval,
		"POST /api/v1/editorial/articles/{id}/publication": h.publishArticle,
		"POST /api/v1/editorial/articles/{id}/withdrawal":  h.withdrawArticle,
		"GET /api/v1/editorial/articles/{id}/provenance":   h.articleProvenance,
		"POST /api/v1/editorial/sources":                   h.createSource,
		"GET /api/v1/editorial/sources":                    h.listSources,
	}
}

// Patterns lists this module's ServeMux patterns ("METHOD /path"), sorted.
// Every one of them sits under the prefix the composition root mounts, and
// behind the requireEditor gate. The catch-all is deliberately absent: it
// is the error convention for paths nobody serves, not an endpoint a
// client can call.
func Patterns() []string {
	var h Handler
	return slices.Sorted(maps.Keys(h.routes()))
}

// handleUnrouted answers anything under the editorial prefix that no route
// claimed - still behind the auth gate, so an unauthenticated probe of an
// unknown path answers 401 before this runs. A known path reached with the
// wrong method is 405 (with Allow, as HTTP requires); anything else is 404.
func (h *Handler) handleUnrouted(w http.ResponseWriter, r *http.Request) {
	if allow := h.allow.methodsFor(r.URL.Path); allow != "" {
		w.Header().Set("Allow", allow)
		platformhttp.Problem(w, http.StatusMethodNotAllowed,
			r.Method+" is not allowed on this endpoint; use "+allow)
		return
	}
	platformhttp.Problem(w, http.StatusNotFound, "no such endpoint")
}

// allowTable answers what methods a known editorial path accepts - the
// test for "wrong method" rather than "wrong address". It is derived from
// routes() itself in NewHandler, never written by hand, so a route added
// there is classified here by construction: drift between the mux and the
// 405 classifier is unrepresentable rather than merely tested for.
type allowTable []allowEntry

// allowEntry is one registered path pattern, pre-split into segments (a
// "{name}" segment matches any single non-empty path segment) beside the
// Allow header its methods render to.
type allowEntry struct {
	segments []string
	methods  string
}

// buildAllowTable groups the route table's "METHOD /path" keys by path and
// renders each path's Allow header. GET implies HEAD, exactly as ServeMux
// serves every "GET /path" pattern for HEAD requests too.
func buildAllowTable(routes map[string]http.HandlerFunc) allowTable {
	byPath := make(map[string][]string, len(routes))
	for pattern := range routes {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			// routes() keys are always "METHOD /path"; a bare path would
			// register for every method and never reach the catch-all.
			continue
		}
		byPath[path] = append(byPath[path], method)
		if method == http.MethodGet {
			byPath[path] = append(byPath[path], http.MethodHead)
		}
	}
	table := make(allowTable, 0, len(byPath))
	for path, methods := range byPath {
		slices.Sort(methods)
		table = append(table, allowEntry{
			segments: strings.Split(path, "/"),
			methods:  strings.Join(slices.Compact(methods), ", "),
		})
	}
	return table
}

// methodsFor reports the Allow header for a path some registered pattern
// matches, or "" for a path this module does not serve.
func (t allowTable) methodsFor(path string) string {
	segments := strings.Split(path, "/")
	for _, entry := range t {
		if matchesPattern(entry.segments, segments) {
			return entry.methods
		}
	}
	return ""
}

// matchesPattern reports whether a request path's segments satisfy a
// registered pattern's, segment by segment: a "{name}" wildcard takes any
// single non-empty segment, everything else matches literally. This mirrors
// how ServeMux itself reads the pattern, minus the corners the catch-all
// never sees (ServeMux canonicalises doubled slashes and dots away with a
// redirect before any handler runs).
func matchesPattern(pattern, path []string) bool {
	if len(pattern) != len(path) {
		return false
	}
	for i, want := range pattern {
		wildcard := strings.HasPrefix(want, "{") && strings.HasSuffix(want, "}")
		if wildcard {
			if path[i] == "" {
				return false
			}
			continue
		}
		if want != path[i] {
			return false
		}
	}
	return true
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
	// Places is where the article publishes to, as place slugs - at least
	// one, because the front page is scoped by place and an article tagged
	// to no place is unreachable by every reader (the 0006 trigger refuses
	// it at commit; this field is validated before any write).
	Places []string `json:"places"`
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
	var unknownPlace UnknownPlaceError
	switch {
	// The place table is the vocabulary, and the same 400 the reader's
	// front page answers for a slug it does not know. The failed approval
	// rolled back whole: no article, no places, no events.
	case errors.As(err, &unknownPlace):
		platformhttp.Problem(w, http.StatusBadRequest, "unknown place "+strconv.Quote(unknownPlace.Slug))
		return
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

	// Every timestamp is normalised to UTC before formatting, here and in
	// every response below, for queueItem's reason: pgx decodes timestamptz
	// into the PROCESS's local zone, and the contract's timestamps read Z
	// wherever the server stands.
	body := approvalResponse{
		ArticleID:  article.ID.String(),
		ApprovedBy: article.ApprovedBy.String(),
		ApprovedAt: article.ApprovedAt.UTC().Format(timeFormat),
	}
	if article.PublishedAt != nil {
		published := article.PublishedAt.UTC().Format(timeFormat)
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
	// The database checks the actor's role again, under a row lock, inside
	// the publishing transaction. Reaching this arm means the caller passed
	// the HTTP gate and the account was demoted before the write - the
	// database wins, and no publication happened.
	case errors.Is(err, ErrNotEditor):
		platformhttp.Problem(w, http.StatusForbidden, "the editor role is required")
		return
	case err != nil:
		h.internalError(w, r, "publishing article", err)
		return
	}
	h.writeJSON(w, r, http.StatusOK, publicationResponse{
		ArticleID:   article.ID.String(),
		PublishedAt: article.PublishedAt.UTC().Format(timeFormat),
	})
}

// withdrawalRequest is the withdrawal payload: why publication is ending.
// Who is ending it comes from the bearer token, never from the body.
type withdrawalRequest struct {
	Reason string `json:"reason"`
}

// withdrawalResult is the recorded withdrawal. Reason is the value the
// database froze into article.withdrawal_reason - not an echo of the
// request - because the confirmation screen renders this response as the
// record of what happened, and a record that omits its own justification
// reads as blank (the audit banner's only text is the reason).
type withdrawalResult struct {
	ArticleID   string `json:"article_id"`
	WithdrawnAt string `json:"withdrawn_at"`
	WithdrawnBy string `json:"withdrawn_by"`
	Reason      string `json:"reason"`
}

// withdrawArticle implements POST /api/v1/editorial/articles/{id}/withdrawal.
func (h *Handler) withdrawArticle(w http.ResponseWriter, r *http.Request) {
	id, ok := pathArticleID(w, r)
	if !ok {
		return
	}
	var req withdrawalRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if blank(req.Reason) {
		platformhttp.Problem(w, http.StatusBadRequest,
			"reason is required and must not be blank: a withdrawal is part of the audit record, and an unexplained one explains nothing")
		return
	}

	editor := editorFrom(r.Context())
	withdrawal, err := h.store.Withdraw(r.Context(), id, editor.ID, strings.TrimSpace(req.Reason))
	switch {
	case errors.Is(err, ErrArticleNotFound):
		platformhttp.Problem(w, http.StatusNotFound, "no article with this id")
		return
	// A never-published article has no publication to end. It answers 404
	// rather than 409 per the contract: the existence of unpublished work
	// is not something this endpoint confirms either.
	case errors.Is(err, ErrArticleNotPublished):
		platformhttp.Problem(w, http.StatusNotFound, "no published article with this id")
		return
	case errors.Is(err, ErrAlreadyWithdrawn):
		platformhttp.Problem(w, http.StatusConflict,
			"this article is already withdrawn; withdrawal is one-way and final, and who withdrew it, when and why is frozen")
		return
	// The database checks the withdrawer's role again, symmetrically with
	// approval. Reaching this arm means the role changed under the request.
	case errors.Is(err, ErrNotEditor):
		platformhttp.Problem(w, http.StatusForbidden, "the editor role is required")
		return
	case err != nil:
		h.internalError(w, r, "withdrawing article", err)
		return
	}

	h.writeJSON(w, r, http.StatusOK, withdrawalResult{
		ArticleID:   withdrawal.ArticleID.String(),
		WithdrawnAt: withdrawal.WithdrawnAt.UTC().Format(timeFormat),
		WithdrawnBy: withdrawal.WithdrawnBy.String(),
		Reason:      withdrawal.Reason,
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
	// The front page is scoped by place, so an article tagged to no place
	// is invisible to every reader - the database refuses it at commit, and
	// this earlier answer names the field before anything is written.
	case len(req.Places) == 0:
		return NewApproval{}, "places is required with at least one place slug: an article tagged to no place can never appear on any front page", false
	}
	seen := make(map[string]bool, len(req.Places))
	for _, slug := range req.Places {
		if blank(slug) {
			return NewApproval{}, "places must not contain a blank slug", false
		}
		// Rejected rather than collapsed, for the reason the queue rejects
		// a repeated parameter: silently deduplicating would read as
		// acceptance of a list this endpoint did not record.
		if seen[slug] {
			return NewApproval{}, "place " + strconv.Quote(slug) + " was supplied more than once; supply each place at most once", false
		}
		seen[slug] = true
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
		Places:      req.Places,
		ApprovedBy:  editor.ID,
	}
	if req.SourceItemID != nil {
		approval.SourceItemID = &id
	} else {
		approval.TranslationID = &id
	}
	return approval, "", true
}

// provenanceResponse is the audit trace of one article (I-5, US5): the
// full chain the article_provenance view answers in one query, plus the
// article's slice of the audit stream. The `source` object is identity
// only - the legal basis is always the retrieval-time snapshots inside
// `source_item` (I-4).
type provenanceResponse struct {
	ArticleID   string                    `json:"article_id"`
	Headline    string                    `json:"headline"`
	Places      []string                  `json:"places"`
	Source      provenanceSource          `json:"source"`
	SourceItem  provenanceSourceItem      `json:"source_item"`
	Translation *provenanceTranslation    `json:"translation"`
	Approval    provenanceApproval        `json:"approval"`
	PublishedAt *string                   `json:"published_at"`
	Withdrawal  *provenanceWithdrawal     `json:"withdrawal"`
	Events      []provenanceEventResponse `json:"events"`
}

// provenanceSource is the feed's identity. Identity ONLY: no licence
// fields belong here, because the mutable source row is never the legal
// basis of anything already retrieved (I-4).
type provenanceSource struct {
	Name         string `json:"name"`
	FeedURL      string `json:"feed_url"`
	Jurisdiction string `json:"jurisdiction"`
}

// provenanceSourceItem is the immutable retrieval evidence with its
// trigger-written snapshots.
type provenanceSourceItem struct {
	SourceURL                  string  `json:"source_url"`
	OriginalTitle              *string `json:"original_title"`
	RetrievedAt                string  `json:"retrieved_at"`
	ContentHash                string  `json:"content_hash"`
	LicenceSnapshot            string  `json:"licence_snapshot"`
	UsageRuleSnapshot          string  `json:"usage_rule_snapshot"`
	PermissionEvidenceSnapshot *string `json:"permission_evidence_snapshot"`
	OriginalAuthor             *string `json:"original_author"`
}

// provenanceTranslation is the translation lineage (FR-005) and its
// recorded cost (FR-006); the whole object is null for an article
// published untranslated.
type provenanceTranslation struct {
	Model         string `json:"model"`
	PromptVersion string `json:"prompt_version"`
	TargetLocale  string `json:"target_locale"`
	GeneratedAt   string `json:"generated_at"`
	CostMicroUSD  int64  `json:"cost_microusd"`
}

// provenanceApproval names the human whose decision the article is (I-1).
type provenanceApproval struct {
	ApproverName  string `json:"approver_name"`
	ApproverEmail string `json:"approver_email"`
	ApprovedAt    string `json:"approved_at"`
}

// provenanceWithdrawal is the recorded end of publication, null while
// publication has not ended.
type provenanceWithdrawal struct {
	WithdrawnAt string `json:"withdrawn_at"`
	WithdrawnBy string `json:"withdrawn_by"`
	Reason      string `json:"reason"`
}

// provenanceEventResponse is one audit-stream row, oldest first.
type provenanceEventResponse struct {
	Type       string `json:"type"`
	OccurredAt string `json:"occurred_at"`
	Detail     string `json:"detail"`
}

// articleProvenance implements GET /api/v1/editorial/articles/{id}/provenance.
func (h *Handler) articleProvenance(w http.ResponseWriter, r *http.Request) {
	id, ok := pathArticleID(w, r)
	if !ok {
		return
	}
	p, err := h.store.Provenance(r.Context(), id)
	switch {
	case errors.Is(err, ErrArticleNotFound):
		platformhttp.Problem(w, http.StatusNotFound, "no article with this id")
		return
	case err != nil:
		h.internalError(w, r, "reading article provenance", err)
		return
	}
	h.writeJSON(w, r, http.StatusOK, provenanceBody(p))
}

// provenanceBody renders the audit chain for the wire.
func provenanceBody(p Provenance) provenanceResponse {
	body := provenanceResponse{
		ArticleID: p.ArticleID.String(),
		Headline:  p.Headline,
		// Empty renders as [], never null: an article with no places is not
		// the same claim as one whose places are unknown.
		Places: make([]string, 0, len(p.Places)),
		Source: provenanceSource{
			Name:         p.Source.Name,
			FeedURL:      p.Source.FeedURL,
			Jurisdiction: p.Source.Jurisdiction,
		},
		SourceItem: provenanceSourceItem{
			SourceURL:                  p.SourceItem.SourceURL,
			OriginalTitle:              p.SourceItem.OriginalTitle,
			RetrievedAt:                p.SourceItem.RetrievedAt.UTC().Format(timeFormat),
			ContentHash:                p.SourceItem.ContentHash,
			LicenceSnapshot:            p.SourceItem.LicenceSnapshot,
			UsageRuleSnapshot:          p.SourceItem.UsageRuleSnapshot,
			PermissionEvidenceSnapshot: p.SourceItem.PermissionEvidenceSnapshot,
			OriginalAuthor:             p.SourceItem.OriginalAuthor,
		},
		Approval: provenanceApproval{
			ApproverName:  p.Approval.ApproverName,
			ApproverEmail: p.Approval.ApproverEmail,
			ApprovedAt:    p.Approval.ApprovedAt.UTC().Format(timeFormat),
		},
		Events: make([]provenanceEventResponse, 0, len(p.Events)),
	}
	body.Places = append(body.Places, p.Places...)
	if p.Translation != nil {
		body.Translation = &provenanceTranslation{
			Model:         p.Translation.Model,
			PromptVersion: p.Translation.PromptVersion,
			TargetLocale:  p.Translation.TargetLocale,
			GeneratedAt:   p.Translation.GeneratedAt.UTC().Format(timeFormat),
			CostMicroUSD:  p.Translation.CostMicroUSD,
		}
	}
	if p.PublishedAt != nil {
		published := p.PublishedAt.UTC().Format(timeFormat)
		body.PublishedAt = &published
	}
	if p.Withdrawal != nil {
		body.Withdrawal = &provenanceWithdrawal{
			WithdrawnAt: p.Withdrawal.WithdrawnAt.UTC().Format(timeFormat),
			WithdrawnBy: p.Withdrawal.WithdrawnBy.String(),
			Reason:      p.Withdrawal.Reason,
		}
	}
	for _, e := range p.Events {
		body.Events = append(body.Events, provenanceEventResponse{
			Type:       e.Type,
			OccurredAt: e.OccurredAt.UTC().Format(timeFormat),
			Detail:     eventDetail(e.Payload),
		})
	}
	return body
}

// eventDetail renders an event payload as the audit screen's one-line
// detail: the recorded payload itself, compact, with sorted keys - minus
// article_id, which every listed event carries and the screen is already
// scoped to. The record, not a paraphrase of it: inventing prose here
// would put words into an audit stream that deliberately has none.
func eventDetail(payload json.RawMessage) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		// Not an object; the recorded bytes are still the honest answer.
		return string(payload)
	}
	delete(fields, "article_id")
	out, err := json.Marshal(fields)
	if err != nil {
		return string(payload)
	}
	return string(out)
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
//
// The second block is the evidence the approval rests on (#87): the
// approval is permanent and article_guard freezes the attribution at the
// click, so the original text, its author and declared publication date,
// the content fingerprint and the translation lineage with its cost have
// to be on the screen before the click, not discoverable after it.
type queueItemResponse struct {
	SourceItemID       string  `json:"source_item_id"`
	TranslationID      *string `json:"translation_id"`
	SourceName         string  `json:"source_name"`
	HeadlineOriginal   *string `json:"headline_original"`
	HeadlineTranslated *string `json:"headline_translated"`
	ExtractTranslated  *string `json:"extract_translated"`
	RetrievedAt        string  `json:"retrieved_at"`
	LicenceSnapshot    string  `json:"licence_snapshot"`
	// SourceURL is the original article at the publisher.
	SourceURL string `json:"source_url"`
	// OriginalAuthor is null when the feed named none - absent stays
	// absent, never invented (FR-002).
	OriginalAuthor *string `json:"original_author"`
	// OriginalPublishedAt is the publication date the feed declared, null
	// when it declared none. The attribution default MUST come from this
	// field: falling back to retrieved_at would freeze the retrieval date
	// into the attribution as the publication date, permanently.
	OriginalPublishedAt *string `json:"original_published_at"`
	// ContentHash is the database-computed fingerprint of the evidence.
	ContentHash string `json:"content_hash"`
	// ExtractOriginal is the retrieved original reduced to bounded prose
	// (D9, at most 300 runes): evidence beside the translation, never the
	// unbounded raw body.
	ExtractOriginal string `json:"extract_original"`
	// SourceLang and TargetLang are the two ends of the translation;
	// target_lang is null for an untranslated origin.
	SourceLang string  `json:"source_lang"`
	TargetLang *string `json:"target_lang"`
	// Model, PromptVersion and CostMicroUSD are the lineage (FR-005) and
	// recorded cost (FR-006), null together for an untranslated origin.
	Model         *string `json:"model"`
	PromptVersion *string `json:"prompt_version"`
	CostMicroUSD  *int64  `json:"cost_microusd"`
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
		next := encodeCursor(queueCursors, page.NextCursor.RetrievedAt, page.NextCursor.RowID)
		body.NextCursor = &next
	}
	h.writeJSON(w, r, http.StatusOK, body)
}

// queueItem renders one queue row for the wire. Every timestamp is
// normalised to UTC before formatting: pgx decodes timestamptz into the
// PROCESS's local zone, so without .UTC() a non-UTC host would render
// `2026-08-13T07:58:00+02:00` where the contract's every other timestamp
// reads Z - and original_published_at is the date the attribution freezes
// forever, which does not get to depend on where the server stood.
func queueItem(item QueueItem) queueItemResponse {
	out := queueItemResponse{
		SourceItemID:        item.SourceItemID.String(),
		SourceName:          item.SourceName,
		HeadlineOriginal:    item.HeadlineOriginal,
		HeadlineTranslated:  item.HeadlineTranslated,
		ExtractTranslated:   item.ExtractTranslated,
		RetrievedAt:         item.RetrievedAt.UTC().Format(timeFormat),
		LicenceSnapshot:     item.LicenceSnapshot,
		SourceURL:           item.SourceURL,
		OriginalAuthor:      item.OriginalAuthor,
		ContentHash:         item.ContentHash,
		ExtractOriginal:     item.ExtractOriginal,
		SourceLang:          item.SourceLang,
		TargetLang:          item.TargetLang,
		Model:               item.Model,
		PromptVersion:       item.PromptVersion,
		CostMicroUSD:        item.CostMicroUSD,
		CorrectionCandidate: item.CorrectionCandidate(),
		Withdrawals:         make([]withdrawalResponse, 0, len(item.Withdrawals)),
	}
	if item.TranslationID != nil {
		id := item.TranslationID.String()
		out.TranslationID = &id
	}
	if item.OriginalPublishedAt != nil {
		published := item.OriginalPublishedAt.UTC().Format(timeFormat)
		out.OriginalPublishedAt = &published
	}
	for _, wd := range item.Withdrawals {
		out.Withdrawals = append(out.Withdrawals, withdrawalResponse{
			ArticleID:   wd.ArticleID.String(),
			WithdrawnAt: wd.WithdrawnAt.UTC().Format(timeFormat),
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
		at, rowID, err := decodeCursor(queueCursors, values.Get("cursor"))
		if err != nil {
			return QueueQuery{}, "cursor is not one this endpoint issued; pass back the next_cursor from the previous page", false
		}
		query.Cursor = &QueueCursor{RetrievedAt: at, RowID: rowID}
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
		CreatedAt:    created.CreatedAt.UTC().Format(timeFormat),
	})
}

// listedSourceResponse is one registered source on the wire. The licensing
// fields are the CURRENT row and the payload says so in the contract: the
// legal basis of anything already retrieved is the snapshot on source_item
// (I-4), served by the provenance endpoint, never by this list.
//
// permission_evidence is served deliberately: the screen exists to make the
// licensing basis visible, this whole route sits behind the editor gate,
// and the field is what separates a lawful full_text source from an
// impossible one (#70 named it as part of the read). A founder call,
// reversible in one line.
type listedSourceResponse struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	URL                string  `json:"url"`
	Language           string  `json:"language"`
	Jurisdiction       string  `json:"jurisdiction"`
	LicenceTerms       string  `json:"licence_terms"`
	UsageRule          string  `json:"usage_rule"`
	PermissionEvidence *string `json:"permission_evidence"`
	Active             bool    `json:"active"`
	// LastPolledAt is null for a feed the poll loop has never attempted.
	LastPolledAt *string `json:"last_polled_at"`
	CreatedAt    string  `json:"created_at"`
}

// pollCycleResponse is the last poll cycle: sums of the active sources'
// last-poll counters, and the failed feeds by name. duplicates_skipped is
// the FR-014 fingerprint at work.
type pollCycleResponse struct {
	Retrieved         int64    `json:"retrieved"`
	DuplicatesSkipped int64    `json:"duplicates_skipped"`
	Failures          []string `json:"failures"`
}

// sourcesListResponse is one page of the source list, plus the cycle the
// screen renders beside it.
type sourcesListResponse struct {
	Items []listedSourceResponse `json:"items"`
	// NextCursor is null on the last page, like every list here.
	NextCursor *string           `json:"next_cursor"`
	Cycle      pollCycleResponse `json:"cycle"`
}

// listSources implements GET /api/v1/editorial/sources.
func (h *Handler) listSources(w http.ResponseWriter, r *http.Request) {
	query, detail, ok := parseSourcesQuery(r.URL.Query())
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}

	page, err := h.store.ListSources(r.Context(), query)
	if err != nil {
		h.internalError(w, r, "listing sources", err)
		return
	}
	cycle, err := h.store.LastPollCycle(r.Context())
	if err != nil {
		h.internalError(w, r, "reading last poll cycle", err)
		return
	}

	body := sourcesListResponse{
		Items: make([]listedSourceResponse, 0, len(page.Items)),
		Cycle: pollCycleResponse{
			Retrieved:         cycle.Retrieved,
			DuplicatesSkipped: cycle.Duplicates,
			// Empty renders as [], never null: "no failures" is a reading.
			Failures: cycle.Failures,
		},
	}
	for _, item := range page.Items {
		body.Items = append(body.Items, listedSource(item))
	}
	if page.NextCursor != nil {
		next := encodeCursor(sourcesCursors, page.NextCursor.CreatedAt, page.NextCursor.ID)
		body.NextCursor = &next
	}
	h.writeJSON(w, r, http.StatusOK, body)
}

// listedSource renders one source row for the wire.
func listedSource(item ListedSource) listedSourceResponse {
	out := listedSourceResponse{
		ID:                 item.ID.String(),
		Name:               item.Name,
		URL:                item.URL,
		Language:           item.Language,
		Jurisdiction:       item.Jurisdiction,
		LicenceTerms:       item.LicenceTerms,
		UsageRule:          item.UsageRule,
		PermissionEvidence: item.PermissionEvidence,
		Active:             item.Active,
		CreatedAt:          item.CreatedAt.UTC().Format(timeFormat),
	}
	if item.LastPolledAt != nil {
		polled := item.LastPolledAt.UTC().Format(timeFormat)
		out.LastPolledAt = &polled
	}
	return out
}

// parseSourcesQuery validates the query string and builds the store query,
// reporting the 400 detail when it cannot. Unknown and repeated parameters
// are rejected for parseQueueQuery's reasons: a misspelled or contradictory
// filter silently half-honoured would read as acceptance.
func parseSourcesQuery(values url.Values) (query SourcesQuery, detail string, ok bool) {
	for name, supplied := range values {
		switch name {
		case "active", "limit", "cursor":
			if len(supplied) > 1 {
				return SourcesQuery{}, "query parameter " + strconv.Quote(name) + " was supplied " + strconv.Itoa(len(supplied)) + " times; supply it at most once", false
			}
		default:
			return SourcesQuery{}, "unknown query parameter " + strconv.Quote(name) + "; this endpoint accepts active, limit and cursor", false
		}
	}

	query.Limit = defaultQueueLimit
	if values.Has("limit") {
		limit, err := strconv.ParseInt(values.Get("limit"), 10, 32)
		if err != nil || limit < 1 || limit > maxQueueLimit {
			return SourcesQuery{}, "limit must be a whole number between 1 and " + strconv.Itoa(maxQueueLimit), false
		}
		query.Limit = int32(limit)
	}

	// Exactly the JSON booleans, not ParseBool's zoo of aliases: `active=1`
	// accepted here would be a second spelling the contract never made.
	if values.Has("active") {
		switch values.Get("active") {
		case "true":
			active := true
			query.Active = &active
		case "false":
			active := false
			query.Active = &active
		default:
			return SourcesQuery{}, "active must be true or false", false
		}
	}

	if values.Has("cursor") {
		at, rowID, err := decodeCursor(sourcesCursors, values.Get("cursor"))
		if err != nil {
			return SourcesQuery{}, "cursor is not one this endpoint issued; pass back the next_cursor from the previous page", false
		}
		query.Cursor = &SourceCursor{CreatedAt: at, ID: rowID}
	}
	return query, "", true
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
