package db_test

// Shared fixtures for the cashback invariant suite. Every test here runs
// inside a transaction that is rolled back (beginTx), so the chain below is
// rebuilt per test and no case can see another's rows.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// cashbackFixtures is a fully valid catalogue chain: network -> publisher
// account -> merchant (with copy and reach) -> route -> offer, plus a
// member account to click it. Illegal-write tests break one link at a time.
type cashbackFixtures struct {
	suffix            string
	accountID         string
	networkID         string
	networkAccountID  string
	merchantID        string
	merchantNetworkID string
	placeID           string
	offerID           string

	// Set by seedCashbackEvidence only.
	clickID    string
	clickRef   string
	externalID string
	networkTxn string

	// Set by seedCashbackEntry only.
	entryID string

	// Set by seedCashbackWithdrawal only.
	approverID    string
	destinationID string
	requestID     string
}

// cashbackApproverRole is the role a payout approver holds in the fixtures.
// Migration 0014 requires only a named human (C-4, a NOT NULL column);
// 0019 tightens that to the operator role, checked by payout_insert_guard
// with a locking read. This constant is the single place that moves.
const cashbackApproverRole = "reader"

func seedCashback(t *testing.T, tx pgx.Tx) cashbackFixtures {
	t.Helper()
	ctx := context.Background()
	suffix := randomSuffix(t)
	f := cashbackFixtures{suffix: suffix}

	// A member: an ordinary account, no role required to browse or click.
	err := tx.QueryRow(ctx,
		`insert into account (email, display_name) values ($1, $2) returning id`,
		"member-"+suffix+"@example.test", "Test Member "+suffix,
	).Scan(&f.accountID)
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}

	f.networkID = "fixture_" + suffix
	_, err = tx.Exec(ctx,
		`insert into cashback.network
		     (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_second, active)
		 values ($1, $2, 'clickref', 31, 5, true)`,
		f.networkID, "Fixture Network "+suffix)
	if err != nil {
		t.Fatalf("seed network: %v", err)
	}

	err = tx.QueryRow(ctx,
		`insert into cashback.network_account
		     (network_id, external_publisher_id, credential_ref, active)
		 values ($1, $2, $3, true) returning id`,
		f.networkID, "publisher-"+suffix, "network.fixture.credential",
	).Scan(&f.networkAccountID)
	if err != nil {
		t.Fatalf("seed network_account: %v", err)
	}

	err = tx.QueryRow(ctx,
		`insert into cashback.merchant (slug, country, source_language_code)
		 values ($1, 'DE', 'de') returning id`,
		"merchant-"+suffix,
	).Scan(&f.merchantID)
	if err != nil {
		t.Fatalf("seed merchant: %v", err)
	}

	// The route: this retailer as reached through this network. A second
	// route through another network is what several tests add on top.
	err = tx.QueryRow(ctx,
		`insert into cashback.merchant_network
		     (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload, preferred)
		 values ('fixture', $1, $2, $3, now(), '{"id":"fixture"}'::jsonb, true) returning id`,
		f.merchantID, f.networkID, "ext-"+suffix,
	).Scan(&f.merchantNetworkID)
	if err != nil {
		t.Fatalf("seed merchant_network: %v", err)
	}

	_, err = tx.Exec(ctx,
		`insert into cashback.merchant_copy (merchant_id, language_code, name, summary)
		 values ($1, 'de', $2, 'Testhändler')`,
		f.merchantID, "Testhändler "+suffix)
	if err != nil {
		t.Fatalf("seed merchant_copy: %v", err)
	}

	// Munich: the alpha reference place seeded by 0002.
	if err := tx.QueryRow(ctx, `select id from place where slug = 'munich'`).Scan(&f.placeID); err != nil {
		t.Fatalf("reading the munich reference place: %v", err)
	}
	_, err = tx.Exec(ctx,
		`insert into cashback.merchant_place (merchant_id, place_id) values ($1, $2)`,
		f.merchantID, f.placeID)
	if err != nil {
		t.Fatalf("seed merchant_place: %v", err)
	}

	err = tx.QueryRow(ctx,
		`insert into cashback.offer
		     (merchant_network_id, rate_kind, rate_bps, member_share_bps, conditions, deeplink_template)
		 values ($1, 'percent', 400, 5000, 'Excludes gift cards', 'https://example.test/deep?clickref={ref}')
		 returning id`,
		f.merchantNetworkID,
	).Scan(&f.offerID)
	if err != nil {
		t.Fatalf("seed offer: %v", err)
	}

	return f
}

// seedCashbackEvidence extends the catalogue chain with the evidence a
// credit rests on: a tracked click, and one confirmed network report
// carrying that click's reference. Both tables are immutable, so a test
// that needs a different report inserts a superseding row rather than
// editing this one.
func seedCashbackEvidence(t *testing.T, tx pgx.Tx) cashbackFixtures {
	t.Helper()
	ctx := context.Background()
	f := seedCashback(t, tx)

	// 32 hex characters: comfortably past the 22 the click_ref check
	// requires, and drawn from crypto/rand like the real reference.
	f.clickRef = randomSuffix(t) + randomSuffix(t)
	err := tx.QueryRow(ctx,
		`insert into cashback.click
		     (click_ref, account_id, offer_id, rate_snapshot, member_share_bps_snapshot, context_digest)
		 values ($1, $2, $3, $4::jsonb, 5000, 'ctx-digest') returning id`,
		f.clickRef, f.accountID, f.offerID,
		`{"rate_kind":"percent","rate_bps":400,"member_share_bps":5000}`,
	).Scan(&f.clickID)
	if err != nil {
		t.Fatalf("seed click: %v", err)
	}

	f.externalID = "txn-" + f.suffix
	err = tx.QueryRow(ctx,
		`insert into cashback.network_transaction
		     (network_id, network_account_id, external_id, click_ref, status_raw, status,
		      sale_amount_minor, commission_minor, currency, transacted_at,
		      query_window_start, query_window_end, raw_payload)
		 values ($1, $2, $3, $4, 'approved', 'confirmed', 10000, 500, 'EUR', now(),
		         now() - interval '1 day', now(), '{"id":"txn"}'::jsonb)
		 returning id`,
		f.networkID, f.networkAccountID, f.externalID, f.clickRef,
	).Scan(&f.networkTxn)
	if err != nil {
		t.Fatalf("seed network_transaction: %v", err)
	}

	return f
}

// seedCashbackEntry extends the evidence chain with a confirmed credit and
// the transition and ledger posting that made it real. It is the shape
// every wallet and payout test starts from.
func seedCashbackEntry(t *testing.T, tx pgx.Tx) cashbackFixtures {
	t.Helper()
	ctx := context.Background()
	f := seedCashbackEvidence(t, tx)

	err := tx.QueryRow(ctx,
		`insert into cashback.entry
		     (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency)
		 values ('fixture', $1, $2, $3, 'confirmed', 250, 'EUR') returning id`,
		f.accountID, f.networkTxn, f.clickID,
	).Scan(&f.entryID)
	if err != nil {
		t.Fatalf("seed entry: %v", err)
	}

	var transitionID string
	err = tx.QueryRow(ctx,
		`insert into cashback.entry_transition (entry_id, from_state, to_state, ledger_transfer_ref)
		 values ($1, null, 'confirmed', $2) returning id`,
		f.entryID, "transfer-"+f.suffix,
	).Scan(&transitionID)
	if err != nil {
		t.Fatalf("seed entry_transition: %v", err)
	}

	_, err = tx.Exec(ctx,
		`insert into cashback.ledger_link (transition_id, entry_id, ledger_transfer_ref)
		 values ($1, $2, $3)`, transitionID, f.entryID, "transfer-"+f.suffix)
	if err != nil {
		t.Fatalf("seed ledger_link: %v", err)
	}

	return f
}

// seedCashbackWithdrawal extends the earnings chain with an approver, a
// verified destination and a withdrawal request whose reservation transfer
// already exists (D9). What it deliberately does NOT create is the payout:
// every payout test writes that itself, because the write is the invariant.
func seedCashbackWithdrawal(t *testing.T, tx pgx.Tx) cashbackFixtures {
	t.Helper()
	ctx := context.Background()
	f := seedCashbackEntry(t, tx)

	err := tx.QueryRow(ctx,
		`insert into account (email, display_name, role) values ($1, $2, $3) returning id`,
		"approver-"+f.suffix+"@example.test", "Test Approver "+f.suffix, cashbackApproverRole,
	).Scan(&f.approverID)
	if err != nil {
		t.Fatalf("seed approver: %v", err)
	}

	err = tx.QueryRow(ctx,
		`insert into cashback.payout_destination
		     (account_id, kind, details_ref, verified_at, verified_method)
		 values ($1, 'manual', $2, now(), 'micro_deposit') returning id`,
		f.accountID, "vault/destination/"+f.suffix,
	).Scan(&f.destinationID)
	if err != nil {
		t.Fatalf("seed payout_destination: %v", err)
	}

	err = tx.QueryRow(ctx,
		`insert into cashback.withdrawal_request
		     (account_id, destination_id, amount_minor, currency, reserved_transfer_ref)
		 values ($1, $2, 250, 'EUR', $3) returning id`,
		f.accountID, f.destinationID, "reserve-"+f.suffix,
	).Scan(&f.requestID)
	if err != nil {
		t.Fatalf("seed withdrawal_request: %v", err)
	}

	return f
}
