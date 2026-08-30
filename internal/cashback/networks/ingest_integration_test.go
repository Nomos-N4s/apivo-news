// The ingest chain, driven end to end by the poller against a real Postgres
// (T060, US2 scenarios 3 and 4).
//
// The pieces have their own tests: the supersede path is asserted directly
// in supersede_test.go, and the poller's window arithmetic and cursor
// behaviour in poller_schema_test.go. What is asserted HERE is the property
// that only appears when they run together - what a SECOND poll of the SAME
// WINDOW does to the evidence.
//
// That is not a contrived scenario. It is what the trailing re-read does
// four times a day for every period it walks, and it is the one operation
// whose failure mode is invisible: a duplicate row would not raise anything,
// would not stop the poll, and would show up months later as a member
// credited twice for one purchase.

package networks_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks"
	"github.com/Nomos-N4s/apivo-news/internal/cashback/networks/store"
)

// storedChain is what the evidence table holds for one transaction, oldest
// report first.
type storedChain struct {
	IDs        []uuid.UUID
	Statuses   []string
	Supersedes []uuid.UUID
	Digests    []string
}

// chainOf reads every report stored for one transaction, in CHAIN order:
// the root first, then each report that supersedes the one before it.
//
// Walked rather than sorted, and the difference matters. retrieved_at is not
// a tiebreak - the poller reads its clock once per poll, so two reports of
// one poll share it exactly, and a test clock that does not move shares it
// across polls too. The order that exists is the one the schema builds: one
// root, no forks, therefore one path. Reading it that way also asserts the
// chain is walkable at all, which is what "exactly one current row" is
// derived from.
func chainOf(ctx context.Context, t *testing.T, tx pgx.Tx, account networks.PublisherAccount, externalID string) storedChain {
	t.Helper()
	rows, err := tx.Query(ctx, `
		with recursive chain as (
		    select nt.id, nt.status, nt.supersedes_id, nt.content_digest, 0 as depth
		      from cashback.network_transaction nt
		     where nt.network_id = $1 and nt.external_id = $2 and nt.supersedes_id is null
		    union all
		    select nt.id, nt.status, nt.supersedes_id, nt.content_digest, c.depth + 1
		      from cashback.network_transaction nt
		      join chain c on nt.supersedes_id = c.id
		)
		select id, status, coalesce(supersedes_id::text, ''), content_digest
		  from chain order by depth`,
		string(account.Network()), externalID)
	if err != nil {
		t.Fatalf("reading the chain: %v", err)
	}
	defer rows.Close()

	var chain storedChain
	for rows.Next() {
		var id, status, supersedes, digest string
		if err := rows.Scan(&id, &status, &supersedes, &digest); err != nil {
			t.Fatalf("scanning a report: %v", err)
		}
		chain.IDs = append(chain.IDs, uuid.MustParse(id))
		chain.Statuses = append(chain.Statuses, status)
		chain.Digests = append(chain.Digests, digest)
		if supersedes == "" {
			chain.Supersedes = append(chain.Supersedes, uuid.Nil)
			continue
		}
		chain.Supersedes = append(chain.Supersedes, uuid.MustParse(supersedes))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the chain: %v", err)
	}
	return chain
}

// repoll puts the main cursor back so the next forward poll reads the same
// window again.
//
// It is how a test asks the question the TRAILING sweep asks in production -
// the same period, read again, later - without waiting the hundred days
// ADR-0003 puts between them. The forward cursor is used rather than the
// trailing one because the trailing sweep walks forward too, so it cannot be
// made to read one period twice either.
func repoll(ctx context.Context, t *testing.T, tx pgx.Tx, account networks.PublisherAccount) {
	t.Helper()
	if _, err := tx.Exec(ctx, `update cashback.network_account set cursor_at = null where id = $1`,
		pgtype.UUID{Bytes: account.ID(), Valid: true}); err != nil {
		t.Fatalf("putting the cursor back: %v", err)
	}
}

// TestRePollingAWindowCreatesNoDuplicate is US2 scenario 3, through the
// whole chain: the same window, read again, with the network saying exactly
// what it said before.
func TestRePollingAWindowCreatesNoDuplicate(t *testing.T) {
	t.Parallel()
	ctx, tx := pollerSchemaConnect(t)
	account := pollerSchemaAccount(ctx, t, tx)
	first, second := pollerSchemaPair(t)

	adapter := pollerTestNetwork(account, pollerTestReports(first, second))
	poller := pollerSchemaPoller(t, tx)

	initial, err := poller.PollForward(ctx, adapter)
	if err != nil {
		t.Fatalf("the first poll: %v", err)
	}
	if initial.Outcome.FirstReports != 2 {
		t.Fatalf("the first poll reported %s, want 2 first reports", initial.Outcome)
	}
	before := storedFor(ctx, t, tx, account)

	// Three more polls of the same window, because once could pass by
	// accident and the trailing sweep will ask this thousands of times.
	for again := range 3 {
		repoll(ctx, t, tx, account)
		poll, err := poller.PollForward(ctx, adapter)
		if err != nil {
			t.Fatalf("re-poll %d: %v", again, err)
		}
		if !poll.Ran {
			t.Fatalf("re-poll %d had nothing to read; the window was put back", again)
		}
		if poll.Outcome.Unchanged != 2 || poll.Outcome.Changed() {
			t.Errorf("re-poll %d reported %s, want both reports unchanged and nothing changed", again, poll.Outcome)
		}
	}

	if after := storedFor(ctx, t, tx, account); after != before {
		t.Errorf("%d evidence row(s) after three re-polls, want the %d the first poll stored", after, before)
	}
	// And each transaction is still one report: a duplicate would sit in the
	// chain rather than beside it, so counting rows for the account is not
	// enough on its own.
	for _, externalID := range []string{first.ExternalID, second.ExternalID} {
		if chain := chainOf(ctx, t, tx, account, externalID); len(chain.IDs) != 1 {
			t.Errorf("%s has %d report(s) after three re-polls, want 1", externalID, len(chain.IDs))
		}
	}
}

// TestAChangedStatusSupersedesAndBothStayReadable is US2 scenario 4. The
// network changes its mind about one transaction and says the same thing
// about the other; the changed one gains a report naming the one it
// replaced, and NEITHER report is edited.
//
// "Both stay readable" is the half worth the integration test. C-3 makes it
// structurally true, but it is what an operator needs on the day a member
// asks why a confirmed purchase was reversed, and it is only observable
// after the supersede has actually happened through the poller.
func TestAChangedStatusSupersedesAndBothStayReadable(t *testing.T) {
	t.Parallel()
	ctx, tx := pollerSchemaConnect(t)
	account := pollerSchemaAccount(ctx, t, tx)
	first, second := pollerSchemaPair(t)

	confirmed := first
	confirmed.StatusRaw, confirmed.Status = "validated", networks.StatusConfirmed
	adapter := pollerTestNetwork(account, func(call int, _ networks.QueryWindow) ([]networks.Reported, error) {
		if call == 0 {
			return []networks.Reported{first, second}, nil
		}
		// The advertiser validated one; the other is re-reported verbatim.
		return []networks.Reported{confirmed, second}, nil
	})
	poller := pollerSchemaPoller(t, tx)

	if _, err := poller.PollForward(ctx, adapter); err != nil {
		t.Fatalf("the first poll: %v", err)
	}
	original := chainOf(ctx, t, tx, account, first.ExternalID)
	if len(original.IDs) != 1 {
		t.Fatalf("%s has %d report(s) after the first poll, want 1", first.ExternalID, len(original.IDs))
	}

	repoll(ctx, t, tx, account)
	poll, err := poller.PollForward(ctx, adapter)
	if err != nil {
		t.Fatalf("the poll that found the change: %v", err)
	}
	if poll.Outcome.Superseded != 1 || poll.Outcome.Unchanged != 1 || poll.Outcome.FirstReports != 0 {
		t.Fatalf("the poll reported %s, want one superseded and one unchanged", poll.Outcome)
	}

	// The changed transaction now has two reports: the original, untouched,
	// and a new one naming it.
	chain := chainOf(ctx, t, tx, account, first.ExternalID)
	if len(chain.IDs) != 2 {
		t.Fatalf("%s has %d report(s), want 2", first.ExternalID, len(chain.IDs))
	}
	if chain.IDs[0] != original.IDs[0] {
		t.Errorf("the first report is now %s, want the original %s; superseding must not replace", chain.IDs[0], original.IDs[0])
	}
	if chain.Statuses[0] != original.Statuses[0] || chain.Digests[0] != original.Digests[0] {
		t.Errorf("the original report now reads %s/%s, want %s/%s; superseding must not edit",
			chain.Statuses[0], chain.Digests[0], original.Statuses[0], original.Digests[0])
	}
	if chain.Statuses[1] != string(networks.StatusConfirmed) {
		t.Errorf("the new report reads %s, want the confirmation the network sent", chain.Statuses[1])
	}
	if chain.Supersedes[0] != uuid.Nil {
		t.Errorf("the original report names %s as its predecessor, want none; it is the root", chain.Supersedes[0])
	}
	if chain.Supersedes[1] != chain.IDs[0] {
		t.Errorf("the new report supersedes %s, want the original %s", chain.Supersedes[1], chain.IDs[0])
	}

	// One root and one tip, which is what "exactly one current row" is
	// derived from - and the tip is the confirmation.
	queries := store.New(tx)
	current, err := queries.GetCurrentNetworkTransaction(ctx, store.GetCurrentNetworkTransactionParams{
		NetworkID:  string(account.Network()),
		ExternalID: first.ExternalID,
	})
	if err != nil {
		t.Fatalf("GetCurrentNetworkTransaction(): %v", err)
	}
	if uuid.UUID(current.ID.Bytes) != chain.IDs[1] {
		t.Errorf("the current report is %v, want the confirmation %s", current.ID, chain.IDs[1])
	}
	// Both are still readable, one at a time, with the facts each carried
	// when it was written.
	for i, id := range chain.IDs {
		stored, err := queries.GetNetworkTransaction(ctx, pgtype.UUID{Bytes: id, Valid: true})
		if err != nil {
			t.Fatalf("reading report %d back: %v", i, err)
		}
		if stored.Status != chain.Statuses[i] {
			t.Errorf("report %d reads %s, want %s", i, stored.Status, chain.Statuses[i])
		}
	}

	// The transaction the network did not change gained nothing.
	if unchanged := chainOf(ctx, t, tx, account, second.ExternalID); len(unchanged.IDs) != 1 {
		t.Errorf("%s has %d report(s), want the 1 it started with", second.ExternalID, len(unchanged.IDs))
	}
}
