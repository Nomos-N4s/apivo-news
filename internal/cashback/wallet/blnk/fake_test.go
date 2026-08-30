package blnk_test

// A stand-in Blnk server, good enough to hold this adapter to the wire it
// actually speaks.
//
// Docker is unavailable on the founder's machine, so the real ledger cannot
// run here at all and the BLNK_URL suite in integration_test.go skips. That
// would leave the adapter untested everywhere except CI, which is too late
// to learn that a request was built wrong. So this file implements the
// endpoints the adapter calls, decodes each request into the shape the SDK
// is documented to send, and lets a test assert on what arrived.
//
// It is deliberately strict. A fake that accepts anything proves nothing: a
// request naming a balance it does not hold is a 404 here, a duplicate
// reference is a 409, and a source it would leave negative without
// permission is a refusal - because those are the three refusals the port's
// error mapping is built on, and a fake that waved them through would let a
// mis-mapped error pass as correct.
//
// What it is NOT is evidence about Blnk. It encodes this repository's
// reading of the SDK and of spike S2; only the integration suite, against a
// real ledger in the cashback CI job, can confirm that reading.

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

// wireLeg is one split leg as the SDK sends it.
type wireLeg struct {
	Identifier          string `json:"identifier"`
	Distribution        string `json:"distribution,omitempty"`
	PreciseDistribution string `json:"precise_distribution,omitempty"`
	Narration           string `json:"narration,omitempty"`
}

// wireTransaction is a create-a-transaction request, decoded. Every field
// the adapter is expected to set is here, and so are the two it must never
// set to a money value: Amount, which is a float, and the legs'
// Distribution, which is a percentage or a decimal number.
type wireTransaction struct {
	Amount         float64        `json:"amount"`
	Reference      string         `json:"reference"`
	Precision      int64          `json:"precision"`
	Description    string         `json:"description"`
	Currency       string         `json:"currency"`
	Sources        []wireLeg      `json:"sources"`
	Destinations   []wireLeg      `json:"destinations"`
	Source         string         `json:"source"`
	Destination    string         `json:"destination"`
	PreciseAmount  *json.Number   `json:"precise_amount"`
	SkipQueue      bool           `json:"skip_queue"`
	Atomic         bool           `json:"atomic"`
	Inflight       bool           `json:"inflight"`
	AllowOverdraft bool           `json:"allow_overdraft"`
	MetaData       map[string]any `json:"meta_data"`
}

// wireCreateBalance is a create-a-balance request, decoded.
type wireCreateBalance struct {
	LedgerID  string `json:"ledger_id"`
	Currency  string `json:"currency"`
	Indicator string `json:"indicator"`
}

// wireRefusal is a refusal written back verbatim: a status and the exact
// body that carries it. The body is raw rather than built here because the
// point of most of these is a body no ledger would send - an authenticating
// proxy's, a gateway's - reaching this package's error mapping.
type wireRefusal struct {
	status int
	body   string
}

// balanceRow is one account inside the fake.
type balanceRow struct {
	id        string
	ledgerID  string
	currency  string
	indicator string
	minor     *big.Int
}

// txnRow is one recorded transaction inside the fake.
type txnRow struct {
	id        string
	parent    string
	reference string
	currency  string
	precise   *big.Int
	source    string
	dest      string
	sources   []wireLeg
	dests     []wireLeg
	status    string
	createdAt time.Time
	metadata  map[string]any
}

// createRecord is one create-a-transaction request as it arrived: the raw
// bytes, so a test can assert on the JSON itself, and the decoded shape.
type createRecord struct {
	body []byte
	txn  wireTransaction
}

// fakeBlnk is the server. Every knob on it exists because some refusal or
// timing the adapter has to survive cannot be produced any other way.
type fakeBlnk struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	// ledgers, balances and transactions, keyed the way the endpoints look
	// them up.
	ledgers      map[string]string
	balances     map[string]*balanceRow
	byIndicator  map[string]string
	transactions map[string]*txnRow
	byReference  map[string]string
	seq          int

	// creates records every create-a-transaction request, in order.
	creates []createRecord
	// balanceCreates records every create-a-balance request, in order.
	balanceCreates []wireCreateBalance
	// requests counts calls per method-and-path, so a test can prove a
	// second EnsureAccount cost no round trip.
	requests map[string]int

	// dropIndicator makes created balances come back nameless, which is
	// the shape of a server that does not support the field the adapter's
	// whole account identity rests on.
	dropIndicator bool
	// createdStatus is the status a create answers with. Blnk applies a
	// skip-queue transaction inline; a server that queued it anyway is
	// what the settle wait exists for.
	createdStatus string
	// settleAfter is how many reference reads must happen before a queued
	// transaction reports itself applied. Zero means it never does.
	settleAfter int
	settleReads int
	// balanceOverride replaces what a balance read answers with, verbatim,
	// so a figure larger than an int64 can be put on the wire.
	balanceOverride map[string]string
	// beforeCreate runs inside the create handler, before anything is
	// recorded, so a test can make two posts of one key genuinely race.
	beforeCreate func()
	// refusal, when set, is answered to every create-a-transaction request
	// verbatim, so a test can put any refusal a server or anything between
	// it and this process might send on the wire.
	refusal *wireRefusal
	// balanceCreateLosesRace makes the first create of each account record
	// the balance and then answer as though another process had got there
	// first. It is how the cold-start race between two replicas is played
	// out without two replicas.
	balanceCreateLosesRace bool
	// balanceCreateDuplicates makes a create record a SECOND balance under
	// the same name and leave the lookup pointing at it, which is what a
	// server that does not enforce the uniqueness this package's account
	// identity assumes would do.
	balanceCreateDuplicates bool
	// pageCap caps how many rows a filtered page answers with, whatever
	// limit was asked for. A server free to cap its own pages is ordinary,
	// and a reader that treats a short page as the last one loses
	// everything past it. Zero honours the limit.
	pageCap int
	// freesRejectedReference chooses what the duplicate-reference check
	// does with a reference held only by a transaction the ledger refused.
	// Blnk has no unique index on the column and refuses duplicates in
	// application code (spike S2), so which of the two it does is not
	// settled here; the strict answer is the default, because a fake more
	// forgiving than the server is how a defect reaches CI instead of the
	// desk.
	freesRejectedReference bool
	// filterShape chooses how a filtered page is written back. Servers
	// answer a list in more than one shape and an empty page in more than
	// one way again; a history with nothing in it is an ordinary answer,
	// so the adapter has to read all of them.
	filterShape string
}

// newFakeBlnk starts a fake ledger and returns it with the server torn down
// when the test ends.
func newFakeBlnk(t *testing.T) *fakeBlnk {
	t.Helper()
	f := &fakeBlnk{
		t:               t,
		ledgers:         make(map[string]string),
		balances:        make(map[string]*balanceRow),
		byIndicator:     make(map[string]string),
		transactions:    make(map[string]*txnRow),
		byReference:     make(map[string]string),
		requests:        make(map[string]int),
		createdStatus:   statusApplied,
		balanceOverride: make(map[string]string),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ledgers", f.listLedgers)
	mux.HandleFunc("POST /ledgers", f.createLedger)
	mux.HandleFunc("POST /balances", f.createBalance)
	mux.HandleFunc("GET /balances/indicator/{indicator}/currency/{currency}", f.getBalanceByIndicator)
	mux.HandleFunc("GET /balances/{id}", f.getBalance)
	mux.HandleFunc("POST /transactions", f.createTransaction)
	mux.HandleFunc("GET /transactions/reference/{reference}", f.getTransactionByReference)
	mux.HandleFunc("GET /transactions/{id}", f.getTransaction)
	mux.HandleFunc("POST /transactions/filter", f.filterTransactions)
	mux.HandleFunc("/", f.unknown)

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// Transaction statuses, spelled here so the fake and the assertions cannot
// drift apart.
const (
	statusApplied  = "APPLIED"
	statusQueued   = "QUEUED"
	statusRejected = "REJECTED"
)

// URL is the endpoint to point a ledger at.
func (f *fakeBlnk) URL() string { return f.server.URL }

// unknown fails the test: an endpoint this fake does not implement is an
// assumption the adapter is making that nothing has been written down.
func (f *fakeBlnk) unknown(w http.ResponseWriter, r *http.Request) {
	f.t.Errorf("the adapter called an endpoint this fake does not implement: %s %s", r.Method, r.URL.Path)
	http.Error(w, `{"error":"not implemented"}`, http.StatusNotImplemented)
}

// count records one call, so a test can assert on how many round trips an
// operation cost.
func (f *fakeBlnk) count(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests[r.Method+" "+r.Pattern]++
}

// calls answers how many times a route was called.
func (f *fakeBlnk) calls(pattern string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[pattern]
}

// createdRequests returns every create-a-transaction request that arrived.
func (f *fakeBlnk) createdRequests() []createRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]createRecord(nil), f.creates...)
}

// onlyCreate returns the single create-a-transaction request that arrived,
// failing the test when there was not exactly one.
func (f *fakeBlnk) onlyCreate() createRecord {
	f.t.Helper()
	got := f.createdRequests()
	if len(got) != 1 {
		f.t.Fatalf("the ledger received %d create-a-transaction requests, want exactly 1", len(got))
	}
	return got[0]
}

// indicatorOf answers with the name a balance was created under.
func (f *fakeBlnk) indicatorOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.balances[id]; ok {
		return row.indicator
	}
	return ""
}

// balanceOf answers with what a balance holds.
func (f *fakeBlnk) balanceOf(id string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.balances[id]
	if !ok {
		f.t.Fatalf("no balance %q", id)
	}
	return row.minor.Int64()
}

// seed puts money on a balance without going through a transaction, so a
// test that is about spending does not have to be about funding first.
func (f *fakeBlnk) seed(id string, minor int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.balances[id]
	if !ok {
		f.t.Fatalf("no balance %q", id)
	}
	row.minor = big.NewInt(minor)
}

func (f *fakeBlnk) nextID(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s_%d", prefix, f.seq)
}

func (f *fakeBlnk) listLedgers(w http.ResponseWriter, r *http.Request) {
	f.count(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, 0, len(f.ledgers))
	for id, name := range f.ledgers {
		out = append(out, map[string]any{"ledger_id": id, "name": name, "created_at": time.Unix(0, 0).UTC()})
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *fakeBlnk) createLedger(w http.ResponseWriter, r *http.Request) {
	f.count(r)
	var body struct {
		Name string `json:"name"`
	}
	if !decode(f.t, w, r, &body) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID("ldg")
	f.ledgers[id] = body.Name
	writeJSON(w, http.StatusCreated, map[string]any{"ledger_id": id, "name": body.Name, "created_at": time.Unix(0, 0).UTC()})
}

func (f *fakeBlnk) createBalance(w http.ResponseWriter, r *http.Request) {
	f.count(r)
	var body wireCreateBalance
	if !decode(f.t, w, r, &body) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balanceCreates = append(f.balanceCreates, body)

	key := body.Indicator + "/" + body.Currency
	if _, taken := f.byIndicator[key]; taken && body.Indicator != "" {
		// Two balances under one name would be one account's money in two
		// places, so this server refuses rather than obliges. Whether the
		// real one does is unproven, which is why the adapter converges on
		// the lookup either way.
		writeError(w, http.StatusConflict, "BAL_DUPLICATE_INDICATOR", "indicator already exists")
		return
	}

	row := &balanceRow{
		id:        f.nextID("bln"),
		ledgerID:  body.LedgerID,
		currency:  body.Currency,
		indicator: body.Indicator,
		minor:     big.NewInt(0),
	}
	if f.dropIndicator {
		row.indicator = ""
	}
	f.balances[row.id] = row
	if row.indicator != "" {
		f.byIndicator[key] = row.id
	}
	if f.balanceCreateDuplicates {
		shadow := &balanceRow{
			id:        f.nextID("bln"),
			ledgerID:  row.ledgerID,
			currency:  row.currency,
			indicator: row.indicator,
			minor:     big.NewInt(0),
		}
		f.balances[shadow.id] = shadow
		f.byIndicator[key] = shadow.id
	}
	if f.balanceCreateLosesRace {
		f.balanceCreateLosesRace = false
		writeError(w, http.StatusConflict, "BAL_DUPLICATE_INDICATOR", "indicator already exists")
		return
	}
	writeJSON(w, http.StatusCreated, f.renderBalance(row))
}

func (f *fakeBlnk) getBalanceByIndicator(w http.ResponseWriter, r *http.Request) {
	f.count(r)
	key := r.PathValue("indicator") + "/" + r.PathValue("currency")
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byIndicator[key]
	if !ok {
		writeError(w, http.StatusNotFound, "BAL_NOT_FOUND", "balance not found")
		return
	}
	writeJSON(w, http.StatusOK, f.renderBalance(f.balances[id]))
}

func (f *fakeBlnk) getBalance(w http.ResponseWriter, r *http.Request) {
	f.count(r)
	id := r.PathValue("id")
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.balances[id]
	if !ok {
		writeError(w, http.StatusNotFound, "BAL_NOT_FOUND", "balance not found")
		return
	}
	if override, ok := f.balanceOverride[id]; ok {
		// Written as raw JSON so a figure no int64 can hold reaches the
		// adapter exactly as a real ledger would send it.
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"balance_id":%q,"balance":%s,"currency":%q,"ledger_id":%q,"indicator":%q,"precision":100}`,
			row.id, override, row.currency, row.ledgerID, row.indicator)
		return
	}
	writeJSON(w, http.StatusOK, f.renderBalance(row))
}

// renderBalance is called with the lock held.
func (f *fakeBlnk) renderBalance(row *balanceRow) map[string]any {
	return map[string]any{
		"balance_id": row.id,
		"balance":    row.minor,
		"currency":   row.currency,
		"ledger_id":  row.ledgerID,
		"indicator":  row.indicator,
		"precision":  100,
		"created_at": time.Unix(0, 0).UTC(),
	}
}

func (f *fakeBlnk) createTransaction(w http.ResponseWriter, r *http.Request) {
	f.count(r)
	raw, body, ok := decodeRaw[wireTransaction](f.t, w, r)
	if !ok {
		return
	}
	if f.beforeCreate != nil {
		f.beforeCreate()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, createRecord{body: raw, txn: body})

	if f.refusal != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.refusal.status)
		_, _ = fmt.Fprint(w, f.refusal.body)
		return
	}

	if held, taken := f.byReference[body.Reference]; taken {
		if !f.freesRejectedReference || f.transactions[held].status != statusRejected {
			writeError(w, http.StatusConflict, "TXN_DUPLICATE_REFERENCE", "reference already exists")
			return
		}
	}

	if f.createdStatus == statusRejected {
		// A ledger that accepts the request and then refuses the movement
		// answers 2xx with a rejected transaction and moves no balance.
		// The row still exists, and it still holds the reference.
		row := &txnRow{
			id:        f.nextID("txn"),
			reference: body.Reference,
			currency:  body.Currency,
			precise:   big.NewInt(0),
			source:    body.Source,
			dest:      body.Destination,
			sources:   body.Sources,
			dests:     body.Destinations,
			status:    statusRejected,
			createdAt: time.Now().UTC(),
			metadata:  body.MetaData,
		}
		f.transactions[row.id] = row
		f.byReference[row.reference] = row.id
		writeJSON(w, http.StatusCreated, f.renderTransaction(row))
		return
	}

	moves, err := movementsOf(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "TXN_INVALID", err.Error())
		return
	}
	for id := range moves {
		if _, known := f.balances[id]; !known {
			writeError(w, http.StatusNotFound, "BAL_NOT_FOUND", "balance "+id+" not found")
			return
		}
	}
	if !body.AllowOverdraft {
		for id, delta := range moves {
			after := new(big.Int).Add(f.balances[id].minor, delta)
			if after.Sign() < 0 {
				writeError(w, http.StatusBadRequest, "TXN_INSUFFICIENT_FUNDS",
					"insufficient funds in balance "+id)
				return
			}
		}
	}
	for id, delta := range moves {
		f.balances[id].minor = new(big.Int).Add(f.balances[id].minor, delta)
	}

	precise := new(big.Int)
	if body.PreciseAmount != nil {
		precise.SetString(body.PreciseAmount.String(), 10)
	}
	row := &txnRow{
		id:        f.nextID("txn"),
		reference: body.Reference,
		currency:  body.Currency,
		precise:   precise,
		source:    body.Source,
		dest:      body.Destination,
		sources:   body.Sources,
		dests:     body.Destinations,
		status:    f.createdStatus,
		createdAt: time.Now().UTC(),
		metadata:  body.MetaData,
	}
	f.transactions[row.id] = row
	f.byReference[row.reference] = row.id
	f.splitIntoChildren(row)
	writeJSON(w, http.StatusCreated, f.renderTransaction(row))
}

// splitIntoChildren records the child transaction the ledger keeps for each
// leg of a split: a row of its own, naming one account at one end and
// pointing back at the transfer it belongs to.
//
// This is the model the adapter is written against, and modelling it here
// is the whole point of the fake. The filter below matches on the scalar
// source and destination columns and on nothing else, so a leg account is
// found through its child and never through the parent, while the account
// that travelled as the scalar end is found through BOTH - which is exactly
// the shape that double-counts a movement if a reader takes each row it is
// handed as a posting of its own.
//
// Children carry neither the reference nor the annotation: the transfer's
// identity lives on the parent, and a fake that copied it down would let a
// reader that never looks at the parent pass.
func (f *fakeBlnk) splitIntoChildren(parent *txnRow) {
	for _, side := range []struct {
		legs     []wireLeg
		asSource bool
	}{{parent.sources, true}, {parent.dests, false}} {
		for _, leg := range side.legs {
			amount, ok := new(big.Int).SetString(leg.PreciseDistribution, 10)
			if !ok {
				continue
			}
			child := &txnRow{
				id:        f.nextID("txn"),
				parent:    parent.id,
				currency:  parent.currency,
				precise:   amount,
				status:    parent.status,
				createdAt: parent.createdAt,
			}
			if side.asSource {
				child.source, child.dest = leg.Identifier, parent.dest
			} else {
				child.source, child.dest = parent.source, leg.Identifier
			}
			f.transactions[child.id] = child
		}
	}
}

func (f *fakeBlnk) getTransaction(w http.ResponseWriter, r *http.Request) {
	f.count(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.transactions[r.PathValue("id")]
	if !ok {
		writeError(w, http.StatusNotFound, "TXN_NOT_FOUND", "transaction not found")
		return
	}
	writeJSON(w, http.StatusOK, f.renderTransaction(row))
}

func (f *fakeBlnk) getTransactionByReference(w http.ResponseWriter, r *http.Request) {
	f.count(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byReference[r.PathValue("reference")]
	if !ok {
		writeError(w, http.StatusNotFound, "TXN_NOT_FOUND", "transaction not found")
		return
	}
	row := f.transactions[id]
	if row.status == statusQueued {
		f.settleReads++
		if f.settleAfter > 0 && f.settleReads >= f.settleAfter {
			row.status = statusApplied
		}
	}
	writeJSON(w, http.StatusOK, f.renderTransaction(row))
}

func (f *fakeBlnk) filterTransactions(w http.ResponseWriter, r *http.Request) {
	f.count(r)
	var body struct {
		Filters []struct {
			Field    string `json:"field"`
			Operator string `json:"operator"`
			Value    any    `json:"value"`
		} `json:"filters"`
		Limit     int    `json:"limit"`
		Offset    int    `json:"offset"`
		SortBy    string `json:"sort_by"`
		SortOrder string `json:"sort_order"`
	}
	if !decode(f.t, w, r, &body) {
		return
	}
	if len(body.Filters) != 1 {
		f.t.Errorf("the adapter filtered on %d field(s), want exactly 1", len(body.Filters))
	}
	// Offset paging is only safe over a stable order, so the sort the
	// adapter asks for is checked rather than assumed: a field name this
	// server does not know would be ignored by a real one, and pages over
	// an unstable order drop rows between them.
	if body.SortBy != "created_at" {
		f.t.Errorf("the adapter sorted a page by %q, which this ledger does not order on", body.SortBy)
	}
	if body.SortOrder != "asc" && body.SortOrder != "desc" {
		f.t.Errorf("the adapter asked for the sort order %q, which this ledger does not know", body.SortOrder)
	}
	field, value := body.Filters[0].Field, fmt.Sprint(body.Filters[0].Value)

	f.mu.Lock()
	defer f.mu.Unlock()
	matched := make([]map[string]any, 0, len(f.transactions))
	for _, row := range sortedRows(f.transactions) {
		if matchesField(row, field, value) {
			matched = append(matched, f.renderTransaction(row))
		}
	}
	if body.Offset >= len(matched) {
		matched = nil
	} else {
		matched = matched[body.Offset:]
	}
	if body.Limit > 0 && len(matched) > body.Limit {
		matched = matched[:body.Limit]
	}
	if f.pageCap > 0 && len(matched) > f.pageCap {
		matched = matched[:f.pageCap]
	}
	switch {
	case f.filterShape == shapeBareArray:
		writeJSON(w, http.StatusOK, matched)
	case f.filterShape == shapeUnknownEnvelope:
		writeJSON(w, http.StatusOK, map[string]any{"transactions": matched, "total_count": len(matched)})
	case f.filterShape == shapeNullPage && len(matched) == 0:
		writeJSON(w, http.StatusOK, map[string]any{"data": nil})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"data": matched})
	}
}

// The shapes a filtered page can arrive in.
const (
	// shapeEnvelope wraps the rows in an object under data, and writes an
	// empty page as an empty array.
	shapeEnvelope = ""
	// shapeBareArray answers with the rows and nothing around them.
	shapeBareArray = "array"
	// shapeNullPage wraps the rows, but writes an empty page as a null.
	shapeNullPage = "null"
	// shapeUnknownEnvelope wraps the rows under a key this package does
	// not read, which is what a server the adapter has guessed wrong about
	// answers with. It must be an error and never an empty history.
	shapeUnknownEnvelope = "unknown"
)

// matchesField is the fake's whole filter language: equality on the two
// columns the adapter asks about.
func matchesField(row *txnRow, field, value string) bool {
	switch field {
	case "source":
		return row.source == value
	case "destination":
		return row.dest == value
	default:
		return false
	}
}

// sortedRows returns the fake's transactions in the order they were
// recorded, ties broken by id. The tie-break is not tidiness: offset paging
// over an unstable order drops rows between pages, so a server that offered
// one would be a server no reader could page through - and a fake that
// offered one would make its own paging test flake instead of failing.
func sortedRows(rows map[string]*txnRow) []*txnRow {
	out := make([]*txnRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b *txnRow) int {
		if c := a.createdAt.Compare(b.createdAt); c != 0 {
			return c
		}
		return cmp.Compare(a.id, b.id)
	})
	return out
}

// renderTransaction is called with the lock held.
//
// The float amount it sends back is deliberate nonsense. Blnk answers with
// both an amount and a precise_amount; this adapter must read only the
// integer, and a fake that echoed a consistent float would let a reader of
// the wrong field pass every test here.
func (f *fakeBlnk) renderTransaction(row *txnRow) map[string]any {
	out := map[string]any{
		"transaction_id": row.id,
		"reference":      row.reference,
		"currency":       row.currency,
		"precise_amount": row.precise,
		"amount":         999999.99,
		"precision":      100,
		"status":         row.status,
		"created_at":     row.createdAt,
		"meta_data":      row.metadata,
	}
	if row.parent != "" {
		out["parent_transaction"] = row.parent
	}
	if row.source != "" {
		out["source"] = row.source
	}
	if row.dest != "" {
		out["destination"] = row.dest
	}
	if len(row.sources) > 0 {
		out["sources"] = row.sources
	}
	if len(row.dests) > 0 {
		out["destinations"] = row.dests
	}
	return out
}

// record adds a transaction the adapter did not create, so History can be
// shown what it does with a shape only a real ledger produces - a split
// child pointing at its parent, or a leg carrying a decimal distribution.
func (f *fakeBlnk) record(row *txnRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row.id == "" {
		row.id = f.nextID("txn")
	}
	if row.status == "" {
		row.status = statusApplied
	}
	if row.precise == nil {
		row.precise = big.NewInt(0)
	}
	f.transactions[row.id] = row
	if row.reference != "" {
		f.byReference[row.reference] = row.id
	}
}

// movementsOf works out what a create request would do to each balance,
// which is what lets the fake refuse an overdraft it was not given
// permission for.
func movementsOf(body wireTransaction) (map[string]*big.Int, error) {
	total := new(big.Int)
	if body.PreciseAmount != nil {
		if _, ok := total.SetString(body.PreciseAmount.String(), 10); !ok {
			return nil, fmt.Errorf("precise_amount %q is not an integer", body.PreciseAmount.String())
		}
	}
	moves := make(map[string]*big.Int)
	add := func(id string, delta *big.Int) {
		if existing, ok := moves[id]; ok {
			moves[id] = new(big.Int).Add(existing, delta)
			return
		}
		moves[id] = delta
	}

	if body.Source != "" {
		add(body.Source, new(big.Int).Neg(total))
	}
	if body.Destination != "" {
		add(body.Destination, new(big.Int).Set(total))
	}
	for _, side := range []struct {
		legs     []wireLeg
		negative bool
	}{{body.Sources, true}, {body.Destinations, false}} {
		sum := new(big.Int)
		for _, leg := range side.legs {
			amount, ok := new(big.Int).SetString(leg.PreciseDistribution, 10)
			if !ok {
				return nil, fmt.Errorf("leg %q carries precise_distribution %q, which is not an integer", leg.Identifier, leg.PreciseDistribution)
			}
			sum.Add(sum, amount)
			if side.negative {
				amount = new(big.Int).Neg(amount)
			}
			add(leg.Identifier, amount)
		}
		if len(side.legs) > 0 && sum.Cmp(total) != 0 {
			return nil, fmt.Errorf("the legs sum to %s but the transaction is for %s", sum, total)
		}
	}
	return moves, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError answers in the structured shape Blnk uses, error_detail and
// all, because that is the shape the adapter's error mapping reads.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error":        message,
		"error_detail": map[string]string{"code": code, "message": message},
	})
}

func decode[T any](t *testing.T, w http.ResponseWriter, r *http.Request, into *T) bool {
	t.Helper()
	_, body, ok := decodeRaw[T](t, w, r)
	if ok {
		*into = body
	}
	return ok
}

// decodeRaw keeps the bytes as well as the decoded value: some assertions
// are about the JSON itself - that an amount is the integer 0 and not a
// float carrying money - and a decoded float64 cannot answer them.
func decodeRaw[T any](t *testing.T, w http.ResponseWriter, r *http.Request) (raw []byte, body T, ok bool) {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("reading the request body: %v", err)
		writeError(w, http.StatusBadRequest, "GEN_BAD_REQUEST", "unreadable body")
		return nil, body, false
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Errorf("the adapter sent a body this fake cannot decode: %v\n%s", err, raw)
		writeError(w, http.StatusBadRequest, "GEN_BAD_REQUEST", "undecodable body")
		return nil, body, false
	}
	return raw, body, true
}
