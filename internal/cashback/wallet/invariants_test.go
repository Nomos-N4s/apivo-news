package wallet_test

// The C-1 zero-sum check, drilled from the wallet's side (T031).
//
// Two suites already drill the check itself. internal/platform/db proves
// cashback.ledger_zero_sum can see an imbalance in a stand-in ledger built
// for the purpose, and the schema suite beside the Postgres adapter proves
// both that the exit-route ledger's own triggers refuse illegal SQL and
// that the redirected view can see an imbalance staged there - but every
// row either suite reads or writes arrives in raw SQL, built for the
// drill. What neither asks is the question this package has to care about:
// is the invariant the wallet RELIES on - every currency sums to zero
// across the ledger the wallet posts to - enforced by the database the
// wallet actually writes through the port? A check watching a ledger
// nobody posts to and a ledger nobody checks would each pass their own
// suite and still leave the money unguarded in between.
//
// So this file closes that loop through the port. The Postgres wallet
// ledger creates the accounts and posts a real transfer; the zero-sum check
// is pointed at the schema those postings landed in (the redirect 0020
// provides and 0022 documents); and the check must read zero for the
// currency the wallet just moved - a row it can only have learned from the
// wallet's own rows. Then an imbalance is planted behind the wallet's back,
// as direct SQL into the ledger schema, the one writer the port cannot
// vet: the check must go red with the exact currency and hole, and the
// database itself must refuse the state at COMMIT by SQLSTATE. That last
// assertion is ADR-0002's mitigation made testable - C-1 is "a real SQL
// query over real rows" that fails loudly, not a Go function a different
// writer could simply not call.
//
// A second drill poses the subtler counterfeit: a transfer that nets to
// zero only ACROSS currencies. "Per currency" is the load-bearing phrase
// of C-1 - a judgement that summed a transfer without grouping would bless
// money minted in one currency and destroyed in another - and nothing else
// notices when the grouping goes. The schema drill's mixed-currency
// transfer is unbalanced globally too, so an ungrouped sum still refuses
// it there, and the port's Validate refuses a cross-currency offset before
// any I/O happens, so only SQL behind the port can even put the question
// to the database.
//
// Like the sibling suites this runs against a real Postgres, keyed on
// DATABASE_URL, over the pool the conformance TestMain opens. Each drill
// lives whole in one transaction that is never allowed to commit - the
// planted imbalance sees to that - so nothing survives a run.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/postgres"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// codeRaiseException is the SQLSTATE of RAISE EXCEPTION, which is how the
// ledger schema's deferred zero-sum trigger refuses a transfer at COMMIT
// (0022): the shape of refusal this drill requires from the database.
const codeRaiseException = "P0001"

// invariantCurrency is the currency these drills trade in. XTS is the
// ISO-4217 code reserved for testing and nothing else in this repository
// posts it, so a row for it in the zero-sum view can only have come from
// the rows this test wrote - which turns "the check sees the wallet's
// postings" from a hope into an assertion.
const invariantCurrency = money.Currency("XTS")

// crossCurrency is the cross-currency drill's second leg. XXX - ISO-4217's
// "no currency" - can no more belong to a deployment than XTS can, and
// nothing else in this repository posts it either, so the isolation
// argument above covers both codes.
const crossCurrency = money.Currency("XXX")

// requirePool skips when no database is available, in the same words the
// sibling suites use, and hands back the shared pool otherwise.
func requirePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if conformPool == nil {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set DATABASE_URL to exercise the postgres ledger")
	}
	return conformPool
}

func wantPgCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want SQLSTATE %s, but the database accepted the write", code)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("want *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != code {
		t.Fatalf("want SQLSTATE %s, got %s: %s", code, pgErr.Code, pgErr.Message)
	}
}

// wantC1Refusal requires that err is the zero-sum trigger speaking: the
// RAISE EXCEPTION SQLSTATE, in the trigger's own words. The SQLSTATE alone
// is the letter of Principle VIII, but P0001 is what every RAISE in the
// schema shares - the solvency trigger raises it today - so a drill that
// accepted any P0001 would stay green with the C-1 trigger gone the moment
// some other commit-path refusal happened to fire on its rows. The message
// check is what would notice that disappearance.
func wantC1Refusal(t *testing.T, err error) {
	t.Helper()
	wantPgCode(t, err, codeRaiseException)
	var pgErr *pgconn.PgError
	_ = errors.As(err, &pgErr)
	if !strings.Contains(pgErr.Message, "does not sum to zero") {
		t.Fatalf("the commit was refused, but not by the zero-sum trigger: %s", pgErr.Message)
	}
}

// zeroSum reads every row of the C-1 view - currency and net - keyed by
// currency. The whole view rather than only the violations, so one query
// serves both halves of the drill: the rows that must all be zero while
// the wallet is the only writer are the same rows the imbalance must
// appear in once it is not. If the two halves ran different queries, the
// second would stop being evidence about the first. The wording is this
// file's own for now, because the continuous job the deployment will run
// does not exist yet (T046); when it lands, the query IT runs is the one
// to drill here, or the deployed check could drift while this test stayed
// green.
func zeroSum(t *testing.T, tx pgx.Tx) map[string]int64 {
	t.Helper()
	rows, err := tx.Query(context.Background(),
		`select currency, net_minor from cashback.ledger_zero_sum`)
	if err != nil {
		t.Fatalf("the C-1 zero-sum check failed to run: %v", err)
	}
	defer rows.Close()

	held := map[string]int64{}
	for rows.Next() {
		var currency string
		var net int64
		if err := rows.Scan(&currency, &net); err != nil {
			t.Fatalf("scanning the zero-sum check: %v", err)
		}
		held[currency] = net
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating the zero-sum check: %v", err)
	}
	return held
}

// TestANonZeroCurrencySumFailsTheC1Check is C-1 asserted where the wallet
// stands: the check watches the ledger this package writes through, a
// currency that stops netting to zero fails it, and the database - not a
// convention, not the port's own Validate - is what refuses the illegal
// state, by SQLSTATE, at the one moment a transfer's postings are all on
// the table.
func TestANonZeroCurrencySumFailsTheC1Check(t *testing.T) {
	t.Parallel()
	pool := requirePool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// A no-op once the commit below has been refused; everywhere else it
	// is what keeps this drill's rows out of the database.
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })

	// Point the C-1 check at the schema the Postgres wallet ledger writes
	// through - the redirect 0022 documents for exactly this move.
	// set_config's third argument is is_local: the setting dies with this
	// transaction, so it cannot follow the connection back into the pool
	// and change what another test's check reads.
	if _, err := tx.Exec(ctx,
		`select set_config('cashback.ledger_schema', 'ledger', true)`); err != nil {
		t.Fatalf("pointing the C-1 check at the wallet's ledger: %v", err)
	}

	// The wallet's side of the drill goes through the port and nothing
	// else. The ledger is built over the transaction rather than the pool
	// (postgres.DB admits both, and a Post over a transaction nests inside
	// it), so the wallet's transfer and the imbalance planted behind its
	// back live and die together in the one transaction the refused commit
	// takes down whole.
	var ledger wallet.Ledger = postgres.New(tx)
	house, err := ledger.EnsureAccount(ctx,
		wallet.HouseAccount("invariant-house-"+uuid.NewString()), invariantCurrency)
	if err != nil {
		t.Fatalf("ensuring the house account: %v", err)
	}
	member, err := ledger.EnsureAccount(ctx,
		wallet.MemberAccount(uuid.New(), wallet.StageConfirmed), invariantCurrency)
	if err != nil {
		t.Fatalf("ensuring the member account: %v", err)
	}
	if _, err := ledger.Post(ctx, wallet.Transfer{
		IdempotencyKey: "invariant-c1-" + uuid.NewString(),
		Reference:      "a confirmed earning",
		Postings: []wallet.Posting{
			{Account: house, Amount: amt(-2500, invariantCurrency)},
			{Account: member, Amount: amt(2500, invariantCurrency)},
		},
	}); err != nil {
		t.Fatalf("posting through the wallet ledger: %v", err)
	}

	// The check is watching the right ledger. The currency the wallet just
	// moved has a row - the view's currency list is read from the ledger's
	// own balances - and it nets to zero, because the transfer balanced
	// and nothing unbalanced has ever been let commit.
	held := zeroSum(t, tx)
	net, seen := held[string(invariantCurrency)]
	if !seen {
		t.Fatalf("the zero-sum check reports no %s row after the wallet posted in %s: the check is not reading the ledger the wallet writes through",
			invariantCurrency, invariantCurrency)
	}
	if net != 0 {
		t.Fatalf("%s nets to %d minor units after one balanced transfer, want 0", invariantCurrency, net)
	}
	for currency, net := range held {
		if net != 0 {
			t.Fatalf("C-1 already violated before the drill planted anything: %s nets to %d minor units", currency, net)
		}
	}

	// The row alone is not yet proof the check can see the wallet's
	// POSTINGS: balances left-joins accounts, so a wallet that ensured its
	// accounts and then quietly wrote nothing would still put an XTS row
	// netting zero in the view. So the drill reads the wallet's own
	// figures back out of ledger.balances - the very relation the
	// redirected check resolves and sums (0020, 0022) - where only the
	// postings themselves can have put them.
	for account, want := range map[wallet.LedgerAccountID]int64{house: -2500, member: 2500} {
		var got int64
		if err := tx.QueryRow(ctx,
			`select balance from ledger.balances where account_id = $1`,
			string(account)).Scan(&got); err != nil {
			t.Fatalf("reading the balance the check sums for %s: %v", account, err)
		}
		if got != want {
			t.Fatalf("account %s holds %d minor units in the relation the check sums, want %d: the wallet's postings are not reaching the ledger the check watches",
				account, got, want)
		}
	}

	// The imbalance, planted where the port cannot vet it: a transfer of a
	// single posting, written straight into the ledger schema against the
	// very account the wallet ensured. The insert itself must succeed -
	// the zero-sum trigger is deferred so a transfer's postings may arrive
	// in any order inside their transaction - and that window between
	// insert and judgement is exactly where the view has to be able to see
	// the damage already.
	var planted string
	if err := tx.QueryRow(ctx,
		`insert into ledger.transfer (idempotency_key) values ($1) returning ref`,
		"invariant-imbalance-"+uuid.NewString()).Scan(&planted); err != nil {
		t.Fatalf("planting the transfer: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`insert into ledger.posting (transfer_ref, account_id, amount_minor, currency)
		 values ($1, $2, 77, $3)`,
		planted, string(house), string(invariantCurrency)); err != nil {
		t.Fatalf("planting the unbalanced posting: %v", err)
	}

	// The check goes red, naming the currency and the exact hole. A
	// non-zero row IS the C-1 failure - the deployed check treats one as
	// an incident, not a metric (SC-003) - so this drill requires exactly
	// the row it made true and no other.
	broken := map[string]int64{}
	for currency, net := range zeroSum(t, tx) {
		if net != 0 {
			broken[currency] = net
		}
	}
	if len(broken) != 1 || broken[string(invariantCurrency)] != 77 {
		t.Fatalf("the C-1 check reported %v against a ledger deliberately holding 77 %s from nowhere, want exactly that imbalance",
			broken, invariantCurrency)
	}
	t.Logf("C-1 drill: the zero-sum check reported %v against the wallet's ledger holding 77 %s from nowhere", broken, invariantCurrency)

	// And the database itself refuses the state. COMMIT is when the
	// deferred trigger judges the planted transfer whole, and the refusal
	// must arrive as the SQLSTATE the schema raises - the enforcement a
	// writer that never runs the wallet's Go cannot opt out of - in the
	// zero-sum trigger's own words, so no refusal this schema may one day
	// grow beside it can stand in for the one this drill exists to prove.
	wantC1Refusal(t, tx.Commit(ctx))

	// The refusal took the whole transaction down, the wallet's balanced
	// transfer included. A fresh look at the committed world must find no
	// trace of the drill's currency: failing loudly means the illegal
	// state was never recorded, not recorded and flagged.
	after, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin after the refused commit: %v", err)
	}
	defer func() { _ = after.Rollback(context.Background()) }()
	if _, err := after.Exec(ctx,
		`select set_config('cashback.ledger_schema', 'ledger', true)`); err != nil {
		t.Fatalf("pointing the C-1 check at the wallet's ledger again: %v", err)
	}
	if net, seen := zeroSum(t, after)[string(invariantCurrency)]; seen {
		t.Fatalf("the committed ledger still carries a %s row netting %d after the refused commit: the drill leaked into the database", invariantCurrency, net)
	}
}

// TestACrossCurrencyOffsetFailsTheC1Check is the phrase "per currency"
// given its own drill. The transfer it plants nets to zero over the whole
// transfer and to zero in no currency it touches: 50 minted from nowhere
// in one, 50 destroyed into nowhere in the other - a ledger quietly
// exchanging currencies nobody converted. A zero-sum judgement that
// summed a transfer without grouping by currency would bless it, and the
// lone-posting drill above would never notice, because a lone posting is
// unbalanced under either reading. Here the check must report both holes
// by currency and the database must refuse the transfer whole.
func TestACrossCurrencyOffsetFailsTheC1Check(t *testing.T) {
	t.Parallel()
	pool := requirePool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// A no-op once the commit below has been refused; everywhere else it
	// is what keeps this drill's rows out of the database.
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })

	if _, err := tx.Exec(ctx,
		`select set_config('cashback.ledger_schema', 'ledger', true)`); err != nil {
		t.Fatalf("pointing the C-1 check at the wallet's ledger: %v", err)
	}

	// The accounts are the wallet's own, ensured through the port - one
	// per currency, because an account holds exactly one (C-6). House on
	// both sides, and deliberately: the destroyed leg leaves its account
	// below zero, the solvency rule exempts house accounts (0022), and so
	// nothing is left standing between this commit and the zero-sum
	// trigger's judgement.
	var ledger wallet.Ledger = postgres.New(tx)
	minted, err := ledger.EnsureAccount(ctx,
		wallet.HouseAccount("invariant-minted-"+uuid.NewString()), invariantCurrency)
	if err != nil {
		t.Fatalf("ensuring the minted-side account: %v", err)
	}
	destroyed, err := ledger.EnsureAccount(ctx,
		wallet.HouseAccount("invariant-destroyed-"+uuid.NewString()), crossCurrency)
	if err != nil {
		t.Fatalf("ensuring the destroyed-side account: %v", err)
	}

	// The offsetting pair goes in as SQL behind the port, because it can
	// go in no other way: the port's Validate refuses a cross-currency
	// offset before any I/O, so the only writer able to ask the database
	// this question is the one that never runs the wallet's Go.
	var planted string
	if err := tx.QueryRow(ctx,
		`insert into ledger.transfer (idempotency_key) values ($1) returning ref`,
		"invariant-cross-"+uuid.NewString()).Scan(&planted); err != nil {
		t.Fatalf("planting the transfer: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`insert into ledger.posting (transfer_ref, account_id, amount_minor, currency)
		 values ($1, $2, 50, $3), ($1, $4, -50, $5)`,
		planted, string(minted), string(invariantCurrency),
		string(destroyed), string(crossCurrency)); err != nil {
		t.Fatalf("planting the offsetting postings: %v", err)
	}

	// The trap is armed for real: observed, not assumed, the planted
	// postings sum to zero when the currencies are ignored - the exact
	// figure an ungrouped judgement would wave through.
	var global int64
	if err := tx.QueryRow(ctx,
		`select coalesce(sum(amount_minor), 0) from ledger.posting where transfer_ref = $1`,
		planted).Scan(&global); err != nil {
		t.Fatalf("summing the planted transfer across currencies: %v", err)
	}
	if global != 0 {
		t.Fatalf("the planted transfer nets to %d minor units across currencies, want 0: the drill is not posing the cross-currency question", global)
	}

	// Judged per currency, both holes show, each with its sign: money from
	// nowhere in one currency, money to nowhere in the other.
	broken := map[string]int64{}
	for currency, net := range zeroSum(t, tx) {
		if net != 0 {
			broken[currency] = net
		}
	}
	if len(broken) != 2 || broken[string(invariantCurrency)] != 50 || broken[string(crossCurrency)] != -50 {
		t.Fatalf("the C-1 check reported %v against a ledger deliberately minting 50 %s and destroying 50 %s, want exactly those two imbalances",
			broken, invariantCurrency, crossCurrency)
	}

	// And the database refuses the transfer whole at COMMIT, in the
	// zero-sum trigger's own words.
	wantC1Refusal(t, tx.Commit(ctx))

	// The refusal left nothing behind in either currency.
	after, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin after the refused commit: %v", err)
	}
	defer func() { _ = after.Rollback(context.Background()) }()
	if _, err := after.Exec(ctx,
		`select set_config('cashback.ledger_schema', 'ledger', true)`); err != nil {
		t.Fatalf("pointing the C-1 check at the wallet's ledger again: %v", err)
	}
	held := zeroSum(t, after)
	for _, currency := range []money.Currency{invariantCurrency, crossCurrency} {
		if net, seen := held[string(currency)]; seen {
			t.Fatalf("the committed ledger still carries a %s row netting %d after the refused commit: the drill leaked into the database", currency, net)
		}
	}
}
