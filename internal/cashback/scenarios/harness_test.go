package scenarios_test

// The world each scenario happens in, and the production paths it is driven
// through.
//
// Every helper here composes the same constructors the composition root
// composes. Nothing is a stand-in except the deeplink builder, which would
// otherwise be an HTTP call to a network that does not exist - and what a
// deeplink builder returns is the adapter's own contract test, not an
// acceptance gate.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

// The band the member clicks, and the one published afterwards. Far apart on
// purpose: a credit computed from the second is a fifth of the commission at
// a sixth of the share, so no rounding accident can make them agree.
const (
	clickTimeRateBps   = 500
	clickTimeShareBps  = 6000
	laterRateBps       = 100
	laterShareBps      = 1000
	reportedSaleMinor  = 4999
	reportedCommission = 249
	// 249 × 0.6 = 149.4, rounded to the member's favour (Q4).
	memberShareMinor = 150
	// What the later band would have earned. The number no scenario may see.
	laterShareMinor = 25

	scenarioBrand    = "scenario-brand"
	houseReceivable  = "network-receivable-scenarios"
	scenarioCurrency = money.Currency("EUR")
)

// world is one scenario's transaction and everything seeded inside it.
type world struct {
	ctx       context.Context
	tx        pgx.Tx
	networkID string
	publisher pgtype.UUID
	operator  uuid.UUID
	member    uuid.UUID
	merchant  uuid.UUID
	route     uuid.UUID
	offer     uuid.UUID
	ledger    *postgres.Ledger
}

// suffix keeps one run's identifiers from colliding with another's, so
// scenarios may run beside each other and beside every other suite.
func suffix(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(raw)
}

// staticDeeplinks stands in for a network adapter's redirect builder. The
// click is not recorded without one, and what it returns is T064's to check.
type staticDeeplinks struct{}

func (staticDeeplinks) Build(_ context.Context, _ networks.DeeplinkTarget, ref networks.IssuedClickRef) (string, error) {
	return "https://example.test/deeplink?clickref=" + ref.Ref(), nil
}

// begin opens the transaction a scenario runs in, and rolls it back however
// the scenario ends.
func begin(t *testing.T) *world {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; run `make cashback-up` and set it to run the acceptance gates")
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
	// Point C-1's own view at the ledger this scenario posts in, for this
	// transaction only. Migration 0022 documents this as the supported way:
	// the check follows the ledger rather than the ledger being shaped to
	// fit the check.
	if _, err := tx.Exec(ctx, `select set_config('cashback.ledger_schema', 'ledger', true)`); err != nil {
		t.Fatalf("pointing the zero-sum check at the ledger: %v", err)
	}
	return &world{ctx: ctx, tx: tx, ledger: postgres.New(tx)}
}

// seed writes the world: a network, the publisher account reports arrive on,
// an operator, a member who has opted in, a retailer reachable through that
// network, and the band they are about to click.
func (w *world) seed(t *testing.T) *world {
	t.Helper()
	id := suffix(t)
	w.networkID = "scenario_" + id

	if _, err := w.tx.Exec(w.ctx, `
		insert into cashback.network (id, display_name, click_ref_param, max_query_window_days, rate_limit_per_minute, active)
		values ($1, 'Scenario Network', 'clickref', 31, 300, true)`, w.networkID); err != nil {
		t.Fatalf("seeding the network: %v", err)
	}
	if err := w.tx.QueryRow(w.ctx, `
		insert into cashback.network_account (network_id, external_publisher_id, credential_ref, active)
		values ($1, 'publisher-1', 'config:networks.scenario.credential', true)
		returning id`, w.networkID).Scan(&w.publisher); err != nil {
		t.Fatalf("seeding the publisher account: %v", err)
	}
	w.member = w.account(t, "member-"+id+"@example.test", "Scenario Member", "reader")
	w.operator = w.account(t, "operator-"+id+"@example.test", "Scenario Operator", "operator")
	if _, err := w.tx.Exec(w.ctx, `
		insert into cashback.participation (account_id, brand_id, terms_version, default_currency)
		values ($1, $2, '1.0.0', 'EUR')`, w.member, scenarioBrand); err != nil {
		t.Fatalf("opting the member in: %v", err)
	}
	if err := w.tx.QueryRow(w.ctx, `
		insert into cashback.merchant (slug, country, source_language_code, status)
		values ($1, 'DE', 'de', 'active') returning id`, "scenario-"+id).Scan(&w.merchant); err != nil {
		t.Fatalf("seeding the merchant: %v", err)
	}
	if err := w.tx.QueryRow(w.ctx, `
		insert into cashback.merchant_network
		    (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload, status, preferred)
		values ($1, $2, $3, $4, now(), '{"id":"scenario"}'::jsonb, 'active', true) returning id`,
		scenarioBrand, w.merchant, w.networkID, "ext-"+id).Scan(&w.route); err != nil {
		t.Fatalf("seeding the route: %v", err)
	}
	if err := w.tx.QueryRow(w.ctx, `
		insert into cashback.offer
		    (merchant_network_id, rate_kind, rate_bps, member_share_bps, valid_from, deeplink_template)
		values ($1, 'percent', $2, $3, now() - interval '1 day', 'https://example.test/deeplink?ref={ref}')
		returning id`, w.route, clickTimeRateBps, clickTimeShareBps).Scan(&w.offer); err != nil {
		t.Fatalf("seeding the offer: %v", err)
	}
	return w
}

// account inserts one person.
func (w *world) account(t *testing.T, email, name, role string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := w.tx.QueryRow(w.ctx, `
		insert into public.account (email, display_name, role)
		values ($1, $2, $3) returning id`, email, name, role).Scan(&id); err != nil {
		t.Fatalf("seeding %s: %v", role, err)
	}
	return id
}

// clickOut issues a tracked redirect the way the endpoint does, through the
// recorder that commits the click and its event together.
func (w *world) clickOut(t *testing.T) clickout.Click {
	t.Helper()
	clicks, err := clickout.NewAnnouncedClicks(w.tx)
	if err != nil {
		t.Fatalf("NewAnnouncedClicks(): %v", err)
	}
	clickouts, err := clickout.NewClickOuts(
		catalogue.NewOfferReader(cataloguestore.New(w.tx)), clicks, staticDeeplinks{})
	if err != nil {
		t.Fatalf("NewClickOuts(): %v", err)
	}
	issued, err := clickouts.Issue(w.ctx, clickout.Request{Member: w.member, OfferID: w.offer})
	if err != nil {
		t.Fatalf("Issue(): %v", err)
	}
	return issued.Click
}

// republish changes the band the member clicked, and proves the change took.
// Without that proof a failed UPDATE would look exactly like the snapshot
// governing, and the scenario would pass for the wrong reason.
func (w *world) republish(t *testing.T) {
	t.Helper()
	if _, err := w.tx.Exec(w.ctx, `
		update cashback.offer set rate_bps = $2, member_share_bps = $3 where id = $1`,
		w.offer, laterRateBps, laterShareBps); err != nil {
		t.Fatalf("publishing the new rate: %v", err)
	}
	live, err := catalogue.NewOfferReader(cataloguestore.New(w.tx)).LiveOffer(w.ctx, w.offer, time.Now())
	if err != nil {
		t.Fatalf("re-reading the offer: %v", err)
	}
	if live.Rate.Percent != laterRateBps || live.MemberShare != laterShareBps {
		t.Fatalf("the catalogue still publishes %d bps at a %d bps share; the rate change did not take",
			live.Rate.Percent, live.MemberShare)
	}
}

// reported is one purchase as the network described it.
type reported struct {
	id       uuid.UUID
	external string
}

// reports stores a PENDING report citing a reference: what the network says
// first, and the state every purchase passes through. reportsAs is for the
// gates that need another status, another id or another amount.
func (w *world) reports(t *testing.T, ref string) reported {
	t.Helper()
	return w.reportsAs(t, "SCEN-"+suffix(t), ref, networks.StatusPending, reportedCommission)
}

// reportsAs is reports with the network's own id and the commission stated,
// for the scenarios that need a second report about the same purchase or a
// smaller amount than the statement.
func (w *world) reportsAs(t *testing.T, external, ref string, status networks.Status, commission int64) reported {
	t.Helper()
	at := time.Now().Add(-time.Hour)
	var id uuid.UUID
	var clickRef any
	if ref != "" {
		clickRef = ref
	}
	if err := w.tx.QueryRow(w.ctx, `
		insert into cashback.network_transaction (
			network_id, network_account_id, external_id, click_ref,
			status_raw, status, sale_amount_minor, commission_minor, currency,
			transacted_at, retrieved_at, query_window_start, query_window_end, raw_payload)
		values ($1, $2, $3, $4, $5, $5, $6, $7, 'EUR', $8, now(), $9, $10, $11)
		returning id`,
		w.networkID, w.publisher, external, clickRef, string(status),
		reportedSaleMinor, commission,
		at, at.Add(-48*time.Hour), at.Add(48*time.Hour), []byte(`{"transaction_id":"SCENARIO"}`),
	).Scan(&id); err != nil {
		t.Fatalf("storing the report: %v", err)
	}
	return reported{id: id, external: external}
}

// match runs the attribution the poller runs: which click, if any, earned
// this purchase.
func (w *world) match(t *testing.T, report reported, ref string) earnings.Attribution {
	t.Helper()
	clicks, err := clickout.NewClicks(clickoutstore.New(w.tx))
	if err != nil {
		t.Fatalf("NewClicks(): %v", err)
	}
	matcher, err := earnings.NewMatcher(clicks, earningsstore.New(w.tx))
	if err != nil {
		t.Fatalf("NewMatcher(): %v", err)
	}
	attributed, err := matcher.Match(w.ctx, w.tx, earnings.Report{ID: report.id, Ref: networks.NewClickRef(ref)})
	if err != nil {
		t.Fatalf("Match(): %v", err)
	}
	return attributed
}

// machines builds the entry state machine and the confirmer over this
// scenario's transaction and ledger.
func (w *world) machines(t *testing.T) (*earnings.Entries, *earnings.Confirmations) {
	t.Helper()
	queries := earningsstore.New(w.tx)
	machine, err := earnings.NewEntries(queries, w.ledger, houseReceivable)
	if err != nil {
		t.Fatalf("NewEntries(): %v", err)
	}
	confirmations, err := earnings.NewConfirmations(machine, queries)
	if err != nil {
		t.Fatalf("NewConfirmations(): %v", err)
	}
	return machine, confirmations
}

// shareOf is the member's cut of the reported commission under a click's
// own snapshot - which is the whole of FR-013: the band at the click, never
// the band as it now stands.
func (w *world) shareOf(t *testing.T, click clickout.Click) money.Amount {
	t.Helper()
	amount, err := money.New(reportedCommission, scenarioCurrency)
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	share, err := earnings.ShareOf(amount, click.Promised)
	if err != nil {
		t.Fatalf("ShareOf(): %v", err)
	}
	return share.Member
}

// balance answers what one of the member's stage accounts holds, read
// straight from the ledger.
//
// This is NOT independent of the wallet: [wallet.Projector] reads a stage by
// making these same two calls in this same order, so a bug in either would
// move both together. Reconciling the two against each other is
// [world.wantWalletMatchesLedger]'s job, and SC-006 is that, not this.
func (w *world) balance(t *testing.T, stage wallet.Stage) int64 {
	t.Helper()
	account, err := w.ledger.EnsureAccount(w.ctx, wallet.MemberAccount(w.member, stage), scenarioCurrency)
	if err != nil {
		t.Fatalf("resolving the %s account: %v", stage, err)
	}
	held, err := w.ledger.Balance(w.ctx, account, scenarioCurrency)
	if err != nil {
		t.Fatalf("reading the %s balance: %v", stage, err)
	}
	return held.Minor
}

// wantZeroSum is C-1 read from the deployed check rather than recomputed
// here: whatever the scenario posted, no money was created or destroyed
// (SC-003).
func (w *world) wantZeroSum(t *testing.T) {
	t.Helper()
	rows, err := w.tx.Query(w.ctx, `select currency, net_minor from cashback.ledger_zero_sum`)
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

// importsAStatement records the reconciliation run covering the purchase,
// with the operator who imported it: reconciliation is an accounting act
// with a person behind it (US6), and FR-043 will not confirm a credit
// without one.
func (w *world) importsAStatement(t *testing.T) {
	t.Helper()
	w.importsStatement(t, `{"lines":[]}`)
}

// importsStatement is importsAStatement with the network's own lines in it,
// for the gate that compares a statement against what was reported.
func (w *world) importsStatement(t *testing.T, raw string) uuid.UUID {
	t.Helper()
	var run uuid.UUID
	if err := w.tx.QueryRow(w.ctx, `
		insert into cashback.reconciliation_run
		    (network_account_id, statement_period_start, statement_period_end, imported_by, raw_statement)
		values ($1, now() - interval '30 days', now() + interval '1 day', $2, $3::jsonb)
		returning id`, w.publisher, w.operator, raw).Scan(&run); err != nil {
		t.Fatalf("importing the statement: %v", err)
	}
	return run
}

// eventsAbout reads what was announced about one subject, grouped by type.
//
// Not ordered: every event a scenario appends is inside one transaction, so
// they share an occurred_at and their order between them is decided by a
// random id. Asserting on that would be asserting on gen_random_uuid.
func (w *world) eventsAbout(t *testing.T, subject uuid.UUID) map[string][]map[string]any {
	t.Helper()
	rows, err := w.tx.Query(w.ctx, `select type, payload from domain_event where subject = $1`, subject.String())
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

// wantNoOrphanCredits is SC-002 as a query: every credit traces to a stored
// network transaction and, where attributed, to a click.
//
// The foreign keys make most of this unrepresentable, which is the point -
// running it proves the keys are still there and still not deferrable, and
// it would catch the day somebody added a nullable path around them.
func (w *world) wantNoOrphanCredits(t *testing.T) {
	t.Helper()
	var orphans int
	if err := w.tx.QueryRow(w.ctx, `
		select count(*)
		  from cashback.entry e
		  left join cashback.network_transaction nt on nt.id = e.network_transaction_id
		  left join cashback.click c on c.id = e.click_id
		 where nt.id is null
		    or (e.click_id is not null and c.id is null)
		    or (c.id is not null and c.account_id <> e.account_id)`).Scan(&orphans); err != nil {
		t.Fatalf("running the SC-002 query: %v", err)
	}
	if orphans != 0 {
		t.Errorf("%d credit(s) trace to no stored transaction, to no click, or to another member's click (SC-002)", orphans)
	}
}

// entryFacts is an entry's columns as a comparable value, so "the original
// is untouched" can be asserted over all of them at once rather than over
// the ones somebody thought to name.
// Values, never pointers: a comparison of pointers compares addresses, and
// two reads of one unchanged row would then differ every time.
type entryFacts struct {
	state       string
	amountMinor int64
	currency    string
	holdRule    string
	reversalOf  string
	report      string
	click       string
	createdAt   time.Time
}

// entryRow reads one entry whole.
func (w *world) entryRow(t *testing.T, id uuid.UUID) entryFacts {
	t.Helper()
	var e entryFacts
	if err := w.tx.QueryRow(w.ctx, `
		select state, amount_minor, currency, coalesce(hold_rule, ''),
		       coalesce(reversal_of_id::text, ''), network_transaction_id::text,
		       coalesce(click_id::text, ''), created_at
		  from cashback.entry where id = $1`, id).Scan(
		&e.state, &e.amountMinor, &e.currency, &e.holdRule,
		&e.reversalOf, &e.report, &e.click, &e.createdAt); err != nil {
		t.Fatalf("reading entry %s: %v", id, err)
	}
	return e
}

// supersedes writes the new report a status change arrives as (C-3): a row
// of its own naming the one it replaces, never an edit.
func (w *world) supersedes(t *testing.T, original reported, raw string, status networks.Status) reported {
	t.Helper()
	var id uuid.UUID
	if err := w.tx.QueryRow(w.ctx, `
		insert into cashback.network_transaction (
			network_id, network_account_id, external_id, click_ref,
			status_raw, status, sale_amount_minor, commission_minor, currency,
			transacted_at, retrieved_at, query_window_start, query_window_end, raw_payload, supersedes_id)
		select network_id, network_account_id, external_id, click_ref,
		       $2, $3, sale_amount_minor, commission_minor, currency,
		       transacted_at, now(), query_window_start, query_window_end, raw_payload, id
		  from cashback.network_transaction where id = $1
		returning id`, original.id, raw, string(status)).Scan(&id); err != nil {
		t.Fatalf("superseding the report: %v", err)
	}
	return reported{id: id, external: original.external}
}

// entriesFor counts a member's entries, for the gates that assert nothing
// was credited at all.
func (w *world) entriesFor(t *testing.T, member uuid.UUID) int {
	t.Helper()
	var n int
	if err := w.tx.QueryRow(w.ctx, `select count(*) from cashback.entry where account_id = $1`, member).Scan(&n); err != nil {
		t.Fatalf("counting entries: %v", err)
	}
	return n
}

// wantDecisionAnnounced asserts one operator decision reached the stream
// carrying both halves FR-061 asks for: who decided, and why.
func (w *world) wantDecisionAnnounced(t *testing.T, entry uuid.UUID, eventType, actorField, reason string) {
	t.Helper()
	announced := w.eventsAbout(t, entry)[eventType]
	if len(announced) != 1 {
		t.Fatalf("announced %s %d time(s) about entry %s, want once", eventType, len(announced), entry)
	}
	if got, _ := announced[0]["reason"].(string); got != reason {
		t.Errorf("%s announced reason %q, want %q", eventType, got, reason)
	}
	if got, _ := announced[0][actorField].(string); got != w.operator.String() {
		t.Errorf("%s announced %s = %q, want the operator %s", eventType, actorField, got, w.operator)
	}
}

// holdAnother opens a second held credit on a fresh click and report, for
// the gates that need one decision per credit.
func (w *world) holdAnother(t *testing.T, holds *earnings.Holds, machine *earnings.Entries) uuid.UUID {
	t.Helper()
	click := w.clickOut(t)
	report := w.reports(t, click.Ref.Ref())
	matched := w.match(t, report, click.Ref.Ref())
	sale, err := money.New(reportedSaleMinor, scenarioCurrency)
	if err != nil {
		t.Fatalf("money.New(): %v", err)
	}
	hold, err := holds.Evaluate(w.ctx, earnings.Candidate{
		Member: w.member, Click: matched.Click, Sale: sale, At: time.Now(),
	})
	if err != nil {
		t.Fatalf("Evaluate(): %v", err)
	}
	if !hold.Held() {
		t.Fatal("the second candidate was not held under the same cap as the first")
	}
	opened, err := machine.Open(w.ctx, w.tx, hold.Open(earnings.Credit{
		Member: w.member, Brand: scenarioBrand, Report: report.id, Click: click.ID,
		State: earnings.StatePending, Amount: w.shareOf(t, click),
		Reason: "the network reported the purchase",
	}))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	return opened.ID
}

// confirmedPurchase is one whole earn, from click to confirmed credit, for
// the gates that need money already standing before they begin.
//
// The caller names the network's own id for the transaction rather than
// having one generated, because a statement is written about transaction
// ids and one run covers a whole period: a statement cannot be imported
// after the purchases it names without either naming ids that do not exist
// yet or importing a second run for the same period, which
// reconciliation_run_statement_once refuses - correctly.
func (w *world) confirmedPurchase(t *testing.T, external string) reported {
	t.Helper()
	click := w.clickOut(t)
	report := w.reportsAs(t, external, click.Ref.Ref(), networks.StatusConfirmed, reportedCommission)
	machine, confirmations := w.machines(t)
	opened, err := machine.Open(w.ctx, w.tx, earnings.Credit{
		Member: w.member, Brand: scenarioBrand, Report: report.id, Click: click.ID,
		State: earnings.StatePending, Amount: w.shareOf(t, click),
		Reason: "the network reported the purchase",
	})
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	if _, err := confirmations.Confirm(w.ctx, w.tx, opened, networks.StatusConfirmed, report.id); err != nil {
		t.Fatalf("Confirm(): %v", err)
	}
	return report
}

// wantWalletMatchesLedger is SC-006: the totals a member is SHOWN reconcile
// with the ledger, to the minor unit.
//
// It reads both sides through the code each one actually uses - the wallet's
// own projection for what a member sees, and the ledger directly for what
// was posted - and then holds both to the figure the scenario computed for
// itself. Two agreeing numbers could still both be wrong; three cannot, not
// without the arithmetic being wrong in the same direction twice.
func (w *world) wantWalletMatchesLedger(t *testing.T, pending, confirmed int64) {
	t.Helper()
	projector, err := wallet.NewProjector(w.ledger)
	if err != nil {
		t.Fatalf("NewProjector(): %v", err)
	}
	shown, err := projector.Of(w.ctx, w.member, scenarioCurrency)
	if err != nil {
		t.Fatalf("Of(): %v", err)
	}
	for _, check := range []struct {
		stage  wallet.Stage
		shown  int64
		posted int64
		want   int64
	}{
		{wallet.StagePending, shown.Pending.Minor, w.balance(t, wallet.StagePending), pending},
		{wallet.StageConfirmed, shown.Confirmed.Minor, w.balance(t, wallet.StageConfirmed), confirmed},
	} {
		if check.shown != check.posted {
			t.Errorf("the member is shown %d %s and the ledger holds %d: the wallet and the ledger disagree (SC-006)",
				check.shown, check.stage, check.posted)
		}
		if check.shown != check.want {
			t.Errorf("the member is shown %d %s, want %d", check.shown, check.stage, check.want)
		}
	}
}
