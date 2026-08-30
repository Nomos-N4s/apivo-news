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
// It models what the server SOURCE does, not what would be convenient here.
// That distinction is the whole reason this file has its present shape: an
// earlier version modelled balances as things a client creates by name,
// which is what the adapter wished were true, and every test here passed
// while the first run against a real ledger failed sixteen times over. So
// the four facts that version had wrong are modelled deliberately:
//
//   - a balance cannot be created by name. The create-a-balance endpoint
//     refuses outright here, because the real one silently drops the
//     indicator and hands back a balance nothing can find again.
//   - a balance is created by naming it as an "@" source or destination in
//     a transaction, and the name it is stored under keeps the "@".
//   - a recorded transaction stores the balance ids the names resolved to,
//     never the names.
//   - a split records ONLY children: one per leg, each carrying a copy of
//     the whole transfer's annotation and a reference with an ordinal
//     appended. The parent is answered in the response and never stored.
//
// It is deliberately strict beyond that. A fake that accepts anything
// proves nothing: a transaction with no description is a 400 here, a
// duplicate reference is a 409, and a source it would leave negative
// without permission is a refusal - because those are refusals the port's
// error mapping is built on, and a fake that waved them through would let a
// mis-mapped error pass as correct.
//
// What it is NOT is evidence about Blnk. It encodes this repository's
// reading of the server source at v0.15.2; only the integration suite,
// against a real ledger in the cashback CI job, can confirm that reading.

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
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

// wireRefusal is a refusal written back verbatim: a status and the exact
// body that carries it. The body is raw rather than built here because the
// point of most of these is a body no ledger would send - an authenticating
// proxy's, a gateway's - reaching this package's error mapping.
type wireRefusal struct {
	status int
	body   string
}

// balanceRow is one account inside the fake. It exists only because some
// transaction named it, which is the only way the real ledger makes one.
type balanceRow struct {
	id        string
	ledgerID  string
	currency  string
	indicator string
	minor     *big.Int
}

// txnRow is one recorded transaction inside the fake. source and dest hold
// balance ids: the server resolves an "@" name before it stores anything,
// and the column is a foreign key into the balances table, so a name can
// never be in one.
type txnRow struct {
	id        string
	parent    string
	reference string
	currency  string
	precise   *big.Int
	source    string
	dest      string
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
	// requests counts calls per method-and-path, so a test can prove an
	// operation cost the round trips it should and no others.
	requests map[string]int

	// createdStatus is the status a create answers with. Blnk applies a
	// skip-queue transaction inline; a server that queued it anyway is
	// what the settle wait exists for.
	createdStatus string
	// settleAfter is how many reference reads must happen before a queued
	// transaction reports itself applied. Zero means it never does.
	settleAfter int
	settleReads int
	// balanceOverride replaces what a balance lookup answers with, keyed by
	// account name and written verbatim, so a figure larger than an int64
	// can be put on the wire.
	balanceOverride map[string]string
	// beforeCreate runs inside the create handler, before anything is
	// recorded, so a test can make two posts of one key genuinely race.
	beforeCreate func()
	// refusal, when set, is answered to every create-a-transaction request
	// verbatim, so a test can put any refusal a server or anything between
	// it and this process might send on the wire.
	refusal *wireRefusal
	// splitJoin is what a split puts between the transfer's reference and
	// the child's ordinal. The server spells it "-" on the synchronous path
	// - the only one this adapter asks for - and "_" on the queued one, and
	// the adapter has to find its own key under either.
	splitJoin string
	// pageCap caps how many rows a filtered page answers with, on top of
	// the ceiling the server already imposes. Zero applies only that.
	pageCap int
	// freesRejectedReference chooses what the duplicate-reference check
	// does with a reference held only by a transaction the ledger refused.
	// Which of the two the server does is not settled here; the strict
	// answer is the default, because a fake more forgiving than the server
	// is how a defect reaches CI instead of the desk.
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
		splitJoin:       splitJoinSynchronous,
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

// The two spellings a split puts between a transfer's reference and a
// child's ordinal, one per path through the server.
const (
	// splitJoinSynchronous is what the skip-queue path writes, and the
	// adapter asks for nothing else - so it is the default here.
	splitJoinSynchronous = "-"
	// splitJoinQueued is what the queued path rewrites it to. The adapter
	// probes for it as well, because a key whose transfer it failed to find
	// would be posted a second time.
	splitJoinQueued = "_"
)

// generalLedgerID is the ledger Blnk creates an "@"-named balance in,
// whatever ledger the client is configured for. It is deliberately NOT one
// of the ledgers this fake hands out, so an adapter that judged a balance
// by which ledger it sat in would fail every read.
const generalLedgerID = "general_ledger_id"

// indicatorPrefix marks a source or destination as a name to resolve rather
// than a balance id, and is kept in the name the balance is stored under.
const indicatorPrefix = "@"

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

// accounts answers how many balances exist. EnsureAccount must leave the
// count where it found it: an account is a name here, and the balance
// behind it appears when a transfer names it.
func (f *fakeBlnk) accounts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.balances)
}

// knows reports whether a balance exists under an account name.
func (f *fakeBlnk) knows(indicator string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.byIndicator[indicatorKey(indicator)]
	return ok
}

// balanceOf answers what an account holds, failing the test when no
// transfer has ever named it.
func (f *fakeBlnk) balanceOf(indicator string) int64 {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byIndicator[indicatorKey(indicator)]
	if !ok {
		f.t.Fatalf("no balance is named %q; nothing has been posted to it", indicator)
	}
	return f.balances[id].minor.Int64()
}

// holds answers what an account holds, treating one no transfer has named
// as holding nothing - which is what it holds. It is the assertion for a
// balance that must not have moved, where balanceOf is the assertion for
// one that must have.
func (f *fakeBlnk) holds(indicator string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byIndicator[indicatorKey(indicator)]
	if !ok {
		return 0
	}
	return f.balances[id].minor.Int64()
}

// seed puts money on an account without going through a transaction,
// opening the balance if nothing has named it yet, so a test that is about
// spending does not have to be about funding first.
func (f *fakeBlnk) seed(indicator string, minor int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open(indicator).minor = big.NewInt(minor)
}

// open returns the balance an account name refers to, creating it exactly
// as the transaction path does: in the General Ledger, under the name with
// its "@" kept. Called with the lock held.
func (f *fakeBlnk) open(indicator string) *balanceRow {
	key := indicatorKey(indicator)
	if id, ok := f.byIndicator[key]; ok {
		return f.balances[id]
	}
	row := &balanceRow{
		id:        f.nextID("bln"),
		ledgerID:  generalLedgerID,
		currency:  currencyOf(indicator),
		indicator: indicator,
		minor:     big.NewInt(0),
	}
	f.balances[row.id] = row
	f.byIndicator[key] = row.id
	return row
}

// indicatorKey is how a balance is looked up: by name and currency, with no
// ledger anywhere in it. Uniqueness is global for that reason, which is
// what the ledger id inside the adapter's names exists to work around.
func indicatorKey(indicator string) string {
	return indicator + "/" + currencyOf(indicator)
}

// currencyOf reads the currency out of an account name, which ends with it.
// The fake needs one wherever it opens a balance, and the name is the only
// place it can come from - as it is for the server, which takes it from the
// transaction that named the account.
func currencyOf(indicator string) string {
	at := strings.LastIndex(indicator, ".")
	if at < 0 {
		return ""
	}
	return indicator[at+1:]
}

func (f *fakeBlnk) nextID(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s_%d", prefix, f.seq)
}

// addLedger records a ledger this package did not create, so a test can
// push the one it cares about off the first page.
func (f *fakeBlnk) addLedger(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID("ldg")
	f.ledgers[id] = name
	return id
}

// ledgersNamed counts the ledgers carrying one name. The real server does
// not constrain the name, so this is how a test asks whether the adapter
// created more namespaces than it meant to.
func (f *fakeBlnk) ledgersNamed(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	found := 0
	for _, candidate := range f.ledgers {
		if candidate == name {
			found++
		}
	}
	return found
}

// listLedgers answers a PAGE of ledgers, because that is what the real
// endpoint does: it reads limit and offset from the query and defaults the
// limit to ten when none is given. A fake that answered every ledger would
// hide the failure this models - an adapter that asks for no page finds
// nothing once the eleventh ledger exists, and creates another namespace
// for accounts that already have one.
//
// The order is by id, so paging is stable and a ledger cannot be seen twice
// or missed entirely as pages are walked.
func (f *fakeBlnk) listLedgers(w http.ResponseWriter, r *http.Request) {
	f.count(r)

	limit, err := pageParam(r, "limit", defaultLedgerPage)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Invalid limit value"})
		return
	}
	offset, err := pageParam(r, "offset", 0)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Invalid offset value"})
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	ids := make([]string, 0, len(f.ledgers))
	for id := range f.ledgers {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	out := make([]map[string]any, 0, limit)
	for i := offset; i < len(ids) && len(out) < limit; i++ {
		out = append(out, map[string]any{
			"ledger_id":  ids[i],
			"name":       f.ledgers[ids[i]],
			"created_at": time.Unix(0, 0).UTC(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// defaultLedgerPage is the page size the real endpoint falls back to when a
// caller names none: api/ledger.go reads c.DefaultQuery("limit", "10").
const defaultLedgerPage = 10

// pageParam reads one paging query parameter, refusing what the real
// endpoint refuses - a value that is not a number, or is below one.
func pageParam(r *http.Request, name string, fallback int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	if name == "limit" && value < 1 {
		return 0, fmt.Errorf("limit below one")
	}
	if value < 0 {
		return 0, fmt.Errorf("offset below zero")
	}
	return value, nil
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

// createBalance fails the test. The real endpoint accepts a body carrying
// no indicator field at all, so it answers with a nameless balance the
// lookup by name will never find again - and an adapter that called it
// would make one of those per call and split an account's money across all
// of them. There is nothing here to model but the refusal.
func (f *fakeBlnk) createBalance(w http.ResponseWriter, r *http.Request) {
	f.count(r)
	f.t.Error("the adapter tried to create a balance; this ledger creates one only when a transaction names it, and a balance created any other way carries no name to find it by")
	writeError(w, http.StatusBadRequest, "BAL_VALIDATION", "balances are not created this way")
}

func (f *fakeBlnk) getBalanceByIndicator(w http.ResponseWriter, r *http.Request) {
	f.count(r)
	indicator := r.PathValue("indicator")
	if !strings.HasPrefix(indicator, indicatorPrefix) {
		f.t.Errorf("the adapter looked up the account name %q, which carries no %q; the name is stored with it", indicator, indicatorPrefix)
	}
	key := indicator + "/" + r.PathValue("currency")

	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byIndicator[key]
	if !ok {
		writeError(w, http.StatusNotFound, "BAL_NOT_FOUND", "balance not found")
		return
	}
	row := f.balances[id]
	if override, ok := f.balanceOverride[row.indicator]; ok {
		// Written as raw JSON so a figure no int64 can hold reaches the
		// adapter exactly as a real ledger would send it.
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"balance_id":%q,"balance":%s,"currency":%q,"ledger_id":%q,"indicator":%q,"precision":100}`,
			row.id, override, row.currency, row.ledgerID, row.indicator)
		return
	}
	writeJSON(w, http.StatusOK, f.renderBalance(row))
}

// getBalance fails the test. An account id is a name here, so nothing holds
// a Blnk balance id to read a balance by - and one that reached this
// endpoint would be an id the adapter had let escape into a port type.
func (f *fakeBlnk) getBalance(w http.ResponseWriter, r *http.Request) {
	f.count(r)
	f.t.Errorf("the adapter read a balance by id (%q); accounts are addressed by name here", r.PathValue("id"))
	writeError(w, http.StatusNotFound, "BAL_NOT_FOUND", "balance not found")
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

// createTransaction records one transfer the way the server does.
//
// The order of the steps is the server's: validate the request, split it
// into the transactions that will actually be recorded, then record each of
// them in turn - resolving its ends, refusing an overdraft it was not given
// permission for, and moving the balances. A failure part-way leaves the
// children before it recorded, because that is what the server does and a
// fake that rolled back would hide it.
//
// What comes back is the PARENT: the id this call minted and the reference
// the request carried, with the status the transfer ended in. No row is
// stored under either.
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
	// Both are required by the server's own validation, and the
	// description is the one the port does not require of its callers.
	if strings.TrimSpace(body.Description) == "" {
		writeError(w, http.StatusBadRequest, "TXN_VALIDATION", "description: cannot be blank")
		return
	}
	if strings.TrimSpace(body.Reference) == "" {
		writeError(w, http.StatusBadRequest, "TXN_VALIDATION", "reference: cannot be blank")
		return
	}

	parentID := f.nextID("txn")
	recording, err := f.splitTransaction(parentID, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "TXN_VALIDATION", err.Error())
		return
	}

	// The duplicate check runs per recorded transaction, on the reference
	// that transaction will carry - so a split is guarded by its children's
	// references and never by the transfer's own, which no row holds.
	for _, row := range recording {
		if held, taken := f.byReference[row.reference]; taken {
			if !f.freesRejectedReference || f.transactions[held].status != statusRejected {
				writeError(w, http.StatusConflict, "TXN_DUPLICATE_REFERENCE", "reference already exists")
				return
			}
		}
	}

	for _, row := range recording {
		if failure := f.recordTransaction(row, body.AllowOverdraft); failure != nil {
			writeError(w, failure.status, failure.code, failure.message)
			return
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"transaction_id": parentID,
		"reference":      body.Reference,
		"currency":       body.Currency,
		"precise_amount": preciseOf(body),
		"amount":         999999.99,
		"precision":      body.Precision,
		"status":         f.createdStatus,
		"created_at":     time.Now().UTC(),
		"meta_data":      body.MetaData,
	})
}

// splitTransaction turns one create request into the transactions the
// server will actually record.
//
// Exactly one side is split, and which is not a choice: the sources are
// taken when there are any and the destinations otherwise. Every child is a
// copy of the whole transfer - annotation and all - carrying one leg at one
// end and the transfer's scalar at the other, under the parent's id and the
// transfer's reference with an ordinal appended. A transfer split on BOTH
// sides has no scalar to give its children, so they are built with an empty
// end and the recording below fails on it: that is what the server does,
// and the adapter is expected to refuse the shape before it gets here.
//
// A request with no legs is recorded as itself, under its own reference and
// the parent's id, and is the only shape leaving a row a reader can find by
// the transfer's own reference.
func (f *fakeBlnk) splitTransaction(parentID string, body wireTransaction) ([]*txnRow, error) {
	base := func() *txnRow {
		return &txnRow{
			currency:  body.Currency,
			status:    f.createdStatus,
			createdAt: time.Now().UTC(),
			metadata:  body.MetaData,
		}
	}

	legs, asSource := body.Sources, true
	if len(legs) == 0 {
		legs, asSource = body.Destinations, false
	}
	if len(legs) == 0 {
		row := base()
		row.id, row.reference = parentID, body.Reference
		row.precise = preciseOf(body)
		row.source, row.dest = body.Source, body.Destination
		return []*txnRow{row}, nil
	}

	children := make([]*txnRow, 0, len(legs))
	total := new(big.Int)
	for i, leg := range legs {
		share, ok := new(big.Int).SetString(leg.PreciseDistribution, 10)
		if !ok {
			return nil, fmt.Errorf("leg %q carries precise_distribution %q, which is not an integer", leg.Identifier, leg.PreciseDistribution)
		}
		total.Add(total, share)

		row := base()
		row.id = f.nextID("txn")
		row.parent = parentID
		row.reference = fmt.Sprintf("%s%s%d", body.Reference, f.splitJoin, i+1)
		row.precise = share
		if asSource {
			row.source, row.dest = leg.Identifier, body.Destination
		} else {
			row.source, row.dest = body.Source, leg.Identifier
		}
		children = append(children, row)
	}
	if total.Cmp(preciseOf(body)) != 0 {
		return nil, fmt.Errorf("the legs sum to %s but the transaction is for %s", total, preciseOf(body))
	}
	return children, nil
}

// recordFailure is a refusal the recording step makes, in the shape the
// endpoint writes it back in.
type recordFailure struct {
	status  int
	code    string
	message string
}

// recordTransaction resolves one transaction's ends, applies it and stores
// it. Called with the lock held.
//
// An end beginning with "@" is a name: the balance behind it is created if
// there is none, and what is STORED is the balance id it resolved to. An
// end that is neither a name nor a balance this server holds is a refusal,
// which is how an empty end - the shape a both-sides split produces -
// arrives.
func (f *fakeBlnk) recordTransaction(row *txnRow, allowOverdraft bool) *recordFailure {
	ends := make(map[string]*balanceRow, 2)
	for _, end := range []string{row.source, row.dest} {
		if _, done := ends[end]; done {
			continue
		}
		if strings.HasPrefix(end, indicatorPrefix) {
			ends[end] = f.open(end)
			continue
		}
		held, known := f.balances[end]
		if !known {
			return &recordFailure{http.StatusNotFound, "BAL_NOT_FOUND", "balance " + strconv.Quote(end) + " not found"}
		}
		ends[end] = held
	}

	source, dest := ends[row.source], ends[row.dest]
	if row.status != statusRejected {
		if !allowOverdraft {
			if after := new(big.Int).Sub(source.minor, row.precise); after.Sign() < 0 {
				return &recordFailure{http.StatusBadRequest, "TXN_INSUFFICIENT_FUNDS", "insufficient funds in balance " + source.id}
			}
		}
		source.minor = new(big.Int).Sub(source.minor, row.precise)
		dest.minor = new(big.Int).Add(dest.minor, row.precise)
	}

	row.source, row.dest = source.id, dest.id
	f.transactions[row.id] = row
	f.byReference[row.reference] = row.id
	return nil
}

// preciseOf reads the integer amount off a create request, treating an
// absent one as zero.
func preciseOf(body wireTransaction) *big.Int {
	total := new(big.Int)
	if body.PreciseAmount != nil {
		total.SetString(body.PreciseAmount.String(), 10)
	}
	return total
}

// getTransaction fails the test. The adapter reads a transfer by reference
// and gathers a split from the rows a filter answers with, so nothing needs
// this endpoint - and a split's parent, the one id it might be tempted to
// ask for, is not a row that exists.
func (f *fakeBlnk) getTransaction(w http.ResponseWriter, r *http.Request) {
	f.count(r)
	f.t.Errorf("the adapter read a transaction by id (%q); a split's parent is no row, so an id read cannot answer for one", r.PathValue("id"))
	writeError(w, http.StatusNotFound, "TXN_NOT_FOUND", "transaction not found")
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

	// The server's own clamp, modelled because it is silent: a page asked
	// for above the ceiling is not refused, it is answered with the default
	// instead - so a reader that stopped at a short page would lose a
	// member's record without a word about it.
	limit := body.Limit
	if limit <= 0 || limit > filterPageCeiling {
		limit = filterPageDefault
	}

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
	if len(matched) > limit {
		matched = matched[:limit]
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

// What a filtered page answers with, whatever was asked for.
const (
	// filterPageCeiling is the largest page the server honours.
	filterPageCeiling = 100
	// filterPageDefault is what it answers with instead of a page above the
	// ceiling - quietly, which is the part that matters.
	filterPageDefault = 20
)

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
// columns the adapter asks about, both of which hold balance ids.
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
	return out
}

// record adds a transaction the adapter did not create, so History can be
// shown a shape no Post of its own would produce: a row carrying no
// annotation, a child whose siblings are missing, a transaction that never
// applied.
//
// source and dest are given as account NAMES and stored as the balance ids
// they resolve to, exactly as a recorded transaction holds them; the
// balances are opened if nothing has named them yet.
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
	for _, end := range []*string{&row.source, &row.dest} {
		if strings.HasPrefix(*end, indicatorPrefix) {
			*end = f.open(*end).id
		}
	}
	f.transactions[row.id] = row
	if row.reference != "" {
		f.byReference[row.reference] = row.id
	}
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
