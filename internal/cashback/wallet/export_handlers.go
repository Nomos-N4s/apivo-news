// The export over HTTP: the member's whole history as JSON or CSV
// (T081, FR-003, US3).
//
// One route, two renderings, and the same entries behind both. The JSON is
// the SAME item shape GET /wallet/entries returns, so a client parses one
// thing for both endpoints; the CSV is that shape flattened, because a
// spreadsheet has no nesting and a member opening this in one is the point
// of offering it at all.

package wallet

import (
	"encoding/csv"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
)

// ExportPrefix is the member's own history as a document. A sibling of the
// wallet rather than a path beneath it (contracts/http-api.md), for the
// reason ParticipationPrefix is: FR-003 pairs it with leaving, and a member
// who has left still has one to take.
const ExportPrefix = "/api/v1/cashback/export"

// The renderings this endpoint offers.
const (
	// FormatJSON is the default: the same items GET /wallet/entries
	// returns, wrapped in the export's own envelope.
	FormatJSON = "json"
	// FormatCSV is the flattened rendering, for a spreadsheet.
	FormatCSV = "csv"
)

// exportFilename is what a browser saves the download as. Neither the brand
// nor the member is in it: a filename lands in a downloads folder, gets
// mailed on and ends up in a support ticket, and neither belongs there.
const exportFilename = "cashback-history"

// exportColumns is the CSV header, and the order every row is written in.
//
// Each money figure is TWO columns, minor units and currency, because C-6
// holds in a spreadsheet exactly as it holds on the wire: one column of
// bare integers is a column somebody sums across currencies.
var exportColumns = []string{
	"entry_id",
	"transacted_at",
	"merchant_name",
	"merchant_name_language",
	"merchant_name_is_fallback",
	"sale_amount_minor",
	"sale_currency",
	"cashback_amount_minor",
	"cashback_currency",
	"state",
	"hold_rule",
	"reversal_of_id",
	"reason",
}

// exportEnvelope is the JSON document.
type exportEnvelope struct {
	// ExportedAt is when this document was taken. It is here because the
	// walk only ever moves backwards through the history: an entry written
	// while the export was being assembled is newer than the pages already
	// read and is not in the file, and a document that says when it was
	// taken is one a member can tell that about.
	ExportedAt string `json:"exported_at"`
	// EntryCount is how many entries follow. Written out rather than left
	// to be counted, so a truncated file is one a reader can DETECT: the
	// count and the array would disagree.
	EntryCount int `json:"entry_count"`
	// Entries is the whole history, newest first - the same order and the
	// same item shape as GET /wallet/entries.
	Entries []entryItem `json:"entries"`
}

// getExport implements GET /api/v1/cashback/export.
func (h *Handler) getExport(w http.ResponseWriter, r *http.Request) {
	member := memberFrom(r.Context())
	format, language, detail, ok := parseExport(r.URL.Query())
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}

	history, err := h.exports.All(r.Context(), ExportRequest{Member: member.ID, Language: language})
	switch {
	case errors.Is(err, ErrExportTooLarge):
		// 413 rather than 500: nothing is broken, the document asked for
		// cannot be built in one piece, and the difference matters to
		// whoever is paged.
		h.log.ErrorContext(r.Context(), "a history was too large to export in one document",
			"limit", MaxExportEntries)
		platformhttp.Problem(w, http.StatusRequestEntityTooLarge,
			"this history is too large to export as one document; it holds more than "+
				strconv.Itoa(MaxExportEntries)+" entries")
		return
	case err != nil:
		h.log.ErrorContext(r.Context(), "exporting a member's history", "error", err)
		platformhttp.Problem(w, http.StatusInternalServerError, "")
		return
	}

	if format == FormatCSV {
		h.writeCSV(w, r, history)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+exportFilename+`.json"`)
	h.writeJSON(w, r, http.StatusOK, exportEnvelope{
		ExportedAt: time.Now().UTC().Format(timeFormat),
		EntryCount: len(history),
		Entries:    itemsFrom(history),
	})
}

// itemsFrom renders the history as the contract's items - the same shape
// GET /wallet/entries returns, from the same function, so the two endpoints
// cannot describe one entry differently.
func itemsFrom(history []Entry) []entryItem {
	items := make([]entryItem, 0, len(history))
	for _, entry := range history {
		items = append(items, itemFrom(entry))
	}
	return items
}

// writeCSV renders the history as a spreadsheet.
//
// The header goes out first and the rows follow in the history's own order,
// newest first - the same order as the JSON, because two orderings of one
// export is a difference somebody would eventually rely on.
func (h *Handler) writeCSV(w http.ResponseWriter, r *http.Request, history []Entry) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+exportFilename+`.csv"`)
	w.WriteHeader(http.StatusOK)

	out := csv.NewWriter(w)
	if err := out.Write(exportColumns); err != nil {
		h.log.WarnContext(r.Context(), "writing the export header", "error", err)
		return
	}
	for _, entry := range history {
		if err := out.Write(csvRow(entry)); err != nil {
			h.log.WarnContext(r.Context(), "writing an export row", "error", err)
			return
		}
	}
	out.Flush()
	if err := out.Error(); err != nil {
		h.log.WarnContext(r.Context(), "finishing the export", "error", err)
	}
}

// csvRow flattens one entry, in exportColumns order.
//
// An absent value is an empty cell. That conflates "no merchant" with "a
// merchant whose name is blank", and it is safe only because the schema has
// no blank names - a merchant copy row carries a name or does not exist. The
// JSON rendering keeps the two apart with null, which is what a client that
// needs the difference should read.
func csvRow(entry Entry) []string {
	item := itemFrom(entry)
	return []string{
		item.EntryID,
		item.TransactedAt,
		cell(deref(item.MerchantName)),
		cell(deref(item.MerchantNameLanguage)),
		strconv.FormatBool(item.MerchantNameIsFallback),
		strconv.FormatInt(item.SaleAmount.Minor, 10),
		item.SaleAmount.Currency,
		strconv.FormatInt(item.CashbackAmount.Minor, 10),
		item.CashbackAmount.Currency,
		item.State,
		cell(deref(item.HoldRule)),
		cell(deref(item.ReversalOfID)),
		cell(deref(item.Reason)),
	}
}

// deref reads an optional string, answering "" for absent.
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// formulaLeaders are the characters a spreadsheet reads as the start of a
// FORMULA rather than of text.
const formulaLeaders = "=+-@\t\r"

// cell neutralises a value a spreadsheet would execute.
//
// A merchant name comes from an affiliate network's catalogue and a reason
// is typed by an operator, so both are text this process did not write. A
// cell beginning =, +, -, @, a tab or a carriage return is a formula to
// Excel, LibreOffice and Sheets - which is how a name like
// `=HYPERLINK(...)` becomes a link a member clicks in a file their own
// wallet handed them.
//
// The mitigation is a leading apostrophe, which those applications consume
// as "the rest is literal text". It is visible in a plain-text reader, and
// that is the trade: a character somebody may notice, against a cell that
// runs. Only fields that could carry such a value are passed through here;
// the ids, timestamps and amounts are written by this process and cannot.
func cell(value string) string {
	if value == "" || !strings.ContainsAny(value[:1], formulaLeaders) {
		return value
	}
	return "'" + value
}

// parseExport reads the query parameters.
//
// Anything it does not serve is REFUSED rather than ignored, and state and
// limit are refused by name with the reason: an export is the complete
// record, and a client that asked for part of one and silently got all of
// it - or thought it had asked for all and got part - would be handing a
// member a document that is not what they think it is.
func parseExport(values url.Values) (format, language, detail string, ok bool) {
	for name, given := range values {
		// A repeated parameter is refused rather than resolved. url.Values
		// answers the FIRST of them, so ?format=csv&format=xlsx would send a
		// CSV to a client that also asked for something else and was never
		// told it did not get it. The paging endpoint resolves them the same
		// silent way and can afford to: a wrong page is one request. A wrong
		// export is a file a member keeps.
		if len(given) > 1 {
			return "", "", strconv.Quote(name) + " was given " + strconv.Itoa(len(given)) +
				" times; this endpoint answers one export and cannot tell which you meant", false
		}
		switch name {
		case "format", "lang":
		case "state", "limit", "cursor":
			return "", "", "an export is always the complete history, so it takes no " +
				strconv.Quote(name) + "; use /api/v1/cashback/wallet/entries to page or filter", false
		default:
			return "", "", "unknown query parameter " + strconv.Quote(name) +
				"; this endpoint accepts format and lang", false
		}
	}
	format = values.Get("format")
	switch format {
	case "":
		format = FormatJSON
	case FormatJSON, FormatCSV:
	default:
		return "", "", "format must be " + FormatJSON + " or " + FormatCSV, false
	}
	return format, values.Get("lang"), "", true
}
