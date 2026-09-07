// GET /catalogue: the shelf, one page at a time, in the reader's language
// and scoped to the places they follow (#545, US5 scenario 1, FR-010).
//
// Every rate on the shelf is the member's, exactly as it is on the shop
// page: the same bands, resolved by the same reader, rendered by the same
// rateOf. A shelf that showed one number and a page that showed another
// would be the product contradicting itself between two clicks.

package catalogue

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// catalogueItemResponse is one retailer on the shelf. The shop page's shape
// minus what only the page shows - the country, the terms, the confirmation
// estimate - so a client renders a card from it and follows the slug for
// the rest.
type catalogueItemResponse struct {
	MerchantID     string         `json:"merchant_id"`
	Slug           string         `json:"slug"`
	Name           string         `json:"name"`
	NameLanguage   string         `json:"name_language"`
	NameIsFallback bool           `json:"name_is_fallback"`
	Summary        *string        `json:"summary"`
	Rates          []rateResponse `json:"rates"`
}

// cataloguePageResponse is GET /catalogue: the contract's paged shape.
type cataloguePageResponse struct {
	// Items is always a list, empty when the places hold no retailer, so a
	// client renders "nothing here yet" without telling null from absent.
	Items []catalogueItemResponse `json:"items"`
	// NextCursor is where the next page starts, or null on the last page.
	NextCursor *string `json:"next_cursor"`
}

// listCatalogue implements GET /api/v1/cashback/catalogue.
func (h *Handler) listCatalogue(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Has(categoryParam) {
		platformhttp.Problem(w, http.StatusBadRequest, "category has nothing behind it yet and is refused rather than ignored (#414)")
		return
	}
	limit := DefaultListLimit
	if raw := query.Get(limitParam); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			platformhttp.Problem(w, http.StatusBadRequest, "limit must be a positive whole number")
			return
		}
		limit = n
	}

	page, err := h.listings.List(r.Context(), ListQuery{
		Language: query.Get(languageParam),
		Places:   query[placeParam],
		Search:   query.Get(searchParam),
		Limit:    limit,
		Cursor:   query.Get(cursorParam),
	}, h.now())
	switch {
	case errors.Is(err, ErrNoLanguage):
		platformhttp.Problem(w, http.StatusBadRequest, "lang is required: the reader's language as a BCP-47 primary subtag, such as el or de")
		return
	case errors.Is(err, ErrNoPlace):
		platformhttp.Problem(w, http.StatusBadRequest, "place is required: at least one slug of a place the reader follows")
		return
	case errors.Is(err, ErrUnknownPlace):
		// The refusal names the slug, which the reader typed and can
		// correct; nothing else in the error is theirs to see.
		platformhttp.Problem(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), "catalogue: "))
		return
	case errors.Is(err, ErrBadCursor):
		platformhttp.Problem(w, http.StatusBadRequest, "cursor is not one this listing issued; start again without it")
		return
	case err != nil:
		h.log.ErrorContext(r.Context(), "listing the catalogue", "error", err)
		platformhttp.Problem(w, http.StatusInternalServerError, "")
		return
	}

	h.writeJSON(w, r, http.StatusOK, pageOf(page))
}

// pageOf maps the domain page onto the wire.
func pageOf(page Page) cataloguePageResponse {
	body := cataloguePageResponse{Items: make([]catalogueItemResponse, 0, len(page.Items))}
	for _, item := range page.Items {
		card := catalogueItemResponse{
			MerchantID:     item.ID.String(),
			Slug:           item.Slug,
			Name:           item.Copy.Name,
			NameLanguage:   item.Copy.Language,
			NameIsFallback: item.Copy.Fallback,
			Summary:        textOrNull(item.Copy.Summary),
			Rates:          make([]rateResponse, 0, len(item.Bands)),
		}
		for _, band := range item.Bands {
			card.Rates = append(card.Rates, rateOf(band))
		}
		body.Items = append(body.Items, card)
	}
	if page.NextCursor != "" {
		next := page.NextCursor
		body.NextCursor = &next
	}
	return body
}
