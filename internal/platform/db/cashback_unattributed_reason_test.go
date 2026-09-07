package db_test

// 0038 (spec 004, FR-098): a queue row says why it is there.
//
// The reason is part of the observation, not part of the resolution, so it
// is subject to the same rule the rest of the observation is: written once,
// never re-labelled. A row whose cause could be edited afterwards would no
// longer record what an operator was actually shown - which is the whole
// reason 0013 froze the report and the detection instant, and 0024 froze
// the resolution's direction.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestAQueuedReportSaysWhyItIsQueued(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashbackEvidence(t, tx)

	queue := func(sp pgx.Tx, reason any) error {
		_, err := sp.Exec(ctx,
			`insert into cashback.unattributed_transaction (network_transaction_id, reason)
			 values ($1, $2)`, f.networkTxn, reason)
		return err
	}

	// Every cause the schema admits, including the one nothing writes yet:
	// route_cannot_attribute is carried so its writer (T233/T241) does not
	// widen an enum clients have already been shown.
	//
	// refused runs one statement under a savepoint and answers its error, so
	// a nil answer here is the row being accepted and rolled back - which is
	// what lets six mutually exclusive rows be tried against one report.
	for _, reason := range []string{
		"no_reference",
		"unknown_reference",
		"foreign_network",
		"click_already_credited",
		"foreign_currency",
		"route_cannot_attribute",
	} {
		t.Run(reason+" is a cause a row may carry", func(t *testing.T) {
			if err := refused(ctx, tx, func(sp pgx.Tx) error { return queue(sp, reason) }); err != nil {
				t.Fatalf("a queue row citing %s was refused: %v", reason, err)
			}
		})
	}

	// A cause outside the six is a client - or a future statement - inventing
	// a vocabulary the operator surface cannot render or act on.
	t.Run("a cause outside the six is refused", func(t *testing.T) {
		var pgErr *pgconn.PgError
		err := refused(ctx, tx, func(sp pgx.Tx) error { return queue(sp, "seemed_odd") })
		if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.CheckViolation {
			t.Fatalf("error = %v, want a check violation", err)
		}
		if pgErr.ConstraintName != "unattributed_reason_known" {
			t.Errorf("refused by %q, want %q", pgErr.ConstraintName, "unattributed_reason_known")
		}
	})

	t.Run("a row with no cause at all is refused", func(t *testing.T) {
		var pgErr *pgconn.PgError
		err := refused(ctx, tx, func(sp pgx.Tx) error { return queue(sp, nil) })
		if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.NotNullViolation {
			t.Fatalf("error = %v, want a not-null violation", err)
		}
	})

	// The freeze. A dismissal may still be recorded on this row - that is the
	// one decision left open - but why it was queued is evidence.
	t.Run("a recorded cause cannot be re-labelled", func(t *testing.T) {
		var queued string
		if err := tx.QueryRow(ctx,
			`insert into cashback.unattributed_transaction (network_transaction_id, reason)
			 values ($1, 'unknown_reference') returning id`, f.networkTxn).Scan(&queued); err != nil {
			t.Fatalf("seeding the queue row: %v", err)
		}

		var pgErr *pgconn.PgError
		err := refused(ctx, tx, func(sp pgx.Tx) error {
			_, err := sp.Exec(ctx,
				`update cashback.unattributed_transaction set reason = 'foreign_network' where id = $1`, queued)
			return err
		})
		if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.RaiseException {
			t.Fatalf("error = %v, want the guard's own exception", err)
		}

		// The resolution is still writable on the same row, so the freeze is
		// the reason's and not the row's.
		if err := refused(ctx, tx, func(sp pgx.Tx) error {
			_, err := sp.Exec(ctx,
				`update cashback.unattributed_transaction
				    set resolved_by = $2, resolved_reason = 'dismissed: the sibling report credited', resolved_at = now()
				  where id = $1`, queued, f.accountID)
			return err
		}); err != nil {
			t.Fatalf("recording a resolution on a row with a frozen cause: %v", err)
		}
	})

	// The backfill's promise, read from the catalog rather than assumed: a
	// row written before 0038 - or by anything that forgot - cannot exist.
	t.Run("no queue row carries no cause", func(t *testing.T) {
		var nullable string
		if err := tx.QueryRow(ctx,
			`select is_nullable from information_schema.columns
			  where table_schema = 'cashback' and table_name = 'unattributed_transaction'
			    and column_name = 'reason'`).Scan(&nullable); err != nil {
			t.Fatalf("reading the catalog: %v", err)
		}
		if nullable != "NO" {
			t.Errorf("cashback.unattributed_transaction.reason is nullable")
		}
		var unfilled int
		if err := tx.QueryRow(ctx,
			`select count(*) from cashback.unattributed_transaction where reason is null`).Scan(&unfilled); err != nil {
			t.Fatalf("counting unfilled rows: %v", err)
		}
		if unfilled != 0 {
			t.Errorf("%d queue row(s) carry no cause after the backfill", unfilled)
		}
	})
}
