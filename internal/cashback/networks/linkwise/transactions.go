// Reading one window of transactions off Linkwise's report and translating
// each row into the port's own value (T247, contract rules 1, 2, 3, 7 and 8).
//
// The whole window arrives in ONE response. This network has no paging - nine
// paging parameters were each sent and each ignored, the answer coming back
// byte-identical every time - so there is no page loop here and no cursor
// inside a window. The date window is the only lever, which is why
// [Limits.MaxWindow] is small and why it is a latency budget rather than a
// limit Linkwise imposes.

package linkwise

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"time"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// transactionsEndpoint is the report every window is read from.
const transactionsEndpoint = "reports_transaction.html"

// reportFields is the comma-separated list every query asks for, and it is
// the ONLY required parameter this endpoint takes.
//
// RECONSTRUCTED FROM THE RECORDING rather than chosen. Every token below
// appears in the field list the endpoint's own usage text publishes
// (testdata/error-400.json), and this exact set is what produced
// testdata/transactions.json - which is what makes that file evidence for
// this list and not merely for the fields the adapter happens to read.
//
// It asks for more than it reads, deliberately. subid2 and subid3, the
// payout categories and the payment status are never looked at by the code
// below; they are in the answer so that they are in RawPayload, which is the
// only thing a normalisation fix can ever be re-derived from once the network
// has stopped serving the window (contract rule 1, FR-032).
//
// subid4 and subid5 are NOT asked for. The usage text lists them and they are
// very probably fine; no recording proves it, and a field that turns out not
// to be accepted takes the whole window down with a 400 rather than arriving
// empty. Adding one means re-recording first.
const reportFields = "program,subid1,subid2,subid3,transaction_id,type,status," +
	"subaction,amended,amount,commission,click_date,transaction_date,status_date," +
	"click_ref_url,payout_cat,payment_status"

// FetchTransactions yields every transaction Linkwise reported inside window
// (contract rule 3: the window is refused before any I/O if it is wider than
// [Limits] allows, never clamped).
//
// The report is asked for on the TRANSACTION-date axis. Linkwise can answer
// on the status-date axis too - based_on=status, which would find a
// validation that landed months after the purchase without re-reading the
// purchase's own period - and that is genuinely the better shape for late
// approvals. It is not used here because the port's window is a window of
// transaction dates and nothing on this interface can say which axis was
// meant; an adapter that silently switched would answer a different question
// than the poller's cursor is tracking.
func (c *Client) FetchTransactions(ctx context.Context, window networks.QueryWindow) (iter.Seq2[networks.Reported, error], error) {
	if err := c.Limits().ValidateWindow(window); err != nil {
		return nil, fmt.Errorf("linkwise: %w", err)
	}
	query, err := windowQuery(window)
	if err != nil {
		return nil, fmt.Errorf("linkwise: %w", err)
	}
	query.Set("fields", reportFields)
	// The transaction-date axis, stated rather than defaulted: the usage text
	// says the parameter decides which dates the limits apply to, and a
	// default is a thing a network can change.
	query.Set("based_on", "transaction")

	return func(yield func(networks.Reported, error) bool) {
		body, err := c.Get(ctx, transactionsEndpoint, query)
		if err != nil {
			// Everything that comes from CONTACTING the network is yielded
			// rather than returned, whether it happened on the first byte or
			// the last, so an eager adapter and a lazy one report an expired
			// credential through the same channel.
			//
			// Except when it was US who stopped. A cancelled context reaches
			// here as a transport failure like any other, and reporting it as
			// one would be true and useless: rule 8 exists so that a caller
			// can tell "the network did not answer" from "we gave up", and
			// only the second means the window was never read at all.
			yield(networks.Reported{}, c.stopped(ctx, window, err))
			return
		}
		rows, err := decodeReport(body)
		if err != nil {
			yield(networks.Reported{}, fmt.Errorf("linkwise: reading %s: %w", window, err))
			return
		}
		for i, row := range rows {
			// Checked per row rather than once: the whole window is in
			// memory, so the only thing left to abandon is the translation,
			// and a poller shutting down mid-window must not have the loop
			// end looking like a whole answer (contract rule 8).
			if err := ctx.Err(); err != nil {
				yield(networks.Reported{}, networks.AbandonedIteration(err))
				return
			}
			reported, err := c.translate(row)
			if err != nil {
				yield(networks.Reported{}, fmt.Errorf("linkwise: transaction %d of %s: %w", i+1, window, err))
				return
			}
			// The port's single definition of membership, so every adapter
			// filters identically. Nothing should reach here outside the
			// window - the query's upper bound is the window's own, one
			// second back - but an adapter that returned what it was not
			// asked for would have a poller store transactions its cursor
			// never covered.
			if !window.Contains(reported.TransactedAt) {
				continue
			}
			if !yield(reported, nil) {
				return
			}
		}
	}, nil
}

// stopped classifies a failed fetch: abandonment if this process was the one
// that stopped, and the network's own failure otherwise.
//
// The context is checked rather than the error, deliberately. A cancelled
// request surfaces through several layers - the limiter waiting for a token,
// the backoff sleeping between attempts, the HTTP client mid-body - and each
// wraps context.Canceled differently or, in one case, not at all. What is
// certain is whether the caller's own context is done.
func (c *Client) stopped(ctx context.Context, window networks.QueryWindow, err error) error {
	if cause := ctx.Err(); cause != nil {
		return networks.AbandonedIteration(fmt.Errorf("reading %s: %w", window, cause))
	}
	return fmt.Errorf("linkwise: reading %s: %w", window, err)
}

// reportRow is one transaction as the report returns it.
//
// The names are Linkwise's, and two of them are worth pausing on: the
// transaction's own id arrives as "id" although the field is requested as
// "transaction_id", and the transaction date arrives as "date" although it is
// requested as "transaction_date". Requesting one name and reading another is
// exactly the kind of thing that is obvious in a recording and invisible in
// documentation, which is why the recording is what this struct was written
// from.
type reportRow struct {
	ID     json.Number `json:"id"`
	Type   string      `json:"type"`
	Amount string      `json:"amount"`
	Commis string      `json:"commission"`
	Date   string      `json:"date"`
	SubID1 *string     `json:"subid1"`
	Status struct {
		Name string `json:"name"`
		Date string `json:"date"`
	} `json:"status"`
	Click struct {
		Date   string  `json:"date"`
		RefURL *string `json:"ref_url"`
	} `json:"click"`
	Program struct {
		ID   json.Number `json:"id"`
		Name string      `json:"name"`
	} `json:"program"`

	// raw is this row's own bytes, kept so the evidence carries the fragment
	// it was normalised from rather than a re-encoding of the fields above.
	// A re-encoding would silently drop every field this struct does not
	// name, which is most of them.
	raw json.RawMessage
}

// decodeReport turns one response body into rows, accepting either envelope
// this API can answer in.
//
// The default is a bare JSON array and rest_json_force_object=on makes it an
// object carrying "response"; both are recorded. This adapter never sends
// that option, so the object form should not arrive - but reading it costs a
// second attempt at unmarshalling and buys immunity to a deployment-wide
// default nobody here set.
//
// An error body arriving with a 200 is the case worth naming: without this,
// {"error":{...}} would fail to unmarshal into a slice and be reported as
// "cannot unmarshal object into []reportRow", which is a true sentence that
// names neither the network nor what it said.
func decodeReport(body []byte) ([]reportRow, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil, fmt.Errorf("%w: Linkwise answered 200 with an empty body", networks.ErrNetworkUnavailable)
	}

	// Raw first, so each row keeps its own bytes for RawPayload.
	var rawRows []json.RawMessage
	if err := json.Unmarshal(body, &rawRows); err != nil {
		var envelope struct {
			Response []json.RawMessage `json:"response"`
			Error    struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			return nil, fmt.Errorf("%w: Linkwise's answer is neither a report nor an error: %w",
				networks.ErrNetworkUnavailable, err)
		}
		if envelope.Error.Name != "" || envelope.Error.Description != "" {
			return nil, fmt.Errorf("%w: Linkwise answered 200 carrying an error (%s: %s)",
				networks.ErrNetworkUnavailable, envelope.Error.Name, firstLine(envelope.Error.Description))
		}
		rawRows = envelope.Response
	}

	rows := make([]reportRow, 0, len(rawRows))
	for i, raw := range rawRows {
		var row reportRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("%w: transaction %d of the report will not parse: %w",
				networks.ErrNetworkUnavailable, i+1, err)
		}
		row.raw = raw
		rows = append(rows, row)
	}
	return rows, nil
}

// translate turns one reported row into the port's value, and calls
// [networks.Reported.Validate] on it before anybody sees it (contract rule
// 7): a mis-mapped field is then caught at the adapter that made it rather
// than at an INSERT halfway through a window.
func (c *Client) translate(row reportRow) (networks.Reported, error) {
	externalID := strings.TrimSpace(row.ID.String())

	status, err := mapTransactionStatus(externalID, row.Status.Name)
	if err != nil {
		return networks.Reported{}, err
	}
	sale, err := c.amount(externalID, "amount", row.Amount)
	if err != nil {
		return networks.Reported{}, err
	}
	commission, err := c.amount(externalID, "commission", row.Commis)
	if err != nil {
		return networks.Reported{}, err
	}
	transactedAt, err := timestamp(externalID, "transaction date", row.Date)
	if err != nil {
		return networks.Reported{}, err
	}

	reported := networks.Reported{
		ExternalID:   externalID,
		ClickRef:     clickRef(row.SubID1),
		StatusRaw:    row.Status.Name,
		Status:       status,
		SaleAmount:   sale,
		Commission:   commission,
		TransactedAt: transactedAt,
		RawPayload:   row.raw,
	}
	if err := reported.Validate(); err != nil {
		return networks.Reported{}, err
	}
	return reported, nil
}

// clickRef reads the reference off subid1, distinguishing "the network
// reported none" from "the network reported a blank one".
//
// JSON null and a missing field both arrive as a nil pointer and both mean
// absent, which is ORDINARY rather than broken: a transaction with no
// matching click is recorded as unattributed and never auto-credited
// (FR-034). An empty string is a different thing - the network said
// something, and it said nothing - and it is carried through as present so
// that [networks.Reported.Validate] refuses it. Collapsing the two here
// would make an adapter bug that blanks every reference look exactly like a
// network that reports none.
func clickRef(subID *string) networks.ClickRef {
	if subID == nil {
		return networks.ClickRef{}
	}
	return networks.NewClickRef(*subID)
}

// amount turns one of Linkwise's decimal strings into minor units of the
// account's currency.
func (c *Client) amount(externalID, field, raw string) (money.Amount, error) {
	minor, err := minorUnits(raw)
	if err != nil {
		return money.Amount{}, fmt.Errorf("transaction %s: %s: %w", strconv.Quote(externalID), field, err)
	}
	return money.New(minor, c.currency)
}

// timestamp parses one of Linkwise's dates, which arrive in ISO 8601 with an
// offset - "2024-06-07T19:10:54+03:00" - because rest_date_format is left at
// its default rather than switched to a Unix timestamp.
//
// Converted to UTC on the way in. The offset is Athens', and a report read at
// one time of year would otherwise sit an hour from the same instant read at
// another; every window this adapter is asked for is a pair of UTC instants.
func timestamp(externalID, field, raw string) (time.Time, error) {
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, fmt.Errorf("transaction %s: %s %s is not an ISO 8601 timestamp: %w",
			strconv.Quote(externalID), field, strconv.Quote(raw), err)
	}
	return at.UTC(), nil
}
