package earnings_test

// The full earn journey against the real schema, with the published rate
// changed in the middle of it (T073, FR-013, FR-040, FR-043, SC-003).
//
// Every step is the production code path: the click is issued through
// clickout, the report is stored as evidence, the attribution is the
// matcher's, the share is computed by share.go from the SNAPSHOT the click
// carries, the entry is opened and confirmed by the state machine, and the
// money moves in the Postgres ledger. Nothing here is a stand-in except the
// deeplink builder, which would otherwise be an HTTP call.
//
// The property this exists for is one line of FR-013: the rate as published
// AT THE CLICK governs the credit. So the offer's published rate is changed
// between the click and the report - to a fifth of what it was, and a sixth
// of the member's share - and the credit is asserted against the old one. A
// test that changed nothing would pass whether or not the snapshot was read.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue"
	cataloguestore "github.com/Nomos-N4s/apivo-news/internal/cashback/catalogue/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/clickout"
	clickoutstore "github.com/Nomos-N4s/apivo-news/internal/cashback/clickout/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/earnings"
	earningsstore "github.com/Nomos-N4s/apivo-news/internal/cashback/earnings/store"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/wallet/postgres"
	"github.com/Nomos-N4s/apivo-news/internal/platform/db"
	"github.com/Nomos-N4s/apivo-news/internal/platform/money"
)

// tag keeps one run's identifiers from colliding with another's.
func tag(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(raw)
}

// The band as published when the member clicks, and the band published
// afterwards. Deliberately far apart: a credit computed from the second
// would be a fifth of the commission and a sixth of the share, so no
// rounding accident can make the two agree.
const (
	clickTimeRateBps   = 500
	clickTimeShareBps  = 6000
	afterwardsRateBps  = 100
	afterwardsShareBps = 1000
	reportedSaleMinor  = 4999
	reportedCommission = 249
	// 249 * 0.6 = 149.4, rounded to the member's favour (Q4).
	clickTimeMemberShare = 150
	// What the same commission would earn under the band published after the
	// click: 249 * 0.1 = 24.9, rounded the same way. The number this test
	// must never see on the entry.
	afterwardsMemberShare = 25
	houseReceivable       = "network-receivable-journey"
)

// theJourney is everything one run of the journey needs to talk to.
type theJourney struct {
	ctx       context.Context
	tx        pgx.Tx
	networkID string
	publisher pgtype.UUID
	operator  uuid.UUID
	member    uuid.UUID
	offer     uuid.UUID
	ledger    *postgres.Ledger
}

// staticDeeplinks builds a redirect without an adapter. The redirect itself
// is T064's to test; what matters here is that one exists, because the click
// is not recorded without it.
type staticDeeplinks struct{}

func (staticDeeplinks) Build(_ context.Context, _ networks.DeeplinkTarget, ref networks.IssuedClickRef) (string, error) {
	return "https://example.test/deeplink?clickref=" + ref.Ref(), nil
}

// begin opens the transaction the whole journey runs in and rolls back
// afterwards. One transaction for all of it, because that is what the
// production path does: the click, the credit and their events are one
// commit each, and the ledger is co-located.
func begin(t *testing.T) *theJourney {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `docker compose up -d postgres` and set it to exercise the journey")
	}
	if err := db.Migrate(url); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		pool.Close()
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback(ctx)
		pool.Close()
	})
	// Point C-1's own view at the ledger this journey posts in, for this
	// transaction only. Migration 0022 documents this as the supported way:
	// the check follows the ledger rather than the ledger being shaped to
	// fit the check.
	if _, err := tx.Exec(ctx, `select set_config('cashback.ledger_schema', 'ledger', true)`); err != nil {
		t.Fatalf("pointing the zero-sum check at the ledger: %v", err)
	}
	return &theJourney{ctx: ctx, tx: tx, ledger: postgres.New(tx)}
}

// seed writes the world the journey happens in: a network, the publisher
// account reports arrive on, an operator to import a statement, a member, a
// retailer reachable through that network, and the band they clicked.
func (j *theJourney) seed(t *testing.T) {
	t.Helper()
	id := tag(t)
	j.networkID = "journey_" + id

	if _, err := j.tx.Exec(j.ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_second, active)
		values ($1, 'Journey Network', 'clickref', 31, 5, true)`, j.networkID); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	if err := j.tx.QueryRow(j.ctx, `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active)
		values ($1, 'publisher-1', 'config:networks.journey.credential', true)
		returning id`, j.networkID).Scan(&j.publisher); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}
	if err := j.tx.QueryRow(j.ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Journey Member', 'reader') returning id`,
		"member-"+id+"@example.test").Scan(&j.member); err != nil {
		t.Fatalf("seeding the member: %v", err)
	}
	if err := j.tx.QueryRow(j.ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Journey Operator', 'operator') returning id`,
		"operator-"+id+"@example.test").Scan(&j.operator); err != nil {
		t.Fatalf("seeding the operator: %v", err)
	}
	var merchant, route uuid.UUID
	if err := j.tx.QueryRow(j.ctx, `
		insert into cashback.merchant (slug, country, source_language_code, status)
		values ($1, 'DE', 'de', 'active') returning id`, "journey-"+id).Scan(&merchant); err != nil {
		t.Fatalf("seeding the merchant: %v", err)
	}
	if err := j.tx.QueryRow(j.ctx, `
		insert into cashback.merchant_network
		    (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload, status, preferred)
		values ('apivo-de', $1, $2, $3, now(), '{"id":"journey"}'::jsonb, 'active', true) returning id`,
		merchant, j.networkID, "ext-"+id).Scan(&route); err != nil {
		t.Fatalf("seeding the route: %v", err)
	}
	if err := j.tx.QueryRow(j.ctx, `
		insert into cashback.offer
		    (merchant_network_id, rate_kind, rate_bps, member_share_bps, valid_from, deeplink_template)
		values ($1, 'percent', $2, $3, now() - interval '1 day', 'https://example.test/deeplink?ref={ref}')
		returning id`, route, clickTimeRateBps, clickTimeShareBps).Scan(&j.offer); err != nil {
		t.Fatalf("seeding the offer: %v", err)
	}
}

// clickOut issues a tracked redirect the way the endpoint does, through the
// recorder the composition root wires: the one that opens its own
// transaction so the click and its event commit together.
func (j *theJourney) clickOut(t *testing.T) clickout.Click {
	t.Helper()
	clicks, err := clickout.NewAnnouncedClicks(j.tx)
	if err != nil {
		t.Fatalf("NewAnnouncedClicks(): %v", err)
	}
	clickouts, err := clickout.NewClickOuts(
		catalogue.NewOfferReader(cataloguestore.New(j.tx)), clicks, staticDeeplinks{})
	if err != nil {
		t.Fatalf("NewClickOuts(): %v", err)
	}
	issued, err := clickouts.Issue(j.ctx, clickout.Request{Member: j.member, OfferID: j.offer})
	if err != nil {
		t.Fatalf("Issue(): %v", err)
	}
	return issued.Click
}

// publishANewRate changes the band the member clicked, and checks the change
// took. Without that check a failure to update would look exactly like the
// snapshot governing, and this test would pass for the wrong reason.
func (j *theJourney) publishANewRate(t *testing.T) {
	t.Helper()
	if _, err := j.tx.Exec(j.ctx, `
		update cashback.offer set rate_bps = $2, member_share_bps = $3 where id = $1`,
		j.offer, afterwardsRateBps, afterwardsShareBps); err != nil {
		t.Fatalf("publishing the new rate: %v", err)
	}
	live, err := catalogue.NewOfferReader(cataloguestore.New(j.tx)).LiveOffer(j.ctx, j.offer, time.Now())
	if err != nil {
		t.Fatalf("re-reading the offer: %v", err)
	}
	if live.Rate.Percent != afterwardsRateBps || live.MemberShare != afterwardsShareBps {
		t.Fatalf("the catalogue still publishes %d bps at a %d bps share; the rate change did not take",
			live.Rate.Percent, live.MemberShare)
	}
}

// reports stores what the network said about the purchase, citing the click.
func (j *theJourney) reports(t *testing.T, ref string, status networks.Status) uuid.UUID {
	t.Helper()
	at := time.Now().Add(-time.Hour)
	var id uuid.UUID
	if err := j.tx.QueryRow(j.ctx, `
		insert into cashback.network_transaction (
			network_id, network_account_id, external_id, click_ref,
			status_raw, status, sale_amount_minor, commission_minor, currency,
			transacted_at, retrieved_at, query_window_start, query_window_end, raw_payload)
		values ($1, $2, $3, $4, $5, $5, $6, $7, 'EUR', $8, now(), $9, $10, $11)
		returning id`,
		j.networkID, j.publisher, "JOURNEY-"+tag(t), ref, string(status),
		reportedSaleMinor, reportedCommission,
		at, at.Add(-48*time.Hour), at.Add(48*time.Hour), []byte(`{"transaction_id":"JOURNEY"}`),
	).Scan(&id); err != nil {
		t.Fatalf("storing the report: %v", err)
	}
	return id
}

// entries builds the state machine over this journey's transaction and
// ledger, and the confirmer over the same.
func (j *theJourney) entries(t *testing.T) (*earnings.Entries, *earnings.Confirmations) {
	t.Helper()
	queries := earningsstore.New(j.tx)
	machine, err := earnings.NewEntries(queries, j.ledger, houseReceivable)
	if err != nil {
		t.Fatalf("NewEntries(): %v", err)
	}
	confirmations, err := earnings.NewConfirmations(machine, queries)
	if err != nil {
		t.Fatalf("NewConfirmations(): %v", err)
	}
	return machine, confirmations
}

// balance answers what one of the member's stage accounts holds.
func (j *theJourney) balance(t *testing.T, stage wallet.Stage) int64 {
	t.Helper()
	account, err := j.ledger.EnsureAccount(j.ctx, wallet.MemberAccount(j.member, stage), "EUR")
	if err != nil {
		t.Fatalf("resolving the %s account: %v", stage, err)
	}
	held, err := j.ledger.Balance(j.ctx, account, "EUR")
	if err != nil {
		t.Fatalf("reading the %s balance: %v", stage, err)
	}
	return held.Minor
}

// wantZeroSum is C-1 read from the deployed check rather than recomputed
// here: whatever the journey posted, no money was created or destroyed
// (SC-003).
func (j *theJourney) wantZeroSum(t *testing.T) {
	t.Helper()
	rows, err := j.tx.Query(j.ctx, `select currency, net_minor from cashback.ledger_zero_sum`)
	if err != nil {
		t.Fatalf("reading the zero-sum check: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var currency string
		var net int64
		if err := rows.Scan(&currency, &net); err != nil {
			t.Fatalf("scanning the zero-sum check: %v", err)
		}
		seen++
		if net != 0 {
			t.Errorf("the ledger is out by %d in %s; money was created or destroyed inside it", net, currency)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the zero-sum check: %v", err)
	}
	if seen == 0 {
		t.Error("the zero-sum check saw no currency at all, so it asserted nothing")
	}
}

// announcedAbout reads the events this journey appended about one subject,
// grouped by type.
//
// Not ordered, deliberately. Every event here is appended inside ONE
// transaction, so they all carry the same occurred_at and the stream's own
// order between them is decided by a random id. Asserting on that order
// would be asserting on gen_random_uuid.
func (j *theJourney) announcedAbout(t *testing.T, subject uuid.UUID) map[string][]map[string]any {
	t.Helper()
	rows, err := j.tx.Query(j.ctx, `
		select type, payload from domain_event where subject = $1`, subject.String())
	if err != nil {
		t.Fatalf("reading the outbox: %v", err)
	}
	defer rows.Close()
	out := map[string][]map[string]any{}
	for rows.Next() {
		var eventType string
		var raw []byte
		if err := rows.Scan(&eventType, &raw); err != nil {
			t.Fatalf("scanning an event: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("the payload is not a JSON object: %v", err)
		}
		out[eventType] = append(out[eventType], payload)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the outbox: %v", err)
	}
	return out
}

// TestTheClickTimeRateGovernsTheCredit walks the whole journey with the
// published rate changed in the middle of it.
func TestTheClickTimeRateGovernsTheCredit(t *testing.T) {
	j := begin(t)
	j.seed(t)

	// 1. The member clicks, and the band as published NOW is snapshotted on
	//    the click (FR-013, FR-020).
	click := j.clickOut(t)
	if click.Promised.Rate.Percent != clickTimeRateBps || click.Promised.MemberShare != clickTimeShareBps {
		t.Fatalf("the click snapshotted %d bps at a %d bps share, want %d at %d",
			click.Promised.Rate.Percent, click.Promised.MemberShare, clickTimeRateBps, clickTimeShareBps)
	}

	// 2. The rate is republished, lower, before the purchase is reported.
	j.publishANewRate(t)

	// 3. The network reports the purchase, citing the reference it was sent.
	report := j.reports(t, click.Ref.Ref(), networks.StatusPending)

	// 4. Attribution: which click earned this purchase (T067).
	clicks, err := clickout.NewClicks(clickoutstore.New(j.tx))
	if err != nil {
		t.Fatalf("NewClicks(): %v", err)
	}
	matcher, err := earnings.NewMatcher(clicks, earningsstore.New(j.tx))
	if err != nil {
		t.Fatalf("NewMatcher(): %v", err)
	}
	// What the network ECHOED, built as such: the matcher takes a reported
	// reference rather than a string, so "no reference" stays a branch a
	// caller has to handle.
	reported := networks.NewClickRef(click.Ref.Ref())
	attributed, err := matcher.Match(j.ctx, earnings.Report{ID: report, Ref: reported})
	if err != nil {
		t.Fatalf("Match(): %v", err)
	}
	if !attributed.Matched || attributed.Click.ID != click.ID {
		t.Fatalf("the report matched %v (matched=%v), want the click %s",
			attributed.Click.ID, attributed.Matched, click.ID)
	}

	// 5. The share, computed from the SNAPSHOT the matched click carries and
	//    not from the catalogue as it now stands (T068, FR-013).
	commission, err := money.New(reportedCommission, "EUR")
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	share, err := earnings.ShareOf(commission, attributed.Click.Promised)
	if err != nil {
		t.Fatalf("ShareOf(): %v", err)
	}
	if share.Member.Minor != clickTimeMemberShare {
		t.Fatalf("the member's share is %d, want %d (the click-time band); %d would be the band published after the click",
			share.Member.Minor, clickTimeMemberShare, afterwardsMemberShare)
	}

	// 6. The entry, opened pending because that is what the network said.
	machine, confirmations := j.entries(t)
	opened, err := machine.Open(j.ctx, j.tx, earnings.Credit{
		Member: j.member,
		Brand:  "apivo-de",
		Report: report,
		Click:  click.ID,
		State:  earnings.StatePending,
		Amount: share.Member,
		Reason: "the network reported the purchase",
	})
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	if opened.Amount.Minor != clickTimeMemberShare {
		t.Errorf("the entry holds %d, want %d", opened.Amount.Minor, clickTimeMemberShare)
	}
	if got := j.balance(t, wallet.StagePending); got != clickTimeMemberShare {
		t.Errorf("the member's pending balance is %d, want %d", got, clickTimeMemberShare)
	}
	j.wantZeroSum(t)

	// 7. The statement arrives and the network approves. Both halves, which
	//    is what FR-043 asks for.
	j.importsAStatement(t)
	confirmed, err := confirmations.Confirm(j.ctx, j.tx, opened, networks.StatusConfirmed, report)
	if err != nil {
		t.Fatalf("Confirm(): %v", err)
	}
	if confirmed.State != earnings.StateConfirmed {
		t.Fatalf("the entry is %s, want confirmed", confirmed.State)
	}

	// The member's total did not change - only which bucket counts toward
	// the withdrawal threshold (FR-050).
	if got := j.balance(t, wallet.StagePending); got != 0 {
		t.Errorf("the member's pending balance is %d after confirmation, want 0", got)
	}
	if got := j.balance(t, wallet.StageConfirmed); got != clickTimeMemberShare {
		t.Errorf("the member's confirmed balance is %d, want %d", got, clickTimeMemberShare)
	}
	j.wantZeroSum(t)

	// 8. And the stream says the same as the tables (T076).
	announced := j.announcedAbout(t, opened.ID)
	created := announced[earnings.TypeEntryCreated]
	if len(created) != 1 {
		t.Fatalf("announced %s %d times, want once", earnings.TypeEntryCreated, len(created))
	}
	amount, shaped := created[0]["amount"].(map[string]any)
	if !shaped || amount["minor"] != float64(clickTimeMemberShare) {
		t.Errorf("the creation announced amount %#v, want %d", created[0]["amount"], clickTimeMemberShare)
	}
	// Two moves: the opening, and the confirmation. Both name a transfer,
	// which is what lets a consumer follow the money without reading this
	// module's tables.
	moves := announced[earnings.TypeEntryStateChanged]
	if len(moves) != 2 {
		t.Fatalf("announced %s %d times, want twice (the opening and the confirmation)",
			earnings.TypeEntryStateChanged, len(moves))
	}
	destinations := map[string]bool{}
	for _, move := range moves {
		destinations[move["to"].(string)] = true
		if ref, named := move["ledger_transfer_ref"].(string); !named || ref == "" {
			t.Errorf("a move to %v names no transfer, so nothing can trace the money from the stream", move["to"])
		}
	}
	if !destinations[string(earnings.StatePending)] || !destinations[string(earnings.StateConfirmed)] {
		t.Errorf("the moves announced went to %v, want pending and confirmed", destinations)
	}
	// The click's own event is about the CLICK. Finding it here would mean a
	// subject was written that nothing about this entry should carry.
	if announced[clickout.TypeClickCreated] != nil {
		t.Error("the click's event was announced about the entry, not about the click")
	}
	if len(j.announcedAbout(t, click.ID)[clickout.TypeClickCreated]) != 1 {
		t.Error("the click that started all of this was not announced about itself")
	}
}

// TestTheCreditIsRefusedWithoutAReconciledStatement is FR-043's other half,
// on the same journey. The network's word alone is not enough.
func TestTheCreditIsRefusedWithoutAReconciledStatement(t *testing.T) {
	j := begin(t)
	j.seed(t)

	click := j.clickOut(t)
	report := j.reports(t, click.Ref.Ref(), networks.StatusConfirmed)
	commission, err := money.New(reportedCommission, "EUR")
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	share, err := earnings.ShareOf(commission, click.Promised)
	if err != nil {
		t.Fatalf("ShareOf(): %v", err)
	}

	machine, confirmations := j.entries(t)
	opened, err := machine.Open(j.ctx, j.tx, earnings.Credit{
		Member: j.member, Brand: "apivo-de", Report: report, Click: click.ID,
		State: earnings.StatePending, Amount: share.Member,
	})
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}

	// No statement imported, so nothing says the money arrived.
	_, err = confirmations.Confirm(j.ctx, j.tx, opened, networks.StatusConfirmed, report)
	if !errors.Is(err, earnings.ErrNotReconciled) {
		t.Fatalf("Confirm() error = %v, want one wrapping %v", err, earnings.ErrNotReconciled)
	}
	if got := j.balance(t, wallet.StageConfirmed); got != 0 {
		t.Errorf("the member's confirmed balance is %d with no statement imported, want 0", got)
	}
}

// importsAStatement records the reconciliation run covering the purchase,
// with the operator who imported it - reconciliation is an accounting act
// with a person behind it (US6).
func (j *theJourney) importsAStatement(t *testing.T) {
	t.Helper()
	if _, err := j.tx.Exec(j.ctx, `
		insert into cashback.reconciliation_run
		    (network_account_id, statement_period_start, statement_period_end, imported_by, raw_statement)
		values ($1, now() - interval '30 days', now() + interval '1 day', $2, '{"statement":"journey"}'::jsonb)`,
		j.publisher, j.operator); err != nil {
		t.Fatalf("importing the statement: %v", err)
	}
}
