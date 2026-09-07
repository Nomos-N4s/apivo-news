package db_test

// 0039 (B6, #548, FR-051 with FR-061): a verification names the person who
// performed it, and that name is as frozen as the rest of the verification.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestAVerificationNamesItsVerifier(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashback(t, tx)

	destination := func(sp pgx.Tx) string {
		var id string
		if err := sp.QueryRow(ctx, `
			insert into cashback.payout_destination (account_id, kind, details_ref)
			values ($1, 'sepa', $2) returning id`,
			f.accountID, "openbao:secret/x/"+randomSuffix(t)).Scan(&id); err != nil {
			t.Fatalf("seeding the destination: %v", err)
		}
		return id
	}

	// A verifier without a verification is not a state: there would be
	// nothing for them to have verified.
	t.Run("a verifier with no verification is refused", func(t *testing.T) {
		var pgErr *pgconn.PgError
		err := refused(ctx, tx, func(sp pgx.Tx) error {
			_, err := sp.Exec(ctx,
				`update cashback.payout_destination set verified_by = $2 where id = $1`,
				destination(sp), f.accountID)
			return err
		})
		if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.CheckViolation {
			t.Fatalf("error = %v, want a check violation", err)
		}
		if pgErr.ConstraintName != "payout_destination_verifier_verified" {
			t.Errorf("refused by %q, want %q", pgErr.ConstraintName, "payout_destination_verifier_verified")
		}
	})

	// The state ADR-0006's revisit trigger depends on: a destination a
	// payment provider verified at tokenisation has a method and an instant
	// and no human at all, and must stay storable.
	t.Run("a verification with no human is a state", func(t *testing.T) {
		if err := refused(ctx, tx, func(sp pgx.Tx) error {
			_, err := sp.Exec(ctx,
				`update cashback.payout_destination
				    set verified_at = now(), verified_method = 'provider: tokenised'
				  where id = $1`, destination(sp))
			return err
		}); err != nil {
			t.Fatalf("a provider-verified destination was refused: %v", err)
		}
	})

	// The freeze. It is the evidence a withdrawal was allowed to name the
	// destination, and who performed it is part of that evidence.
	t.Run("a recorded verifier cannot be changed", func(t *testing.T) {
		var id string
		if err := tx.QueryRow(ctx, `
			insert into cashback.payout_destination
			    (account_id, kind, details_ref, verified_at, verified_method, verified_by)
			values ($1, 'sepa', $2, now(), 'operator: checked', $1) returning id`,
			f.accountID, "openbao:secret/x/"+randomSuffix(t)).Scan(&id); err != nil {
			t.Fatalf("seeding the verified destination: %v", err)
		}

		var other string
		if err := tx.QueryRow(ctx, `
			insert into public.account (email, display_name, role)
			values ($1, 'Another Operator', 'operator') returning id`,
			"other-"+randomSuffix(t)+"@example.test").Scan(&other); err != nil {
			t.Fatalf("seeding the other operator: %v", err)
		}

		var pgErr *pgconn.PgError
		err := refused(ctx, tx, func(sp pgx.Tx) error {
			_, err := sp.Exec(ctx,
				`update cashback.payout_destination set verified_by = $2 where id = $1`, id, other)
			return err
		})
		if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.RaiseException {
			t.Fatalf("error = %v, want the guard's own exception", err)
		}
	})

	// The name has to be a real account: "a named human" is the foreign
	// key's guarantee and not any module's.
	t.Run("a verifier who is not an account is refused", func(t *testing.T) {
		var pgErr *pgconn.PgError
		err := refused(ctx, tx, func(sp pgx.Tx) error {
			_, err := sp.Exec(ctx,
				`update cashback.payout_destination
				    set verified_at = now(), verified_method = 'operator: checked', verified_by = gen_random_uuid()
				  where id = $1`, destination(sp))
			return err
		})
		if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.ForeignKeyViolation {
			t.Fatalf("error = %v, want a foreign key violation", err)
		}
	})
}
