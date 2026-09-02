package ops_test

// The two journals against the real, migrated schema (T114, FR-062): that a
// window holds exactly the rows written inside it, in the order they were
// written, with the joins an accountant needs.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Nomos-N4s/apivo-news/internal/cashback/ops"
)

// member seeds a member account.
func member(ctx context.Context, t *testing.T, tx pgx.Tx) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `insert into public.account (email, display_name, role) values ($1, 'A Member', 'reader') returning id`,
		"member-"+suffix(t)+"@example.test").Scan(&id); err != nil {
		t.Fatalf("seeding the member: %v", err)
	}
	return id
}

// moved writes an entry citing report and one transition of it at the given
// instant, answering the transition id.
func moved(ctx context.Context, t *testing.T, tx pgx.Tx, memberID, report uuid.UUID, from *string, to, ref string, at time.Time, actor *uuid.UUID, reason *string) uuid.UUID {
	t.Helper()
	var entry uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.entry (account_id, brand_id, network_transaction_id, click_id, state, amount_minor, currency)
		values ($1, 'fixture', $2, null, $3, 250, 'EUR') returning id`, memberID, report, to).Scan(&entry); err != nil {
		t.Fatalf("seeding the entry: %v", err)
	}
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into cashback.entry_transition (entry_id, from_state, to_state, ledger_transfer_ref, reason, actor_id, occurred_at)
		values ($1, $2, $3, $4, $5, $6, $7) returning id`, entry, from, to, ref, reason, actor, at).Scan(&id); err != nil {
		t.Fatalf("seeding the transition: %v", err)
	}
	return id
}

func TestTheAccountingJournalsAgainstSchema(t *testing.T) {
	t.Parallel()
	ctx, tx, done := schemaPool(t)
	defer done()
	parties := seedStatementParties(ctx, t, tx)
	window := ops.ExportWindow{From: august.Start, To: august.End}

	each(ctx, t, tx, "the ledger journal holds the window's transitions, in order, with their entries", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		memberID := member(ctx, t, tx)
		a := reported(ctx, t, tx, parties, "A", 499, inAugust, uuid.Nil)
		b := reported(ctx, t, tx, parties, "B", 300, inAugust, uuid.Nil)
		c := reported(ctx, t, tx, parties, "C", 200, beforeAugust, uuid.Nil)
		pending := "pending"
		reason := "opened on the network's report"
		later := moved(ctx, t, tx, memberID, b, nil, "pending", "tx-"+suffix(t), inAugust.Add(time.Hour), nil, nil)
		earlier := moved(ctx, t, tx, memberID, a, &pending, "confirmed", "tx-"+suffix(t), inAugust, &parties.operator.ID, &reason)
		moved(ctx, t, tx, memberID, c, nil, "pending", "tx-"+suffix(t), beforeAugust, nil, nil)

		rows, err := store.ExportLedger(ctx, window)
		if err != nil {
			t.Fatalf("ExportLedger(): %v", err)
		}
		// Other cases and other packages write transitions too; keep the
		// ones this case wrote.
		mine := map[uuid.UUID]ops.LedgerRow{}
		for _, row := range rows {
			if row.Member == memberID {
				mine[row.TransitionID] = row
			}
		}
		if len(mine) != 2 {
			t.Fatalf("%d of this member's transitions in the window, want 2 (the July one is outside)", len(mine))
		}
		first, second := mine[earlier], mine[later]
		switch {
		case first.From != "pending" || first.To != "confirmed" || first.Report != a || first.Actor != parties.operator.ID || first.Reason != reason:
			t.Errorf("the earlier transition = %+v, want pending->confirmed on A by the operator with its reason", first)
		case first.Amount.Minor != 250 || first.Amount.Currency != "EUR" || first.Brand != "fixture" || first.TransferRef == "":
			t.Errorf("the earlier transition's money = %+v", first)
		case second.From != "" || second.To != "pending" || second.Actor != uuid.Nil || second.Report != b:
			t.Errorf("the later transition = %+v, want an opening on B by nobody", second)
		}
		var seenEarlier bool
		for _, row := range rows {
			switch row.TransitionID {
			case earlier:
				seenEarlier = true
			case later:
				if !seenEarlier {
					t.Error("the later transition came before the earlier one; the journal is in the order things happened")
				}
			}
		}
	})

	each(ctx, t, tx, "the reconciliation journal holds the window's differences with their statements", func(t *testing.T, tx pgx.Tx, store *ops.PGStore) {
		id := anOpenDifference(ctx, t, tx, store, parties)
		if _, err := store.ResolveDifference(ctx, ops.Resolution{ID: id, Verdict: ops.VerdictAbsorbed, Reason: "too small", Operator: parties.operator}); err != nil {
			t.Fatalf("resolving: %v", err)
		}
		// Detected now, so the window is the present day.
		now := time.Now().UTC()
		rows, err := store.ExportReconciliation(ctx, ops.ExportWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour)})
		if err != nil {
			t.Fatalf("ExportReconciliation(): %v", err)
		}
		var found *ops.ReconciliationRow
		for i := range rows {
			if rows[i].DifferenceID == id {
				found = &rows[i]
			}
		}
		if found == nil {
			t.Fatalf("the difference is not in the journal (%d rows)", len(rows))
		}
		switch {
		case found.Account != parties.account || found.Network != parties.network || found.Publisher != "publisher-1":
			t.Errorf("the statement's parties = %s/%s/%s, want this account at %s", found.Account, found.Network, found.Publisher, parties.network)
		case !found.Period.Start.Equal(august.Start) || !found.Period.End.Equal(august.End):
			t.Errorf("statement period = %+v, want August", found.Period)
		case found.Kind != ops.AmountMismatch || found.TransactionID != "A" || found.Report == uuid.Nil:
			t.Errorf("the difference = %+v, want the mismatch on A naming its report", found)
		case found.Expected == nil || found.Expected.Minor != 499 || found.Actual == nil || found.Actual.Minor != 450 || found.Delta.Minor != -49:
			t.Errorf("figures = %v/%v/%v, want 499/450/-49", found.Expected, found.Actual, found.Delta)
		case found.Resolution == nil || found.Resolution.Verdict != ops.VerdictAbsorbed || found.Resolution.ResolvedBy != parties.operator.ID || found.Resolution.Reason != "too small":
			t.Errorf("resolution = %+v, want absorbed by the operator", found.Resolution)
		}
		outside, err := store.ExportReconciliation(ctx, ops.ExportWindow{From: now.Add(-48 * time.Hour), To: now.Add(-47 * time.Hour)})
		if err != nil {
			t.Fatalf("ExportReconciliation() for an earlier window: %v", err)
		}
		for _, row := range outside {
			if row.DifferenceID == id {
				t.Error("a difference detected now appeared in a window two days ago")
			}
		}
	})

	each(ctx, t, tx, "a window that is not one is refused", func(t *testing.T, _ pgx.Tx, store *ops.PGStore) {
		for _, w := range []ops.ExportWindow{{}, {From: august.End, To: august.Start}, {From: august.Start, To: august.Start}} {
			if _, err := store.ExportLedger(ctx, w); !errors.Is(err, ops.ErrInvalidWindow) {
				t.Errorf("ExportLedger(%+v) = %v, want ErrInvalidWindow", w, err)
			}
			if _, err := store.ExportReconciliation(ctx, w); !errors.Is(err, ops.ErrInvalidWindow) {
				t.Errorf("ExportReconciliation(%+v) = %v, want ErrInvalidWindow", w, err)
			}
		}
	})
}
