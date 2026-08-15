package content

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/content/store"
	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
	"github.com/Nomos-N4s/apivo-news/internal/platform/text"
)

// Pagination bounds per the API contract: limit defaults to 20, capped at
// 100; the cursor is opaque to clients.
const (
	defaultLimit = 20
	maxLimit     = 100
	// maxCursorBytes bounds the encoded cursor the endpoint will even look
	// at. A real one is an RFC 3339 timestamp, a separator and a UUID -
	// comfortably under a hundred bytes - so anything larger is malformed by
	// construction and is rejected before it is decoded. The transport caps
	// header size too, but a value this cheap to state should not be left
	// resting on a limit set somewhere else.
	maxCursorBytes = 256
)

// feedItem is the reader item shape shared by the front page and the
// article page (the contract defines the article payload as "same shape as
// front items plus approved_at").
type feedItem struct {
	ID          string    `json:"id"`
	Headline    string    `json:"headline"`
	Extract     string    `json:"extract"`
	Lang        string    `json:"lang"`
	Places      []string  `json:"places"`
	Attribution string    `json:"attribution"`
	SourceURL   string    `json:"source_url"`
	PublishedAt time.Time `json:"published_at"`
}

type frontResponse struct {
	Items      []feedItem `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

type articleResponse struct {
	feedItem
	ApprovedAt time.Time `json:"approved_at"`
}

// Handler serves the public reader endpoints. It is wired by the
// composition root in cmd, which mounts the route table on the platform
// server; no other module imports it.
type Handler struct {
	log *slog.Logger
	q   *store.Queries
}

// NewHandler builds the content module's public route table over a database
// handle (*pgxpool.Pool in production; a transaction in tests).
func NewHandler(log *slog.Logger, db store.DBTX) http.Handler {
	h := &Handler{log: log, q: store.New(db)}
	mux := http.NewServeMux()
	for pattern, handler := range h.routes() {
		mux.HandleFunc(pattern, handler)
	}
	// Every error under /api/v1 is problem+json, including the ones nobody
	// wrote a handler for. Without this catch-all the ServeMux answers an
	// unknown path or a wrong method with a text/plain body, which breaks
	// the contract's single error convention for exactly the requests a
	// confused client is most likely to make.
	mux.HandleFunc("/api/v1/", h.handleUnrouted)
	return mux
}

// routes maps every reader endpoint to its handler. NewHandler registers
// exactly this map and Patterns reports exactly its keys, so an endpoint
// cannot exist without being listed - which is what lets the OpenAPI
// document be checked against the routes rather than against memory.
func (h *Handler) routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /api/v1/front":         h.handleFront,
		"GET /api/v1/articles/{id}": h.handleArticle,
	}
}

// Patterns lists this module's ServeMux patterns ("METHOD /path"), sorted.
// The catch-all is deliberately absent: it is the error convention for paths
// nobody serves, not an endpoint a client can call.
func Patterns() []string {
	var h Handler
	return slices.Sorted(maps.Keys(h.routes()))
}

// handleUnrouted answers anything under /api/v1 that no route claimed. A
// known path reached with the wrong method is 405 (with Allow, as HTTP
// requires); anything else is 404.
func (h *Handler) handleUnrouted(w http.ResponseWriter, r *http.Request) {
	if isReaderPath(r.URL.Path) {
		w.Header().Set("Allow", "GET, HEAD")
		platformhttp.Problem(w, http.StatusMethodNotAllowed,
			fmt.Sprintf("%s is not allowed on this endpoint; use GET", r.Method))
		return
	}
	platformhttp.Problem(w, http.StatusNotFound, "no such endpoint")
}

// isReaderPath reports whether path is one this module serves - the test for
// "wrong method" rather than "wrong address". It mirrors the patterns
// registered above; a route added there belongs here too.
func isReaderPath(path string) bool {
	if path == "/api/v1/front" {
		return true
	}
	id, ok := strings.CutPrefix(path, "/api/v1/articles/")
	return ok && id != "" && !strings.Contains(id, "/")
}

// handleFront serves GET /api/v1/front: the locale-scoped front page feed.
func (h *Handler) handleFront(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	lang := query.Get("lang")
	if lang != "el" && lang != "de" {
		platformhttp.Problem(w, http.StatusBadRequest,
			fmt.Sprintf("unknown lang %q: the reader languages are el and de", lang))
		return
	}

	places := query["place"]
	if len(places) == 0 {
		platformhttp.Problem(w, http.StatusBadRequest,
			"at least one place is required (repeat place= to follow several)")
		return
	}
	known, err := h.q.ListPlacesBySlugs(ctx, places)
	if err != nil {
		h.serverError(w, r, "resolving places", err)
		return
	}
	for _, p := range places {
		if !slices.Contains(known, p) {
			platformhttp.Problem(w, http.StatusBadRequest, fmt.Sprintf("unknown place %q", p))
			return
		}
	}

	limit, ok := parseLimit(query.Get("limit"))
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest,
			fmt.Sprintf("invalid limit %q: expected an integer between 1 and %d", query.Get("limit"), maxLimit))
		return
	}

	// One row beyond the page tells whether a next page exists without a
	// second query.
	params := store.ListFrontPageParams{Lang: lang, Places: places, RowLimit: limit + 1}
	if c := query.Get("cursor"); c != "" {
		params.CursorPublishedAt, params.CursorID, err = decodeCursor(c)
		if err != nil {
			platformhttp.Problem(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}

	rows, err := h.q.ListFrontPage(ctx, params)
	if err != nil {
		h.serverError(w, r, "listing front page", err)
		return
	}

	resp := frontResponse{Items: make([]feedItem, 0, len(rows))}
	if len(rows) > int(limit) {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		cursor := encodeCursor(last.PublishedAt, last.ID)
		resp.NextCursor = &cursor
	}
	for _, row := range rows {
		resp.Items = append(resp.Items, itemFrom(row))
	}
	h.writeJSON(w, resp)
}

// handleArticle serves GET /api/v1/articles/{id}: the article page payload.
// Unknown, unpublished and withdrawn articles are one indistinguishable 404
// - the existence of unpublished work is not public.
func (h *Handler) handleArticle(w http.ResponseWriter, r *http.Request) {
	parsed, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		platformhttp.Problem(w, http.StatusNotFound, "no such article")
		return
	}
	row, err := h.q.GetPublishedArticle(r.Context(), pgtype.UUID{Bytes: parsed, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		platformhttp.Problem(w, http.StatusNotFound, "no such article")
		return
	}
	if err != nil {
		h.serverError(w, r, "loading article", err)
		return
	}
	h.writeJSON(w, articleResponse{
		feedItem: itemFrom(store.ListFrontPageRow{
			ID:                  row.ID,
			PublishedAt:         row.PublishedAt,
			AttributionBlock:    row.AttributionBlock,
			TranslationHeadline: row.TranslationHeadline,
			TranslationExtract:  row.TranslationExtract,
			TargetLocale:        row.TargetLocale,
			OriginalTitle:       row.OriginalTitle,
			RawBody:             row.RawBody,
			SourceUrl:           row.SourceUrl,
			SourceLanguage:      row.SourceLanguage,
			PlaceSlugs:          row.PlaceSlugs,
		}),
		ApprovedAt: row.ApprovedAt.Time.UTC(),
	})
}

// itemFrom applies the contract's column backing: a translated origin
// renders the translation's headline and extract in its target locale; an
// untranslated origin renders the feed-provided title with the extract
// derived from the raw body at read time (research D9), in the source's
// language.
func itemFrom(row store.ListFrontPageRow) feedItem {
	item := feedItem{
		ID:          uuid.UUID(row.ID.Bytes).String(),
		Places:      row.PlaceSlugs,
		Attribution: row.AttributionBlock,
		SourceURL:   row.SourceUrl,
		PublishedAt: row.PublishedAt.Time.UTC(),
	}
	if item.Places == nil {
		item.Places = []string{}
	}
	if row.TargetLocale.Valid {
		item.Lang = row.TargetLocale.String
		item.Headline = row.TranslationHeadline.String
		item.Extract = row.TranslationExtract.String
	} else {
		item.Lang = row.SourceLanguage
		item.Headline = row.OriginalTitle.String
		// raw_body is fetched only for this shape; an absent body simply
		// yields an empty extract rather than a failed request.
		item.Extract = text.DeriveExtract(row.RawBody.String)
	}
	return item
}

// parseLimit applies the contract's pagination bounds. Absent means the
// default; anything not an integer in [1, maxLimit] is rejected.
func parseLimit(s string) (int32, bool) {
	if s == "" {
		return defaultLimit, true
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || n < 1 || n > maxLimit {
		return 0, false
	}
	return int32(n), true
}

// encodeCursor packs a keyset position - the (published_at, id) of the last
// row of a page - into the opaque cursor the contract promises.
func encodeCursor(publishedAt pgtype.Timestamptz, id pgtype.UUID) string {
	raw := publishedAt.Time.UTC().Format(time.RFC3339Nano) + "|" + uuid.UUID(id.Bytes).String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor is the inverse of encodeCursor. Any malformed cursor is an
// error; the handler maps it to 400.
func decodeCursor(s string) (pgtype.Timestamptz, pgtype.UUID, error) {
	if len(s) > maxCursorBytes {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("content: cursor is %d bytes, over the %d-byte bound", len(s), maxCursorBytes)
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("content: cursor encoding: %w", err)
	}
	ts, idPart, ok := strings.Cut(string(raw), "|")
	if !ok {
		return pgtype.Timestamptz{}, pgtype.UUID{}, errors.New("content: cursor lacks its separator")
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("content: cursor timestamp: %w", err)
	}
	id, err := uuid.Parse(idPart)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("content: cursor id: %w", err)
	}
	return pgtype.Timestamptz{Time: at, Valid: true}, pgtype.UUID{Bytes: id, Valid: true}, nil
}

// writeJSON writes a 200 JSON response.
func (h *Handler) writeJSON(w http.ResponseWriter, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		// Unreachable for these response types; keep the wire well-formed.
		platformhttp.Problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		h.log.Warn("writing response", "error", err)
	}
}

// serverError logs the failure and answers an opaque 500 problem - database
// detail never reaches the public surface.
func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, action string, err error) {
	h.log.ErrorContext(r.Context(), action, "error", err)
	platformhttp.Problem(w, http.StatusInternalServerError, "internal error")
}
