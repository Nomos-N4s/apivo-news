package db_test

// These tests assert C-3 the way the constitution states it: network
// transaction records and click records reject UPDATE, DELETE and TRUNCATE,
// and a status change is a NEW superseding record, never an edit.
//
// The supersession tests are the interesting half. Immutability means the
// previous row can never be marked as superseded, so "exactly one current
// row" is not stamped anywhere - it is derived from one root per
// transaction plus no forks in the chain. These tests attack both halves of
// that derivation, because if either fails the wallet has two truths about
// what a network said.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// wantImmutableRefusal is wantPgCode for C-3, and it asks one thing more.
//
// Every RAISE in the schema carries SQLSTATE P0001, so a bare code check
// says "something refused this" rather than "the immutability guard refused
// this". That distinction has teeth: raise_immutable() names the table and
// the operation in its message, and a guard replaced by any other P0001 -
// a check somebody added, a trigger that fires for a different reason -
// would keep a code-only assertion green while the row became editable.
//
// The same argument the wallet's C-1 tests make (internal/cashback/wallet/
// invariants_test.go), applied to the rule that keeps a member's evidence
// from being rewritten.
func wantImmutableRefusal(t *testing.T, err error, table, op string) {
	t.Helper()
	wantPgCode(t, err, codeRaiseException)

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("want *pgconn.PgError, got %T: %v", err, err)
	}
	// raise_immutable() says: table % is immutable: % is not allowed (...)
	want := "table " + table + " is immutable: " + op + " is not allowed"
	if !strings.Contains(pgErr.Message, want) {
		t.Fatalf("the refusal was %q, want the immutability guard's own words %q; any other P0001 would satisfy a code-only check while the row became editable",
			pgErr.Message, want)
	}
}

// TestClickIsImmutable asserts the attribution evidence cannot be altered.
// A rewritable rate snapshot would let a credit be recomputed at a rate the
// member never saw (FR-013).
func TestClickIsImmutable(t *testing.T) {
	t.Parallel()

	// op is the operation raise_immutable() will name in its refusal, so a
	// case asserts the guard it meant to provoke rather than any P0001.
	tests := []struct {
		name string
		op   string
		stmt string
	}{
		{name: "reassign the member", op: "UPDATE", stmt: `update cashback.click set account_id = gen_random_uuid() where id = $1`},
		{name: "rewrite the rate snapshot", op: "UPDATE", stmt: `update cashback.click set rate_snapshot = '{"rate_bps":9000}'::jsonb where id = $1`},
		{name: "rewrite the member share", op: "UPDATE", stmt: `update cashback.click set member_share_bps_snapshot = 10000 where id = $1`},
		{name: "move the click in time", op: "UPDATE", stmt: `update cashback.click set clicked_at = now() - interval '30 days' where id = $1`},
		{name: "delete the click", op: "DELETE", stmt: `delete from cashback.click where id = $1`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seedCashbackEvidence(t, tx)
			_, err := tx.Exec(context.Background(), tt.stmt, f.clickID)
			wantImmutableRefusal(t, err, "click", tt.op)
		})
	}
}

// TestNetworkTransactionIsImmutable asserts C-3 on the record a member's
// money actually rests on: what the network reported cannot be edited into
// something more convenient.
func TestNetworkTransactionIsImmutable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		op   string
		stmt string
	}{
		{name: "promote the status", op: "UPDATE", stmt: `update cashback.network_transaction set status = 'confirmed' where id = $1`},
		{name: "rewrite the network's own status", op: "UPDATE", stmt: `update cashback.network_transaction set status_raw = 'approved!' where id = $1`},
		{name: "inflate the commission", op: "UPDATE", stmt: `update cashback.network_transaction set commission_minor = 999999 where id = $1`},
		{name: "reassign the click reference", op: "UPDATE", stmt: `update cashback.network_transaction set click_ref = null where id = $1`},
		{name: "rewrite the payload", op: "UPDATE", stmt: `update cashback.network_transaction set raw_payload = '{}'::jsonb where id = $1`},
		{name: "delete the evidence", op: "DELETE", stmt: `delete from cashback.network_transaction where id = $1`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seedCashbackEvidence(t, tx)
			_, err := tx.Exec(context.Background(), tt.stmt, f.networkTxn)
			wantImmutableRefusal(t, err, "network_transaction", tt.op)
		})
	}
}

// TestCashbackEvidenceRejectsTruncate closes the bulk route: row-level
// triggers do not fire on TRUNCATE, so the statement-level triggers must.
// Deliberately not parallel - TRUNCATE takes ACCESS EXCLUSIVE locks that
// would contend with the other subtests' open transactions.
func TestCashbackEvidenceRejectsTruncate(t *testing.T) {
	tests := []struct {
		name string
		stmt string
	}{
		{name: "click", stmt: `truncate cashback.click cascade`},
		{name: "network_transaction", stmt: `truncate cashback.network_transaction cascade`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := beginTx(t)
			ctx := context.Background()
			if _, err := tx.Exec(ctx, `set local lock_timeout = '10s'`); err != nil {
				t.Fatalf("set lock_timeout: %v", err)
			}
			_, err := tx.Exec(ctx, tt.stmt)
			wantImmutableRefusal(t, err, tt.name, "TRUNCATE")
		})
	}
}

// TestCashbackEvidenceRejectsIllegalWrites is the rejection table for the
// two evidence tables: everything that would make a click guessable, an
// attribution anonymous, or a reported transaction ambiguous.
func TestCashbackEvidenceRejectsIllegalWrites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rule     string
		write    func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error
		wantCode string
	}{
		{
			name: "click with a guessable reference",
			rule: "FR-020",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.click (click_ref, account_id, offer_id, rate_snapshot, member_share_bps_snapshot)
					 values ('42', $1, $2, '{}'::jsonb, 5000)`, f.accountID, f.offerID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "click reference that is not URL-safe",
			rule: "FR-021",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// A reference that has to be escaped into the network's
				// click parameter is a reference that comes back mangled.
				_, err := tx.Exec(ctx,
					`insert into cashback.click (click_ref, account_id, offer_id, rate_snapshot, member_share_bps_snapshot)
					 values ('this ref/has?separators&in-it', $1, $2, '{}'::jsonb, 5000)`, f.accountID, f.offerID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "anonymous click",
			rule: "FR-023",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// The whole point of the NOT NULL: an anonymous click can
				// never be credited later because it can never exist.
				_, err := tx.Exec(ctx,
					`insert into cashback.click (click_ref, account_id, offer_id, rate_snapshot, member_share_bps_snapshot)
					 values ($1, null, $2, '{}'::jsonb, 5000)`,
					randomSuffix(t)+randomSuffix(t), f.offerID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "click with no rate snapshot",
			rule: "FR-013",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.click (click_ref, account_id, offer_id, rate_snapshot, member_share_bps_snapshot)
					 values ($1, $2, $3, null, 5000)`,
					randomSuffix(t)+randomSuffix(t), f.accountID, f.offerID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "two clicks on one reference",
			rule: "FR-020",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// A reused reference is two members' claim on one report.
				_, err := tx.Exec(ctx,
					`insert into cashback.click (click_ref, account_id, offer_id, rate_snapshot, member_share_bps_snapshot)
					 values ($1, $2, $3, '{}'::jsonb, 5000)`, f.clickRef, f.accountID, f.offerID)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "report in an unknown status",
			rule: "FR-033",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.network_transaction
					     (network_id, network_account_id, external_id, status_raw, status,
					      sale_amount_minor, commission_minor, currency, transacted_at,
					      query_window_start, query_window_end, raw_payload)
					 values ($1, $2, $3, 'weird', 'maybe', 100, 10, 'EUR', now(), now(), now(), '{}'::jsonb)`,
					f.networkID, f.networkAccountID, "other-"+f.suffix)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "report with a lowercase currency",
			rule: "C-6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.network_transaction
					     (network_id, network_account_id, external_id, status_raw, status,
					      sale_amount_minor, commission_minor, currency, transacted_at,
					      query_window_start, query_window_end, raw_payload)
					 values ($1, $2, $3, 'approved', 'confirmed', 100, 10, 'eur', now(), now(), now(), '{}'::jsonb)`,
					f.networkID, f.networkAccountID, "other-"+f.suffix)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "report with no payload",
			rule: "FR-032",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// Normalisation can be wrong; without the verbatim payload
				// there is nothing left to prove what was actually said.
				_, err := tx.Exec(ctx,
					`insert into cashback.network_transaction
					     (network_id, network_account_id, external_id, status_raw, status,
					      sale_amount_minor, commission_minor, currency, transacted_at,
					      query_window_start, query_window_end, raw_payload)
					 values ($1, $2, $3, 'approved', 'confirmed', 100, 10, 'EUR', now(), now(), now(), null)`,
					f.networkID, f.networkAccountID, "other-"+f.suffix)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "report carrying a blank click reference",
			rule: "C-3",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// Blank is not "no reference": the digest folds null to the
				// empty string, so a blank would fingerprint identically to
				// an unattributed report while still counting as present in
				// the attributed index. Two different claims, one row shape.
				_, err := tx.Exec(ctx,
					`insert into cashback.network_transaction
					     (network_id, network_account_id, external_id, click_ref, status_raw, status,
					      sale_amount_minor, commission_minor, currency, transacted_at,
					      query_window_start, query_window_end, raw_payload)
					 values ($1, $2, $3, '   ', 'approved', 'confirmed', 100, 10, 'EUR', now(),
					         now() - interval '1 day', now(), '{}'::jsonb)`,
					f.networkID, f.networkAccountID, "blankref-"+f.suffix)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "report whose query window ends before it starts",
			rule: "FR-031",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.network_transaction
					     (network_id, network_account_id, external_id, status_raw, status,
					      sale_amount_minor, commission_minor, currency, transacted_at,
					      query_window_start, query_window_end, raw_payload)
					 values ($1, $2, $3, 'approved', 'confirmed', 100, 10, 'EUR', now(),
					         now(), now() - interval '1 day', '{}'::jsonb)`,
					f.networkID, f.networkAccountID, "other-"+f.suffix)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "unchanged re-report offered as a new root",
			rule: "US2 scenario 3",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// The poll that finds nothing new must store nothing new.
				_, err := tx.Exec(ctx,
					`insert into cashback.network_transaction
					     (network_id, network_account_id, external_id, click_ref, status_raw, status,
					      sale_amount_minor, commission_minor, currency, transacted_at,
					      query_window_start, query_window_end, raw_payload)
					 select network_id, network_account_id, external_id, click_ref, status_raw, status,
					        sale_amount_minor, commission_minor, currency, transacted_at,
					        query_window_start, query_window_end, raw_payload
					   from cashback.network_transaction where id = $1`, f.networkTxn)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "unchanged re-report offered as a supersession",
			rule: "US2 scenario 3",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.network_transaction
					     (network_id, network_account_id, external_id, click_ref, status_raw, status,
					      sale_amount_minor, commission_minor, currency, transacted_at,
					      query_window_start, query_window_end, raw_payload, supersedes_id)
					 select network_id, network_account_id, external_id, click_ref, status_raw, status,
					        sale_amount_minor, commission_minor, currency, transacted_at,
					        query_window_start, query_window_end, raw_payload, id
					   from cashback.network_transaction where id = $1`, f.networkTxn)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "a second root for one transaction",
			rule: "one current row",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// Different facts, so the digest differs and the dedup
				// constraint does not catch it: the root index must.
				_, err := tx.Exec(ctx,
					`insert into cashback.network_transaction
					     (network_id, network_account_id, external_id, status_raw, status,
					      sale_amount_minor, commission_minor, currency, transacted_at,
					      query_window_start, query_window_end, raw_payload)
					 values ($1, $2, $3, 'declined', 'declined', 10000, 0, 'EUR', now(),
					         now() - interval '1 day', now(), '{}'::jsonb)`,
					f.networkID, f.networkAccountID, f.externalID)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "two reports superseding the same row",
			rule: "no forks",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// A fork would give one transaction two tips, and the wallet
				// two truths about what the network said.
				const supersede = `insert into cashback.network_transaction
				     (network_id, network_account_id, external_id, click_ref, status_raw, status,
				      sale_amount_minor, commission_minor, currency, transacted_at,
				      query_window_start, query_window_end, raw_payload, supersedes_id)
				 values ($1, $2, $3, $4, $5, $6, 10000, 500, 'EUR', now(),
				         now() - interval '1 day', now(), '{}'::jsonb, $7)`
				if _, err := tx.Exec(ctx, supersede,
					f.networkID, f.networkAccountID, f.externalID, f.clickRef,
					"reversed", "reversed", f.networkTxn); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, supersede,
					f.networkID, f.networkAccountID, f.externalID, f.clickRef,
					"declined", "declined", f.networkTxn)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "superseding a report about a different transaction",
			rule: "one chain per transaction",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.network_transaction
					     (network_id, network_account_id, external_id, status_raw, status,
					      sale_amount_minor, commission_minor, currency, transacted_at,
					      query_window_start, query_window_end, raw_payload, supersedes_id)
					 values ($1, $2, $3, 'approved', 'confirmed', 100, 10, 'EUR', now(),
					         now() - interval '1 day', now(), '{}'::jsonb, $4)`,
					f.networkID, f.networkAccountID, "unrelated-"+f.suffix, f.networkTxn)
				return err
			},
			wantCode: codeRaiseException,
		},
		{
			name: "superseding a report that does not exist",
			rule: "one chain per transaction",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.network_transaction
					     (network_id, network_account_id, external_id, status_raw, status,
					      sale_amount_minor, commission_minor, currency, transacted_at,
					      query_window_start, query_window_end, raw_payload, supersedes_id)
					 values ($1, $2, $3, 'approved', 'confirmed', 100, 10, 'EUR', now(),
					         now() - interval '1 day', now(), '{}'::jsonb, gen_random_uuid())`,
					f.networkID, f.networkAccountID, f.externalID)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seedCashbackEvidence(t, tx)
			wantPgCode(t, tt.write(context.Background(), tx, f), tt.wantCode)
		})
	}
}

// TestNetworkTransactionDigestIsComputedByTheDatabase asserts that the
// caller is not the authority on the fingerprint, exactly as source_item's
// content_hash is not the caller's to supply. A poller that could choose
// the digest could make a changed report look unchanged - and a reversal
// that looks unchanged is a reversal that never reaches the member.
func TestNetworkTransactionDigestIsComputedByTheDatabase(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashbackEvidence(t, tx)

	const lie = "0000000000000000000000000000000000000000000000000000000000000000"

	var stored string
	err := tx.QueryRow(ctx,
		`insert into cashback.network_transaction
		     (network_id, network_account_id, external_id, click_ref, status_raw, status,
		      sale_amount_minor, commission_minor, currency, transacted_at,
		      query_window_start, query_window_end, raw_payload, content_digest, supersedes_id)
		 values ($1, $2, $3, $4, 'reversed', 'reversed', 10000, -500, 'EUR', now(),
		         now() - interval '1 day', now(), '{}'::jsonb, $5, $6)
		 returning content_digest`,
		f.networkID, f.networkAccountID, f.externalID, f.clickRef, lie, f.networkTxn,
	).Scan(&stored)
	if err != nil {
		t.Fatalf("a valid superseding report was rejected: %v", err)
	}
	if stored == lie {
		t.Fatal("the caller's content_digest was stored: a poller could make a changed report look unchanged")
	}
	if len(stored) != 64 {
		t.Fatalf("content_digest is %d characters, want a 64-character sha256 hex digest", len(stored))
	}

	// The same false digest offered with different facts is accepted, which
	// is the other half of the proof: the column is derived from the facts
	// and the supplied value is discarded rather than merely validated.
	var second string
	err = tx.QueryRow(ctx,
		`insert into cashback.network_transaction
		     (network_id, network_account_id, external_id, status_raw, status,
		      sale_amount_minor, commission_minor, currency, transacted_at,
		      query_window_start, query_window_end, raw_payload, content_digest)
		 values ($1, $2, $3, 'approved', 'confirmed', 2500, 125, 'EUR', now(),
		         now() - interval '1 day', now(), '{}'::jsonb, $4)
		 returning content_digest`,
		f.networkID, f.networkAccountID, "second-"+f.suffix, lie).Scan(&second)
	if err != nil {
		t.Fatalf("an unrelated report carrying the same false digest was rejected: %v", err)
	}
	if second == stored || second == lie {
		t.Fatalf("digest %q does not distinguish two different reports", second)
	}
}

// TestSupersessionKeepsExactlyOneCurrentRow is the derivation stated as a
// test: after a status change there are two rows of evidence and exactly
// one of them is current, without anything having been edited (C-3).
func TestSupersessionKeepsExactlyOneCurrentRow(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashbackEvidence(t, tx)

	var reversalID string
	err := tx.QueryRow(ctx,
		`insert into cashback.network_transaction
		     (network_id, network_account_id, external_id, click_ref, status_raw, status,
		      sale_amount_minor, commission_minor, currency, transacted_at,
		      query_window_start, query_window_end, raw_payload, supersedes_id)
		 values ($1, $2, $3, $4, 'reversed', 'reversed', 10000, -500, 'EUR', now(),
		         now() - interval '1 day', now(), '{"reversed":true}'::jsonb, $5)
		 returning id`,
		f.networkID, f.networkAccountID, f.externalID, f.clickRef, f.networkTxn,
	).Scan(&reversalID)
	if err != nil {
		t.Fatalf("a reversing report was rejected: %v", err)
	}

	// The current row is the tip: the one no other row supersedes.
	rows, err := tx.Query(ctx,
		`select nt.id, nt.status
		   from cashback.network_transaction nt
		  where nt.network_id = $1 and nt.external_id = $2
		    and not exists (
		        select 1 from cashback.network_transaction later
		         where later.supersedes_id = nt.id
		    )`, f.networkID, f.externalID)
	if err != nil {
		t.Fatalf("querying for the current row: %v", err)
	}
	defer rows.Close()

	var current []string
	var status string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id, &status); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		current = append(current, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating: %v", err)
	}

	if len(current) != 1 {
		t.Fatalf("%d current rows for one transaction, want exactly 1", len(current))
	}
	if current[0] != reversalID {
		t.Fatal("the current row is not the latest report: the superseded row is still being read as current")
	}
	if status != "reversed" {
		t.Fatalf("current status is %q, want reversed", status)
	}

	// Both rows survive: the evidence trail is the whole point of never
	// editing the earlier report.
	var kept int
	if err := tx.QueryRow(ctx,
		`select count(*) from cashback.network_transaction where network_id = $1 and external_id = $2`,
		f.networkID, f.externalID).Scan(&kept); err != nil {
		t.Fatalf("counting the evidence trail: %v", err)
	}
	if kept != 2 {
		t.Fatalf("%d rows of evidence after a status change, want 2", kept)
	}
}
