package db_test

// These tests assert C-2 the way the constitution states it: a cashback
// credit cannot exist without a reference to exactly one network
// transaction record and, through it, at most one click record. Credits
// with no evidence are unrepresentable.
//
// "Unrepresentable" is the word that matters. The tests below do not check
// that a service refuses to create such a credit; they check that Postgres
// does, by SQLSTATE, whatever the caller.
//
// The transition tests carry D7: every transition writes a ledger transfer,
// and a state recorded without its posting is exactly the wallet-versus-
// ledger disagreement C-1 exists to prevent.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestCashbackEarningsRejectIllegalWrites is the C-2 rejection table.
//
// Every case here is legal in every respect EXCEPT the one it probes. That
// is not tidiness: BEFORE INSERT triggers run ahead of CHECK and NOT NULL
// constraints, so a row that also fails C-2's attribution guard is rejected
// by that guard and the constraint under test is never reached - the case
// would pass while proving nothing about the rule it names. So the cases
// below that cite the fixture's report also cite the click that report
// names, and only the field being tested is wrong.
func TestCashbackEarningsRejectIllegalWrites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rule     string
		write    func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error
		wantCode string
	}{
		{
			name: "credit with no evidence at all",
			rule: "C-2",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.entry (brand_id, account_id, network_transaction_id, state, amount_minor, currency)
					 values ('fixture', $1, null, 'pending', 250, 'EUR')`, f.accountID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "credit citing evidence that does not exist",
			rule: "C-2",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.entry (brand_id, account_id, network_transaction_id, state, amount_minor, currency)
					 values ('fixture', $1, gen_random_uuid(), 'pending', 250, 'EUR')`, f.accountID)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "credit resting on somebody else's click",
			rule: "C-2",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// A real click, real evidence, wrong member: the citation is
				// present but it is evidence of the wrong thing. The report
				// is a fresh one, so the exactly-once index cannot be what
				// rejects this - the ownership key has to be.
				var otherAccount string
				if err := tx.QueryRow(ctx,
					`insert into account (email, display_name) values ($1, 'Other Member') returning id`,
					"other-"+f.suffix+"@example.test").Scan(&otherAccount); err != nil {
					return err
				}
				var otherReport string
				if err := tx.QueryRow(ctx,
					`insert into cashback.network_transaction
					     (network_id, network_account_id, external_id, status_raw, status,
					      sale_amount_minor, commission_minor, currency, transacted_at,
					      query_window_start, query_window_end, raw_payload)
					 values ($1, $2, $3, 'approved', 'confirmed', 5000, 250, 'EUR', now(),
					         now() - interval '1 day', now(), '{}'::jsonb) returning id`,
					f.networkID, f.networkAccountID, "foreign-"+f.suffix).Scan(&otherReport); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into cashback.entry (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency)
					 values ('fixture', $1, $2, $3, 'pending', 250, 'EUR')`,
					otherAccount, otherReport, f.clickID)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "a second credit on one report",
			rule: "exactly once",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.entry (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency)
					 values ('fixture', $1, $2, $3, 'pending', 250, 'EUR')`,
					f.accountID, f.networkTxn, f.clickID)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "credit with no brand",
			rule: "ADR-0004",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// An entry is money owed to a member by a brand; a row whose
				// brand nobody stated is a row nobody can scope later.
				_, err := tx.Exec(ctx,
					`insert into cashback.entry (account_id, network_transaction_id, click_id, state, amount_minor, currency)
					 values ($1, $2, $3, 'pending', 250, 'EUR')`, f.accountID, f.networkTxn, f.clickID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "credit with a blank brand",
			rule: "ADR-0004",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.entry (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency)
					 values ('  ', $1, $2, $3, 'pending', 250, 'EUR')`, f.accountID, f.networkTxn, f.clickID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "credit of nothing",
			rule: "C-6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.entry (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency)
					 values ('fixture', $1, $2, $3, 'pending', 0, 'EUR')`, f.accountID, f.networkTxn, f.clickID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "credit expressed as a negative amount",
			rule: "SC-010",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// A reversal is a separate entry, never a negative one, so
				// the sign never has to be interpreted anywhere.
				_, err := tx.Exec(ctx,
					`insert into cashback.entry (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency)
					 values ('fixture', $1, $2, $3, 'pending', -250, 'EUR')`, f.accountID, f.networkTxn, f.clickID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "credit with no currency",
			rule: "C-6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.entry (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency)
					 values ('fixture', $1, $2, $3, 'pending', 250, null)`, f.accountID, f.networkTxn, f.clickID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "held entry naming no rule",
			rule: "US7",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// The operator queue is "entries with a hold rule"; a held
				// entry with none would be held and invisible.
				_, err := tx.Exec(ctx,
					`insert into cashback.entry (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency)
					 values ('fixture', $1, $2, $3, 'held', 250, 'EUR')`, f.accountID, f.networkTxn, f.clickID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "unheld entry carrying a hold rule",
			rule: "US7",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.entry (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency, hold_rule)
					 values ('fixture', $1, $2, $3, 'pending', 250, 'EUR', 'first_purchase_review')`,
					f.accountID, f.networkTxn, f.clickID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "reversal wearing a credit's state",
			rule: "SC-010",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.entry (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency, reversal_of_id)
					 values ('fixture', $1, $2, $3, 'confirmed', 250, 'EUR', $4)`,
					f.accountID, f.networkTxn, f.clickID, f.entryID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "a second reversal of one credit",
			rule: "SC-010",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// Two debits, one clawback. Each looks correct alone.
				reverse := func(external string) error {
					var report string
					if err := tx.QueryRow(ctx,
						`insert into cashback.network_transaction
						     (network_id, network_account_id, external_id, click_ref, status_raw, status,
						      sale_amount_minor, commission_minor, currency, transacted_at,
						      query_window_start, query_window_end, raw_payload)
						 values ($1, $2, $3, $4, 'reversed', 'reversed', 10000, -500, 'EUR', now(),
						         now() - interval '1 day', now(), '{}'::jsonb) returning id`,
						f.networkID, f.networkAccountID, external, f.clickRef).Scan(&report); err != nil {
						return err
					}
					_, err := tx.Exec(ctx,
						`insert into cashback.entry
						     (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency, reversal_of_id)
						 values ('fixture', $1, $2, $3, 'reversed', 250, 'EUR', $4)`,
						f.accountID, report, f.clickID, f.entryID)
					return err
				}
				if err := reverse("rev-a-" + f.suffix); err != nil {
					return err
				}
				return reverse("rev-b-" + f.suffix)
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "credit omitting the click the network reported",
			rule: "C-2",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// The report names a click reference, so the evidence
				// exists; an entry that ignores it is permanently
				// evidence-disconnected, because entry_guard freezes
				// click_id and it can never be repaired.
				var report string
				if err := tx.QueryRow(ctx,
					`insert into cashback.network_transaction
					     (network_id, network_account_id, external_id, click_ref, status_raw, status,
					      sale_amount_minor, commission_minor, currency, transacted_at,
					      query_window_start, query_window_end, raw_payload)
					 values ($1, $2, $3, $4, 'approved', 'confirmed', 5000, 250, 'EUR', now(),
					         now() - interval '1 day', now(), '{}'::jsonb) returning id`,
					f.networkID, f.networkAccountID, "attributed-"+f.suffix, f.clickRef).Scan(&report); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into cashback.entry (brand_id, account_id, network_transaction_id, state, amount_minor, currency)
					 values ('fixture', $1, $2, 'pending', 250, 'EUR')`, f.accountID, report)
				return err
			},
			wantCode: codeRaiseException,
		},
		{
			name: "credit citing a click the network never mentioned",
			rule: "C-2",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// A real click, owned by the right member, that this report
				// says nothing about. The composite key alone accepts it.
				otherRef := randomSuffix(t) + randomSuffix(t)
				var otherClick string
				if err := tx.QueryRow(ctx,
					`insert into cashback.click (click_ref, account_id, offer_id, rate_snapshot, member_share_bps_snapshot)
					 values ($1, $2, $3, '{}'::jsonb, 5000) returning id`,
					otherRef, f.accountID, f.offerID).Scan(&otherClick); err != nil {
					return err
				}
				var report string
				if err := tx.QueryRow(ctx,
					`insert into cashback.network_transaction
					     (network_id, network_account_id, external_id, click_ref, status_raw, status,
					      sale_amount_minor, commission_minor, currency, transacted_at,
					      query_window_start, query_window_end, raw_payload)
					 values ($1, $2, $3, $4, 'approved', 'confirmed', 5000, 250, 'EUR', now(),
					         now() - interval '1 day', now(), '{}'::jsonb) returning id`,
					f.networkID, f.networkAccountID, "mismatch-"+f.suffix, f.clickRef).Scan(&report); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into cashback.entry (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency)
					 values ('fixture', $1, $2, $3, 'pending', 250, 'EUR')`, f.accountID, report, otherClick)
				return err
			},
			wantCode: codeRaiseException,
		},
		{
			name: "posting claiming a different transfer than its transition",
			rule: "C-7",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// Both tables carry the reference; if they may disagree, the
				// provenance view answers with whichever it happens to read.
				var transitionID string
				if err := tx.QueryRow(ctx,
					`insert into cashback.entry_transition (entry_id, from_state, to_state, ledger_transfer_ref)
					 values ($1, 'confirmed', 'reserved', $2) returning id`,
					f.entryID, "reserve-"+f.suffix).Scan(&transitionID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into cashback.ledger_link (transition_id, entry_id, ledger_transfer_ref)
					 values ($1, $2, $3)`, transitionID, f.entryID, "some-other-transfer-"+f.suffix)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "re-homing an unattributed report",
			rule: "FR-034",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				var queued string
				if err := tx.QueryRow(ctx,
					`insert into cashback.unattributed_transaction (network_transaction_id) values ($1) returning id`,
					f.networkTxn).Scan(&queued); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`update cashback.unattributed_transaction set network_transaction_id = gen_random_uuid() where id = $1`,
					queued)
				return err
			},
			wantCode: codeRaiseException,
		},
		{
			name: "transition with no ledger transfer",
			rule: "D7",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// A state with no posting behind it is the wallet and the
				// ledger disagreeing, which is what C-1 exists to prevent.
				_, err := tx.Exec(ctx,
					`insert into cashback.entry_transition (entry_id, from_state, to_state, ledger_transfer_ref)
					 values ($1, 'confirmed', 'reserved', null)`, f.entryID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "transition with a blank ledger transfer",
			rule: "D7",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.entry_transition (entry_id, from_state, to_state, ledger_transfer_ref)
					 values ($1, 'confirmed', 'reserved', '   ')`, f.entryID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "transition that goes nowhere",
			rule: "D7",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.entry_transition (entry_id, from_state, to_state, ledger_transfer_ref)
					 values ($1, 'confirmed', 'confirmed', $2)`, f.entryID, "noop-"+f.suffix)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "posting attached to another entry's transition",
			rule: "C-7",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// The provenance view joins on this pair; a mismatched link
				// would silently reattribute money in the audit answer.
				var transitionID string
				if err := tx.QueryRow(ctx,
					`insert into cashback.entry_transition (entry_id, from_state, to_state, ledger_transfer_ref)
					 values ($1, 'confirmed', 'reserved', $2) returning id`,
					f.entryID, "reserve-"+f.suffix).Scan(&transitionID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into cashback.ledger_link (transition_id, entry_id, ledger_transfer_ref)
					 values ($1, gen_random_uuid(), $2)`, transitionID, "mismatch-"+f.suffix)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "two postings claiming one ledger transfer",
			rule: "C-1",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				var transitionID string
				if err := tx.QueryRow(ctx,
					`insert into cashback.entry_transition (entry_id, from_state, to_state, ledger_transfer_ref)
					 values ($1, 'confirmed', 'reserved', $2) returning id`,
					f.entryID, "reserve-"+f.suffix).Scan(&transitionID); err != nil {
					return err
				}
				// "transfer-"+suffix is already linked by the fixture.
				_, err := tx.Exec(ctx,
					`insert into cashback.ledger_link (transition_id, entry_id, ledger_transfer_ref)
					 values ($1, $2, $3)`, transitionID, f.entryID, "transfer-"+f.suffix)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "two unattributed rows for one report",
			rule: "FR-034",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				if _, err := tx.Exec(ctx,
					`insert into cashback.unattributed_transaction (network_transaction_id) values ($1)`,
					f.networkTxn); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`insert into cashback.unattributed_transaction (network_transaction_id) values ($1)`,
					f.networkTxn)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "half-recorded resolution of an unattributed report",
			rule: "FR-060",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.unattributed_transaction (network_transaction_id, resolved_by, resolved_at)
					 values ($1, $2, now())`, f.networkTxn, f.accountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seedCashbackEntry(t, tx)
			wantPgCode(t, tt.write(context.Background(), tx, f), tt.wantCode)
		})
	}
}

// TestEntryMoneyFactsAreFrozen asserts that an entry's member, evidence and
// amount cannot be edited into something else after the fact, and that paid
// and reversed are terminal - the rule that stops the same money being paid
// twice by walking an entry backwards.
func TestEntryMoneyFactsAreFrozen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		stmt string
	}{
		{name: "reassign the member", stmt: `update cashback.entry set account_id = gen_random_uuid() where id = $1`},
		{name: "swap the evidence", stmt: `update cashback.entry set network_transaction_id = gen_random_uuid() where id = $1`},
		{name: "drop the click", stmt: `update cashback.entry set click_id = null where id = $1`},
		{name: "inflate the amount", stmt: `update cashback.entry set amount_minor = 99999 where id = $1`},
		{name: "change the currency", stmt: `update cashback.entry set currency = 'USD' where id = $1`},
		{name: "move it to another brand", stmt: `update cashback.entry set brand_id = 'other' where id = $1`},
		{name: "backdate the entry", stmt: `update cashback.entry set created_at = now() - interval '1 year' where id = $1`},
		{name: "delete the entry", stmt: `delete from cashback.entry where id = $1`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seedCashbackEntry(t, tx)
			_, err := tx.Exec(context.Background(), tt.stmt, f.entryID)
			wantPgCode(t, err, codeRaiseException)
		})
	}
}

// TestPaidEntryCannotBecomePayableAgain is the terminal-state rule on its
// own, because it is the one that has money on it: an entry walked back
// from paid to confirmed would be paid a second time.
func TestPaidEntryCannotBecomePayableAgain(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashbackEntry(t, tx)

	for _, state := range []string{"reserved", "paid"} {
		if _, err := tx.Exec(ctx, `update cashback.entry set state = $1 where id = $2`, state, f.entryID); err != nil {
			t.Fatalf("advancing the entry to %s: %v", state, err)
		}
	}

	_, err := tx.Exec(ctx, `update cashback.entry set state = 'confirmed' where id = $1`, f.entryID)
	wantPgCode(t, err, codeRaiseException)
}

// TestEntryTransitionHistoryIsAppendOnly asserts that the record of how a
// member's money moved is evidence in the same sense the network report is.
func TestEntryTransitionHistoryIsAppendOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		stmt string
	}{
		{name: "rewrite the destination state", stmt: `update cashback.entry_transition set to_state = 'paid' where entry_id = $1`},
		{name: "rewrite the transfer reference", stmt: `update cashback.entry_transition set ledger_transfer_ref = 'other' where entry_id = $1`},
		{name: "delete the transition", stmt: `delete from cashback.entry_transition where entry_id = $1`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seedCashbackEntry(t, tx)
			_, err := tx.Exec(context.Background(), tt.stmt, f.entryID)
			wantPgCode(t, err, codeRaiseException)
		})
	}
}

// TestEntryStateCannotMoveWithoutItsTransition is D7 enforced rather than
// asserted. Before the deferred trigger existed, this migration claimed
// "no state is ever recorded without its posting" while the schema
// happily accepted a bare `update cashback.entry set state = 'paid'` -
// the rule lived in a comment and nowhere else.
//
// The check is DEFERRABLE INITIALLY DEFERRED, so it runs at COMMIT and a
// rolled-back transaction would never reach it. `set constraints all
// immediate` fires the pending checks on demand, which lets both
// directions be proved inside one rolled-back transaction: nothing is
// committed, and nothing is left behind.
//
// Both directions matter. A guard that rejects the illegal move but also
// rejects the legal one is not enforcement, it is breakage.
func TestEntryStateCannotMoveWithoutItsTransition(t *testing.T) {
	t.Parallel()

	t.Run("a bare state change is rejected", func(t *testing.T) {
		t.Parallel()
		tx := beginTx(t)
		ctx := context.Background()
		f := seedCashbackEntry(t, tx)

		// Accepted here: the check is deferred precisely so the entry and
		// its transition may arrive in either order.
		if _, err := tx.Exec(ctx,
			`update cashback.entry set state = 'reserved' where id = $1`, f.entryID); err != nil {
			t.Fatalf("the state change must be accepted until the checks run: %v", err)
		}
		_, err := tx.Exec(ctx, `set constraints all immediate`)
		wantPgCode(t, err, codeRaiseException)
	})

	t.Run("a transition recording a different hop does not count", func(t *testing.T) {
		t.Parallel()
		tx := beginTx(t)
		ctx := context.Background()
		f := seedCashbackEntry(t, tx)

		if _, err := tx.Exec(ctx,
			`update cashback.entry set state = 'reserved' where id = $1`, f.entryID); err != nil {
			t.Fatalf("state change: %v", err)
		}
		// A real transition, with a real transfer, for a hop that did not
		// happen. Existence of *a* transition must not be enough.
		if _, err := tx.Exec(ctx,
			`insert into cashback.entry_transition (entry_id, from_state, to_state, ledger_transfer_ref)
			 values ($1, 'pending', 'confirmed', $2)`, f.entryID, "wrong-hop-"+f.suffix); err != nil {
			t.Fatalf("recording the unrelated transition: %v", err)
		}
		_, err := tx.Exec(ctx, `set constraints all immediate`)
		wantPgCode(t, err, codeRaiseException)
	})

	t.Run("the state change and its transition together are accepted", func(t *testing.T) {
		t.Parallel()
		tx := beginTx(t)
		ctx := context.Background()
		f := seedCashbackEntry(t, tx)

		if _, err := tx.Exec(ctx,
			`update cashback.entry set state = 'reserved' where id = $1`, f.entryID); err != nil {
			t.Fatalf("state change: %v", err)
		}
		var transitionID string
		if err := tx.QueryRow(ctx,
			`insert into cashback.entry_transition (entry_id, from_state, to_state, ledger_transfer_ref)
			 values ($1, 'confirmed', 'reserved', $2) returning id`,
			f.entryID, "reserve-"+f.suffix).Scan(&transitionID); err != nil {
			t.Fatalf("recording the transition: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`insert into cashback.ledger_link (transition_id, entry_id, ledger_transfer_ref)
			 values ($1, $2, $3)`, transitionID, f.entryID, "reserve-"+f.suffix); err != nil {
			t.Fatalf("linking the posting: %v", err)
		}
		if _, err := tx.Exec(ctx, `set constraints all immediate`); err != nil {
			t.Fatalf("the legal move with its transition was rejected: %v", err)
		}
	})
}

// TestLedgerLinkIsImmutable asserts that the seam C-7 answers through is
// evidence like everything else it joins: a posting that could be rewritten
// after the fact would make the audit answer worth no more than the last
// person to edit it.
func TestLedgerLinkIsImmutable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		stmt string
	}{
		{name: "rewrite the transfer reference", stmt: `update cashback.ledger_link set ledger_transfer_ref = 'other' where entry_id = $1`},
		{name: "backdate the posting", stmt: `update cashback.ledger_link set posted_at = now() - interval '1 year' where entry_id = $1`},
		{name: "delete the posting", stmt: `delete from cashback.ledger_link where entry_id = $1`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seedCashbackEntry(t, tx)
			_, err := tx.Exec(context.Background(), tt.stmt, f.entryID)
			wantPgCode(t, err, codeRaiseException)
		})
	}
}

// TestEarningsTablesRejectTruncate closes the bulk route on the two
// append-only tables here. Not parallel: TRUNCATE takes ACCESS EXCLUSIVE
// locks that would contend with the other subtests' open transactions.
func TestEarningsTablesRejectTruncate(t *testing.T) {
	tests := []struct {
		name string
		stmt string
	}{
		{name: "entry", stmt: `truncate cashback.entry cascade`},
		{name: "entry_transition", stmt: `truncate cashback.entry_transition cascade`},
		{name: "ledger_link", stmt: `truncate cashback.ledger_link cascade`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := beginTx(t)
			ctx := context.Background()
			if _, err := tx.Exec(ctx, `set local lock_timeout = '10s'`); err != nil {
				t.Fatalf("set lock_timeout: %v", err)
			}
			_, err := tx.Exec(ctx, tt.stmt)
			wantPgCode(t, err, codeRaiseException)
		})
	}
}

// TestReversalLeavesAnAuditablePair is SC-010 as a test: a reversal never
// edits the credit it undoes, so both rows survive and the pair is what an
// auditor reads.
func TestReversalLeavesAnAuditablePair(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashbackEntry(t, tx)

	// The reversal rests on its own evidence: the superseding report that
	// carried the network's reversal.
	var reversalReport string
	err := tx.QueryRow(ctx,
		`insert into cashback.network_transaction
		     (network_id, network_account_id, external_id, click_ref, status_raw, status,
		      sale_amount_minor, commission_minor, currency, transacted_at,
		      query_window_start, query_window_end, raw_payload, supersedes_id)
		 values ($1, $2, $3, $4, 'reversed', 'reversed', 10000, -500, 'EUR', now(),
		         now() - interval '1 day', now(), '{}'::jsonb, $5) returning id`,
		f.networkID, f.networkAccountID, f.externalID, f.clickRef, f.networkTxn,
	).Scan(&reversalReport)
	if err != nil {
		t.Fatalf("storing the reversing report: %v", err)
	}

	var reversalEntry string
	err = tx.QueryRow(ctx,
		`insert into cashback.entry
		     (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency, reversal_of_id)
		 values ('fixture', $1, $2, $3, 'reversed', 250, 'EUR', $4) returning id`,
		f.accountID, reversalReport, f.clickID, f.entryID,
	).Scan(&reversalEntry)
	if err != nil {
		t.Fatalf("a valid reversing entry was rejected: %v", err)
	}

	var originalState string
	var pair int
	err = tx.QueryRow(ctx,
		`select (select state from cashback.entry where id = $1),
		        (select count(*) from cashback.entry where id in ($1, $2))`,
		f.entryID, reversalEntry).Scan(&originalState, &pair)
	if err != nil {
		t.Fatalf("reading the pair: %v", err)
	}
	if originalState != "confirmed" {
		t.Fatalf("the reversed credit is now %q: reversal edited the entry it should have left alone", originalState)
	}
	if pair != 2 {
		t.Fatalf("%d entries after a reversal, want the auditable pair of 2", pair)
	}
}

// TestValidEarningsChainIsAccepted is the positive control: hold, release,
// confirm - each transition carrying its ledger transfer, each posting
// linked, nothing rejected. Without it the rejection tables above could be
// satisfied by a schema that refuses everything.
func TestValidEarningsChainIsAccepted(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashbackEvidence(t, tx)

	var entryID string
	err := tx.QueryRow(ctx,
		`insert into cashback.entry
		     (brand_id, account_id, network_transaction_id, click_id, state, amount_minor, currency, hold_rule)
		 values ('fixture', $1, $2, $3, 'held', 250, 'EUR', 'first_purchase_review') returning id`,
		f.accountID, f.networkTxn, f.clickID).Scan(&entryID)
	if err != nil {
		t.Fatalf("a held entry was rejected: %v", err)
	}

	steps := []struct {
		from, to, transfer string
		clearHold          bool
	}{
		{from: "", to: "held", transfer: "hold-" + f.suffix},
		{from: "held", to: "pending", transfer: "release-" + f.suffix, clearHold: true},
		{from: "pending", to: "confirmed", transfer: "confirm-" + f.suffix},
	}

	for _, step := range steps {
		if step.from != "" {
			// Releasing clears the hold rule in the same statement that
			// moves the state, because the constraint ties them together.
			if step.clearHold {
				_, err = tx.Exec(ctx,
					`update cashback.entry set state = $1, hold_rule = null where id = $2`, step.to, entryID)
			} else {
				_, err = tx.Exec(ctx, `update cashback.entry set state = $1 where id = $2`, step.to, entryID)
			}
			if err != nil {
				t.Fatalf("moving the entry to %s: %v", step.to, err)
			}
		}

		var transitionID string
		var from any
		if step.from != "" {
			from = step.from
		}
		err = tx.QueryRow(ctx,
			`insert into cashback.entry_transition (entry_id, from_state, to_state, ledger_transfer_ref)
			 values ($1, $2, $3, $4) returning id`,
			entryID, from, step.to, step.transfer).Scan(&transitionID)
		if err != nil {
			t.Fatalf("recording the transition to %s: %v", step.to, err)
		}
		if _, err := tx.Exec(ctx,
			`insert into cashback.ledger_link (transition_id, entry_id, ledger_transfer_ref)
			 values ($1, $2, $3)`, transitionID, entryID, step.transfer); err != nil {
			t.Fatalf("linking the posting for %s: %v", step.to, err)
		}
	}

	var transitions, postings int
	var state string
	err = tx.QueryRow(ctx,
		`select (select count(*) from cashback.entry_transition where entry_id = $1),
		        (select count(*) from cashback.ledger_link where entry_id = $1),
		        (select state from cashback.entry where id = $1)`,
		entryID).Scan(&transitions, &postings, &state)
	if err != nil {
		t.Fatalf("reading the finished chain: %v", err)
	}
	if transitions != 3 || postings != 3 {
		t.Fatalf("%d transitions and %d postings, want 3 and 3: every transition carries exactly one posting (D7)",
			transitions, postings)
	}
	if state != "confirmed" {
		t.Fatalf("entry state is %q, want confirmed", state)
	}
}
