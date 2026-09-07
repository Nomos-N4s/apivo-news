// The first-attribution canary against the schema that arbitrates it
// (#524).
//
// Every verdict here is a property of the count, and the count is a
// statement over four tables joined the way the schema joins them. A fake
// would only agree with whatever this file believed about clicks, offers,
// reports and queue rows; the question is what Postgres says when they are
// actually written, and in particular what it says about the three cases
// that would make the canary lie - a transaction reported three times, a
// report dated before the first click, and an entry written by hand around
// the very failure the canary watches for.

package networks_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
)

// unreachableDB is a database that answers every statement with the same
// failure, so a case can see what the canary makes of a count it could not
// take.
type unreachableDB struct{}

var errUnreachable = errors.New("the database is not answering")

func (unreachableDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errUnreachable
}

func (unreachableDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errUnreachable
}

func (unreachableDB) QueryRow(context.Context, string, ...any) pgx.Row { return unreachableRow{} }

type unreachableRow struct{}

func (unreachableRow) Scan(...any) error { return errUnreachable }

// canaryClickAt is when the member clicked in every case that has a click:
// a day into the backfill, so that the reports the poller stores - dated
// four, five and six days in - come after it.
var canaryClickAt = pollerSchemaStart.Add(24 * time.Hour)

// canaryRef is a reference that satisfies cashback.click's own rule for one:
// at least 22 URL-safe characters (0012, FR-020).
const canaryRef = "canary-0mB7hQ2xKp4vT9sLcNfR1w"

// canaryReports are three distinct transactions, none carrying a reference,
// dated after canaryClickAt. Three is the threshold, so storing all three
// through the poller is what crosses it.
func canaryReports(t *testing.T) []networks.Reported {
	t.Helper()
	return []networks.Reported{
		pollerTestReport(t, "CAN-1", networks.StatusPending, "pending", pollerSchemaStart.Add(96*time.Hour), 499),
		pollerTestReport(t, "CAN-2", networks.StatusPending, "pending", pollerSchemaStart.Add(120*time.Hour), 599),
		pollerTestReport(t, "CAN-3", networks.StatusPending, "pending", pollerSchemaStart.Add(144*time.Hour), 699),
	}
}

// withRef is a report that carries the given reference.
func withRef(report networks.Reported, ref string) networks.Reported {
	report.ClickRef = networks.NewClickRef(ref)
	return report
}

// canaryClick records one click through the account's network: a member,
// a merchant routed through that network, an offer on the route, and the
// click itself, carrying canaryRef and dated at.
//
// The whole chain is written because the canary counts from the first
// click and finds it by walking offer -> route -> network; a click on an
// offer at some other network is exactly what must NOT count.
func canaryClick(ctx context.Context, t *testing.T, tx pgx.Tx, account networks.PublisherAccount, at time.Time) (member, click uuid.UUID) {
	t.Helper()
	suffix := uuid.NewString()[:8]
	if err := tx.QueryRow(ctx, `
		insert into public.account (email, display_name, role)
		values ($1, 'Canary Member', 'reader') returning id`,
		"canary-"+suffix+"@example.test").Scan(&member); err != nil {
		t.Fatalf("seeding the member: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into cashback.participation (account_id, brand_id, terms_version, default_currency)
		values ($1, 'fixture', '1.0.0', 'EUR')`, member); err != nil {
		t.Fatalf("opting the member in: %v", err)
	}
	var merchant, route, offer uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.merchant (slug, country, source_language_code, status)
		values ($1, 'GR', 'el', 'active') returning id`, "canary-"+suffix).Scan(&merchant); err != nil {
		t.Fatalf("seeding the merchant: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into cashback.merchant_network
		    (brand_id, merchant_id, network_id, external_merchant_id, retrieved_at, raw_payload, status, preferred)
		values ('fixture', $1, $2, $3, now(), '{"id":"canary"}'::jsonb, 'active', true) returning id`,
		merchant, account.Network().String(), "ext-"+suffix).Scan(&route); err != nil {
		t.Fatalf("seeding the route: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into cashback.offer
		    (merchant_network_id, rate_kind, rate_fixed_minor, currency, member_share_bps, valid_from, deeplink_template)
		values ($1, 'fixed', 250, 'EUR', 6000, $2, 'https://example.test/deeplink?ref={ref}')
		returning id`, route, pollerSchemaStart).Scan(&offer); err != nil {
		t.Fatalf("seeding the offer: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into cashback.click
		    (click_ref, account_id, offer_id, merchant_network_id, network_id, clicked_at, rate_snapshot, member_share_bps_snapshot)
		values ($1, $2, $3, $4, $5, $6, '{"kind":"fixed"}'::jsonb, 6000) returning id`,
		canaryRef, member, offer, route, account.Network().String(), at).Scan(&click); err != nil {
		t.Fatalf("seeding the click: %v", err)
	}
	return member, click
}

// storeThroughPoller drives one forward poll over the given reports, so the
// evidence and the queue rows are written by the code that writes them in
// production rather than planted.
func storeThroughPoller(ctx context.Context, t *testing.T, tx pgx.Tx, account networks.PublisherAccount, reports ...networks.Reported) {
	t.Helper()
	poll, err := pollerSchemaPoller(t, tx).PollForward(ctx, pollerTestNetwork(account, pollerTestReports(reports...)))
	if err != nil {
		t.Fatalf("PollForward(): %v", err)
	}
	if !poll.Ran {
		t.Fatal("the poll had nothing to read")
	}
}

// reportID finds the evidence row the poller stored for one external id.
func reportID(ctx context.Context, t *testing.T, tx pgx.Tx, account networks.PublisherAccount, externalID string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		select id from cashback.network_transaction
		 where network_account_id = $1 and external_id = $2 and supersedes_id is null`,
		pgtype.UUID{Bytes: account.ID(), Valid: true}, externalID).Scan(&id); err != nil {
		t.Fatalf("finding report %s: %v", externalID, err)
	}
	return id
}

// chainIDs lists every evidence row stored for one external id, root first.
func chainIDs(ctx context.Context, t *testing.T, tx pgx.Tx, account networks.PublisherAccount, externalID string) []uuid.UUID {
	t.Helper()
	rows, err := tx.Query(ctx, `
		select id from cashback.network_transaction
		 where network_account_id = $1 and external_id = $2
		 order by retrieved_at, id`,
		pgtype.UUID{Bytes: account.ID(), Valid: true}, externalID)
	if err != nil {
		t.Fatalf("listing the chain for %s: %v", externalID, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatalf("reading the chain for %s: %v", externalID, err)
	}
	return ids
}

// canary builds the canary over the case's transaction.
func canary(t *testing.T, tx pgx.Tx) *networks.AttributionCanary {
	t.Helper()
	c, err := networks.NewAttributionCanary(tx)
	if err != nil {
		t.Fatalf("NewAttributionCanary(): %v", err)
	}
	return c
}

func TestTheAttributionCanaryAgainstTheRealSchema(t *testing.T) {
	t.Parallel()
	ctx, tx := pollerSchemaConnect(t)

	eachPoll(ctx, t, tx, "with no click there is nothing to judge, however many reports went unattributed", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		storeThroughPoller(ctx, t, tx, account, canaryReports(t)...)

		verdict, err := canary(t, tx).Check(ctx, account.Network())
		if err != nil {
			t.Fatalf("Check(): %v", err)
		}
		if !verdict.Idle() || verdict.Suspect() {
			t.Errorf("verdict %+v, want idle: nobody has clicked, so the reports cannot be Apivo's", verdict)
		}
	})

	eachPoll(ctx, t, tx, "below the threshold the canary watches and says nothing", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		canaryClick(ctx, t, tx, account, canaryClickAt)
		storeThroughPoller(ctx, t, tx, account, canaryReports(t)[:networks.AttributionCanaryThreshold-1]...)

		verdict, err := canary(t, tx).Check(ctx, account.Network())
		if err != nil {
			t.Fatalf("Check(): %v", err)
		}
		if verdict.Idle() || verdict.Retired() || verdict.Suspect() {
			t.Errorf("verdict %+v, want watching: %d unattributed is not yet %d", verdict, verdict.Unattributed, networks.AttributionCanaryThreshold)
		}
		if !verdict.FirstClickAt.Equal(canaryClickAt) {
			t.Errorf("first click at %s, want %s", verdict.FirstClickAt, canaryClickAt)
		}
	})

	eachPoll(ctx, t, tx, "reports carrying no reference cross the threshold", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		canaryClick(ctx, t, tx, account, canaryClickAt)
		storeThroughPoller(ctx, t, tx, account, canaryReports(t)...)

		verdict, err := canary(t, tx).Check(ctx, account.Network())
		if !errors.Is(err, networks.ErrAttributionNeverSucceeded) {
			t.Fatalf("Check() = %v, want ErrAttributionNeverSucceeded", err)
		}
		if !verdict.Suspect() || verdict.Unattributed != 3 || verdict.Attributed != 0 {
			t.Errorf("verdict %+v, want suspect with 3 unattributed and 0 attributed", verdict)
		}
		// The line is the whole of the alarm, so it has to carry the count,
		// the remedy and the fact that the poll itself succeeded.
		for _, want := range []string{"3 distinct transaction(s)", "click_ref_param", "#524", "the sweep itself succeeded"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not say %q: %v", want, err)
			}
		}
	})

	eachPoll(ctx, t, tx, "references matching no click cross it the same way", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		canaryClick(ctx, t, tx, account, canaryClickAt)
		// The truncation failure: the network echoes something, and it is
		// not the reference any click carries. The poller stores these and
		// leaves them for the matcher; the matcher's queue write lives in
		// the earnings module, so its outcome is planted here, exactly as
		// the store's own test does.
		reports := canaryReports(t)
		for i := range reports {
			reports[i] = withRef(reports[i], "canary-0mB7hQ2xKp4vT9s")
		}
		storeThroughPoller(ctx, t, tx, account, reports...)
		for _, report := range reports {
			if _, err := tx.Exec(ctx, `insert into cashback.unattributed_transaction (network_transaction_id) values ($1)`,
				reportID(ctx, t, tx, account, report.ExternalID)); err != nil {
				t.Fatalf("planting the matcher's observation: %v", err)
			}
		}

		verdict, err := canary(t, tx).Check(ctx, account.Network())
		if !errors.Is(err, networks.ErrAttributionNeverSucceeded) {
			t.Fatalf("Check() = %v, want ErrAttributionNeverSucceeded", err)
		}
		if verdict.Unattributed != 3 {
			t.Errorf("%d unattributed, want 3", verdict.Unattributed)
		}
	})

	eachPoll(ctx, t, tx, "one transaction reported three times is one transaction", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		canaryClick(ctx, t, tx, account, canaryClickAt)
		// The same purchase, re-reported with a new status each time: three
		// evidence rows in one supersession chain, three queue rows, one
		// piece of evidence about the parameter.
		at := pollerSchemaStart.Add(96 * time.Hour)
		storeThroughPoller(ctx, t, tx, account, pollerTestReport(t, "CAN-1", networks.StatusPending, "pending", at, 499))
		storeThroughPoller(ctx, t, tx, account, pollerTestReport(t, "CAN-1", networks.StatusConfirmed, "approved", at, 499))
		storeThroughPoller(ctx, t, tx, account, pollerTestReport(t, "CAN-1", networks.StatusConfirmed, "approved", at, 501))

		verdict, err := canary(t, tx).Check(ctx, account.Network())
		if err != nil {
			t.Fatalf("Check(): %v", err)
		}
		if verdict.Unattributed != 1 || verdict.Suspect() {
			t.Errorf("verdict %+v, want 1 unattributed and not suspect: three reports of one purchase are one purchase", verdict)
		}
	})

	eachPoll(ctx, t, tx, "reports dated before the first click are not Apivo's and do not count", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		// The click comes AFTER every report: a backfill window reaching
		// history that predates the deployment, or another publisher's link
		// being echoed. Ordinary, and proof of nothing about the parameter.
		canaryClick(ctx, t, tx, account, pollerSchemaStart.Add(200*time.Hour))
		storeThroughPoller(ctx, t, tx, account, canaryReports(t)...)

		verdict, err := canary(t, tx).Check(ctx, account.Network())
		if err != nil {
			t.Fatalf("Check(): %v", err)
		}
		if verdict.Unattributed != 0 || verdict.Suspect() {
			t.Errorf("verdict %+v, want nothing counted: every report predates the first click", verdict)
		}
	})

	eachPoll(ctx, t, tx, "one credit to a click retires the canary for good", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		member, click := canaryClick(ctx, t, tx, account, canaryClickAt)
		// Three unattributed AND one report whose reference round-tripped.
		reports := append(canaryReports(t),
			withRef(pollerTestReport(t, "CAN-OK", networks.StatusConfirmed, "approved", pollerSchemaStart.Add(150*time.Hour), 250), canaryRef))
		storeThroughPoller(ctx, t, tx, account, reports...)
		if _, err := tx.Exec(ctx, `
			insert into cashback.entry (account_id, brand_id, network_transaction_id, click_id, state, amount_minor, currency)
			values ($1, 'fixture', $2, $3, 'pending', 150, 'EUR')`,
			member, reportID(ctx, t, tx, account, "CAN-OK"), click); err != nil {
			t.Fatalf("crediting the click: %v", err)
		}

		verdict, err := canary(t, tx).Check(ctx, account.Network())
		if err != nil {
			t.Fatalf("Check(): %v", err)
		}
		if !verdict.Retired() || verdict.Suspect() || verdict.Attributed != 1 || verdict.Unattributed != 3 {
			t.Errorf("verdict %+v, want retired with 1 attributed beside 3 unattributed", verdict)
		}
	})

	eachPoll(ctx, t, tx, "one purchase credited across two rows of its chain is one attributed transaction", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		member, click := canaryClick(ctx, t, tx, account, canaryClickAt)
		at := pollerSchemaStart.Add(96 * time.Hour)
		// Reported twice - pending, then approved - so the purchase is a
		// chain of two evidence rows, both carrying the reference.
		storeThroughPoller(ctx, t, tx, account, withRef(pollerTestReport(t, "CAN-OK", networks.StatusPending, "pending", at, 250), canaryRef))
		storeThroughPoller(ctx, t, tx, account, withRef(pollerTestReport(t, "CAN-OK", networks.StatusConfirmed, "approved", at, 250), canaryRef))
		// The entry cites the FIRST row, and the chain grows under it: an
		// entry may cite a superseded report, and one click backs one
		// credit (entry_click_id_idx, 0034), so the second row is never
		// credited again. The canary counts purchases and not rows, and a
		// two-row chain with one credit is one purchase.
		chain := chainIDs(ctx, t, tx, account, "CAN-OK")
		if len(chain) != 2 {
			t.Fatalf("the chain has %d row(s), want 2", len(chain))
		}
		if _, err := tx.Exec(ctx, `
			insert into cashback.entry (account_id, brand_id, network_transaction_id, click_id, state, amount_minor, currency)
			values ($1, 'fixture', $2, $3, 'pending', 150, 'EUR')`, member, chain[0], click); err != nil {
			t.Fatalf("crediting the first row of the chain: %v", err)
		}

		verdict, err := canary(t, tx).Check(ctx, account.Network())
		if err != nil {
			t.Fatalf("Check(): %v", err)
		}
		if verdict.Attributed != 1 {
			t.Errorf("%d attributed, want 1: a chain of two rows with one credit is one purchase", verdict.Attributed)
		}
	})

	eachPoll(ctx, t, tx, "a credit written by hand around the failure does not retire it", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		member, _ := canaryClick(ctx, t, tx, account, canaryClickAt)
		storeThroughPoller(ctx, t, tx, account, canaryReports(t)...)
		// An operator attributes a reference-less report by hand. The
		// evidence guard allows the entry to omit its click precisely
		// because there is none to cite - which is the failure itself,
		// worked around, not the reference round-tripping.
		if _, err := tx.Exec(ctx, `
			insert into cashback.entry (account_id, brand_id, network_transaction_id, state, amount_minor, currency)
			values ($1, 'fixture', $2, 'pending', 100, 'EUR')`,
			member, reportID(ctx, t, tx, account, "CAN-1")); err != nil {
			t.Fatalf("hand-attributing the report: %v", err)
		}

		verdict, err := canary(t, tx).Check(ctx, account.Network())
		if !errors.Is(err, networks.ErrAttributionNeverSucceeded) {
			t.Fatalf("Check() = %v, want ErrAttributionNeverSucceeded: a click-less credit proves nothing about the parameter", err)
		}
		if verdict.Attributed != 0 {
			t.Errorf("%d attributed, want 0", verdict.Attributed)
		}
	})

	eachPoll(ctx, t, tx, "the forward sweep carries the refusal, and without the canary it does not", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		canaryClick(ctx, t, tx, account, canaryClickAt)
		adapter := pollerTestNetwork(account, pollerTestReports(canaryReports(t)...))

		captured := &sweepTestLog{}
		sweeps, err := networks.NewSweeps(captured.logger(), pollerSchemaPoller(t, tx), adapter,
			networks.WithAttributionCanary(canary(t, tx)))
		if err != nil {
			t.Fatalf("NewSweeps(): %v", err)
		}
		err = sweeps.RunForward(ctx)
		if !errors.Is(err, networks.ErrAttributionNeverSucceeded) {
			t.Fatalf("RunForward() = %v, want ErrAttributionNeverSucceeded", err)
		}
		// The poll itself succeeded and said so: the refusal is about what
		// happened to the evidence, not about whether it was stored.
		if record := captured.only(t, "INFO"); record["first_reports"] != float64(3) {
			t.Errorf("the sweep did not report storing three reports: %s", captured.buf.String())
		}
		if cursors := cursorsOf(ctx, t, tx, account); !cursors.CursorAt.Valid {
			t.Error("the refusal left the cursor where it was; the evidence was stored and the cursor must move over it")
		}

		// The same run through sweeps built without the option is the run
		// as it was before the canary existed.
		plain, err := networks.NewSweeps(captured.logger(), pollerSchemaPoller(t, tx),
			pollerTestNetwork(pollerSchemaAccount(ctx, t, tx), pollerTestReports(canaryReports(t)...)))
		if err != nil {
			t.Fatalf("NewSweeps(): %v", err)
		}
		if err := plain.RunForward(ctx); err != nil {
			t.Errorf("RunForward() without the canary = %v, want nil", err)
		}
	})

	eachPoll(ctx, t, tx, "the retired and idle states are a debug line each, never a refusal", func(t *testing.T, tx pgx.Tx) {
		account := pollerSchemaAccount(ctx, t, tx)
		adapter := pollerTestNetwork(account, pollerTestReports(canaryReports(t)...))
		captured := &sweepTestLog{}
		sweeps, err := networks.NewSweeps(captured.logger(), pollerSchemaPoller(t, tx), adapter,
			networks.WithAttributionCanary(canary(t, tx)))
		if err != nil {
			t.Fatalf("NewSweeps(): %v", err)
		}
		// Idle: nobody has clicked.
		if err := sweeps.RunForward(ctx); err != nil {
			t.Fatalf("RunForward() = %v, want nil while idle", err)
		}
		records := captured.records(t)
		if len(records) != 2 || records[1]["level"] != "DEBUG" || !strings.Contains(records[1]["msg"].(string), "nothing to judge") {
			t.Errorf("an idle canary logged %v, want one Info line for the poll and one Debug line saying there is nothing to judge", records)
		}
	})
}

func TestTheAttributionCanaryRefusesWhatItCannotCount(t *testing.T) {
	t.Parallel()
	if _, err := networks.NewAttributionCanary(nil); !errors.Is(err, networks.ErrNoCanaryStore) {
		t.Errorf("NewAttributionCanary(nil) = %v, want ErrNoCanaryStore", err)
	}
	c, err := networks.NewAttributionCanary(unreachableDB{})
	if err != nil {
		t.Fatalf("NewAttributionCanary(): %v", err)
	}
	if _, err := c.Check(context.Background(), networks.NetworkID("")); err == nil {
		t.Error("Check() with no network id returned nil, want a refusal before any query")
	}
	// A count that could not be taken is a failure of the canary, and it
	// must never read as a clean bill.
	_, err = c.Check(context.Background(), networks.NetworkID("fixture"))
	if err == nil || errors.Is(err, networks.ErrAttributionNeverSucceeded) {
		t.Errorf("Check() over an unreachable database = %v, want a failure that is neither nil nor the refusal", err)
	}
	if !strings.Contains(err.Error(), "never a clean bill") {
		t.Errorf("the failure does not say it is not a clean bill: %v", err)
	}
}

// AttributionVerdict's predicates, spelled out at the boundaries the schema
// cases cannot cheaply reach.
func TestAttributionVerdictPredicates(t *testing.T) {
	t.Parallel()
	clicked := time.Date(2026, time.June, 2, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                   string
		verdict                networks.AttributionVerdict
		idle, retired, suspect bool
	}{
		{"no click", networks.AttributionVerdict{Unattributed: 99}, true, false, false},
		{"one attributed", networks.AttributionVerdict{FirstClickAt: clicked, Attributed: 1, Unattributed: 99}, false, true, false},
		{"one short of the threshold", networks.AttributionVerdict{FirstClickAt: clicked, Unattributed: networks.AttributionCanaryThreshold - 1}, false, false, false},
		{"at the threshold", networks.AttributionVerdict{FirstClickAt: clicked, Unattributed: networks.AttributionCanaryThreshold}, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.verdict.Idle(); got != tc.idle {
				t.Errorf("Idle() = %t, want %t", got, tc.idle)
			}
			if got := tc.verdict.Retired(); got != tc.retired {
				t.Errorf("Retired() = %t, want %t", got, tc.retired)
			}
			if got := tc.verdict.Suspect(); got != tc.suspect {
				t.Errorf("Suspect() = %t, want %t", got, tc.suspect)
			}
		})
	}
}
