package linkwise_test

// Reading a window of transactions off the report (T247), against the
// recording that is this API's only schema.
//
// The recording is served verbatim rather than rewritten into convenient
// shapes: the point of having captured it is that the adapter is exercised
// against what the network actually sends, three-object nesting, null
// sub-ids, day-first everything and all.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/linkwise"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// The window around the first recorded transaction, 2024-06-07T19:10:54+03:00.
// Seven days, which is the widest this adapter will ask for.
func theJuneWindow() networks.QueryWindow {
	return networks.QueryWindow{
		From: time.Date(2024, time.June, 3, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2024, time.June, 10, 0, 0, 0, 0, time.UTC),
	}
}

// servingRecording answers every request with one of the files in testdata/.
func servingRecording(t *testing.T, name string) *linkwise.Client {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}
	return serving(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})
}

// collect drains an iteration, returning what it yielded and the error it
// ended with, if any.
func collect(t *testing.T, seq func(func(networks.Reported, error) bool)) ([]networks.Reported, error) {
	t.Helper()
	var got []networks.Reported
	var failed error
	for reported, err := range seq {
		if err != nil {
			failed = err
			break
		}
		got = append(got, reported)
	}
	return got, failed
}

// TestTheRecordedTransactionIsTranslated is the round trip: the bytes the
// live API sent, through the adapter, into the value an evidence row is
// written from.
func TestTheRecordedTransactionIsTranslated(t *testing.T) {
	t.Parallel()

	client := servingRecording(t, "transactions.json")
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	got, failed := collect(t, seq)
	if failed != nil {
		t.Fatalf("the iteration ended with %v", failed)
	}

	// One of the three recorded transactions falls in this window; the other
	// two are June's neighbours by four and six months, and the window filter
	// is what leaves them out.
	if len(got) != 1 {
		t.Fatalf("the window yielded %d transactions, want the 1 that falls inside it", len(got))
	}
	reported := got[0]

	if reported.ExternalID != "420343717" {
		t.Errorf("ExternalID = %q, want the network's own transaction id", reported.ExternalID)
	}
	if reported.StatusRaw != "Validated" {
		t.Errorf("StatusRaw = %q, want the network's own word verbatim", reported.StatusRaw)
	}
	if reported.Status != networks.StatusConfirmed {
		t.Errorf("Status = %q, want confirmed", reported.Status)
	}
	if want := (money.Amount{Minor: 1951, Currency: "EUR"}); reported.SaleAmount != want {
		t.Errorf("SaleAmount = %s, want %s (from \"19.51\")", reported.SaleAmount, want)
	}
	if want := (money.Amount{Minor: 293, Currency: "EUR"}); reported.Commission != want {
		t.Errorf("Commission = %s, want %s (from \"2.93\")", reported.Commission, want)
	}
	// 19:10:54+03:00 is 16:10:54 UTC. Stored in UTC so that a report read in
	// July and one read in January name the same instant the same way.
	if want := time.Date(2024, time.June, 7, 16, 10, 54, 0, time.UTC); !reported.TransactedAt.Equal(want) {
		t.Errorf("TransactedAt = %s, want %s", reported.TransactedAt, want)
	}
	if reported.TransactedAt.Location() != time.UTC {
		t.Errorf("TransactedAt carries the zone %s, want UTC", reported.TransactedAt.Location())
	}
	// subid1 is null in the recording, which is ordinary: this account's
	// history predates any click this system issued. Absent, not blank.
	if ref, present := reported.ClickRef.Ref(); present {
		t.Errorf("ClickRef = %q, want the definite absence a null subid1 means", ref)
	}
}

// TestTheRawPayloadIsTheRowsOwnBytes is contract rule 1, and the assertion
// that stops the payload being a re-encoding of the fields the struct names.
//
// A re-encoding would silently drop every field the adapter does not read -
// the payout categories, the payment status, the programme name, subid2 and
// subid3 - which is most of the row and all of what a normalisation fix would
// ever be re-derived from.
func TestTheRawPayloadIsTheRowsOwnBytes(t *testing.T) {
	t.Parallel()

	client := servingRecording(t, "transactions.json")
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	got, failed := collect(t, seq)
	if failed != nil || len(got) != 1 {
		t.Fatalf("the iteration yielded %d transactions and ended with %v", len(got), failed)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(got[0].RawPayload, &payload); err != nil {
		t.Fatalf("the payload is not the row's own object: %v", err)
	}
	// Every field the adapter never looks at, which is the point of keeping
	// the bytes rather than re-encoding what it does.
	for _, field := range []string{
		"payout_categories", "payment_status", "subaction", "amended",
		"type", "subid2", "subid3", "program", "click",
	} {
		if _, ok := payload[field]; !ok {
			t.Errorf("the payload does not carry %q; it is a re-encoding of what the adapter reads rather than what the network sent", field)
		}
	}
}

// TestAWindowWiderThanTheNetworkAllowsIsRefusedBeforeAnyIO is contract rule 3.
//
// Refused, never clamped: a caller that asked for ninety days and silently
// received seven would advance its cursor past eighty-three days it never
// saw. And refused BEFORE the request, which is what the request counter
// here is for - a refusal after the fetch would still have spent the twelve
// seconds the fetch costs.
func TestAWindowWiderThanTheNetworkAllowsIsRefusedBeforeAnyIO(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`[]`))
	})

	from := time.Date(2024, time.June, 1, 0, 0, 0, 0, time.UTC)
	wide := networks.QueryWindow{From: from, To: from.Add(linkwise.Limits().MaxWindow + time.Second)}
	seq, err := client.FetchTransactions(t.Context(), wide)
	if !errors.Is(err, networks.ErrWindowTooWide) {
		t.Fatalf("FetchTransactions(a window one second too wide) = %v, want ErrWindowTooWide", err)
	}
	if seq != nil {
		t.Error("a refused window still returned a sequence to range over")
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("%d requests were made for a window that was refused", got)
	}
}

// TestAnUnusableWindowIsRefusedImmediately: the immediate error covers what
// is checkable without contacting the network, and this is all of it.
func TestAnUnusableWindowIsRefusedImmediately(t *testing.T) {
	t.Parallel()

	at := time.Date(2024, time.June, 7, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name   string
		window networks.QueryWindow
	}{
		{name: "no bounds", window: networks.QueryWindow{}},
		{name: "ends before it starts", window: networks.QueryWindow{From: at, To: at.Add(-time.Hour)}},
		{name: "narrower than a second", window: networks.QueryWindow{From: at, To: at.Add(time.Millisecond)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := servingRecording(t, "transactions.json")
			if _, err := client.FetchTransactions(t.Context(), tt.window); err == nil {
				t.Fatal("an unusable window was accepted")
			}
		})
	}
}

// TestTheQueryAsksForTheFieldsAndTheTransactionAxis. fields is the endpoint's
// only required parameter, and based_on decides which dates the window's
// limits apply to - the port's window is a window of transaction dates, so
// the axis is stated rather than defaulted.
func TestTheQueryAsksForTheFieldsAndTheTransactionAxis(t *testing.T) {
	t.Parallel()

	var seen http.Request
	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		seen = *r.Clone(context.Background())
		_, _ = w.Write([]byte(`[]`))
	})
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	if _, failed := collect(t, seq); failed != nil {
		t.Fatalf("the iteration ended with %v", failed)
	}

	query := seen.URL.Query()
	if got := query.Get("based_on"); got != "transaction" {
		t.Errorf("based_on = %q, want transaction", got)
	}
	fields := query.Get("fields")
	if fields == "" {
		t.Fatal("no fields were asked for, and it is this endpoint's only required parameter")
	}
	// Every field the adapter reads must be in the list, or the answer comes
	// back without it and translation fails on a window that was fetched.
	for _, field := range []string{
		"transaction_id", "status", "status_date", "amount", "commission",
		"transaction_date", "subid1", "program",
	} {
		if !strings.Contains(fields, field) {
			t.Errorf("fields does not ask for %q, which the adapter reads", field)
		}
	}
	// And the day-first window travels with it.
	if got := query.Get("from"); got != "03/06/2024" {
		t.Errorf("from = %q, want 03/06/2024", got)
	}
	if got := query.Get("to"); got != "09/06/2024" {
		t.Errorf("to = %q, want 09/06/2024", got)
	}
}

// TestAnEmptyWindowIsNotAnError. The poller asks for periods that hold
// nothing most of the time, and "no rows" has to be distinguishable from
// "something went wrong" - here it is simply [].
func TestAnEmptyWindowIsNotAnError(t *testing.T) {
	t.Parallel()

	client := servingRecording(t, "transactions-empty.json")
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	got, failed := collect(t, seq)
	if failed != nil {
		t.Fatalf("an empty window ended with %v", failed)
	}
	if len(got) != 0 {
		t.Errorf("an empty window yielded %d transactions", len(got))
	}
}

// TestTheForcedObjectEnvelopeIsRead. This adapter never sends
// rest_json_force_object, so the object form should not arrive - reading it
// costs a second unmarshal and buys immunity to a deployment-wide default
// nobody here set.
func TestTheForcedObjectEnvelopeIsRead(t *testing.T) {
	t.Parallel()

	client := servingRecording(t, "transactions-object-envelope.json")
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	got, failed := collect(t, seq)
	if failed != nil {
		t.Fatalf("the iteration ended with %v", failed)
	}
	if len(got) != 1 || got[0].ExternalID != "420343717" {
		t.Fatalf("the object envelope yielded %d transactions, want the same 1 the array form does", len(got))
	}
}

// TestANetworkFailureIsYieldedRatherThanReturned. The immediate error covers
// only what is checkable without contacting the network; everything else
// travels through the sequence, so an eager adapter and a lazy one report an
// expired credential through the same channel.
func TestANetworkFailureIsYieldedRatherThanReturned(t *testing.T) {
	t.Parallel()

	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions() refused before contacting the network: %v", err)
	}
	got, failed := collect(t, seq)
	if !errors.Is(failed, networks.ErrNetworkRefused) {
		t.Fatalf("the iteration ended with %v, want ErrNetworkRefused", failed)
	}
	if len(got) != 0 {
		t.Errorf("%d transactions were yielded before the refusal", len(got))
	}
}

// TestAnErrorArrivingWithA200IsReportedAsOne. Without this the error object
// would fail to unmarshal into a slice and be reported as "cannot unmarshal
// object into []linkwise.reportRow" - a true sentence naming neither the
// network nor what it said.
func TestAnErrorArrivingWithA200IsReportedAsOne(t *testing.T) {
	t.Parallel()

	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"code":400,"name":"Bad Request","description":"The request parameters are invalid or missing.\n\nUsage:\n..."}}`))
	})
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	_, failed := collect(t, seq)
	if failed == nil {
		t.Fatal("an error body arriving with a 200 was read as an empty report")
	}
	if !strings.Contains(failed.Error(), "Bad Request") {
		t.Errorf("the failure does not say what Linkwise said: %v", failed)
	}
}

// TestAnAnswerThatIsNotAReportIsReported.
func TestAnAnswerThatIsNotAReportIsReported(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ name, body string }{
		{name: "empty", body: ""},
		{name: "html", body: "<html>Usage: ...</html>"},
		{name: "a row that is not an object", body: `["not a transaction"]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			})
			seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
			if err != nil {
				t.Fatalf("FetchTransactions(): %v", err)
			}
			if _, failed := collect(t, seq); !errors.Is(failed, networks.ErrNetworkUnavailable) {
				t.Fatalf("the iteration ended with %v, want ErrNetworkUnavailable", failed)
			}
		})
	}
}

// TestAStatusNobodyMappedFailsTheWindow is contract rule 2 reaching the
// caller. A network that invents a state is telling us something about money
// we are about to promise somebody, and the two available guesses - withhold
// it or pay it - are each wrong in a way nobody would notice.
func TestAStatusNobodyMappedFailsTheWindow(t *testing.T) {
	t.Parallel()

	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"amount":"1.00","commission":"0.10",` +
			`"date":"2024-06-07T19:10:54+03:00","status":{"name":"Escalated","date":"2024-06-08T00:00:00+03:00"}}]`))
	})
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	_, failed := collect(t, seq)
	if !errors.Is(failed, networks.ErrUnmappableStatus) {
		t.Fatalf("the iteration ended with %v, want ErrUnmappableStatus", failed)
	}
	if !strings.Contains(failed.Error(), "Escalated") {
		t.Errorf("the failure does not name the word nobody mapped: %v", failed)
	}
}

// TestAnUnreadableAmountFailsTheWindow. A money field that will not parse is
// not a row to skip: skipping it would leave a member uncredited with nothing
// logged, which is the failure mode this whole port is arranged against.
func TestAnUnreadableAmountFailsTheWindow(t *testing.T) {
	t.Parallel()

	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"amount":"1,00","commission":"0.10",` +
			`"date":"2024-06-07T19:10:54+03:00","status":{"name":"Validated","date":"2024-06-08T00:00:00+03:00"}}]`))
	})
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	if _, failed := collect(t, seq); !errors.Is(failed, linkwise.ErrNotAnAmount) {
		t.Fatalf("the iteration ended with %v, want ErrNotAnAmount", failed)
	}
}

// TestABlankClickReferenceIsRefusedRatherThanCarried. Absent and blank are
// different things: absent is ordinary, and blank is an adapter bug that
// would otherwise look exactly like it.
func TestABlankClickReferenceIsRefusedRatherThanCarried(t *testing.T) {
	t.Parallel()

	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"subid1":"   ","amount":"1.00","commission":"0.10",` +
			`"date":"2024-06-07T19:10:54+03:00","status":{"name":"Validated","date":"2024-06-08T00:00:00+03:00"}}]`))
	})
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	if _, failed := collect(t, seq); !errors.Is(failed, networks.ErrBlankClickRef) {
		t.Fatalf("the iteration ended with %v, want ErrBlankClickRef", failed)
	}
}

// TestASubIDThatIsPresentIsCarried is the other side of that: a real
// reference survives byte for byte, which is the whole attribution path.
func TestASubIDThatIsPresentIsCarried(t *testing.T) {
	t.Parallel()

	const ref = "0mB7hQ2xKp4vT9sLcNfR1w"
	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"subid1":"` + ref + `","amount":"1.00","commission":"0.10",` +
			`"date":"2024-06-07T19:10:54+03:00","status":{"name":"Validated","date":"2024-06-08T00:00:00+03:00"}}]`))
	})
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	got, failed := collect(t, seq)
	if failed != nil || len(got) != 1 {
		t.Fatalf("the iteration yielded %d transactions and ended with %v", len(got), failed)
	}
	carried, present := got[0].ClickRef.Ref()
	if !present {
		t.Fatal("a reported sub-id arrived as an absence")
	}
	if carried != ref {
		t.Errorf("ClickRef = %q, want %q byte for byte", carried, ref)
	}
}

// TestACancelledReadSaysSo is contract rule 8. A range loop that ends having
// yielded no error is the caller's only evidence that a window was read to
// the end, and that evidence is what a durable cursor advances on.
func TestACancelledReadSaysSo(t *testing.T) {
	t.Parallel()

	client := servingRecording(t, "transactions.json")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	seq, err := client.FetchTransactions(ctx, theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	_, failed := collect(t, seq)
	if !errors.Is(failed, networks.ErrIterationAbandoned) {
		t.Fatalf("a cancelled read ended with %v, want one wrapping ErrIterationAbandoned", failed)
	}
	if !errors.Is(failed, context.Canceled) {
		t.Errorf("a cancelled read ended with %v, want one wrapping context.Canceled too; an operator reading a log has to know what stopped it", failed)
	}
}

// TestACallersOwnBreakEndsSilently. Only the adapter stopping for its own
// reason is an abandonment; a caller that stops already knows it stopped.
func TestACallersOwnBreakEndsSilently(t *testing.T) {
	t.Parallel()

	client := servingRecording(t, "transactions.json")
	seq, err := client.FetchTransactions(t.Context(), theJuneWindow())
	if err != nil {
		t.Fatalf("FetchTransactions(): %v", err)
	}
	for _, err := range seq {
		if err != nil {
			t.Fatalf("the iteration yielded %v before the caller stopped", err)
		}
		break
	}
}

// TestTheWindowIsAskedAgainFromTheBeginning is contract rule 4. The port
// offers no resumption point inside a window, because a cursor that could sit
// mid-window would advance over transactions that were yielded but not yet
// stored.
func TestTheWindowIsAskedAgainFromTheBeginning(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("testdata", "transactions.json"))
	if err != nil {
		t.Fatalf("reading the recording: %v", err)
	}
	var queries []url.Values
	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		_, _ = w.Write(body)
	})

	window := theJuneWindow()
	for range 2 {
		seq, err := client.FetchTransactions(t.Context(), window)
		if err != nil {
			t.Fatalf("FetchTransactions(): %v", err)
		}
		if _, failed := collect(t, seq); failed != nil {
			t.Fatalf("the iteration ended with %v", failed)
		}
	}
	if len(queries) != 2 {
		t.Fatalf("%d requests were made for two reads of one window", len(queries))
	}
	if queries[0].Encode() != queries[1].Encode() {
		t.Errorf("re-reading the window asked a different question:\n  %s\n  %s", queries[0].Encode(), queries[1].Encode())
	}
}

// TestTheAdapterSatisfiesThePort. ValidateNetwork is what the composition
// root calls on each adapter it wires, and it is the cheapest place for a
// mismatched network id or a misconfigured account to surface.
func TestTheAdapterSatisfiesThePort(t *testing.T) {
	t.Parallel()

	account, err := networks.NewPublisherAccount(uuid.New(), linkwise.ID, "CD20")
	if err != nil {
		t.Fatalf("NewPublisherAccount(): %v", err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)

	client, err := linkwise.New(account,
		linkwise.WithCredential(theUsername, thePassword),
		linkwise.WithReportCurrency(theCurrency),
		linkwise.WithBaseURL(server.URL),
		linkwise.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if got := client.ReportCurrency(); got != money.Currency(theCurrency) {
		t.Errorf("ReportCurrency() = %q, want %q", got, theCurrency)
	}
	// The adapter is not yet a whole networks.Network - BuildDeeplink and
	// FetchCatalogue are still to come - so what is asserted here is the part
	// that exists: the four values ValidateNetwork judges.
	if err := client.Account().Validate(); err != nil {
		t.Errorf("the account this adapter polls is unusable: %v", err)
	}
	if client.Account().Network() != client.ID() {
		t.Errorf("the adapter is %q and polls an account at %q", client.ID(), client.Account().Network())
	}
	if err := client.Limits().Validate(); err != nil {
		t.Errorf("the adapter's limits describe no usable network: %v", err)
	}
}
