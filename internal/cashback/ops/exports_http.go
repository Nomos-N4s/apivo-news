// The accounting exports over HTTP (T114, FR-062).
//
// Two routes, two renderings each, and the same rows behind both: JSON for
// a system, CSV for a spreadsheet. The CSV is the JSON flattened, with every
// amount as two columns - minor units and currency - because C-6 holds in a
// spreadsheet exactly as it holds on the wire: one column of bare integers
// is a column somebody sums across currencies.

package ops

import (
	"encoding/csv"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	platformhttp "github.com/Nomos-N4s/apivo-news/internal/platform/http"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// The renderings an export offers.
const (
	formatJSON = "json"
	formatCSV  = "csv"
)

// exportEnvelope is the JSON document: when it was taken, what it covers,
// and how many rows follow - written out rather than left to be counted, so
// a truncated file is one a reader can detect.
type exportEnvelope struct {
	ExportedAt string `json:"exported_at"`
	From       string `json:"from"`
	To         string `json:"to"`
	RowCount   int    `json:"row_count"`
	Rows       any    `json:"rows"`
}

// ledgerRowJSON is one ledger journal row on the wire.
type ledgerRowJSON struct {
	TransitionID         string       `json:"transition_id"`
	EntryID              string       `json:"entry_id"`
	AccountID            string       `json:"account_id"`
	BrandID              string       `json:"brand_id"`
	NetworkTransactionID string       `json:"network_transaction_id"`
	FromState            *string      `json:"from_state"`
	ToState              string       `json:"to_state"`
	Amount               money.Amount `json:"amount"`
	LedgerTransferRef    string       `json:"ledger_transfer_ref"`
	Reason               *string      `json:"reason"`
	ActorID              *string      `json:"actor_id"`
	OccurredAt           string       `json:"occurred_at"`
}

// ledgerColumns is the CSV header, and the order every row is written in.
var ledgerColumns = []string{
	"transition_id", "occurred_at", "entry_id", "account_id", "brand_id", "network_transaction_id",
	"from_state", "to_state", "amount_minor", "currency", "ledger_transfer_ref", "actor_id", "reason",
}

// reconciliationRowJSON is one reconciliation journal row on the wire.
type reconciliationRowJSON struct {
	DifferenceID         string          `json:"difference_id"`
	RunID                string          `json:"run_id"`
	NetworkAccountID     string          `json:"network_account_id"`
	NetworkID            string          `json:"network_id"`
	ExternalPublisherID  string          `json:"external_publisher_id"`
	StatementPeriod      periodJSON      `json:"statement_period"`
	Kind                 string          `json:"kind"`
	NetworkTransactionID *string         `json:"network_transaction_id"`
	TransactionID        string          `json:"transaction_id"`
	Expected             *money.Amount   `json:"expected"`
	Actual               *money.Amount   `json:"actual"`
	Delta                money.Amount    `json:"delta"`
	DetectedAt           string          `json:"detected_at"`
	Resolution           *resolutionJSON `json:"resolution"`
}

// reconciliationColumns is the CSV header for the reconciliation journal.
var reconciliationColumns = []string{
	"difference_id", "detected_at", "run_id", "network_account_id", "network_id", "external_publisher_id",
	"statement_period_start", "statement_period_end", "kind", "network_transaction_id", "transaction_id",
	"expected_minor", "actual_minor", "delta_minor", "currency", "resolution", "resolved_by", "resolved_at", "reason",
}

// exportLedger implements GET /api/v1/cashback/ops/exports/ledger.
func (h *Handler) exportLedger(w http.ResponseWriter, r *http.Request) {
	window, format, detail, ok := parseExportQuery(r.URL.Query())
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}
	rows, err := h.reconciliation.ExportLedger(r.Context(), window)
	if !h.exportRead(w, r, "the ledger journal", err) {
		return
	}
	if format == formatCSV {
		records := make([][]string, 0, len(rows))
		for _, row := range rows {
			records = append(records, ledgerRecord(row))
		}
		h.writeCSV(w, r, exportFilename("cashback-ledger", window)+".csv", ledgerColumns, records)
		return
	}
	items := make([]ledgerRowJSON, 0, len(rows))
	for _, row := range rows {
		items = append(items, ledgerRowOf(row))
	}
	h.writeExport(w, r, exportFilename("cashback-ledger", window)+".json", window, len(items), items)
}

// exportReconciliation implements
// GET /api/v1/cashback/ops/exports/reconciliation.
func (h *Handler) exportReconciliation(w http.ResponseWriter, r *http.Request) {
	window, format, detail, ok := parseExportQuery(r.URL.Query())
	if !ok {
		platformhttp.Problem(w, http.StatusBadRequest, detail)
		return
	}
	rows, err := h.reconciliation.ExportReconciliation(r.Context(), window)
	if !h.exportRead(w, r, "the reconciliation journal", err) {
		return
	}
	if format == formatCSV {
		records := make([][]string, 0, len(rows))
		for _, row := range rows {
			records = append(records, reconciliationRecord(row))
		}
		h.writeCSV(w, r, exportFilename("cashback-reconciliation", window)+".csv", reconciliationColumns, records)
		return
	}
	items := make([]reconciliationRowJSON, 0, len(rows))
	for _, row := range rows {
		items = append(items, reconciliationRowOf(row))
	}
	h.writeExport(w, r, exportFilename("cashback-reconciliation", window)+".json", window, len(items), items)
}

// exportRead answers the store's refusals for an export, reporting whether
// the handler may go on.
func (h *Handler) exportRead(w http.ResponseWriter, r *http.Request, journal string, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrInvalidWindow):
		platformhttp.Problem(w, http.StatusBadRequest, detailOf(err, ErrInvalidWindow))
	case errors.Is(err, ErrExportTooLarge):
		// 413 rather than 500: nothing is broken, the document asked for
		// cannot be built in one piece, and a narrower window can.
		platformhttp.Problem(w, http.StatusRequestEntityTooLarge,
			"this window holds more than "+strconv.Itoa(MaxExportRows)+" rows, more than one export carries; ask for a narrower window")
	default:
		h.internalError(w, r, "exporting "+journal, err)
	}
	return false
}

// parseExportQuery reads from, to and format. Anything else is refused
// rather than ignored: an export is the complete record for its window, and
// a client that asked for part of one and silently got all of it would be
// handing an accountant a document that is not what they think it is.
func parseExportQuery(values url.Values) (window ExportWindow, format, detail string, ok bool) {
	for name, given := range values {
		if len(given) > 1 {
			return ExportWindow{}, "", strconv.Quote(name) + " was given " + strconv.Itoa(len(given)) +
				" times; this endpoint answers one export and cannot tell which you meant", false
		}
		switch name {
		case "from", "to", "format":
		default:
			return ExportWindow{}, "", "unknown query parameter " + strconv.Quote(name) + "; this endpoint accepts from, to and format", false
		}
	}
	if !values.Has("from") || !values.Has("to") {
		return ExportWindow{}, "", "an export covers a window: supply from and to as RFC 3339 timestamps", false
	}
	from, err := time.Parse(time.RFC3339Nano, values.Get("from"))
	if err != nil {
		return ExportWindow{}, "", "from is not an RFC 3339 timestamp", false
	}
	to, err := time.Parse(time.RFC3339Nano, values.Get("to"))
	if err != nil {
		return ExportWindow{}, "", "to is not an RFC 3339 timestamp", false
	}
	window = ExportWindow{From: from, To: to}
	if err := window.Validate(); err != nil {
		return ExportWindow{}, "", detailOf(err, ErrInvalidWindow), false
	}
	format = formatJSON
	if values.Has("format") {
		switch values.Get("format") {
		case formatJSON, formatCSV:
			format = values.Get("format")
		default:
			return ExportWindow{}, "", "format must be json or csv", false
		}
	}
	return window, format, "", true
}

// exportFilename is what a browser saves the download as: the journal and
// the window's dates, so two exports do not overwrite each other in a
// downloads folder. No brand and no account in it - a filename gets mailed
// on and ends up in a support ticket.
func exportFilename(journal string, w ExportWindow) string {
	return journal + "-" + w.From.UTC().Format("20060102") + "-" + w.To.UTC().Format("20060102")
}

// writeExport writes the JSON document as an attachment.
func (h *Handler) writeExport(w http.ResponseWriter, r *http.Request, filename string, window ExportWindow, count int, rows any) {
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	h.writeJSON(w, r, exportEnvelope{
		ExportedAt: stamp(time.Now()),
		From:       stamp(window.From),
		To:         stamp(window.To),
		RowCount:   count,
		Rows:       rows,
	})
}

// writeCSV writes the header and the records as an attachment.
func (h *Handler) writeCSV(w http.ResponseWriter, r *http.Request, filename string, columns []string, records [][]string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.WriteHeader(http.StatusOK)
	out := csv.NewWriter(w)
	if err := out.Write(columns); err != nil {
		h.log.WarnContext(r.Context(), "writing the export header", "error", err)
		return
	}
	for _, record := range records {
		if err := out.Write(record); err != nil {
			h.log.WarnContext(r.Context(), "writing an export row", "error", err)
			return
		}
	}
	out.Flush()
	if err := out.Error(); err != nil {
		h.log.WarnContext(r.Context(), "finishing the export", "error", err)
	}
}

// ledgerRowOf renders one ledger row for JSON.
func ledgerRowOf(row LedgerRow) ledgerRowJSON {
	return ledgerRowJSON{
		TransitionID:         row.TransitionID.String(),
		EntryID:              row.EntryID.String(),
		AccountID:            row.Member.String(),
		BrandID:              row.Brand,
		NetworkTransactionID: row.Report.String(),
		FromState:            optional(row.From),
		ToState:              row.To,
		Amount:               row.Amount,
		LedgerTransferRef:    row.TransferRef,
		Reason:               optional(row.Reason),
		ActorID:              optionalID(row.Actor),
		OccurredAt:           stamp(row.OccurredAt),
	}
}

// ledgerRecord flattens one ledger row, in ledgerColumns order.
func ledgerRecord(row LedgerRow) []string {
	return []string{
		row.TransitionID.String(),
		stamp(row.OccurredAt),
		row.EntryID.String(),
		row.Member.String(),
		spreadsheetCell(row.Brand),
		row.Report.String(),
		row.From,
		row.To,
		strconv.FormatInt(row.Amount.Minor, 10),
		row.Amount.Currency.String(),
		spreadsheetCell(row.TransferRef),
		idOrBlank(row.Actor),
		spreadsheetCell(row.Reason),
	}
}

// reconciliationRowOf renders one reconciliation row for JSON.
func reconciliationRowOf(row ReconciliationRow) reconciliationRowJSON {
	item := reconciliationRowJSON{
		DifferenceID:         row.DifferenceID.String(),
		RunID:                row.Run.String(),
		NetworkAccountID:     row.Account.String(),
		NetworkID:            row.Network,
		ExternalPublisherID:  row.Publisher,
		StatementPeriod:      periodJSON{Start: stamp(row.Period.Start), End: stamp(row.Period.End)},
		Kind:                 string(row.Kind),
		NetworkTransactionID: optionalID(row.Report),
		TransactionID:        row.TransactionID,
		Expected:             row.Expected,
		Actual:               row.Actual,
		Delta:                row.Delta,
		DetectedAt:           stamp(row.DetectedAt),
	}
	if row.Resolution != nil {
		item.Resolution = &resolutionJSON{
			Resolution: string(row.Resolution.Verdict),
			ResolvedBy: row.Resolution.ResolvedBy.String(),
			Reason:     row.Resolution.Reason,
			ResolvedAt: stamp(row.Resolution.ResolvedAt),
		}
	}
	return item
}

// reconciliationRecord flattens one reconciliation row, in
// reconciliationColumns order. An absent figure is an empty cell.
func reconciliationRecord(row ReconciliationRow) []string {
	var resolution, resolvedBy, resolvedAt, reason string
	if row.Resolution != nil {
		resolution = string(row.Resolution.Verdict)
		resolvedBy = row.Resolution.ResolvedBy.String()
		resolvedAt = stamp(row.Resolution.ResolvedAt)
		reason = row.Resolution.Reason
	}
	return []string{
		row.DifferenceID.String(),
		stamp(row.DetectedAt),
		row.Run.String(),
		row.Account.String(),
		spreadsheetCell(row.Network),
		spreadsheetCell(row.Publisher),
		stamp(row.Period.Start),
		stamp(row.Period.End),
		string(row.Kind),
		idOrBlank(row.Report),
		spreadsheetCell(row.TransactionID),
		minorOrBlank(row.Expected),
		minorOrBlank(row.Actual),
		strconv.FormatInt(row.Delta.Minor, 10),
		row.Delta.Currency.String(),
		resolution,
		resolvedBy,
		resolvedAt,
		spreadsheetCell(reason),
	}
}

// optional renders "" as null.
func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// optionalID renders uuid.Nil as null.
func optionalID(id uuid.UUID) *string {
	if id == uuid.Nil {
		return nil
	}
	s := id.String()
	return &s
}

// idOrBlank renders uuid.Nil as an empty cell.
func idOrBlank(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// minorOrBlank renders an absent amount as an empty cell.
func minorOrBlank(amount *money.Amount) string {
	if amount == nil {
		return ""
	}
	return strconv.FormatInt(amount.Minor, 10)
}

// formulaLeaders are the characters a spreadsheet reads as the start of a
// FORMULA rather than of text.
const formulaLeaders = "=+-@\t\r"

// spreadsheetCell neutralises a value a spreadsheet would execute - the
// same guard the member's history export applies, for the same reason. A
// reason is typed by an operator, a transaction id comes from a network, a
// brand or publisher id from configuration: none of it was written by this
// process, and a cell beginning =, +, -, @, a tab or a carriage return is a
// formula to Excel, LibreOffice and Sheets. A leading apostrophe makes it
// literal text; ids, timestamps and amounts are this process's own and are
// not passed through here.
func spreadsheetCell(value string) string {
	if value == "" || !strings.ContainsAny(value[:1], formulaLeaders) {
		return value
	}
	return "'" + value
}
