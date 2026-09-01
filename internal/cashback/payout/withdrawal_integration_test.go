// The tests for withdrawal.go, against the real schema and a real ledger.
//
// A scratch database rather than a rolled-back transaction, and the reason is
// what this code does: Request BEGINS a transaction and COMMITS it, because
// D9 requires the reservation and the request to land together. Wrapping that
// in an outer transaction the test throws away would turn the commit into a
// savepoint release, which is not what runs in production. So the writes are
// real, and they are kept out of the shared database instead.
//
// Every case seeds its own member, so nothing here collides with anything
// else even though the rows are committed and cashback.entry cannot be
// deleted.

package payout_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/memory"
	platformdb "github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// withdrawalDatabase is the scratch database these cases commit into.
const withdrawalDatabase = "apivo_payout_withdrawal"

// receivable is the house account earnings are credited from, the same name
// the entry machine is wired with.
const receivable = "network-receivable"

// euros is the currency every case here works in, matching the threshold.
const euros = money.Currency("EUR")

var (
	withdrawalDBOnce sync.Once
	withdrawalDBURL  string
	withdrawalDBErr  error
	// sharedLedger is ONE ledger for every case in this file, because that
	// is what a deployment has. A ledger per case would restart the memory
	// adapter's transfer-reference sequence at 1 each time, and the second
	// case to commit would collide on
	// withdrawal_request_reserved_transfer_ref_unique - a constraint that is
	// right (a transfer reference is the ledger's own identity for one
	// movement) failing against a fixture that was pretending to be several
	// ledgers. Members do not interfere: every case seeds its own, and
	// stage accounts are per member.
	sharedLedger = memory.New()
)

// withdrawalPool skips unless a database is reachable, creates and migrates
// the scratch database once, and hands back a pool over it.
func withdrawalPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise withdrawals")
	}
	withdrawalDBOnce.Do(func() { withdrawalDBURL, withdrawalDBErr = ensureWithdrawalDatabase(base) })
	if withdrawalDBErr != nil {
		t.Fatalf("preparing the scratch database: %v", withdrawalDBErr)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, withdrawalDBURL)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

// ensureWithdrawalDatabase remakes the scratch database beside the one
// DATABASE_URL names and migrates it, once per test process.
func ensureWithdrawalDatabase(base string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		return "", err
	}
	defer admin.Close()

	// DROPPED AND REMADE EACH RUN, not reused. These cases commit withdrawal
	// requests, and a payout will reference one as soon as approving exists -
	// cashback.payout is append-only (payout_no_delete and payout_no_truncate
	// are triggers), so nothing can tidy one away, which is correct: money
	// that has left is not a fixture. That leaves the whole database as the
	// only unit that can be reset, and it is ours alone to reset. WITH
	// (FORCE) closes connections a previous run left behind rather than
	// failing on them.
	//
	// What has to be reset at all is the ledger's transfer references. The
	// memory adapter numbers them from 1 in every process, while
	// withdrawal_request_reserved_transfer_ref_unique is global - so a
	// second run would collide with the first on "transfer-1". The
	// constraint is right; the fixture is what is pretending to be several
	// ledgers, and it starts from nothing instead.
	_, _ = admin.Exec(ctx, `drop database if exists "`+withdrawalDatabase+`" with (force)`)
	// create database cannot run in a transaction and has no IF NOT EXISTS,
	// so a racing creation reports an error. The migrate below is what
	// proves the database is usable; this is carried into its failure only,
	// because "does not exist" and "may not create one" are fixed
	// differently.
	_, createErr := admin.Exec(ctx, `create database "`+withdrawalDatabase+`"`)

	scratch := *parsed
	scratch.Path = "/" + withdrawalDatabase
	scratchURL := scratch.String()
	if err := platformdb.Migrate(scratchURL); err != nil {
		if createErr != nil {
			return "", fmt.Errorf("migrating %s: %w (creating it said: %w)", withdrawalDatabase, err, createErr)
		}
		return "", err
	}
	return scratchURL, nil
}

func euro(t *testing.T, minor int64) money.Amount {
	t.Helper()
	a, err := money.New(minor, euros)
	if err != nil {
		t.Fatalf("money.New(%d, EUR): %v", minor, err)
	}
	return a
}

// fixture is everything one case needs: a member holding confirmed money, a
// verified destination, and the service over a ledger that agrees with the
// entries.
type fixture struct {
	pool        *pgxpool.Pool
	member      uuid.UUID
	destination uuid.UUID
	ledger      *memory.Ledger
	withdrawals *payout.Withdrawals
}

// aFixture seeds a member with the given confirmed entries, a verified
// destination, and builds the service against a threshold.
//
// The ledger is seeded to agree with the entries, because that is what the
// real one holds: an entry in confirmed means a posting into that member's
// confirmed stage account. A fixture that skipped it would have the
// reservation refused for insufficient funds every time - the D9 defence
// firing against the fixture instead of against a real overdraw, which would
// make every case here pass for the wrong reason.
func aFixture(ctx context.Context, t *testing.T, threshold int64, confirmed ...int64) fixture {
	t.Helper()
	_, pool := withdrawalPool(t)
	member := seedMember(ctx, t, pool)
	destination := seedDestination(ctx, t, pool, member, true)
	ledger := sharedLedger

	for i, minor := range confirmed {
		seedConfirmedEntry(ctx, t, pool, member, minor)
		credit(ctx, t, ledger, member, euro(t, minor), fmt.Sprintf("fixture:%s:%d", member, i))
	}

	withdrawals, err := payout.NewWithdrawals(pool, ledger, receivable, euro(t, threshold))
	if err != nil {
		t.Fatalf("NewWithdrawals(): %v", err)
	}
	return fixture{pool: pool, member: member, destination: destination, ledger: ledger, withdrawals: withdrawals}
}

// seedMember writes an account for one case to own.
func seedMember(ctx context.Context, t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'A Member', 'reader') returning id`,
		"withdrawal-"+uuid.NewString()+"@example.test").Scan(&id); err != nil {
		t.Fatalf("seeding the member: %v", err)
	}
	return uuid.UUID(id.Bytes)
}

// seedDestination writes a payout destination, verified or not.
func seedDestination(ctx context.Context, t *testing.T, pool *pgxpool.Pool, member uuid.UUID, verified bool) uuid.UUID {
	t.Helper()
	verifiedAt := "null"
	method := "null"
	if verified {
		verifiedAt, method = "now()", "'fixture'"
	}
	var id pgtype.UUID
	if err := pool.QueryRow(ctx, `
		insert into cashback.payout_destination (account_id, kind, details_ref, verified_at, verified_method)
		values ($1, 'stub', $2, `+verifiedAt+`, `+method+`) returning id`,
		pgtype.UUID{Bytes: member, Valid: true}, "vault:"+uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("seeding the destination: %v", err)
	}
	return uuid.UUID(id.Bytes)
}

// seedNetworkAccount writes the network and publisher account every report
// hangs off, once per scratch database. Named rather than random so a second
// run reuses it: the rows carry no per-case state.
func seedNetworkAccount(ctx context.Context, t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ('withdrawalfixture', 'Withdrawal Fixture', 'clickref', 31, 60, true)
		on conflict (id) do nothing`); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	var id pgtype.UUID
	if err := pool.QueryRow(ctx, `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active)
		values ('withdrawalfixture', 'withdrawal-fixture', 'env:NETWORK_API_KEY', true)
		on conflict (network_id, external_publisher_id) do update set active = excluded.active
		returning id`).Scan(&id); err != nil {
		t.Fatalf("seeding the network account: %v", err)
	}
	return uuid.UUID(id.Bytes)
}

// seedReport writes the network evidence one entry cites (C-2). Each is its
// own row with its own external id, because network_transaction is keyed on
// the pair and an entry may cite only one report.
func seedReport(ctx context.Context, t *testing.T, pool *pgxpool.Pool, minor int64) uuid.UUID {
	t.Helper()
	account := seedNetworkAccount(ctx, t, pool)
	var id pgtype.UUID
	if err := pool.QueryRow(ctx, `
		insert into cashback.network_transaction
		    (network_id, network_account_id, external_id, status_raw, status,
		     sale_amount_minor, commission_minor, currency, transacted_at,
		     query_window_start, query_window_end, raw_payload)
		values ('withdrawalfixture', $1, $2, 'approved', 'confirmed', $3, $3, 'EUR',
		        now(), now(), now(), '{}'::jsonb)
		returning id`,
		pgtype.UUID{Bytes: account, Valid: true}, "withdrawal-"+uuid.NewString(), minor).Scan(&id); err != nil {
		t.Fatalf("seeding the network report: %v", err)
	}
	return uuid.UUID(id.Bytes)
}

// seedConfirmedEntry writes one confirmed entry with the evidence C-2 makes
// mandatory behind it.
func seedConfirmedEntry(ctx context.Context, t *testing.T, pool *pgxpool.Pool, member uuid.UUID, minor int64) uuid.UUID {
	t.Helper()
	report := seedReport(ctx, t, pool, minor)
	var id pgtype.UUID
	if err := pool.QueryRow(ctx, `
		insert into cashback.entry (account_id, brand_id, network_transaction_id, state, amount_minor, currency)
		values ($1, 'apivo-de', $2, 'confirmed', $3, 'EUR') returning id`,
		pgtype.UUID{Bytes: member, Valid: true}, pgtype.UUID{Bytes: report, Valid: true}, minor).Scan(&id); err != nil {
		t.Fatalf("seeding a confirmed entry: %v", err)
	}
	return uuid.UUID(id.Bytes)
}

// credit posts the money into the member's confirmed stage account, so the
// ledger holds what the entries say it does.
func credit(ctx context.Context, t *testing.T, ledger *memory.Ledger, member uuid.UUID, a money.Amount, key string) {
	t.Helper()
	house, err := ledger.EnsureAccount(ctx, wallet.HouseAccount(receivable), a.Currency)
	if err != nil {
		t.Fatalf("ensuring the receivable: %v", err)
	}
	stage, err := ledger.EnsureAccount(ctx, wallet.MemberAccount(member, wallet.StageConfirmed), a.Currency)
	if err != nil {
		t.Fatalf("ensuring the confirmed stage: %v", err)
	}
	debit, err := a.Neg()
	if err != nil {
		t.Fatalf("negating %s: %v", a, err)
	}
	if _, err := ledger.Post(ctx, wallet.Transfer{
		IdempotencyKey: key,
		Reference:      key,
		Postings: []wallet.Posting{
			{Account: house, Amount: debit},
			{Account: stage, Amount: a},
		},
	}); err != nil {
		t.Fatalf("crediting %s: %v", a, err)
	}
}

// stageBalance reads what the ledger holds in one of the member's stages.
func (f fixture) stageBalance(ctx context.Context, t *testing.T, stage wallet.Stage) money.Amount {
	t.Helper()
	account, err := f.ledger.EnsureAccount(ctx, wallet.MemberAccount(f.member, stage), euros)
	if err != nil {
		t.Fatalf("ensuring the %s stage: %v", stage, err)
	}
	balance, err := f.ledger.Balance(ctx, account, euros)
	if err != nil {
		t.Fatalf("reading the %s balance: %v", stage, err)
	}
	return balance
}

// entryStates counts this member's entries by state, which is what proves a
// reservation moved the rows and not only the ledger.
func (f fixture) entryStates(ctx context.Context, t *testing.T) map[string]int {
	t.Helper()
	rows, err := f.pool.Query(ctx,
		`select state, count(*) from cashback.entry where account_id = $1 group by state`,
		pgtype.UUID{Bytes: f.member, Valid: true})
	if err != nil {
		t.Fatalf("counting entry states: %v", err)
	}
	defer rows.Close()
	states := map[string]int{}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			t.Fatalf("scanning entry states: %v", err)
		}
		states[state] = count
	}
	return states
}

// TestAWithdrawalReservesBeforeAnybodyReviewsIt is D9 and US4 scenario 2 in
// one case: the money is out of confirmed the moment the request exists.
func TestAWithdrawalReservesBeforeAnybodyReviewsIt(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 3000, 4000)

	got, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: f.destination, Amount: euro(t, 3000),
	})
	if err != nil {
		t.Fatalf("Request(): %v", err)
	}

	if got.State != payout.StateAwaitingApproval {
		t.Errorf("the request is %s, want %s", got.State, payout.StateAwaitingApproval)
	}
	if got.ReservedTransfer == "" {
		t.Error("the request names no reservation transfer; reserved_transfer_ref is what closes the double-spend window")
	}
	if want := euro(t, 3000); got.Amount != want {
		t.Errorf("reserved %s, want %s", got.Amount, want)
	}
	if balance := f.stageBalance(ctx, t, wallet.StageReserved); balance != euro(t, 3000) {
		t.Errorf("the reserved stage holds %s, want %s", balance, euro(t, 3000))
	}
	if balance := f.stageBalance(ctx, t, wallet.StageConfirmed); balance != euro(t, 4000) {
		t.Errorf("the confirmed stage holds %s, want the 40.00 left behind", balance)
	}
	if states := f.entryStates(ctx, t); states["reserved"] != 1 || states["confirmed"] != 1 {
		t.Errorf("entry states are %v, want one reserved and one confirmed", states)
	}
}

// TestEveryReservedEntryNamesTheRequestsOwnTransfer is C-7's requirement
// against the real schema: migration 0016 joins entry_transition to the
// request on that one reference, so a reservation whose transitions carried
// different references would answer a payout's provenance with nothing.
func TestEveryReservedEntryNamesTheRequestsOwnTransfer(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 1000, 2000, 3000)

	got, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: f.destination, Amount: euro(t, 2500),
	})
	if err != nil {
		t.Fatalf("Request(): %v", err)
	}
	// 10.00 then 20.00 covers 25.00, so two entries and 30.00 reserved.
	if want := euro(t, 3000); got.Amount != want {
		t.Errorf("reserved %s, want %s - entries are taken whole", got.Amount, want)
	}

	var transitions int
	if err := f.pool.QueryRow(ctx, `
		select count(*) from cashback.entry_transition et
		  join cashback.entry e on e.id = et.entry_id
		 where e.account_id = $1
		   and et.to_state = 'reserved'
		   and et.ledger_transfer_ref = $2`,
		pgtype.UUID{Bytes: f.member, Valid: true}, string(got.ReservedTransfer)).Scan(&transitions); err != nil {
		t.Fatalf("counting reserving transitions: %v", err)
	}
	if transitions != 2 {
		t.Errorf("%d transition(s) name the request's transfer, want one per reserved entry", transitions)
	}
}

// TestAWithdrawalBelowTheThresholdIsRefused is FR-050, and it is refused for
// the threshold rather than for the amount: a member with 15.00 asking for
// 10.00 has enough for what they asked and still may not withdraw.
func TestAWithdrawalBelowTheThresholdIsRefused(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 2500, 1500)

	_, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: f.destination, Amount: euro(t, 1000),
	})
	var below payout.BelowThreshold
	if !errors.As(err, &below) {
		t.Fatalf("Request() = %v, want a %T", err, below)
	}
	if want := euro(t, 1000); below.Short != want {
		t.Errorf("Short = %s, want %s", below.Short, want)
	}
	if balance := f.stageBalance(ctx, t, wallet.StageReserved); !balance.IsZero() {
		t.Errorf("the reserved stage holds %s after a refusal, want nothing", balance)
	}
}

// TestAWithdrawalBeyondTheConfirmedBalanceStatesTheShortfall is US4 scenario
// 1: the member is told how much they are short, not merely that they are.
func TestAWithdrawalBeyondTheConfirmedBalanceStatesTheShortfall(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 2000, 1000)

	_, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: f.destination, Amount: euro(t, 5000),
	})
	var short earnings.ShortConfirmedBalance
	if !errors.As(err, &short) {
		t.Fatalf("Request() = %v, want a %T", err, short)
	}
	if want := euro(t, 2000); short.Short != want {
		t.Errorf("Short = %s, want %s", short.Short, want)
	}
	if states := f.entryStates(ctx, t); states["reserved"] != 0 {
		t.Errorf("entry states are %v after a refusal, want nothing reserved", states)
	}
}

// TestAnotherMembersDestinationIsRefused is US4 scenario 6 and T099's
// contract: the destination is not this member's, so it is not found - the
// same answer an id naming nothing gets.
func TestAnotherMembersDestinationIsRefused(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	stranger := seedMember(ctx, t, pool)
	theirs := seedDestination(ctx, t, pool, stranger, true)

	_, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: theirs, Amount: euro(t, 2000),
	})
	if !errors.Is(err, payout.ErrDestinationNotFound) {
		t.Fatalf("Request() = %v, want one wrapping %v", err, payout.ErrDestinationNotFound)
	}
	if balance := f.stageBalance(ctx, t, wallet.StageReserved); !balance.IsZero() {
		t.Errorf("the reserved stage holds %s, want nothing", balance)
	}
}

// TestAnUnverifiedDestinationIsRefused is FR-051. The database refuses it
// too; this proves the member is told which rule they hit, and that nothing
// was posted before they were.
func TestAnUnverifiedDestinationIsRefused(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	unverified := seedDestination(ctx, t, pool, f.member, false)

	_, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: unverified, Amount: euro(t, 2000),
	})
	if !errors.Is(err, payout.ErrDestinationNotVerified) {
		t.Fatalf("Request() = %v, want one wrapping %v", err, payout.ErrDestinationNotVerified)
	}
	if balance := f.stageBalance(ctx, t, wallet.StageReserved); !balance.IsZero() {
		t.Errorf("the reserved stage holds %s, want nothing", balance)
	}
}

// TestTwoRequestsForTheWholeBalanceLeaveOneOfThem is SC-004's companion, and
// the whole reason the reservation happens at request time. The second
// request reads what the first left, and there is nothing left.
func TestTwoRequestsForTheWholeBalanceLeaveOneOfThem(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)

	first, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: f.destination, Amount: euro(t, 5000),
	})
	if err != nil {
		t.Fatalf("the first request: %v", err)
	}
	if _, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: f.destination, Amount: euro(t, 5000),
	}); err == nil {
		t.Fatal("the second request for the same balance succeeded, want a refusal")
	}

	if balance := f.stageBalance(ctx, t, wallet.StageReserved); balance != euro(t, 5000) {
		t.Errorf("the reserved stage holds %s, want the one reservation of %s", balance, euro(t, 5000))
	}
	var requests int
	if err := f.pool.QueryRow(ctx,
		`select count(*) from cashback.withdrawal_request where account_id = $1`,
		pgtype.UUID{Bytes: f.member, Valid: true}).Scan(&requests); err != nil {
		t.Fatalf("counting requests: %v", err)
	}
	if requests != 1 {
		t.Errorf("%d request(s) exist, want the one that took the balance (%s)", requests, first.ID)
	}
}

// TestAWithdrawalIsReadBackByItsOwner. Another member's request and one that
// does not exist answer the same way, so an id cannot be probed.
func TestAWithdrawalIsReadBackByItsOwner(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)

	made, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: f.destination, Amount: euro(t, 2000),
	})
	if err != nil {
		t.Fatalf("Request(): %v", err)
	}

	got, err := f.withdrawals.Get(ctx, f.member, made.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got.ID != made.ID || got.Amount != made.Amount || got.ReservedTransfer != made.ReservedTransfer {
		t.Errorf("read back %+v, want %+v", got, made)
	}

	stranger := seedMember(ctx, t, pool)
	if _, err := f.withdrawals.Get(ctx, stranger, made.ID); !errors.Is(err, payout.ErrWithdrawalNotFound) {
		t.Errorf("a stranger's Get() = %v, want one wrapping %v", err, payout.ErrWithdrawalNotFound)
	}

	listed, err := f.withdrawals.List(ctx, f.member)
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(listed) != 1 || listed[0].ID != made.ID {
		t.Errorf("listed %d request(s), want the one made", len(listed))
	}
}

// TestAWithdrawalInAnotherCurrencyIsRefused. The threshold states one
// currency, so a request in another has nothing to be checked against - and
// comparing the two numerically is the mistake C-6 exists to prevent.
func TestAWithdrawalInAnotherCurrencyIsRefused(t *testing.T) {
	ctx, _ := withdrawalPool(t)
	f := aFixture(ctx, t, 1000, 5000)
	pounds, err := money.New(2000, money.Currency("GBP"))
	if err != nil {
		t.Fatalf("money.New(2000, GBP): %v", err)
	}

	if _, err := f.withdrawals.Request(ctx, payout.Request{
		Member: f.member, Destination: f.destination, Amount: pounds,
	}); !errors.Is(err, payout.ErrCurrencyNotPaid) {
		t.Fatalf("Request() = %v, want one wrapping %v", err, payout.ErrCurrencyNotPaid)
	}
}

// TestADeploymentWithNoThresholdCannotBeWithdrawnFrom. Nothing is broken; the
// deployment is incomplete, and the two are different things to whoever is
// paged.
func TestADeploymentWithNoThresholdCannotBeWithdrawnFrom(t *testing.T) {
	ctx, pool := withdrawalPool(t)
	member := seedMember(ctx, t, pool)
	destination := seedDestination(ctx, t, pool, member, true)

	withdrawals, err := payout.NewWithdrawals(pool, sharedLedger, receivable, money.Amount{})
	if err != nil {
		t.Fatalf("NewWithdrawals(): %v", err)
	}
	if _, err := withdrawals.Request(ctx, payout.Request{
		Member: member, Destination: destination, Amount: euro(t, 2000),
	}); !errors.Is(err, payout.ErrNoThreshold) {
		t.Fatalf("Request() = %v, want one wrapping %v", err, payout.ErrNoThreshold)
	}
}
