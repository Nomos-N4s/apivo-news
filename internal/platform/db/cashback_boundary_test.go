package db_test

// These tests assert that the product boundary the constitution describes
// is a fact in the DATABASE, not a convention in review comments.
//
// Constitution v1.1.0, "Products": a product domain owns its own Postgres
// schema; the shared reference data (account, place, language,
// domain_event) is the only thing both products read; cross-product
// communication is asynchronous only, through domain_event.
//
// Migration 0010 states that in privileges. What follows checks it, table
// by table, including the tables that must stay unreachable.

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

// cashbackRole is the NOLOGIN group role 0010 creates. Privileges are read
// with has_*_privilege(), so nothing here has to connect as the role.
const cashbackRole = "cashback_domain"

// TestCashbackSchemaAndRoleExist is the precondition for every other
// assertion in this file: without the schema and the role there is no
// boundary to check.
func TestCashbackSchemaAndRoleExist(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)
	ctx := context.Background()

	var schemaExists bool
	if err := tx.QueryRow(ctx,
		`select exists (select 1 from pg_namespace where nspname = 'cashback')`).Scan(&schemaExists); err != nil {
		t.Fatalf("reading pg_namespace: %v", err)
	}
	if !schemaExists {
		t.Fatal("schema cashback does not exist: the cashback product has no boundary to own its tables")
	}

	var roleExists bool
	if err := tx.QueryRow(ctx,
		`select exists (select 1 from pg_roles where rolname = $1)`, cashbackRole).Scan(&roleExists); err != nil {
		t.Fatalf("reading pg_roles: %v", err)
	}
	if !roleExists {
		t.Fatalf("role %s does not exist: the boundary cannot be granted or revoked in one place", cashbackRole)
	}

	var hasUsage bool
	if err := tx.QueryRow(ctx,
		`select has_schema_privilege($1, 'cashback', 'usage')`, cashbackRole).Scan(&hasUsage); err != nil {
		t.Fatalf("reading schema privilege: %v", err)
	}
	if !hasUsage {
		t.Fatalf("role %s cannot use its own schema", cashbackRole)
	}
}

// TestCashbackRoleReachesOnlySharedReferenceData is the boundary itself.
// The negative cases carry the weight: a news table the cashback domain can
// read is a coupling that will be discovered by a join, not by a review.
func TestCashbackRoleReachesOnlySharedReferenceData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		table     string
		privilege string
		want      bool
		why       string
	}{
		{
			name: "reads accounts", table: "public.account", privilege: "select", want: true,
			why: "members are accounts; the constitution names account as shared reference data",
		},
		{
			name: "reads places", table: "public.place", privilege: "select", want: true,
			why: "merchant scope is a place; place is shared reference data",
		},
		{
			name: "reads languages", table: "public.language", privilege: "select", want: true,
			why: "merchant copy is keyed by language; language is shared reference data",
		},
		{
			name: "never writes accounts", table: "public.account", privilege: "update", want: false,
			why: "shared reference data is read-only across the boundary",
		},
		{
			name: "never writes places", table: "public.place", privilege: "insert", want: false,
			why: "shared reference data is read-only across the boundary",
		},
		{
			name: "appends events", table: "public.domain_event", privilege: "insert", want: true,
			why: "the event stream is the only channel between the two products",
		},
		{
			name: "reads events", table: "public.domain_event", privilege: "select", want: true,
			why: "a consumer must be able to read the stream it subscribes to",
		},
		{
			name: "never updates events", table: "public.domain_event", privilege: "update", want: false,
			why: "the stream is append-only; the trigger blocks it and the grant does not invite it",
		},
		{
			name: "never deletes events", table: "public.domain_event", privilege: "delete", want: false,
			why: "the stream is append-only; the trigger blocks it and the grant does not invite it",
		},
		{
			name: "cannot see articles", table: "public.article", privilege: "select", want: false,
			why: "the news product's own tables are unreachable from cashback",
		},
		{
			name: "cannot see retrieved evidence", table: "public.source_item", privilege: "select", want: false,
			why: "licensing evidence belongs to news alone",
		},
		{
			name: "cannot see sources", table: "public.source", privilege: "select", want: false,
			why: "licence terms belong to news alone",
		},
		{
			name: "cannot see translations", table: "public.translation", privilege: "select", want: false,
			why: "translation lineage belongs to news alone",
		},
		{
			name: "cannot see consent", table: "public.consent", privilege: "select", want: false,
			why: "consent history is not shared reference data; cashback consent is its own record",
		},
		{
			name: "cannot see translation spend", table: "public.translation_spend", privilege: "select", want: false,
			why: "the news translation budget is none of the cashback domain's business",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx := beginTx(t)
			var got bool
			err := tx.QueryRow(context.Background(),
				`select has_table_privilege($1, $2, $3)`, cashbackRole, tt.table, tt.privilege).Scan(&got)
			if err != nil {
				t.Fatalf("reading table privilege: %v", err)
			}
			if got != tt.want {
				t.Fatalf("has_table_privilege(%s, %s, %s) = %v, want %v: %s",
					cashbackRole, tt.table, tt.privilege, got, tt.want, tt.why)
			}
		})
	}
}

// defaultTablePrivileges reads the privileges the default ACL of one schema
// grants to one role, as a sorted list. aclexplode turns the packed aclitem
// array into one row per privilege, so the answer is the actual privilege
// set rather than "the role is mentioned somewhere in the ACL text".
func defaultTablePrivileges(t *testing.T, tx pgx.Tx, schema, role string) []string {
	t.Helper()
	var privileges []string
	err := tx.QueryRow(context.Background(),
		`select coalesce(array_agg(distinct a.privilege_type order by a.privilege_type), array[]::text[])
		   from pg_default_acl d
		   join pg_namespace n on n.oid = d.defaclnamespace
		   cross join lateral aclexplode(d.defaclacl) a
		   join pg_roles r on r.oid = a.grantee
		  where n.nspname = $1
		    and d.defaclobjtype = 'r'
		    and r.rolname = $2`, schema, role).Scan(&privileges)
	if err != nil {
		t.Fatalf("reading the default table privileges of %s in schema %s: %v", role, schema, err)
	}
	return privileges
}

// TestCashbackDefaultPrivilegesCoverFutureTables asserts the grant that
// makes 0011-0017 self-sufficient: every table those migrations create is
// reachable by the domain role without a further GRANT. Without it the
// boundary would be correct on the day it was written and wrong on the day
// a table was added.
//
// It asserts the EXACT privilege set, not merely that the role appears in
// the ACL. A test that passes on the wrong grants is not a weaker test, it
// is a test that reports success about something it never looked at - so
// the second half is a negative control: a scratch schema is given a
// deliberately partial default grant, and the reader must report exactly
// that partial set. If it could not tell the two apart, the assertion above
// would be decoration.
func TestCashbackDefaultPrivilegesCoverFutureTables(t *testing.T) {
	t.Parallel()
	tx := beginTx(t)

	want := []string{"DELETE", "INSERT", "SELECT", "UPDATE"}
	got := defaultTablePrivileges(t, tx, "cashback", cashbackRole)
	if !slices.Equal(got, want) {
		t.Fatalf("default table privileges for %s in schema cashback are %v, want %v: a table added by a later migration would be unreachable, or reachable in the wrong way",
			cashbackRole, got, want)
	}

	// Negative control, in the same rolled-back transaction: a schema whose
	// default grant is SELECT only must read back as SELECT only.
	scratch := "cashback_privilege_control_" + randomSuffix(t)
	ctx := context.Background()
	if _, err := tx.Exec(ctx, `create schema `+scratch); err != nil {
		t.Fatalf("creating the control schema: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`alter default privileges in schema `+scratch+` grant select on tables to `+cashbackRole); err != nil {
		t.Fatalf("granting the control privilege: %v", err)
	}
	if control := defaultTablePrivileges(t, tx, scratch, cashbackRole); !slices.Equal(control, []string{"SELECT"}) {
		t.Fatalf("the privilege reader returned %v for a SELECT-only default grant: it cannot distinguish a partial grant from a complete one, so the assertion above proves nothing",
			control)
	}
}
