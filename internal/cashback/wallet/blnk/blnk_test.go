package blnk_test

// Behaviour tests for the Blnk ledger, run against the fake server in
// fake_test.go so they need neither Docker nor a network. They mirror the
// in-memory reference suite case for case where the port's contract is the
// same for every implementation, and add the cases only this adapter has:
// what it puts on the wire, and what it makes of what comes back.
//
// The suite in integration_test.go asks the same questions of a real
// ledger. This one asks them of the wire.

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/blnk"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// Two currencies are enough to prove every per-currency rule; which
// currencies a deployment trades in is configuration, not a property of the
// ledger.
const (
	eur = money.Currency("EUR")
	gbp = money.Currency("GBP")
)

// newLedger builds a ledger pointed at the fake. The settle window is short
// because the fake answers instantly: a test that has to wait a real
// timeout to see a refusal is a test nobody runs.
func newLedger(t *testing.T, f *fakeBlnk, opts ...blnk.Option) *blnk.Ledger {
	t.Helper()
	settings := []blnk.Option{blnk.WithSettleWindow(2*time.Second, time.Millisecond)}
	ledger, err := blnk.New(f.URL(), append(settings, opts...)...)
	if err != nil {
		t.Fatalf("building a ledger: %v", err)
	}
	return ledger
}

// ensure resolves an account or fails the test.
func ensure(t *testing.T, ledger *blnk.Ledger, ref wallet.AccountRef, currency money.Currency) wallet.LedgerAccountID {
	t.Helper()
	id, err := ledger.EnsureAccount(context.Background(), ref, currency)
	if err != nil {
		t.Fatalf("ensuring %s in %s: %v", ref, currency, err)
	}
	return id
}

// member returns a fresh member's stage account reference.
func member(stage wallet.Stage) wallet.AccountRef {
	return wallet.MemberAccount(uuid.New(), stage)
}

// amount builds an amount or panics; every literal in this file is
// well-formed by construction.
func amount(minor int64, currency money.Currency) money.Amount {
	a, err := money.New(minor, currency)
	if err != nil {
		panic(err)
	}
	return a
}

// TestNewRefusesAMisconfiguredEndpoint keeps a bad BLNK_URL an error a
// composition root can report. The SDK panics on one, which would take the
// process down at wiring time with no explanation of which key was wrong.
func TestNewRefusesAMisconfiguredEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL string
		wantErr bool
	}{
		{name: "an absolute http endpoint", baseURL: "http://ledger:5001", wantErr: false},
		{name: "an absolute https endpoint", baseURL: "https://ledger.internal", wantErr: false},
		{name: "an endpoint with a trailing slash", baseURL: "http://ledger:5001/", wantErr: false},
		{name: "nothing at all", baseURL: "", wantErr: true},
		{name: "a host with no scheme", baseURL: "ledger:5001", wantErr: true},
		{name: "a scheme that is not http", baseURL: "redis://ledger:6379", wantErr: true},
		{name: "a scheme with no host", baseURL: "http://", wantErr: true},
		{name: "not a URL at all", baseURL: "http://[::1", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ledger, err := blnk.New(tc.baseURL)
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("New(%q) was accepted; want a refusal", tc.baseURL)
			case tc.wantErr:
				if strings.Contains(err.Error(), tc.baseURL) && tc.baseURL != "" {
					t.Errorf("the refusal repeats the endpoint, which may carry a credential: %v", err)
				}
			case err != nil:
				t.Fatalf("New(%q): %v", tc.baseURL, err)
			case ledger == nil:
				t.Fatal("New returned no ledger and no error")
			}
		})
	}
}

// TestEnsureAccountRefusesUnusableInput keeps the port's two structural
// refusals before any I/O: the fake fails the test if it is called at all.
func TestEnsureAccountRefusesUnusableInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ref      wallet.AccountRef
		currency money.Currency
		want     error
	}{
		{name: "the zero reference", ref: wallet.AccountRef{}, currency: eur, want: wallet.ErrInvalidAccountRef},
		{name: "a member reference with no member", ref: wallet.MemberAccount(uuid.Nil, wallet.StageConfirmed), currency: eur, want: wallet.ErrInvalidAccountRef},
		{name: "a member reference with no stage", ref: wallet.MemberAccount(uuid.New(), 0), currency: eur, want: wallet.ErrInvalidAccountRef},
		{name: "a house reference with no name", ref: wallet.HouseAccount(""), currency: eur, want: wallet.ErrInvalidAccountRef},
		{name: "a house reference padded with space", ref: wallet.HouseAccount(" rounding"), currency: eur, want: wallet.ErrInvalidAccountRef},
		{name: "no currency", ref: member(wallet.StageConfirmed), currency: "", want: money.ErrInvalidCurrency},
		{name: "a lowercase currency", ref: member(wallet.StageConfirmed), currency: "eur", want: money.ErrInvalidCurrency},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeBlnk(t)
			ledger := newLedger(t, fake)

			_, err := ledger.EnsureAccount(context.Background(), tc.ref, tc.currency)
			if !errors.Is(err, tc.want) {
				t.Fatalf("EnsureAccount error = %v, want one wrapping %v", err, tc.want)
			}
			if got := fake.accounts(); got != 0 {
				t.Errorf("%d balance(s) exist after a reference that names none", got)
			}
		})
	}
}

// TestEnsureAccountNamesAccountsInjectively is the account-naming test. The
// name derived from (ref, currency) IS this adapter's account id, so two
// different accounts sharing one name would be two members' money in one
// balance - and the pairs below are the ones a naive derivation gets wrong:
// a house name holding the separator, a house name spelling a member
// prefix, and one reference across two currencies.
func TestEnsureAccountNamesAccountsInjectively(t *testing.T) {
	t.Parallel()

	one, two := uuid.New(), uuid.New()
	pairs := []struct {
		name     string
		ref      wallet.AccountRef
		currency money.Currency
	}{
		{name: "a member's confirmed euros", ref: wallet.MemberAccount(one, wallet.StageConfirmed), currency: eur},
		{name: "the same member's confirmed pounds", ref: wallet.MemberAccount(one, wallet.StageConfirmed), currency: gbp},
		{name: "the same member's reserved euros", ref: wallet.MemberAccount(one, wallet.StageReserved), currency: eur},
		{name: "the same member's pending euros", ref: wallet.MemberAccount(one, wallet.StagePending), currency: eur},
		{name: "the same member's held euros", ref: wallet.MemberAccount(one, wallet.StageHeld), currency: eur},
		{name: "another member's confirmed euros", ref: wallet.MemberAccount(two, wallet.StageConfirmed), currency: eur},
		{name: "a house account", ref: wallet.HouseAccount("rounding"), currency: eur},
		{name: "a house account in another currency", ref: wallet.HouseAccount("rounding"), currency: gbp},
		{name: "a house account whose name holds the separator", ref: wallet.HouseAccount("rounding.EUR"), currency: eur},
		{name: "a house account whose name is a prefix of another", ref: wallet.HouseAccount("rounding"), currency: "USD"},
		{name: "a house account whose name spells a member reference", ref: wallet.HouseAccount(one.String() + ".confirmed"), currency: eur},
		{name: "a house account whose name holds a slash and a quote", ref: wallet.HouseAccount(`a/b"c`), currency: eur},
		{name: "a house account whose name holds a space", ref: wallet.HouseAccount("clawback loss"), currency: eur},
	}

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)

	ids := make(map[wallet.LedgerAccountID]string, len(pairs))
	for _, pair := range pairs {
		id := ensure(t, ledger, pair.ref, pair.currency)
		if !strings.HasPrefix(string(id), "@") {
			t.Fatalf("%s resolved to %q, which this ledger would read as a balance id rather than a name", pair.name, id)
		}
		if other, clash := ids[id]; clash {
			t.Fatalf("%s and %s are both named %q; two accounts sharing one name is two members' money in one balance", pair.name, other, id)
		}
		ids[id] = pair.name

		// Ensuring again must answer the same id - the whole point of a
		// derived name, and what makes the call idempotent with nothing
		// stored on either side.
		if again := ensure(t, ledger, pair.ref, pair.currency); again != id {
			t.Fatalf("%s: ensuring twice gave %q then %q", pair.name, id, again)
		}
	}

	if got := fake.accounts(); got != 0 {
		t.Errorf("%d balance(s) exist after ensuring %d accounts; a balance appears when a transfer names one, not before", got, len(pairs))
	}
}

// TestEnsureAccountCreatesNothing is the fact the whole design turns on.
// The create-a-balance endpoint takes no name, so a balance made through it
// could never be found again; the only writer of a name is the transaction
// path. So EnsureAccount derives a name and stops, and the balance behind
// it appears when the first transfer names it - which is when this test
// looks for it.
func TestEnsureAccountCreatesNothing(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)

	if got := fake.accounts(); got != 0 {
		t.Fatalf("ensuring two accounts left %d balance(s) behind; the ledger has no endpoint that could have made one", got)
	}
	// Reading one costs nothing and answers zero: an account with no
	// balance row holds nothing, which is not the same as not existing.
	held, err := ledger.Balance(context.Background(), holder, eur)
	if err != nil {
		t.Fatalf("reading an account nothing has been posted to: %v", err)
	}
	if !held.IsZero() {
		t.Errorf("an account no transfer has named holds %s, want nothing", held)
	}

	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-first-touch",
		Reference:      "entry-1",
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-2500, eur)},
			{Account: holder, Amount: amount(2500, eur)},
		},
	}); err != nil {
		t.Fatalf("posting: %v", err)
	}

	for _, id := range []wallet.LedgerAccountID{house, holder} {
		if !fake.knows(string(id)) {
			t.Errorf("no balance is named %q after a transfer named it; every later read of the account would find nothing", id)
		}
	}
	if got := fake.balanceOf(string(holder)); got != 2500 {
		t.Errorf("the account the transfer created holds %d, want 2500", got)
	}
}

// TestEnsureAccountCarriesNoMemberIdentity holds the ledger to the one
// thing it may know about a member: the opaque account uuid. A name
// carrying anything else would put member identity in a system whose only
// job is routing money.
func TestEnsureAccountCarriesNoMemberIdentity(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake, blnk.WithLedgerID("ldg_pinned"))
	memberID := uuid.New()

	id := ensure(t, ledger, wallet.MemberAccount(memberID, wallet.StageConfirmed), eur)

	// The name is scoped to the Blnk ledger this deployment is configured
	// for, because a name is unique across the whole server and two
	// deployments must not resolve each other's accounts; everything after
	// that scope is the account itself.
	want := "member." + memberID.String() + ".confirmed.EUR"
	scope, account, cut := strings.Cut(string(id), ".")
	if !cut || account != want {
		t.Fatalf("the account is named %q, want a ledger scope followed by %q", id, want)
	}
	if scope != "@"+hex.EncodeToString([]byte("ldg_pinned")) {
		t.Fatalf("the account is scoped to %q, want the configured ledger", scope)
	}
}

// TestEnsureAccountScopesAccountsToItsOwnLedger is the isolation
// [blnk.WithLedgerName] and [blnk.WithLedgerID] claim, held to the claim.
// The endpoint that finds a balance by name carries no ledger, so two
// deployments pointed at one Blnk server share one pool of names - and
// house names come from configuration, so "commission" is the same string
// in staging and in production. Two deployments resolving one balance is
// one deployment posting the other's money.
func TestEnsureAccountScopesAccountsToItsOwnLedger(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	first := newLedger(t, fake, blnk.WithLedgerName("cashback-one"))
	second := newLedger(t, fake, blnk.WithLedgerName("cashback-two"))
	memberID := uuid.New()

	for _, tc := range []struct {
		name string
		ref  wallet.AccountRef
	}{
		{name: "a house account named the same in both", ref: wallet.HouseAccount("commission")},
		{name: "the same member's confirmed bucket", ref: wallet.MemberAccount(memberID, wallet.StageConfirmed)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			here := ensure(t, first, tc.ref, eur)
			there := ensure(t, second, tc.ref, eur)
			if here == there {
				t.Fatalf("both deployments resolved balance %q; one deployment's money would land on the other's account", here)
			}
			// And neither may read the other's balance back, however the
			// id was come by.
			if _, err := first.Balance(context.Background(), there, eur); !errors.Is(err, wallet.ErrUnknownAccount) {
				t.Fatalf("reading the neighbouring deployment's balance gave %v, want one wrapping %v", err, wallet.ErrUnknownAccount)
			}
		})
	}
}

// TestEnsureAccountIsIdempotentUnderConcurrency holds the port's promise
// that racing callers all get one account. Two balances under one name is
// one member's money in two places, and no later reconciliation finds it.
func TestEnsureAccountIsIdempotentUnderConcurrency(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	ref := member(wallet.StageConfirmed)

	const callers = 8
	ids := make([]wallet.LedgerAccountID, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ids[i], errs[i] = ledger.EnsureAccount(context.Background(), ref, eur)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("caller %d resolved %q, caller 0 resolved %q", i, ids[i], ids[0])
		}
	}
	if got := fake.accounts(); got != 0 {
		t.Errorf("%d balance(s) exist after %d racing callers ensured one account; there is nothing for them to race for", got, callers)
	}
}

// TestPostSendsIntegersAndNeverFloats is the C-6 boundary, asserted on the
// bytes rather than on a decoded struct. The SDK's transaction carries a
// float amount beside an integer one; this adapter must fill the integer
// and leave the float at zero, because a money value that passed through a
// float64 is a rounding error waiting for an audit to find it.
func TestPostSendsIntegersAndNeverFloats(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)

	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-integers",
		Reference:      "entry-1",
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-2500, eur)},
			{Account: holder, Amount: amount(2500, eur)},
		},
	}); err != nil {
		t.Fatalf("posting: %v", err)
	}

	got := fake.onlyCreate()
	body := string(got.body)
	for _, want := range []string{`"amount":0`, `"precise_amount":2500`, `"skip_queue":true`, `"currency":"EUR"`, `"reference":"key-integers"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the request does not carry %s:\n%s", want, body)
		}
	}
	if got.txn.Amount != 0 {
		t.Errorf("the float amount carries %v; money must travel as the integer", got.txn.Amount)
	}
	if got.txn.PreciseAmount == nil {
		t.Fatal("the request carries no integer amount")
	}
	if raw := got.txn.PreciseAmount.String(); strings.ContainsAny(raw, ".eE") {
		t.Errorf("the integer amount was written as %q, which is not an exact integer literal", raw)
	}
	if !got.txn.SkipQueue {
		t.Error("the request did not ask to skip the queue, so what it recorded need not be readable when Post returns")
	}
}

// TestBalanceRefusesAFigureItCannotCarry holds the other half of the money
// boundary: an integer coming back that this port cannot represent is
// refused, never truncated, and a figure that did not come back at all is
// refused rather than read as an empty account. A balance that reads as its
// own negation, or as nothing, is worse than an error.
func TestBalanceRefusesAFigureItCannotCarry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		balance string
		want    error
	}{
		{name: "a figure larger than the minor units this port carries", balance: "170141183460469231731687303715884105727", want: money.ErrOverflow},
		{name: "a figure smaller than it", balance: "-170141183460469231731687303715884105727", want: money.ErrOverflow},
		{name: "no figure at all", balance: "null"},
		{name: "a figure this port can carry", balance: "2500"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeBlnk(t)
			ledger := newLedger(t, fake)
			holder := ensure(t, ledger, member(wallet.StageConfirmed), eur)
			// Opened first: a figure can only come back wrong for an
			// account some transfer has already named.
			fake.seed(string(holder), 0)
			fake.balanceOverride[string(holder)] = tc.balance

			got, err := ledger.Balance(context.Background(), holder, eur)
			switch {
			case tc.want != nil:
				if !errors.Is(err, tc.want) {
					t.Fatalf("Balance error = %v, want one wrapping %v", err, tc.want)
				}
			case tc.balance == "null":
				if err == nil {
					t.Fatalf("a balance the ledger did not answer with read as %s", got)
				}
			case err != nil:
				t.Fatalf("Balance: %v", err)
			case !got.Equal(amount(2500, eur)):
				t.Fatalf("Balance = %s, want %s", got, amount(2500, eur))
			}
		})
	}
}

// TestPostRefusesACrossCurrencyTransfer is the ErrUnsupportedTransfer case.
// A transaction on this ledger carries one currency, so a transfer spanning
// two cannot be recorded in one atomic act - and refusing is the contract,
// because splitting it into two transactions would promise an atomicity
// this adapter would not be delivering.
func TestPostRefusesACrossCurrencyTransfer(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	houseEUR := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	memberEUR := ensure(t, ledger, member(wallet.StagePending), eur)
	houseGBP := ensure(t, ledger, wallet.HouseAccount("commission"), gbp)
	memberGBP := ensure(t, ledger, member(wallet.StagePending), gbp)

	_, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-two-currencies",
		Postings: []wallet.Posting{
			{Account: houseEUR, Amount: amount(-2500, eur)},
			{Account: memberEUR, Amount: amount(2500, eur)},
			{Account: houseGBP, Amount: amount(-2000, gbp)},
			{Account: memberGBP, Amount: amount(2000, gbp)},
		},
	})
	if !errors.Is(err, wallet.ErrUnsupportedTransfer) {
		t.Fatalf("Post error = %v, want one wrapping %v", err, wallet.ErrUnsupportedTransfer)
	}
	if got := len(fake.createdRequests()); got != 0 {
		t.Errorf("%d transaction(s) were sent for a transfer that was refused; a refusal must record nothing", got)
	}
}

// TestPostDecomposesOneSide covers the split this ledger CAN record: one
// account on one side and any number on the other, in one currency, as one
// transaction. The legs must carry integer amounts summing to the
// transaction's total, and the transaction must be marked atomic - a split
// that succeeded a leg at a time would not be one movement of money.
func TestPostDecomposesOneSide(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		wants map[string]int64
	}{
		{name: "one gives and two receive", wants: map[string]int64{"commission": -1000, "holder": 999, "rounding": 1}},
		{name: "two give and one receives", wants: map[string]int64{"commission": -700, "float": -300, "holder": 1000}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeBlnk(t)
			ledger := newLedger(t, fake)
			ids := map[string]wallet.LedgerAccountID{
				"commission": ensure(t, ledger, wallet.HouseAccount("commission"), eur),
				"float":      ensure(t, ledger, wallet.HouseAccount("float"), eur),
				"rounding":   ensure(t, ledger, wallet.HouseAccount("rounding"), eur),
				"holder":     ensure(t, ledger, member(wallet.StagePending), eur),
			}

			wantLegs := make(map[string]string, len(tc.wants))
			postings := make([]wallet.Posting, 0, len(tc.wants))
			for name, minor := range tc.wants {
				postings = append(postings, wallet.Posting{Account: ids[name], Amount: amount(minor, eur)})
				wantLegs[string(ids[name])] = strconv.FormatInt(max(minor, -minor), 10)
			}
			if _, err := ledger.Post(context.Background(), wallet.Transfer{
				IdempotencyKey: "key-split-" + tc.name,
				Reference:      "entry-" + tc.name,
				Postings:       postings,
			}); err != nil {
				t.Fatalf("posting: %v", err)
			}

			got := fake.onlyCreate()
			if !got.txn.Atomic {
				t.Error("a split transaction was not marked atomic, so its legs could land one at a time")
			}
			// Exactly one side travels as legs; the other is the scalar
			// every child of the split will be given.
			legs := append(append([]wireLeg{}, got.txn.Sources...), got.txn.Destinations...)
			if len(legs) != 2 {
				t.Fatalf("the transaction carries %d leg(s), want the 2 accounts on the split side", len(legs))
			}
			if (got.txn.Source == "") == (got.txn.Destination == "") {
				t.Errorf("the transaction has source %q and destination %q; exactly one side is split and the other is a single account", got.txn.Source, got.txn.Destination)
			}
			scalar := cmp.Or(got.txn.Source, got.txn.Destination)
			delete(wantLegs, scalar)

			for _, leg := range legs {
				want, known := wantLegs[leg.Identifier]
				if !known {
					t.Errorf("the transaction carries a leg on %q, which the transfer does not name as a leg", leg.Identifier)
					continue
				}
				if leg.PreciseDistribution != want {
					t.Errorf("the leg on %q carries %q, want %q", leg.Identifier, leg.PreciseDistribution, want)
				}
				if leg.Distribution != "" {
					t.Errorf("the leg on %q carries the distribution %q; money must travel as an integer", leg.Identifier, leg.Distribution)
				}
				delete(wantLegs, leg.Identifier)
			}
			if len(wantLegs) != 0 {
				t.Errorf("the transaction is missing legs for %v", wantLegs)
			}

			// Every balance moved by exactly what the transfer said.
			for name, want := range tc.wants {
				if got := fake.balanceOf(string(ids[name])); got != want {
					t.Errorf("account %q holds %d, want %d", name, got, want)
				}
			}
		})
	}
}

// TestPostRefusesASplitOnBothSides is the second shape this ledger cannot
// record in one atomic act. It splits ONE side of a transaction into a
// child per leg and hands every child the other side's single account; a
// transfer split on both sides has no such account, so the children are
// built with an end naming nothing and the ledger fails to resolve it.
//
// Refusing before anything is sent is the contract, and it is also the only
// way the caller learns what to do instead: the port's own sentinel says
// the shape is unrecordable, where whatever the ledger answers about an
// unresolvable account would not.
func TestPostRefusesASplitOnBothSides(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	commission := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	float := ensure(t, ledger, wallet.HouseAccount("float"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)
	rounding := ensure(t, ledger, wallet.HouseAccount("rounding"), eur)

	_, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-both-sides",
		Reference:      "entry-both-sides",
		Postings: []wallet.Posting{
			{Account: commission, Amount: amount(-700, eur)},
			{Account: float, Amount: amount(-300, eur)},
			{Account: holder, Amount: amount(999, eur)},
			{Account: rounding, Amount: amount(1, eur)},
		},
	})
	if !errors.Is(err, wallet.ErrUnsupportedTransfer) {
		t.Fatalf("Post error = %v, want one wrapping %v", err, wallet.ErrUnsupportedTransfer)
	}
	if got := len(fake.createdRequests()); got != 0 {
		t.Errorf("%d transaction(s) were sent for a transfer that was refused; a refusal must record nothing", got)
	}

	// And the way out the refusal names works: one transfer per side, each
	// with its own key, each recordable on its own.
	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-both-sides-in",
		Reference:      "entry-both-sides-in",
		Postings: []wallet.Posting{
			{Account: commission, Amount: amount(-700, eur)},
			{Account: float, Amount: amount(-300, eur)},
			{Account: holder, Amount: amount(1000, eur)},
		},
	}); err != nil {
		t.Fatalf("the first half of the split transfer: %v", err)
	}
	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-both-sides-out",
		Reference:      "entry-both-sides-out",
		Postings: []wallet.Posting{
			{Account: holder, Amount: amount(-1, eur)},
			{Account: rounding, Amount: amount(1, eur)},
		},
	}); err != nil {
		t.Fatalf("the second half of the split transfer: %v", err)
	}
	for id, want := range map[wallet.LedgerAccountID]int64{commission: -700, float: -300, holder: 999, rounding: 1} {
		if got := fake.balanceOf(string(id)); got != want {
			t.Errorf("account %q holds %d, want %d", id, got, want)
		}
	}
}

// TestPostFindsTheKeyOfASplitTransfer is the idempotency contract over the
// shape that hides the key. A split rewrites the reference of every child
// and records no row under the transfer's own, so a key that recorded a
// split cannot be found by asking for it - and a Post that failed to find
// it would move the money a second time, which is the one mistake
// idempotency exists to prevent.
//
// The children are numbered from one, so the first child's reference is
// derivable; what is not certain is which character the ledger joins it
// with. The synchronous path writes the hyphen and the queued path the
// underscore, so both are held to here.
func TestPostFindsTheKeyOfASplitTransfer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		join string
	}{
		{name: "the reference the synchronous path writes", join: splitJoinSynchronous},
		{name: "the reference the queued path writes", join: splitJoinQueued},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeBlnk(t)
			fake.splitJoin = tc.join
			ledger := newLedger(t, fake)
			house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
			holder := ensure(t, ledger, member(wallet.StagePending), eur)
			rounding := ensure(t, ledger, wallet.HouseAccount("rounding"), eur)

			transfer := wallet.Transfer{
				IdempotencyKey: "key-rounded-earning",
				Reference:      "entry-transition-1",
				Postings: []wallet.Posting{
					{Account: house, Amount: amount(-1000, eur)},
					{Account: holder, Amount: amount(999, eur)},
					{Account: rounding, Amount: amount(1, eur)},
				},
			}
			first, err := ledger.Post(context.Background(), transfer)
			if err != nil {
				t.Fatalf("posting a rounded earning: %v", err)
			}

			// The same movements in another order: representation, not
			// identity, so this is a replay and must move nothing.
			reordered := transfer
			reordered.Postings = []wallet.Posting{
				{Account: rounding, Amount: amount(1, eur)},
				{Account: holder, Amount: amount(999, eur)},
				{Account: house, Amount: amount(-1000, eur)},
			}
			again, err := ledger.Post(context.Background(), reordered)
			if err != nil {
				t.Fatalf("the replay of a split transfer's key was refused: %v", err)
			}
			if again != first {
				t.Errorf("the replay answered %q, want the transfer's own reference %q", again, first)
			}
			if got := fake.balanceOf(string(holder)); got != 999 {
				t.Errorf("the member holds %d after one key was posted twice, want 999", got)
			}

			// And a different transfer under that key is the collision the
			// key exists to surface, not a second movement.
			conflicting := transfer
			conflicting.Postings = []wallet.Posting{
				{Account: house, Amount: amount(-1000, eur)},
				{Account: holder, Amount: amount(1000, eur)},
			}
			if _, err := ledger.Post(context.Background(), conflicting); !errors.Is(err, wallet.ErrIdempotencyConflict) {
				t.Errorf("a different transfer under a split's key gave %v, want one wrapping %v", err, wallet.ErrIdempotencyConflict)
			}
		})
	}
}

// TestPostDescribesEveryTransferItSends keeps a transfer with no domain
// pointer postable. The ledger requires a description and the port does not
// require a Reference, so a blank one travels as the idempotency key - the
// truest pointer there is when the caller named nothing - rather than as a
// blank the ledger refuses.
func TestPostDescribesEveryTransferItSends(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)

	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-undescribed",
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-2500, eur)},
			{Account: holder, Amount: amount(2500, eur)},
		},
	}); err != nil {
		t.Fatalf("posting a transfer naming no domain record: %v", err)
	}
	if got := fake.onlyCreate().txn.Description; got != "key-undescribed" {
		t.Errorf("the transaction is described as %q, want the idempotency key", got)
	}

	// The document replay identity is judged by still records the blank
	// Reference, so the description standing in for it cannot turn a
	// replay into a conflict.
	raw, ok := fake.onlyCreate().txn.MetaData["wallet_transfer"].(string)
	if !ok {
		t.Fatalf("the transaction carries no identity document: %v", fake.onlyCreate().txn.MetaData)
	}
	if strings.Contains(raw, "key-undescribed") {
		t.Errorf("the identity document records the description in place of the transfer's own reference: %s", raw)
	}
}

// TestPostNetsAnAccountItSeesTwice covers the one place the wire shape and
// the transfer's meaning diverge: a Blnk transaction has one leg per
// account per side, so postings are netted for the request - while what
// makes two transfers the same is still the postings the caller wrote.
func TestPostNetsAnAccountItSeesTwice(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)

	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-netted",
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-100, eur)},
			{Account: house, Amount: amount(-200, eur)},
			{Account: holder, Amount: amount(300, eur)},
		},
	}); err != nil {
		t.Fatalf("posting: %v", err)
	}

	got := fake.onlyCreate()
	if got.txn.Source != string(house) {
		t.Errorf("the transaction's source is %q, want the netted account %q", got.txn.Source, house)
	}
	if got.txn.PreciseAmount == nil || got.txn.PreciseAmount.String() != "300" {
		t.Errorf("the transaction moves %v, want 300", got.txn.PreciseAmount)
	}
	if got := fake.balanceOf(string(house)); got != -300 {
		t.Errorf("the house account holds %d, want -300", got)
	}
}

// TestPostReplayAndConflict is the idempotency contract, case for case with
// the in-memory reference. The first Post records; what a second Post under
// the same key means is decided by content, never by spelling.
func TestPostReplayAndConflict(t *testing.T) {
	t.Parallel()

	first := wallet.Transfer{
		Reference: "entry-1",
		Metadata:  map[string]string{"entry": "1"},
	}

	tests := []struct {
		name string
		// again builds the second transfer from the accounts, given the
		// first one for reference.
		again func(first wallet.Transfer, house, holder wallet.LedgerAccountID) wallet.Transfer
		want  error
	}{
		{
			name: "the very same transfer",
			again: func(f wallet.Transfer, _, _ wallet.LedgerAccountID) wallet.Transfer {
				return f
			},
		},
		{
			name: "the same movements rebuilt in another order",
			again: func(f wallet.Transfer, house, holder wallet.LedgerAccountID) wallet.Transfer {
				f.Postings = []wallet.Posting{
					{Account: holder, Amount: amount(2500, eur)},
					{Account: house, Amount: amount(-2500, eur)},
				}
				return f
			},
		},
		{
			name: "the same transfer with metadata spelled as an empty map",
			again: func(f wallet.Transfer, _, _ wallet.LedgerAccountID) wallet.Transfer {
				f.Metadata = map[string]string{"entry": "1"}
				return f
			},
		},
		{
			name: "different amounts",
			again: func(f wallet.Transfer, house, holder wallet.LedgerAccountID) wallet.Transfer {
				f.Postings = []wallet.Posting{
					{Account: house, Amount: amount(-2600, eur)},
					{Account: holder, Amount: amount(2600, eur)},
				}
				return f
			},
			want: wallet.ErrIdempotencyConflict,
		},
		{
			name: "the same net movement split into two postings",
			again: func(f wallet.Transfer, house, holder wallet.LedgerAccountID) wallet.Transfer {
				f.Postings = []wallet.Posting{
					{Account: house, Amount: amount(-1250, eur)},
					{Account: house, Amount: amount(-1250, eur)},
					{Account: holder, Amount: amount(2500, eur)},
				}
				return f
			},
			want: wallet.ErrIdempotencyConflict,
		},
		{
			name: "another domain reference",
			again: func(f wallet.Transfer, _, _ wallet.LedgerAccountID) wallet.Transfer {
				f.Reference = "entry-2"
				return f
			},
			want: wallet.ErrIdempotencyConflict,
		},
		{
			name: "another annotation under one key",
			again: func(f wallet.Transfer, _, _ wallet.LedgerAccountID) wallet.Transfer {
				f.Metadata = map[string]string{"entry": "2"}
				return f
			},
			want: wallet.ErrIdempotencyConflict,
		},
		{
			name: "an extra annotation",
			again: func(f wallet.Transfer, _, _ wallet.LedgerAccountID) wallet.Transfer {
				f.Metadata = map[string]string{"entry": "1", "batch": "7"}
				return f
			},
			want: wallet.ErrIdempotencyConflict,
		},
		{
			name: "no annotations at all",
			again: func(f wallet.Transfer, _, _ wallet.LedgerAccountID) wallet.Transfer {
				f.Metadata = nil
				return f
			},
			want: wallet.ErrIdempotencyConflict,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeBlnk(t)
			ledger := newLedger(t, fake)
			house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
			holder := ensure(t, ledger, member(wallet.StagePending), eur)

			original := first
			original.IdempotencyKey = "key-replay"
			original.Postings = []wallet.Posting{
				{Account: house, Amount: amount(-2500, eur)},
				{Account: holder, Amount: amount(2500, eur)},
			}
			ref, err := ledger.Post(context.Background(), original)
			if err != nil {
				t.Fatalf("the first post: %v", err)
			}

			again, err := ledger.Post(context.Background(), tc.again(original, house, holder))
			switch {
			case tc.want != nil:
				if !errors.Is(err, tc.want) {
					t.Fatalf("the second post's error = %v, want one wrapping %v", err, tc.want)
				}
			case err != nil:
				t.Fatalf("the replay was refused: %v", err)
			case again != ref:
				t.Fatalf("the replay answered %q, want the original %q", again, ref)
			}

			if got := fake.balanceOf(string(holder)); got != 2500 {
				t.Errorf("the member holds %d after a second post of one key, want 2500: a replay must move no money", got)
			}
			if got := len(fake.createdRequests()); got > 1 {
				t.Errorf("%d transactions were created for one idempotency key", got)
			}
		})
	}
}

// TestPostAnswersAReplayWhenTheLedgerRefusesTheDuplicate covers the race
// the key exists for: two posts of one key where neither sees the other's
// record, so the ledger itself refuses the second. That refusal is a
// replay, not a failure, and both callers must learn the same reference.
func TestPostAnswersAReplayWhenTheLedgerRefusesTheDuplicate(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)

	transfer := wallet.Transfer{
		IdempotencyKey: "key-raced",
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-2500, eur)},
			{Account: holder, Amount: amount(2500, eur)},
		},
	}

	// Both posts reach the create handler before either has recorded, so
	// the second is refused by the reference constraint rather than caught
	// by the read that precedes it.
	var gate sync.WaitGroup
	gate.Add(2)
	fake.beforeCreate = func() {
		gate.Done()
		gate.Wait()
	}

	const callers = 2
	refs := make([]wallet.TransferRef, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refs[i], errs[i] = ledger.Post(context.Background(), transfer)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if refs[0] != refs[1] {
		t.Fatalf("the two callers learned different references: %q and %q", refs[0], refs[1])
	}
	if got := fake.balanceOf(string(holder)); got != 2500 {
		t.Errorf("the member holds %d, want 2500: one key must move money once", got)
	}
}

// TestPostMapsTheLedgersRefusals is the error-mapping table. Every refusal
// the substrate can produce has to arrive as the port's own error, because
// callers branch on those and on nothing else.
func TestPostMapsTheLedgersRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		postings func(house, holder, other wallet.LedgerAccountID) []wallet.Posting
		want     error
	}{
		{
			name: "a posting naming an account nobody issued",
			postings: func(house, _, _ wallet.LedgerAccountID) []wallet.Posting {
				return []wallet.Posting{
					{Account: house, Amount: amount(-2500, eur)},
					{Account: "bln_never-issued", Amount: amount(2500, eur)},
				}
			},
			want: wallet.ErrUnknownAccount,
		},
		{
			name: "a posting in a currency its account does not hold",
			postings: func(house, _, other wallet.LedgerAccountID) []wallet.Posting {
				return []wallet.Posting{
					{Account: house, Amount: amount(-2500, gbp)},
					{Account: other, Amount: amount(2500, gbp)},
				}
			},
			want: money.ErrCurrencyMismatch,
		},
		{
			name: "a member's stage account drawn below nothing",
			postings: func(house, holder, _ wallet.LedgerAccountID) []wallet.Posting {
				return []wallet.Posting{
					{Account: holder, Amount: amount(-2500, eur)},
					{Account: house, Amount: amount(2500, eur)},
				}
			},
			want: wallet.ErrInsufficientFunds,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeBlnk(t)
			ledger := newLedger(t, fake)
			house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
			holder := ensure(t, ledger, member(wallet.StageConfirmed), eur)
			other := ensure(t, ledger, member(wallet.StagePending), eur)

			_, err := ledger.Post(context.Background(), wallet.Transfer{
				IdempotencyKey: "key-refused",
				Postings:       tc.postings(house, holder, other),
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Post error = %v, want one wrapping %v", err, tc.want)
			}
			if got := fake.holds(string(house)); got != 0 {
				t.Errorf("a refused transfer moved the house account to %d; a refusal must change no balance", got)
			}
		})
	}
}

// TestPostMapsTheLedgersOwnInsufficientFunds covers the refusal that comes
// back from the ledger rather than from this package's own check: the case
// the whole double-spend defence rests on, where two reservations race and
// the substrate is what makes exactly one of them win.
func TestPostMapsTheLedgersOwnInsufficientFunds(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	confirmed := ensure(t, ledger, member(wallet.StageConfirmed), eur)
	reserved := ensure(t, ledger, member(wallet.StageReserved), eur)
	fake.seed(string(confirmed), 5000)

	// The balance this package reads says there is enough; the ledger's
	// own view, a moment later, says there is not. Only the ledger's
	// refusal stands between the two of them and a double spend.
	fake.beforeCreate = func() { fake.seed(string(confirmed), 0) }

	_, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-reserve",
		Postings: []wallet.Posting{
			{Account: confirmed, Amount: amount(-5000, eur)},
			{Account: reserved, Amount: amount(5000, eur)},
		},
	})
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("Post error = %v, want one wrapping %v", err, wallet.ErrInsufficientFunds)
	}
}

// TestPostAsksForAnOverdraftOnlyWhereItIsAllowed is the other half of the
// solvency rule, and the shape of the request is the whole of it. Blnk
// grants an overdraft to a transaction, not to a leg, so asking for one
// because a house account gives would exempt every member account giving in
// the same transfer - and the member's stage account may never go negative.
// A transfer drawing on member accounts is therefore sent without the
// permission whatever else it draws on, and the ledger refuses it
// atomically against the balance it actually finds.
func TestPostAsksForAnOverdraftOnlyWhereItIsAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		givers    []string
		houseHeld int64
		wantAsk   bool
		wantErr   error
	}{
		{name: "a house account gives", givers: []string{"house"}, wantAsk: true},
		{name: "a member's stage account gives", givers: []string{"member"}, wantAsk: false},
		{
			name:      "both give and the house account holds what it gives",
			givers:    []string{"house", "member"},
			houseHeld: 5000,
			wantAsk:   false,
		},
		{
			name:    "both give and the house account would go negative",
			givers:  []string{"house", "member"},
			wantErr: wallet.ErrUnsupportedTransfer,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeBlnk(t)
			ledger := newLedger(t, fake)
			house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
			giver := ensure(t, ledger, member(wallet.StageConfirmed), eur)
			taker := ensure(t, ledger, member(wallet.StageReserved), eur)
			fake.seed(string(giver), 10000)
			if tc.houseHeld != 0 {
				fake.seed(string(house), tc.houseHeld)
			}

			postings := []wallet.Posting{{Account: taker, Amount: amount(int64(1000*len(tc.givers)), eur)}}
			for _, giverKind := range tc.givers {
				from := house
				if giverKind == "member" {
					from = giver
				}
				postings = append(postings, wallet.Posting{Account: from, Amount: amount(-1000, eur)})
			}

			_, err := ledger.Post(context.Background(), wallet.Transfer{
				IdempotencyKey: "key-overdraft-" + strings.Join(tc.givers, "-"),
				Postings:       postings,
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Post error = %v, want one wrapping %v", err, tc.wantErr)
				}
				if got := len(fake.createdRequests()); got != 0 {
					t.Errorf("%d transaction(s) were sent for a transfer this ledger cannot record", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("posting: %v", err)
			}

			got := fake.onlyCreate()
			if got.txn.AllowOverdraft != tc.wantAsk {
				t.Fatalf("the transaction asked for an overdraft = %v, want %v", got.txn.AllowOverdraft, tc.wantAsk)
			}
			if !tc.wantAsk && strings.Contains(string(got.body), "allow_overdraft") {
				t.Errorf("a transfer a member's stage account gives in still named allow_overdraft:\n%s", got.body)
			}
		})
	}
}

// TestPostKeepsTheLedgersRefusalWhenAHouseAccountGivesBeside is the race the
// whole-transaction overdraft used to open. A member's stage account and a
// house account give in one transfer; the balance this package read says
// there is enough and the ledger, a moment later, says there is not. Only
// the ledger's own refusal stands between that and a member overdrawn into
// a payout the money does not cover.
func TestPostKeepsTheLedgersRefusalWhenAHouseAccountGivesBeside(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	confirmed := ensure(t, ledger, member(wallet.StageConfirmed), eur)
	reserved := ensure(t, ledger, member(wallet.StageReserved), eur)
	fake.seed(string(house), 5000)
	fake.seed(string(confirmed), 5000)
	fake.beforeCreate = func() { fake.seed(string(confirmed), 0) }

	_, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-mixed-race",
		Postings: []wallet.Posting{
			{Account: confirmed, Amount: amount(-5000, eur)},
			{Account: house, Amount: amount(-1000, eur)},
			{Account: reserved, Amount: amount(6000, eur)},
		},
	})
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("Post error = %v, want one wrapping %v", err, wallet.ErrInsufficientFunds)
	}
	if got := fake.balanceOf(string(confirmed)); got != 0 {
		t.Errorf("the member's account holds %d after a refusal, want 0 - a refusal moves nothing", got)
	}
	if got := fake.balanceOf(string(reserved)); got != 0 {
		t.Errorf("the reserved bucket holds %d after a refusal, want 0", got)
	}
}

// TestPostJudgesEveryMemberAccountItTouches keeps this ledger's own
// pre-flight check the same question the in-memory reference asks. A member
// account already holding less than nothing is not made lawful by a credit
// too small to clear it, and an implementation that judged only the givers
// would accept a transfer the other two refuse.
func TestPostJudgesEveryMemberAccountItTouches(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)
	// Only a ledger this package did not write could leave it here, which
	// is why the balance is seeded rather than posted.
	fake.seed(string(holder), -500)

	_, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-still-short",
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-100, eur)},
			{Account: holder, Amount: amount(100, eur)},
		},
	})
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("Post error = %v, want one wrapping %v", err, wallet.ErrInsufficientFunds)
	}
}

// TestPostWaitsUntilTheTransferIsReadable holds the port's read-your-writes
// promise over a substrate that queues by default. A wallet recomputed
// straight after a transition must not show the total from before it.
func TestPostWaitsUntilTheTransferIsReadable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		settleAfter int
		wantErr     bool
	}{
		{name: "the ledger applies it inline", settleAfter: 1},
		{name: "the ledger applies it after a moment", settleAfter: 3},
		{name: "the ledger never applies it", settleAfter: 0, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeBlnk(t)
			// A create that answers QUEUED is the shape of a ledger that
			// took the transaction but has not applied it yet.
			fake.createdStatus = statusQueued
			fake.settleAfter = tc.settleAfter
			ledger := newLedger(t, fake, blnk.WithSettleWindow(300*time.Millisecond, time.Millisecond))

			house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
			holder := ensure(t, ledger, member(wallet.StagePending), eur)

			_, err := ledger.Post(context.Background(), wallet.Transfer{
				IdempotencyKey: "key-settle",
				Postings: []wallet.Posting{
					{Account: house, Amount: amount(-2500, eur)},
					{Account: holder, Amount: amount(2500, eur)},
				},
			})
			switch {
			case tc.wantErr && err == nil:
				t.Fatal("Post returned while the transfer was still not readable")
			case tc.wantErr:
				if !strings.Contains(err.Error(), "readable") {
					t.Errorf("the refusal does not say what is wrong: %v", err)
				}
			case err != nil:
				t.Fatalf("posting: %v", err)
			}
		})
	}
}

// TestPostRefusesWhatThePortRefuses proves the port's own validation runs
// first, before any I/O. It is checked twice by design - here and in the
// ledger - and a transfer that would create or destroy money must never
// leave the process.
func TestPostRefusesWhatThePortRefuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		transfer func(house, holder wallet.LedgerAccountID) wallet.Transfer
		want     error
	}{
		{
			name: "no idempotency key",
			transfer: func(house, holder wallet.LedgerAccountID) wallet.Transfer {
				return wallet.Transfer{Postings: []wallet.Posting{
					{Account: house, Amount: amount(-100, eur)},
					{Account: holder, Amount: amount(100, eur)},
				}}
			},
			want: wallet.ErrMissingIdempotencyKey,
		},
		{
			name: "one posting",
			transfer: func(house, _ wallet.LedgerAccountID) wallet.Transfer {
				return wallet.Transfer{IdempotencyKey: "k", Postings: []wallet.Posting{
					{Account: house, Amount: amount(-100, eur)},
				}}
			},
			want: wallet.ErrTooFewPostings,
		},
		{
			name: "postings that do not sum to zero",
			transfer: func(house, holder wallet.LedgerAccountID) wallet.Transfer {
				return wallet.Transfer{IdempotencyKey: "k", Postings: []wallet.Posting{
					{Account: house, Amount: amount(-100, eur)},
					{Account: holder, Amount: amount(90, eur)},
				}}
			},
			want: wallet.ErrUnbalanced,
		},
		{
			name: "a transfer that moves nothing",
			transfer: func(house, _ wallet.LedgerAccountID) wallet.Transfer {
				return wallet.Transfer{IdempotencyKey: "k", Postings: []wallet.Posting{
					{Account: house, Amount: amount(-100, eur)},
					{Account: house, Amount: amount(100, eur)},
				}}
			},
			want: wallet.ErrNoMovement,
		},
		{
			name: "a posting copied out of history",
			transfer: func(house, holder wallet.LedgerAccountID) wallet.Transfer {
				return wallet.Transfer{IdempotencyKey: "k", Postings: []wallet.Posting{
					{Account: house, Amount: amount(-100, eur), TransferRef: "txn_1"},
					{Account: holder, Amount: amount(100, eur)},
				}}
			},
			want: wallet.ErrRecycledPosting,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeBlnk(t)
			ledger := newLedger(t, fake)
			house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
			holder := ensure(t, ledger, member(wallet.StagePending), eur)

			_, err := ledger.Post(context.Background(), tc.transfer(house, holder))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Post error = %v, want one wrapping %v", err, tc.want)
			}
			if got := len(fake.createdRequests()); got != 0 {
				t.Errorf("%d transaction(s) were sent for a transfer the port refuses before any I/O", got)
			}
		})
	}
}

// TestBalanceAnswersWhatTheLedgerHolds covers the read side's refusals
// beside its ordinary answer.
func TestBalanceAnswersWhatTheLedgerHolds(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)

	empty, err := ledger.Balance(context.Background(), holder, eur)
	if err != nil {
		t.Fatalf("reading an empty account: %v", err)
	}
	if !empty.Equal(amount(0, eur)) {
		t.Errorf("an account with no postings holds %s, want %s", empty, amount(0, eur))
	}

	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-balance",
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-2500, eur)},
			{Account: holder, Amount: amount(2500, eur)},
		},
	}); err != nil {
		t.Fatalf("posting: %v", err)
	}

	for id, want := range map[wallet.LedgerAccountID]money.Amount{house: amount(-2500, eur), holder: amount(2500, eur)} {
		got, err := ledger.Balance(context.Background(), id, eur)
		if err != nil {
			t.Fatalf("reading %q: %v", id, err)
		}
		if !got.Equal(want) {
			t.Errorf("account %q holds %s, want %s", id, got, want)
		}
	}

	if _, err := ledger.Balance(context.Background(), holder, gbp); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("asking for the wrong currency gave %v, want one wrapping %v", err, money.ErrCurrencyMismatch)
	}
	if _, err := ledger.Balance(context.Background(), "bln_never-issued", eur); !errors.Is(err, wallet.ErrUnknownAccount) {
		t.Errorf("asking about an account nobody issued gave %v, want one wrapping %v", err, wallet.ErrUnknownAccount)
	}
}

// TestBalanceRefusesAnAccountThisPortDidNotCreate keeps the port's promise
// exact. A name is unique across the whole Blnk server rather than within a
// ledger, so a neighbour's balance is one lookup away at all times -
// including one holding real money. Answering for it would let money be
// posted where this port's rules - which account may go negative, above all
// - were never applied.
//
// The judgement is made by reading the name, so it costs no round trip and
// holds for a name whose balance exists as well as for one whose does not.
func TestBalanceRefusesAnAccountThisPortDidNotCreate(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	// A funded balance, named the way something else on this server would
	// name one.
	fake.seed("@somebody-elses-account.EUR", 999999)

	for _, id := range []wallet.LedgerAccountID{
		"@somebody-elses-account.EUR",
		"bln_never-issued",
		"",
	} {
		if _, err := ledger.Balance(context.Background(), id, eur); !errors.Is(err, wallet.ErrUnknownAccount) {
			t.Errorf("Balance over %q gave %v, want one wrapping %v", id, err, wallet.ErrUnknownAccount)
		}
	}
}

// TestHistoryStreamsThePostingsOnAnAccount covers the ordinary read, the
// window, and the two refusals History makes before the iterator exists.
func TestHistoryStreamsThePostingsOnAnAccount(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)

	var refs []wallet.TransferRef
	for i, minor := range []int64{2500, 700, 100} {
		ref, err := ledger.Post(context.Background(), wallet.Transfer{
			IdempotencyKey: fmt.Sprintf("key-history-%d", i),
			Postings: []wallet.Posting{
				{Account: house, Amount: amount(-minor, eur)},
				{Account: holder, Amount: amount(minor, eur)},
			},
		})
		if err != nil {
			t.Fatalf("posting %d: %v", i, err)
		}
		refs = append(refs, ref)
	}

	postings := readHistory(t, ledger, holder, wallet.Window{})
	if len(postings) != 3 {
		t.Fatalf("the account has %d posting(s), want 3", len(postings))
	}
	for i, want := range []int64{2500, 700, 100} {
		if postings[i].Amount.Minor != want {
			t.Errorf("posting %d moves %d, want %d", i+1, postings[i].Amount.Minor, want)
		}
		if postings[i].TransferRef != refs[i] {
			t.Errorf("posting %d carries reference %q, want %q", i+1, postings[i].TransferRef, refs[i])
		}
		if postings[i].Account != holder {
			t.Errorf("posting %d names account %q, want %q", i+1, postings[i].Account, holder)
		}
		if postings[i].PostedAt.IsZero() {
			t.Errorf("posting %d carries no instant", i+1)
		}
	}
	if !postings[0].PostedAt.After(time.Time{}) || postings[2].PostedAt.Before(postings[0].PostedAt) {
		t.Errorf("history is not ordered by ascending instant: %v", postings)
	}

	// The house account saw the other side of every one of them.
	for _, p := range readHistory(t, ledger, house, wallet.Window{}) {
		if !p.Amount.IsNegative() {
			t.Errorf("the giving account's posting %s is not negative", p.Amount)
		}
	}

	// A window that starts after the second posting takes the third alone.
	window := wallet.Window{From: postings[2].PostedAt}
	if got := readHistory(t, ledger, holder, window); len(got) != 1 || got[0].Amount.Minor != 100 {
		t.Errorf("the window from the last posting yielded %v, want just the 100", got)
	}
	// And one that ends at it takes the first two.
	if got := readHistory(t, ledger, holder, wallet.Window{To: postings[2].PostedAt}); len(got) != 2 {
		t.Errorf("the window ending at the last posting yielded %d posting(s), want 2", len(got))
	}

	if _, err := ledger.History(context.Background(), "bln_never-issued", wallet.Window{}); !errors.Is(err, wallet.ErrUnknownAccount) {
		t.Errorf("History over an account nobody issued gave %v, want one wrapping %v", err, wallet.ErrUnknownAccount)
	}
	swapped := wallet.Window{From: time.Now(), To: time.Now().Add(-time.Hour)}
	if _, err := ledger.History(context.Background(), holder, swapped); !errors.Is(err, wallet.ErrInvalidWindow) {
		t.Errorf("History over swapped bounds gave %v, want one wrapping %v", err, wallet.ErrInvalidWindow)
	}
}

// TestHistoryReadsTheShapesOnlyTheLedgerProduces covers what History makes
// of records this adapter did not write: a split, which the ledger keeps as
// children and nothing else, a transaction that never applied, and a
// transaction carrying none of this package's annotations.
//
// The split is the case that matters. Its scalar side is named by EVERY
// child, once for each child's own share, so a reader that took each row it
// was handed as a posting of its own would report that account as having
// given several times - and a reader that looked for a parent row to read
// instead would find nothing at all, because the ledger never wrote one.
func TestHistoryReadsTheShapesOnlyTheLedgerProduces(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	rounding := ensure(t, ledger, wallet.HouseAccount("rounding"), eur)
	at := time.Now().UTC()

	// A split of 1000 out of the house account into 900 for the member and
	// 100 for the rounding remainder: two children pointing at a parent
	// that is not a row, each naming the house account for its own share.
	fake.record(&txnRow{
		id: "txn_child_a", parent: "txn_parent", reference: "key-split-1", currency: "EUR", precise: big.NewInt(900),
		source: string(house), dest: string(holder), createdAt: at,
	})
	fake.record(&txnRow{
		id: "txn_child_b", parent: "txn_parent", reference: "key-split-2", currency: "EUR", precise: big.NewInt(100),
		source: string(house), dest: string(rounding), createdAt: at.Add(time.Microsecond),
	})
	// A transaction that never applied moved no balance, so it is no
	// posting.
	fake.record(&txnRow{
		id: "txn_queued", reference: "key-queued", currency: "EUR", precise: big.NewInt(1),
		source: string(house), dest: string(holder), status: statusQueued,
		createdAt: at.Add(2 * time.Millisecond),
	})
	// An unannotated transfer this package did not write: there is nothing
	// to read but what the ledger recorded.
	fake.record(&txnRow{
		id: "txn_foreign", reference: "key-foreign", currency: "EUR", precise: big.NewInt(50),
		source: string(holder), dest: string(house),
		createdAt: at.Add(3 * time.Millisecond),
	})

	got := readHistory(t, ledger, holder, wallet.Window{})
	if len(got) != 2 {
		t.Fatalf("the account has %d posting(s), want 2 - the queued transaction is not one: %v", len(got), got)
	}
	if got[0].TransferRef != "txn_parent" {
		t.Errorf("a split leg's posting carries reference %q, want the transfer it belongs to, %q", got[0].TransferRef, "txn_parent")
	}
	if got[0].Amount.Minor != 900 {
		t.Errorf("the split leg moved %d, want 900", got[0].Amount.Minor)
	}
	if !got[0].PostedAt.Equal(at) {
		t.Errorf("the split's instant is %s, want the earliest of its children's, %s", got[0].PostedAt, at)
	}
	if got[1].Amount.Minor != -50 || got[1].TransferRef != "txn_foreign" {
		t.Errorf("the unannotated transfer read as %+v, want -50 under txn_foreign", got[1])
	}

	// The account that travelled as the split's scalar end is named by both
	// children. It gave 1000 once, under one reference.
	scalar := readHistory(t, ledger, house, wallet.Window{})
	var total int64
	var refs int
	for _, p := range scalar {
		if p.TransferRef == "txn_parent" {
			total += p.Amount.Minor
			refs++
		}
	}
	if total != -1000 || refs != 1 {
		t.Errorf("the split's scalar side reads as %d posting(s) summing to %d, want 1 summing to -1000; the children are one transfer", refs, total)
	}
}

// TestHistoryRefusesASplitTheLedgerOnlyHalfRecorded is the loud failure
// that keeps a silent one away. A split is recorded a child at a time, so a
// failure part-way leaves some of the transfer's legs on the ledger and the
// rest nowhere - and the transfer's own annotation, copied onto every
// child, still says what the whole of it was meant to move.
//
// The two are cross-checked for exactly this reason. Reporting the
// annotation would report money that did not move; reporting the rows would
// report a transfer the caller never wrote, under a reference the domain
// stored as proof of the whole (D7). Neither is the truth, so neither is
// answered with.
func TestHistoryRefusesASplitTheLedgerOnlyHalfRecorded(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	rounding := ensure(t, ledger, wallet.HouseAccount("rounding"), eur)

	// The house account gave 1000 according to the document, and only the
	// 900 leg reached the ledger.
	document := annotation(t, wallet.Transfer{
		Reference: "entry-half",
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-1000, eur)},
			{Account: holder, Amount: amount(999, eur)},
			{Account: rounding, Amount: amount(1, eur)},
		},
	})
	fake.record(&txnRow{
		id: "txn_half", parent: "txn_parent", reference: "key-half-1", currency: "EUR", precise: big.NewInt(999),
		source: string(house), dest: string(holder), createdAt: time.Now().UTC(),
		metadata: map[string]any{"wallet_transfer": document},
	})

	_, err := collectHistory(ledger, house, wallet.Window{})
	if err == nil {
		t.Fatal("a split the ledger recorded half of was answered for anyway")
	}
	if !strings.Contains(err.Error(), "txn_parent") {
		t.Errorf("the refusal does not name the transfer it could not answer for: %v", err)
	}
}

// annotation renders the identity document this package records on the
// transactions it creates, so a test can put one on a row the adapter did
// not write. It is built here rather than exported from the package,
// because a test that reached into the adapter to build it would be
// checking the adapter against itself.
func annotation(t *testing.T, transfer wallet.Transfer) string {
	t.Helper()
	type posting struct {
		Account  string `json:"account"`
		Minor    int64  `json:"minor"`
		Currency string `json:"currency"`
	}
	document := struct {
		Reference string            `json:"reference"`
		Metadata  map[string]string `json:"metadata,omitempty"`
		Postings  []posting         `json:"postings"`
	}{Reference: transfer.Reference, Metadata: transfer.Metadata}
	for _, p := range transfer.Postings {
		document.Postings = append(document.Postings, posting{
			Account:  string(p.Account),
			Minor:    p.Amount.Minor,
			Currency: string(p.Amount.Currency),
		})
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("rendering an identity document: %v", err)
	}
	return string(raw)
}

// TestHistoryReadsEitherShapeOfAPage keeps the read side working over the
// shapes a filtered page arrives in - rows wrapped in an envelope, rows on
// their own, and the two ways a page with nothing in it is written. An
// empty history is an ordinary answer and must never read as a failure.
func TestHistoryReadsEitherShapeOfAPage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		shape string
	}{
		{name: "rows wrapped in an envelope", shape: shapeEnvelope},
		{name: "rows on their own", shape: shapeBareArray},
		{name: "an empty page written as a null", shape: shapeNullPage},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeBlnk(t)
			fake.filterShape = tc.shape
			ledger := newLedger(t, fake)
			house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
			holder := ensure(t, ledger, member(wallet.StagePending), eur)
			untouched := ensure(t, ledger, member(wallet.StageHeld), eur)

			if _, err := ledger.Post(context.Background(), wallet.Transfer{
				IdempotencyKey: "key-shape",
				Postings: []wallet.Posting{
					{Account: house, Amount: amount(-2500, eur)},
					{Account: holder, Amount: amount(2500, eur)},
				},
			}); err != nil {
				t.Fatalf("posting: %v", err)
			}

			if got := readHistory(t, ledger, holder, wallet.Window{}); len(got) != 1 {
				t.Errorf("the account has %d posting(s), want 1", len(got))
			}
			if got := readHistory(t, ledger, untouched, wallet.Window{}); len(got) != 0 {
				t.Errorf("an account nothing was posted to has %d posting(s), want none", len(got))
			}
		})
	}
}

// TestHistoryPagesThroughALongRecord proves the read does not stop at the
// first page. A member's history outgrows one page quickly, and a wallet
// that silently showed the first two hundred postings would understate
// every long-standing member's total.
func TestHistoryPagesThroughALongRecord(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)

	const postings = 451
	at := time.Now().UTC()
	for i := range postings {
		fake.record(&txnRow{
			reference: fmt.Sprintf("key-page-%d", i),
			currency:  "EUR",
			precise:   big.NewInt(1),
			source:    string(house),
			dest:      string(holder),
			createdAt: at.Add(time.Duration(i) * time.Millisecond),
		})
	}

	got := readHistory(t, ledger, holder, wallet.Window{})
	if len(got) != postings {
		t.Fatalf("the account has %d posting(s), want %d", len(got), postings)
	}
	for i := 1; i < len(got); i++ {
		if got[i].PostedAt.Before(got[i-1].PostedAt) {
			t.Fatalf("posting %d is older than the one before it; history must ascend", i)
		}
	}
}

// TestHistoryIsLazyAndStopsWhenTheReaderDoes keeps the iterator's two
// promises: nothing is fetched until iteration begins, and a reader that
// stops early reads nothing further.
func TestHistoryIsLazyAndStopsWhenTheReaderDoes(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)
	for i := range 3 {
		if _, err := ledger.Post(context.Background(), wallet.Transfer{
			IdempotencyKey: fmt.Sprintf("key-lazy-%d", i),
			Postings: []wallet.Posting{
				{Account: house, Amount: amount(-100, eur)},
				{Account: holder, Amount: amount(100, eur)},
			},
		}); err != nil {
			t.Fatalf("posting %d: %v", i, err)
		}
	}

	before := fake.calls("POST /transactions/filter")
	postings, err := ledger.History(context.Background(), holder, wallet.Window{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if after := fake.calls("POST /transactions/filter"); after != before {
		t.Errorf("History fetched %d page(s) before iteration began; the iterator is meant to be lazy", after-before)
	}

	seen := 0
	for _, err := range postings {
		if err != nil {
			t.Fatalf("streaming: %v", err)
		}
		seen++
		break
	}
	if seen != 1 {
		t.Errorf("a reader that stopped after one posting saw %d", seen)
	}
}

// TestHistoryEndsOnACancelledContext holds the streaming half of the
// contract: a failure mid-stream arrives as the pair's error and iteration
// ends.
func TestHistoryEndsOnACancelledContext(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)
	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-cancel",
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-100, eur)},
			{Account: holder, Amount: amount(100, eur)},
		},
	}); err != nil {
		t.Fatalf("posting: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	postings, err := ledger.History(ctx, holder, wallet.Window{})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	cancel()

	var streamed error
	for _, err := range postings {
		streamed = err
		break
	}
	if !errors.Is(streamed, context.Canceled) {
		t.Fatalf("the cancelled stream yielded %v, want one wrapping %v", streamed, context.Canceled)
	}
}

// TestPostAnnotatesTheTransferForAnAuditor checks what a human reading the
// ledger directly sees: the domain record that caused the movement, and the
// document replay identity is judged by.
func TestPostAnnotatesTheTransferForAnAuditor(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)

	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-annotated",
		Reference:      "entry-transition-42",
		Metadata:       map[string]string{"network": "fixture"},
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-2500, eur)},
			{Account: holder, Amount: amount(2500, eur)},
		},
	}); err != nil {
		t.Fatalf("posting: %v", err)
	}

	got := fake.onlyCreate()
	if got.txn.Description != "entry-transition-42" {
		t.Errorf("the transaction is described as %q, want the domain record that caused it", got.txn.Description)
	}
	raw, ok := got.txn.MetaData["wallet_transfer"].(string)
	if !ok {
		t.Fatalf("the transaction carries no identity document: %v", got.txn.MetaData)
	}
	var recorded struct {
		Reference string            `json:"reference"`
		Metadata  map[string]string `json:"metadata"`
		Postings  []struct {
			Account  string `json:"account"`
			Minor    int64  `json:"minor"`
			Currency string `json:"currency"`
		} `json:"postings"`
	}
	if err := json.Unmarshal([]byte(raw), &recorded); err != nil {
		t.Fatalf("the identity document does not decode: %v", err)
	}
	if recorded.Reference != "entry-transition-42" || recorded.Metadata["network"] != "fixture" {
		t.Errorf("the identity document lost the transfer's annotations: %+v", recorded)
	}
	if len(recorded.Postings) != 2 {
		t.Fatalf("the identity document holds %d posting(s), want 2", len(recorded.Postings))
	}
	// Canonical order: the account is what sorts first, so two spellings
	// of one transfer record the same document.
	if recorded.Postings[0].Account > recorded.Postings[1].Account {
		t.Errorf("the identity document is not in canonical order: %+v", recorded.Postings)
	}
}

// readHistory drains an account's history or fails the test.
func readHistory(t *testing.T, ledger *blnk.Ledger, id wallet.LedgerAccountID, window wallet.Window) []wallet.Posting {
	t.Helper()
	got, err := collectHistory(ledger, id, window)
	if err != nil {
		t.Fatalf("reading the history of %q: %v", id, err)
	}
	return got
}

// collectHistory drains an account's history, returning the first failure.
func collectHistory(ledger *blnk.Ledger, id wallet.LedgerAccountID, window wallet.Window) ([]wallet.Posting, error) {
	postings, err := ledger.History(context.Background(), id, window)
	if err != nil {
		return nil, err
	}
	var out []wallet.Posting
	for posting, err := range postings {
		if err != nil {
			return out, err
		}
		out = append(out, posting)
	}
	return out, nil
}

// TestPostLeavesARefusedKeyFree is the port's promise that a refused Post
// leaves the key exactly as it found it. A transaction the ledger accepts
// the request for and then declines to apply moved no balance, so the key
// it carries has recorded nothing - and because keys are derived from the
// domain fact that caused the transfer (D8), a caller that cannot re-use
// one cannot mint another either: the movement is stuck forever.
//
// Whether Blnk frees the reference a refused transaction holds is not
// settled here, so both answers are held to: where it does, the corrected
// retry records; where it does not, the refusal says so in a way no caller
// could read as "some other transfer owns this key".
func TestPostLeavesARefusedKeyFree(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		frees bool
	}{
		{name: "the ledger frees a refused reference", frees: true},
		{name: "the ledger keeps a refused reference", frees: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeBlnk(t)
			fake.freesRejectedReference = tc.frees
			fake.createdStatus = statusRejected
			ledger := newLedger(t, fake, blnk.WithSettleWindow(200*time.Millisecond, time.Millisecond))
			giver := ensure(t, ledger, member(wallet.StageConfirmed), eur)
			taker := ensure(t, ledger, member(wallet.StageReserved), eur)
			fake.seed(string(giver), 5000)
			// The balance this package read said there was enough; the
			// ledger, a moment later, said there was not.
			fake.beforeCreate = func() { fake.seed(string(giver), 0) }

			transfer := wallet.Transfer{
				IdempotencyKey: "key-refused",
				Postings: []wallet.Posting{
					{Account: giver, Amount: amount(-5000, eur)},
					{Account: taker, Amount: amount(5000, eur)},
				},
			}
			_, err := ledger.Post(context.Background(), transfer)
			if !errors.Is(err, wallet.ErrInsufficientFunds) {
				t.Fatalf("a transfer the ledger refused for want of money gave %v, want one wrapping %v", err, wallet.ErrInsufficientFunds)
			}
			if got := fake.balanceOf(string(taker)); got != 0 {
				t.Errorf("the receiving account holds %d after a refusal, want 0", got)
			}

			fake.beforeCreate = nil
			fake.createdStatus = statusApplied
			fake.seed(string(giver), 5000)
			_, err = ledger.Post(context.Background(), transfer)
			switch {
			case tc.frees && err != nil:
				t.Fatalf("re-posting a refused transfer under its own key: %v", err)
			case tc.frees:
				if got := fake.balanceOf(string(taker)); got != 5000 {
					t.Errorf("the receiving account holds %d after the retry, want 5000", got)
				}
			case err == nil:
				t.Fatal("the ledger holds the reference, so the retry cannot have recorded, yet Post answered as though it had")
			default:
				if errors.Is(err, wallet.ErrIdempotencyConflict) {
					t.Errorf("a key held by a refused transaction was reported as a conflict, which would send a caller to mint a fresh key for money it may already owe: %v", err)
				}
				if !strings.Contains(err.Error(), "key-refused") {
					t.Errorf("the refusal does not name the key that is stuck: %v", err)
				}
			}
		})
	}
}

// TestPostAnswersARefusalWithNoReasonAsItself keeps the mapping honest in
// the other direction. A transaction the ledger declined for a reason this
// port has no word for must not be dressed up as one it does: reporting an
// unexplained refusal as "the member cannot afford this" would have the
// withdrawal flow deny a member for what was never about their balance.
func TestPostAnswersARefusalWithNoReasonAsItself(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	fake.createdStatus = statusRejected
	ledger := newLedger(t, fake, blnk.WithSettleWindow(200*time.Millisecond, time.Millisecond))
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)

	_, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-refused-unexplained",
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-2500, eur)},
			{Account: holder, Amount: amount(2500, eur)},
		},
	})
	if err == nil {
		t.Fatal("a transfer the ledger refused to apply was answered as recorded")
	}
	if errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Errorf("a refusal that is not about money was reported as one that is: %v", err)
	}
	if !strings.Contains(err.Error(), statusRejected) {
		t.Errorf("the refusal does not say what the ledger did: %v", err)
	}
}

// TestPostDoesNotReadAnOutageAsAnEmptyBalance is the error-mapping table
// for the one refusal a caller must not retry. ErrInsufficientFunds is
// terminal to the withdrawal flow - the member does not have the money - so
// a transport failure, a proxy's refusal or a gateway page reported as one
// turns a retryable outage into a member-facing denial.
func TestPostDoesNotReadAnOutageAsAnEmptyBalance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		refusal wireRefusal
		want    error
	}{
		{
			name:    "the ledger refuses for want of money",
			refusal: wireRefusal{status: 400, body: `{"error":"insufficient funds in balance","error_detail":{"code":"TXN_INSUFFICIENT_FUNDS","message":"insufficient funds in balance"}}`},
			want:    wallet.ErrInsufficientFunds,
		},
		{
			name:    "an older ledger refuses with a message and no code",
			refusal: wireRefusal{status: 400, body: `{"error":"insufficient funds in balance bln_1"}`},
			want:    wallet.ErrInsufficientFunds,
		},
		{
			name:    "an authenticating proxy refuses the credential",
			refusal: wireRefusal{status: 403, body: `{"error":"insufficient_scope"}`},
		},
		{
			name:    "a gateway runs out of disk",
			refusal: wireRefusal{status: 507, body: `{"error":"Insufficient Storage"}`},
		},
		{
			name:    "a validation failure echoes the request back",
			refusal: wireRefusal{status: 400, body: `{"error":"unexpected field","error_detail":{"code":"TXN_INVALID","message":"unexpected field: allow_overdraft"}}`},
		},
		{
			name:    "the ledger is unwell",
			refusal: wireRefusal{status: 500, body: `{"error":"internal error"}`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeBlnk(t)
			ledger := newLedger(t, fake)
			house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
			holder := ensure(t, ledger, member(wallet.StagePending), eur)
			refusal := tc.refusal
			fake.refusal = &refusal

			_, err := ledger.Post(context.Background(), wallet.Transfer{
				IdempotencyKey: "key-outage",
				Postings: []wallet.Posting{
					{Account: house, Amount: amount(-2500, eur)},
					{Account: holder, Amount: amount(2500, eur)},
				},
			})
			if err == nil {
				t.Fatal("a refused transfer was answered as recorded")
			}
			switch {
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("Post error = %v, want one wrapping %v", err, tc.want)
			case tc.want == nil && errors.Is(err, wallet.ErrInsufficientFunds):
				t.Fatalf("a failure that is not about money was reported to the domain as an empty balance: %v", err)
			}
		})
	}
}

// TestPostReadsADuplicateReferenceByItsCode keeps the replay branch working
// over a server that answers a duplicate reference with something other
// than a conflict status. Blnk has no unique index on the column and
// refuses duplicates in application code (spike S2), so the status is that
// code's choice - and a caller that lost the race must learn the winner's
// reference either way.
func TestPostReadsADuplicateReferenceByItsCode(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)

	transfer := wallet.Transfer{
		IdempotencyKey: "key-duplicate-code",
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-2500, eur)},
			{Account: holder, Amount: amount(2500, eur)},
		},
	}
	first, err := ledger.Post(context.Background(), transfer)
	if err != nil {
		t.Fatalf("posting: %v", err)
	}

	// The ledger now refuses the duplicate with a 400 carrying the code
	// rather than with a 409.
	fake.refusal = &wireRefusal{
		status: 400,
		body:   `{"error":"reference already exists","error_detail":{"code":"TXN_DUPLICATE_REFERENCE","message":"reference already exists"}}`,
	}
	again, err := newLedger(t, fake).Post(context.Background(), transfer)
	if err != nil {
		t.Fatalf("a duplicate reference refused by code was not read as a replay: %v", err)
	}
	if again != first {
		t.Errorf("the replay answered %q, want the winner's reference %q", again, first)
	}
}

// TestHistoryReadsBackEverySideOfASplit is the read-back the fake exists to
// make possible. Every split shape this ledger can record is posted through
// Post and read back on every account it names, because a history that
// silently omitted a leg - or counted one twice - would misreport a
// member's total while Balance, which is the ledger's own figure, went on
// being right.
//
// The shape with several accounts on both sides is not here because it is
// not one this ledger can record; it has its own test, which is that it is
// refused.
func TestHistoryReadsBackEverySideOfASplit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		wants map[string]int64
	}{
		{name: "one account gives and two receive", wants: map[string]int64{"house": -1000, "holder": 999, "rounding": 1}},
		{name: "two accounts give and one receives", wants: map[string]int64{"house": -600, "float": -400, "holder": 1000}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeBlnk(t)
			ledger := newLedger(t, fake)
			ids := map[string]wallet.LedgerAccountID{
				"house":    ensure(t, ledger, wallet.HouseAccount("commission"), eur),
				"float":    ensure(t, ledger, wallet.HouseAccount("float"), eur),
				"rounding": ensure(t, ledger, wallet.HouseAccount("rounding"), eur),
				"holder":   ensure(t, ledger, member(wallet.StagePending), eur),
			}

			postings := make([]wallet.Posting, 0, len(tc.wants))
			for name, minor := range tc.wants {
				postings = append(postings, wallet.Posting{Account: ids[name], Amount: amount(minor, eur)})
			}
			ref, err := ledger.Post(context.Background(), wallet.Transfer{
				IdempotencyKey: "key-split-" + tc.name,
				Postings:       postings,
			})
			if err != nil {
				t.Fatalf("posting: %v", err)
			}

			for name, want := range tc.wants {
				got := readHistory(t, ledger, ids[name], wallet.Window{})
				if len(got) != 1 {
					t.Errorf("account %q has %d posting(s) from one split transfer, want 1: %v", name, len(got), got)
					continue
				}
				if got[0].Amount.Minor != want {
					t.Errorf("account %q recorded %d, want %d", name, got[0].Amount.Minor, want)
				}
				if got[0].TransferRef != ref {
					t.Errorf("account %q recorded reference %q, want the transfer's own %q", name, got[0].TransferRef, ref)
				}
			}
			// And the balances agree with the history, to the minor unit.
			for name, want := range tc.wants {
				if got := fake.balanceOf(string(ids[name])); got != want {
					t.Errorf("account %q holds %d, want %d", name, got, want)
				}
			}
		})
	}
}

// TestHistoryYieldsThePostingsTheCallerWrote holds this ledger to the same
// posting granularity as the in-memory reference and the Postgres ledger,
// which the shared conformance suite measures all three by. A Blnk
// transaction has one leg per account per side, so the wire cannot answer
// for a transfer that named an account twice - the document this package
// records on the transaction can, and does.
func TestHistoryYieldsThePostingsTheCallerWrote(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)

	ref, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-twice",
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-100, eur)},
			{Account: house, Amount: amount(-200, eur)},
			{Account: holder, Amount: amount(300, eur)},
		},
	})
	if err != nil {
		t.Fatalf("posting: %v", err)
	}

	got := readHistory(t, ledger, house, wallet.Window{})
	if len(got) != 2 {
		t.Fatalf("the account has %d posting(s), want the 2 the caller wrote: %v", len(got), got)
	}
	for i, want := range []int64{-100, -200} {
		if got[i].Amount.Minor != want {
			t.Errorf("posting %d moves %d, want %d", i+1, got[i].Amount.Minor, want)
		}
		if got[i].TransferRef != ref {
			t.Errorf("posting %d carries reference %q, want %q", i+1, got[i].TransferRef, ref)
		}
	}
	if taken := readHistory(t, ledger, holder, wallet.Window{}); len(taken) != 1 || taken[0].Amount.Minor != 300 {
		t.Errorf("the receiving account read as %v, want one posting of 300", taken)
	}
}

// TestHistoryOmitsAnAccountWhosePostingsCancel documents the one divergence
// from the in-memory reference this ledger cannot close. An account whose
// postings cancel inside a transfer is no leg of the transaction, so
// nothing on the ledger names it and no filter can find it; the in-memory
// and Postgres ledgers keep both postings. Balances still agree everywhere,
// and the port permits the shape, so this is written down rather than
// worked around with a leg that moves nothing.
func TestHistoryOmitsAnAccountWhosePostingsCancel(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)
	cancels := ensure(t, ledger, member(wallet.StageHeld), eur)
	fake.seed(string(cancels), 500)

	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-cancels",
		Postings: []wallet.Posting{
			{Account: cancels, Amount: amount(-500, eur)},
			{Account: cancels, Amount: amount(500, eur)},
			{Account: house, Amount: amount(-300, eur)},
			{Account: holder, Amount: amount(300, eur)},
		},
	}); err != nil {
		t.Fatalf("posting: %v", err)
	}

	if got := readHistory(t, ledger, cancels, wallet.Window{}); len(got) != 0 {
		t.Errorf("an account whose postings cancel has %d posting(s) here; the divergence is that it has none: %v", len(got), got)
	}
	if got := readHistory(t, ledger, holder, wallet.Window{}); len(got) != 1 {
		t.Errorf("the account that did move has %d posting(s), want 1", len(got))
	}
}

// TestHistoryPagesThroughAServerThatCapsItsPages keeps a member's record
// whole over a server that answers with fewer rows than were asked for. A
// page cap is an ordinary default, and a reader that treated a short page
// as the last one would silently drop everything past the first - while
// Balance, which is the ledger's own figure, went on being right.
func TestHistoryPagesThroughAServerThatCapsItsPages(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	fake.pageCap = 37
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)

	const postings = 451
	at := time.Now().UTC()
	for i := range postings {
		fake.record(&txnRow{
			reference: fmt.Sprintf("key-capped-%d", i),
			currency:  "EUR",
			precise:   big.NewInt(1),
			source:    string(house),
			dest:      string(holder),
			createdAt: at.Add(time.Duration(i) * time.Millisecond),
		})
	}

	if got := readHistory(t, ledger, holder, wallet.Window{}); len(got) != postings {
		t.Fatalf("the account has %d posting(s), want %d; a capped page is not the last page", len(got), postings)
	}
}

// TestHistoryRefusesAPageItCannotRecognise is the loud failure that stops a
// member's money being reported as absent. The shape of a filtered page is
// one of the things only a real server settles; a page arriving under a key
// this package does not read must be an error, because "no rows here" and
// "the rows are somewhere else" are different sentences.
func TestHistoryRefusesAPageItCannotRecognise(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	fake.filterShape = shapeUnknownEnvelope
	ledger := newLedger(t, fake)
	house := ensure(t, ledger, wallet.HouseAccount("commission"), eur)
	holder := ensure(t, ledger, member(wallet.StagePending), eur)

	if _, err := ledger.Post(context.Background(), wallet.Transfer{
		IdempotencyKey: "key-unknown-page",
		Postings: []wallet.Posting{
			{Account: house, Amount: amount(-2500, eur)},
			{Account: holder, Amount: amount(2500, eur)},
		},
	}); err != nil {
		t.Fatalf("posting: %v", err)
	}

	got, err := collectHistory(ledger, holder, wallet.Window{})
	if err == nil {
		t.Fatalf("a page this package cannot read was answered as %d posting(s); a member's money would read as gone", len(got))
	}
	if !strings.Contains(err.Error(), "data") {
		t.Errorf("the refusal does not say what it looked for: %v", err)
	}
}

// TestEnsureAccountResolvesOneLedgerUnderConcurrency is the money-safety
// property behind every account name this package writes. The name carries
// the resolved ledger's id as its namespace, so two ids for one ledger name
// is one member holding two sets of accounts - and money posted through one
// is invisible to the other.
//
// The server does not stop this happening: blnk.ledgers constrains only the
// id, so concurrent creates of one name all succeed with different ids. The
// adapter has to be the thing that creates it once.
func TestEnsureAccountResolvesOneLedgerUnderConcurrency(t *testing.T) {
	t.Parallel()

	fake := newFakeBlnk(t)
	ledger := newLedger(t, fake)
	ref := member(wallet.StageConfirmed)

	const callers = 8
	ids := make([]wallet.LedgerAccountID, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ids[i], errs[i] = ledger.EnsureAccount(context.Background(), ref, eur)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: ensuring one account from %d at once: %v", i, callers, err)
		}
	}
	for i, id := range ids {
		if id != ids[0] {
			t.Errorf("caller %d resolved %q, caller 0 resolved %q; one reference must resolve to one account",
				i, id, ids[0])
		}
	}
	if made := fake.ledgersNamed(blnk.DefaultLedgerName); made != 1 {
		t.Errorf("%d ledgers carry the configured name, want exactly 1; each one is a namespace of its own", made)
	}
}
