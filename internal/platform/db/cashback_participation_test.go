package db_test

// Participation is the member's opt-in, and the two rules that matter
// about it are both rules about what CANNOT happen:
//
//	FR-002  the accepted terms version is always on the record
//	FR-003  leaving never deletes the financial rows built on it
//
// The second is the one with money on it. A member who leaves stops
// earning; their entries, payouts and evidence stay exactly where they
// were, because accounting and the law both outlive a preference.
//
// Alongside it, the ADR-0004 tenant boundary: the brand id is carried on
// participation, merchant, entry and payout from day one, with no default,
// because a row whose brand nobody stated is a row nobody can scope later.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestCashbackParticipationRejectsIllegalWrites is the opt-in rejection
// table, plus the brand columns that have no honest default.
func TestCashbackParticipationRejectsIllegalWrites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rule     string
		write    func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error
		wantCode string
	}{
		{
			name: "opt-in recording no terms version",
			rule: "FR-002",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// "They agreed to something" is not a record anyone can rely
				// on when the terms are disputed.
				_, err := tx.Exec(ctx,
					`insert into cashback.participation (account_id, brand_id, terms_version, default_currency)
					 values ($1, 'fixture', '   ', 'EUR')`, f.accountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "opt-in with no brand",
			rule: "ADR-0004",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.participation (account_id, brand_id, terms_version, default_currency)
					 values ($1, null, 'terms-v1', 'EUR')`, f.accountID)
				return err
			},
			wantCode: codeNotNullViolation,
		},
		{
			name: "opt-in with a lowercase currency",
			rule: "C-6",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.participation (account_id, brand_id, terms_version, default_currency)
					 values ($1, 'fixture', 'terms-v1', 'eur')`, f.accountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "two opt-ins for one member",
			rule: "FR-001",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				// Two opt-ins would be two accepted terms versions, and no
				// way to say which one the member is held to.
				const optIn = `insert into cashback.participation (account_id, brand_id, terms_version, default_currency)
				               values ($1, 'fixture', 'terms-v1', 'EUR')`
				if _, err := tx.Exec(ctx, optIn, f.accountID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, optIn, f.accountID)
				return err
			},
			wantCode: codeUniqueViolation,
		},
		{
			name: "opt-in for an account that does not exist",
			rule: "FR-001",
			write: func(ctx context.Context, tx pgx.Tx, _ cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.participation (account_id, brand_id, terms_version, default_currency)
					 values (gen_random_uuid(), 'fixture', 'terms-v1', 'EUR')`)
				return err
			},
			wantCode: codeForeignKeyViolation,
		},
		{
			name: "left, but nobody knows when",
			rule: "FR-003",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.participation (account_id, brand_id, terms_version, default_currency, status)
					 values ($1, 'fixture', 'terms-v1', 'EUR', 'left')`, f.accountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "active, but with a leaving date",
			rule: "FR-003",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.participation (account_id, brand_id, terms_version, default_currency, left_at)
					 values ($1, 'fixture', 'terms-v1', 'EUR', now())`, f.accountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
		{
			name: "left before opting in",
			rule: "FR-003",
			write: func(ctx context.Context, tx pgx.Tx, f cashbackFixtures) error {
				_, err := tx.Exec(ctx,
					`insert into cashback.participation
					     (account_id, brand_id, terms_version, default_currency, status, opted_in_at, left_at)
					 values ($1, 'fixture', 'terms-v1', 'EUR', 'left', now(), now() - interval '1 day')`,
					f.accountID)
				return err
			},
			wantCode: codeCheckViolation,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			f := seedCashbackWithdrawal(t, tx)
			wantPgCode(t, tt.write(context.Background(), tx, f), tt.wantCode)
		})
	}
}

// TestParticipationEvidenceIsProtected asserts what FR-002 needs to be
// worth anything: the record of which terms a member accepted, and when,
// cannot be rewritten or deleted after the fact. The one legitimate way it
// changes is a rejoin, which re-states the acceptance rather than editing
// it - so that case is a positive control, not an exception carved out.
func TestParticipationEvidenceIsProtected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		stmt string
	}{
		{name: "rewrite the accepted terms", stmt: `update cashback.participation set terms_version = 'terms-v0' where account_id = $1`},
		{name: "backdate the opt-in", stmt: `update cashback.participation set opted_in_at = now() - interval '2 years' where account_id = $1`},
		{name: "change the wallet currency", stmt: `update cashback.participation set default_currency = 'USD' where account_id = $1`},
		{name: "reassign to another brand", stmt: `update cashback.participation set brand_id = 'other' where account_id = $1`},
		{name: "delete the opt-in", stmt: `delete from cashback.participation where account_id = $1`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			ctx := context.Background()
			f := seedCashbackWithdrawal(t, tx)

			if _, err := tx.Exec(ctx,
				`insert into cashback.participation (account_id, brand_id, terms_version, default_currency)
				 values ($1, 'fixture', 'terms-v1', 'EUR')`, f.accountID); err != nil {
				t.Fatalf("a valid opt-in was rejected: %v", err)
			}
			_, err := tx.Exec(ctx, tt.stmt, f.accountID)
			wantPgCode(t, err, codeRaiseException)
		})
	}
}

// TestRejoiningRestatesTheAcceptance is the positive control for the guard
// above: a member who left may come back, and coming back means accepting
// the terms in force then. That is a transition, so it is allowed - and it
// is the only way the accepted terms ever change.
func TestRejoiningRestatesTheAcceptance(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()
	f := seedCashbackWithdrawal(t, tx)

	if _, err := tx.Exec(ctx,
		`insert into cashback.participation (account_id, brand_id, terms_version, default_currency)
		 values ($1, 'fixture', 'terms-v1', 'EUR')`, f.accountID); err != nil {
		t.Fatalf("a valid opt-in was rejected: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`update cashback.participation set status = 'left', left_at = now() where account_id = $1`,
		f.accountID); err != nil {
		t.Fatalf("leaving: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`update cashback.participation
		    set status = 'active', left_at = null, opted_in_at = now(), terms_version = 'terms-v2'
		  where account_id = $1`, f.accountID); err != nil {
		t.Fatalf("rejoining under new terms was rejected: %v", err)
	}

	var status, terms string
	if err := tx.QueryRow(ctx,
		`select status, terms_version from cashback.participation where account_id = $1`,
		f.accountID).Scan(&status, &terms); err != nil {
		t.Fatalf("reading the reopened opt-in: %v", err)
	}
	if status != "active" || terms != "terms-v2" {
		t.Fatalf("after rejoining: status %q, terms %q; want active and terms-v2", status, terms)
	}
}

// TestTenantBoundaryTablesAllCarryABrand asserts ADR-0004's structural
// promise directly: the four records where a tenant boundary would fall
// each carry a brand id, and none of them has a default that would let a
// row be written without one.
//
// The merchant-side record is the ROUTE, not the retailer. ADR-0004 names
// "merchant availability", and after the many-to-many split availability
// is cashback.merchant_network: the retailer is a fact about the world,
// while which brand publishes them through which network is brand-scoped.
//
// This is the only place the question can be asked of all four at once,
// because participation is the last of them to exist - which is also why
// each column is created with its own table rather than collected here.
func TestTenantBoundaryTablesAllCarryABrand(t *testing.T) {
	t.Parallel()

	for _, table := range []string{"participation", "merchant_network", "entry", "payout"} {
		t.Run(table, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)

			var notNull bool
			var defaultExpr *string
			err := tx.QueryRow(context.Background(),
				`select a.attnotnull, pg_get_expr(d.adbin, d.adrelid)
				   from pg_attribute a
				   join pg_class c on c.oid = a.attrelid
				   join pg_namespace n on n.oid = c.relnamespace
				   left join pg_attrdef d on d.adrelid = a.attrelid and d.adnum = a.attnum
				  where n.nspname = 'cashback'
				    and c.relname = $1
				    and a.attname = 'brand_id'
				    and not a.attisdropped`, table).Scan(&notNull, &defaultExpr)
			if err != nil {
				t.Fatalf("cashback.%s has no brand_id column: the tenant boundary is not carried there (ADR-0004): %v", table, err)
			}
			if !notNull {
				t.Fatalf("cashback.%s.brand_id is nullable: a row with no brand cannot be scoped later", table)
			}
			if defaultExpr != nil {
				t.Fatalf("cashback.%s.brand_id defaults to %s: a brand literal in the schema is exactly what the constitution forbids", table, *defaultExpr)
			}
		})
	}
}
