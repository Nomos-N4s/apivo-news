package scenarios_test

// V4 lives in its own file, with its own database, for one reason: approving
// a withdrawal COMMITS - twice, deliberately, so a rail that answers slowly
// cannot hold a transaction open across a network call. A gate that ran it
// inside a rolled-back transaction would be testing something the product
// does not do.
//
// So this one gets a scratch database of its own, dropped and remade each
// run rather than cleaned up: cashback.payout is append-only by design
// (payout_no_delete), which is exactly the property that makes tidying up
// afterwards impossible and remaking cheap.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/payout/stub"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/memory"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

const (
	// withdrawalDatabase is this gate's own database.
	withdrawalDatabase = "apivo_scenarios_withdrawal"
	// descriptor is what a member reads on their statement. Not a product
	// name: no rail and no test may contain one (FR-073).
	descriptor = "FIXTURE CASHBACK"
	// asked is what the member withdraws, out of twice that confirmed, so
	// nothing here is ever refused for the balance.
	asked int64 = 3000
)

var (
	withdrawalDBOnce sync.Once
	withdrawalDBURL  string
	withdrawalDBErr  error
)

// payouts is one run of V4: a scratch database, a memory ledger, and the
// three services an operator's decision passes through.
type payouts struct {
	ctx          context.Context
	pool         *pgxpool.Pool
	ledger       *memory.Ledger
	member       uuid.UUID
	stranger     uuid.UUID
	destination  uuid.UUID
	theirs       uuid.UUID
	operator     uuid.UUID
	withdrawals  *payout.Withdrawals
	approvals    *payout.Approvals
	rejections   *payout.Rejections
	railInstance *stub.Rail
}

// openPayouts prepares the scratch database and everything in it.
func openPayouts(t *testing.T) *payouts {
	t.Helper()
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Skip("DATABASE_URL not set; run `make cashback-up` and set it to run the acceptance gates")
	}
	withdrawalDBOnce.Do(func() { withdrawalDBURL, withdrawalDBErr = remakeWithdrawalDatabase(base) })
	if withdrawalDBErr != nil {
		t.Fatalf("preparing the scratch database: %v", withdrawalDBErr)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, withdrawalDBURL)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)

	p := &payouts{ctx: ctx, pool: pool, ledger: memory.New(), railInstance: stub.New()}
	p.member = p.person(t, "reader")
	p.stranger = p.person(t, "reader")
	p.operator = p.person(t, "operator")
	p.destination = p.destinationFor(t, p.member)
	p.theirs = p.destinationFor(t, p.stranger)
	// Two confirmed entries, because entries are reserved WHOLE: one
	// request takes the entry it needs entire, so a second request in the
	// same gate needs money of its own rather than the remainder of the
	// first. That is [earnings.Covering]'s rule, not a quirk of this setup.
	p.confirm(t, p.member, asked*2)
	p.confirm(t, p.member, asked*2)

	threshold, err := money.New(1000, scenarioCurrency)
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	if p.withdrawals, err = payout.NewWithdrawals(pool, p.ledger, houseReceivable, threshold); err != nil {
		t.Fatalf("NewWithdrawals(): %v", err)
	}
	if p.approvals, err = payout.NewApprovals(pool, p.railInstance, descriptor); err != nil {
		t.Fatalf("NewApprovals(): %v", err)
	}
	if p.rejections, err = payout.NewRejections(pool, p.ledger, houseReceivable); err != nil {
		t.Fatalf("NewRejections(): %v", err)
	}
	return p
}

// remakeWithdrawalDatabase drops and recreates the scratch database. Dropped
// rather than reused, because everything this gate writes is append-only and
// a second run would find the first run's payouts standing.
func remakeWithdrawalDatabase(base string) (string, error) {
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
	if _, err := admin.Exec(ctx, `drop database if exists "`+withdrawalDatabase+`" with (force)`); err != nil {
		return "", fmt.Errorf("dropping %s: %w", withdrawalDatabase, err)
	}
	if _, err := admin.Exec(ctx, `create database "`+withdrawalDatabase+`"`); err != nil {
		return "", fmt.Errorf("creating %s: %w", withdrawalDatabase, err)
	}
	scratch := *parsed
	scratch.Path = "/" + withdrawalDatabase
	scratchURL := scratch.String()
	if err := db.Migrate(scratchURL); err != nil {
		return "", fmt.Errorf("migrating %s: %w", withdrawalDatabase, err)
	}
	return scratchURL, nil
}

// person writes one account in the given role.
func (p *payouts) person(t *testing.T, role string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := p.pool.QueryRow(p.ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Scenario Person', $2) returning id`,
		role+"-"+uuid.NewString()+"@example.test", role).Scan(&id); err != nil {
		t.Fatalf("seeding the %s: %v", role, err)
	}
	return id
}

// destinationFor writes a verified stub destination owned by one member.
func (p *payouts) destinationFor(t *testing.T, member uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := p.pool.QueryRow(p.ctx, `
		insert into cashback.payout_destination
		    (account_id, kind, details_ref, verified_at, verified_method)
		values ($1, 'stub', $2, now(), 'fixture') returning id`,
		member, "vault:"+uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("seeding the destination: %v", err)
	}
	return id
}

// confirm gives a member money to withdraw: a confirmed entry, and the
// ledger posting behind it.
func (p *payouts) confirm(t *testing.T, member uuid.UUID, minor int64) {
	t.Helper()
	if _, err := p.pool.Exec(p.ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ('scenariopayout', 'Scenario Payout', 'clickref', 31, 60, true)
		on conflict (id) do nothing`); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	var account pgtype.UUID
	if err := p.pool.QueryRow(p.ctx, `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active)
		values ('scenariopayout', 'scenario-payout', 'env:NETWORK_API_KEY', true)
		on conflict (network_id, external_publisher_id) do update set active = excluded.active
		returning id`).Scan(&account); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}
	var report pgtype.UUID
	if err := p.pool.QueryRow(p.ctx, `
		insert into cashback.network_transaction
		    (network_id, network_account_id, external_id, status_raw, status,
		     sale_amount_minor, commission_minor, currency, transacted_at,
		     query_window_start, query_window_end, raw_payload)
		values ('scenariopayout', $1, $2, 'approved', 'confirmed', $3, $3, 'EUR',
		        now(), now(), now(), '{}'::jsonb)
		returning id`, account, "payout-"+uuid.NewString(), minor).Scan(&report); err != nil {
		t.Fatalf("seeding the report: %v", err)
	}
	if _, err := p.pool.Exec(p.ctx, `
		insert into cashback.entry
		    (account_id, brand_id, network_transaction_id, state, amount_minor, currency)
		values ($1, $2, $3, 'confirmed', $4, 'EUR')`,
		member, scenarioBrand, report, minor); err != nil {
		t.Fatalf("seeding the confirmed entry: %v", err)
	}

	amount, err := money.New(minor, scenarioCurrency)
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	house, err := p.ledger.EnsureAccount(p.ctx, wallet.HouseAccount(houseReceivable), scenarioCurrency)
	if err != nil {
		t.Fatalf("ensuring the receivable: %v", err)
	}
	stage, err := p.ledger.EnsureAccount(p.ctx, wallet.MemberAccount(member, wallet.StageConfirmed), scenarioCurrency)
	if err != nil {
		t.Fatalf("ensuring the confirmed stage: %v", err)
	}
	debit, err := amount.Neg()
	if err != nil {
		t.Fatalf("negating: %v", err)
	}
	key := "scenario:confirm:" + member.String() + ":" + uuid.NewString()
	if _, err := p.ledger.Post(p.ctx, wallet.Transfer{
		IdempotencyKey: key, Reference: key,
		Postings: []wallet.Posting{{Account: house, Amount: debit}, {Account: stage, Amount: amount}},
	}); err != nil {
		t.Fatalf("crediting the member: %v", err)
	}
}

// confirmed is what the member can still withdraw.
func (p *payouts) confirmed(t *testing.T, member uuid.UUID) int64 {
	t.Helper()
	account, err := p.ledger.EnsureAccount(p.ctx, wallet.MemberAccount(member, wallet.StageConfirmed), scenarioCurrency)
	if err != nil {
		t.Fatalf("resolving the confirmed account: %v", err)
	}
	held, err := p.ledger.Balance(p.ctx, account, scenarioCurrency)
	if err != nil {
		t.Fatalf("reading the confirmed balance: %v", err)
	}
	return held.Minor
}

// request asks for money to one destination.
func (p *payouts) request(t *testing.T, destination uuid.UUID) payout.Withdrawal {
	t.Helper()
	amount, err := money.New(asked, scenarioCurrency)
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	made, err := p.withdrawals.Request(p.ctx, payout.Request{
		Member: p.member, Destination: destination, Amount: amount,
	})
	if err != nil {
		t.Fatalf("Request(): %v", err)
	}
	return made
}

// payoutsFor counts the payments recorded against one request. The number
// this whole gate is about.
func (p *payouts) payoutsFor(t *testing.T, request uuid.UUID) int {
	t.Helper()
	var n int
	if err := p.pool.QueryRow(p.ctx,
		`select count(*) from cashback.payout where request_id = $1`, request).Scan(&n); err != nil {
		t.Fatalf("counting payouts: %v", err)
	}
	return n
}

// withdrawalExactlyOnce is V4 (US4 · C-4, C-5, SC-004).
//
// One request, at most one payment, and a named human behind it. The rail's
// own misbehaviours - a timeout, a retry that finds the first attempt, a
// permanent failure - are the payout package's to exercise against the rail
// (retry_integration_test.go, settle_integration_test.go,
// exactly_once_test.go, and the stub's own concurrency case). What this gate
// asserts is the part a founder is promised: the money moves once, the
// approval has a name on it, and a refusal gives the money back.
func withdrawalExactlyOnce(t *testing.T) {
	p := openPayouts(t)

	// A member may not be paid to somebody else's account, verified or not.
	// This is the refusal that stops a withdrawal being a transfer.
	amount, err := money.New(asked, scenarioCurrency)
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	if _, err := p.withdrawals.Request(p.ctx, payout.Request{
		Member: p.member, Destination: p.theirs, Amount: amount,
	}); err == nil {
		t.Error("a member was allowed to withdraw to another member's destination")
	}

	// The lawful request moves the money out of confirmed, so it cannot be
	// spent twice while an operator is deciding.
	before := p.confirmed(t, p.member)
	made := p.request(t, p.destination)
	if after := p.confirmed(t, p.member); after >= before {
		t.Errorf("confirmed balance is %d after a request for %d, was %d; the reservation did not move it",
			after, asked, before)
	}

	// Approval carries the operator, and the column is NOT NULL, so a payout
	// without one cannot exist to be read (C-4).
	paid, err := p.approvals.Approve(p.ctx, payout.Approval{Request: made.ID, Operator: p.operator})
	if err != nil {
		t.Fatalf("Approve(): %v", err)
	}
	if paid.ApprovedBy != p.operator {
		t.Errorf("the payout records %s as approver, want the operator %s", paid.ApprovedBy, p.operator)
	}
	if n := p.payoutsFor(t, made.ID); n != 1 {
		t.Fatalf("one approval made %d payout(s), want 1", n)
	}

	// Approving the same request again pays nothing more. The key is the
	// database's own derivation from the request (C-5), so a replay cannot
	// be given a different one by a caller who wanted a second payment.
	if _, err := p.approvals.Approve(p.ctx, payout.Approval{Request: made.ID, Operator: p.operator}); err == nil {
		t.Error("a second approval of one request was accepted")
	}
	if n := p.payoutsFor(t, made.ID); n != 1 {
		t.Errorf("after a replayed approval the request has %d payout(s), want 1", n)
	}

	// And the rule under all of it, asserted against the database rather
	// than against this package's memory of it.
	if _, err := p.pool.Exec(p.ctx, `
		insert into cashback.payout (request_id, brand_id, amount_minor, currency, rail, state)
		values ($1, $2, $3, 'EUR', 'stub', 'submitted')`, made.ID, scenarioBrand, asked); err == nil {
		t.Error("a payout was inserted with no approver; C-4 says the row IS the approval")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23502" {
			t.Errorf("inserting a payout with no approver raised %v, want SQLSTATE 23502", err)
		}
	}

	// A refusal returns the reserved money to confirmed, and records why.
	refused := p.request(t, p.destination)
	reserved := p.confirmed(t, p.member)
	if _, err := p.rejections.Reject(p.ctx, payout.Rejection{
		Request: refused.ID, Operator: p.operator, Reason: "the destination could not be verified",
	}); err != nil {
		t.Fatalf("Reject(): %v", err)
	}
	if after := p.confirmed(t, p.member); after <= reserved {
		t.Errorf("confirmed balance is %d after the refusal, was %d while reserved; a refused withdrawal returns the money",
			after, reserved)
	}
	if n := p.payoutsFor(t, refused.ID); n != 0 {
		t.Errorf("a refused request has %d payout(s), want 0", n)
	}
}
