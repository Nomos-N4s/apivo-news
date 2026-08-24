#!/bin/sh
# Spike S1 (ADR-0002, task T006).
#
# Question: can Blnk live in the same Postgres as Apivo without being able to
# reach it? Concretely — does `blnk migrate up`, run as a role that owns only
# the `blnk` schema, put everything in `blnk` and nothing at all in `public`?
#
# The answer decides a documented fallback. If Blnk cannot be confined,
# ADR-0002 says to give it its own Postgres on the Hetzner host and accept a
# second database plus a periodic reconciliation job in place of the
# cross-schema zero-sum query. That is a founder decision, so this script
# reports rather than works around: every check prints PASS or FAIL, all of
# them run, and the exit status is the verdict.
#
# Usage:
#
#   SPIKE_ADMIN_DATABASE_URL=postgres://apivo:apivo@localhost:5432/apivo?sslmode=disable \
#   SPIKE_BLNK_ROLE_DSN=postgres://blnk_app:blnk_app@localhost:5432/apivo?sslmode=disable \
#   BLNK_IMAGE=jerryenebeli/blnk:0.15.2 \
#   BLNK_REDIS_DNS=127.0.0.1:6379 \
#   sh scripts/spikes/ledger_schema/run.sh
#
# It needs Docker and a Postgres, so it does not run on the founder's machine
# while Docker Desktop is unavailable. It runs in the `cashback` CI job, and
# that run is the evidence.
set -eu

ADMIN_DSN="${SPIKE_ADMIN_DATABASE_URL:-${DATABASE_URL:-}}"
ROLE_DSN="${SPIKE_BLNK_ROLE_DSN:-}"
REDIS_DNS="${BLNK_REDIS_DNS:-127.0.0.1:6379}"
IMAGE="${BLNK_IMAGE:-}"

if [ -z "$ADMIN_DSN" ]; then
    echo "S1: SPIKE_ADMIN_DATABASE_URL (or DATABASE_URL) is required" >&2
    exit 2
fi
if [ -z "$ROLE_DSN" ]; then
    echo "S1: SPIKE_BLNK_ROLE_DSN is required" >&2
    exit 2
fi
if [ -z "$IMAGE" ]; then
    echo "S1: BLNK_IMAGE is required" >&2
    exit 2
fi

HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"
FAILURES=0

# Inline rather than a cleanup function: shellcheck cannot see that a trap
# calls one, and reports the body as unreachable (SC2317).
trap 'rm -rf "$WORK"' EXIT

pass() {
    echo "S1 PASS  $1"
}

fail() {
    echo "S1 FAIL  $1" >&2
    FAILURES=$((FAILURES + 1))
}

admin() {
    psql "$ADMIN_DSN" -v ON_ERROR_STOP=1 -tAqc "$1"
}

# sqlstate runs a statement as the restricted role and prints the SQLSTATE it
# raised, or 00000 when it succeeded. Matching on the code rather than on the
# message is the same discipline the invariant suites use: a message is
# prose, a SQLSTATE is a contract.
sqlstate() {
    if out=$(psql "$ROLE_DSN" -v ON_ERROR_STOP=1 -v VERBOSITY=verbose -tAqc "$1" 2>&1); then
        echo "00000"
        return 0
    fi
    printf '%s\n' "$out" | sed -n 's/^\(psql:\)\{0,1\}.*ERROR:  \([0-9A-Z]\{5\}\):.*/\2/p' | head -1
}

echo "S1: Blnk in a dedicated schema under a restricted role (ADR-0002)"

DBNAME=$(admin "SELECT current_database()")
echo "S1: database=$DBNAME image=$IMAGE"

echo "S1: creating the restricted role and the blnk schema"
psql "$ADMIN_DSN" -v ON_ERROR_STOP=1 -q -v "dbname=$DBNAME" -f "$HERE/bootstrap.sql"

echo "S1: snapshotting public before the ledger migration"
psql "$ADMIN_DSN" -v ON_ERROR_STOP=1 -tAq -f "$HERE/public_snapshot.sql" > "$WORK/public.before"

migrate() {
    docker run --rm --network host \
        -e "BLNK_DATA_SOURCE_DNS=$ROLE_DSN" \
        -e "BLNK_REDIS_DNS=$REDIS_DNS" \
        "$IMAGE" blnk migrate up
}

# Check 1 — the migration runs at all under the restricted role.
#
# The strict posture is tried first: the role owns `blnk` and has no CREATE
# on the database, so it cannot make itself a second home. Blnk's first
# migration issues `CREATE SCHEMA IF NOT EXISTS blnk`, and whether Postgres
# checks the database-level privilege before or after the IF NOT EXISTS
# shortcut is exactly the sort of thing a spike exists to find out rather
# than to reason about. If it needs the grant, the grant is added, the
# migration is retried, and the posture the deployment must use is REPORTED
# — because a wider grant is a fact the founder should read here, not
# discover in a runbook.
POSTURE="schema owner only (no CREATE on database)"
if migrate; then
    pass "check 1: \`blnk migrate up\` succeeded as the restricted role"
else
    echo "S1: the strict posture was refused; granting CREATE ON DATABASE and retrying"
    admin "GRANT CREATE ON DATABASE \"$DBNAME\" TO blnk_app" > /dev/null
    POSTURE="schema owner PLUS CREATE on database (required: Blnk issues CREATE SCHEMA IF NOT EXISTS)"
    if migrate; then
        pass "check 1: \`blnk migrate up\` succeeded once CREATE ON DATABASE was granted"
    else
        fail "check 1: \`blnk migrate up\` failed under the restricted role even with CREATE ON DATABASE"
    fi
fi
echo "S1: posture required = $POSTURE"

echo "S1: snapshotting public after the ledger migration"
psql "$ADMIN_DSN" -v ON_ERROR_STOP=1 -tAq -f "$HERE/public_snapshot.sql" > "$WORK/public.after"

# Check 2 — the headline claim. Nothing about `public` changed: no table, no
# view, no sequence, no function, no type, and no extension relocated into
# it.
if diff -u "$WORK/public.before" "$WORK/public.after" > "$WORK/public.diff"; then
    pass "check 2: the public schema is byte-identical before and after the ledger migration"
else
    fail "check 2: the ledger migration changed the public schema"
    cat "$WORK/public.diff" >&2
fi

# Check 3 — the ledger's tables really are in `blnk`, so check 2 is not
# passing because nothing happened.
BLNK_RELATIONS=$(admin "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'blnk' AND c.relkind = 'r'")
if [ "$BLNK_RELATIONS" -ge 10 ]; then
    pass "check 3: the blnk schema holds $BLNK_RELATIONS tables"
else
    fail "check 3: the blnk schema holds only $BLNK_RELATIONS tables; the migration cannot have landed there"
fi

# Check 4 — the migration bookkeeping is inside `blnk` too. This is the one
# object a migration tool most often leaves in the default schema, and one
# stray `public.gorp_migrations` would mean two components writing to the
# same namespace.
BOOKKEEPING=$(admin "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relname = 'gorp_migrations' AND n.nspname = 'public'")
if [ "$BOOKKEEPING" = "0" ]; then
    pass "check 4: no migration bookkeeping table was left in public"
else
    fail "check 4: the ledger left its migration bookkeeping in public"
fi

# Check 5 — everything in `blnk` is owned by the restricted role, so nothing
# there is silently the property of a superuser.
FOREIGN_OWNED=$(admin "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_roles r ON r.oid = c.relowner WHERE n.nspname = 'blnk' AND r.rolname <> 'blnk_app'")
if [ "$FOREIGN_OWNED" = "0" ]; then
    pass "check 5: every relation in the blnk schema is owned by the restricted role"
else
    fail "check 5: $FOREIGN_OWNED relations in the blnk schema are owned by another role"
fi

# Check 6 — the confinement is enforced by Postgres, not by Blnk's good
# manners. 42501 is insufficient_privilege.
CODE=$(sqlstate "CREATE TABLE public.spike_s1_should_not_exist (id integer)")
if [ "$CODE" = "42501" ]; then
    pass "check 6: the restricted role is refused CREATE in public (SQLSTATE 42501)"
else
    fail "check 6: CREATE in public as the restricted role returned SQLSTATE $CODE, expected 42501"
    admin "DROP TABLE IF EXISTS public.spike_s1_should_not_exist" > /dev/null
fi

# Check 7 — and it cannot read what is already there. This is the one that
# matters for the news product: Apivo's tables are legal evidence, and the
# ledger has no business seeing them.
CODE=$(sqlstate "SELECT count(*) FROM public.spike_s1_probe")
if [ "$CODE" = "42501" ]; then
    pass "check 7: the restricted role is refused SELECT on a public table (SQLSTATE 42501)"
else
    fail "check 7: SELECT on a public table as the restricted role returned SQLSTATE $CODE, expected 42501"
fi

# Check 8 — the boundary is a boundary, not a wall: the role must still be
# able to work inside its own schema, or the ledger cannot run at all.
CODE=$(sqlstate "CREATE TABLE blnk.spike_s1_scratch (id integer); DROP TABLE blnk.spike_s1_scratch")
if [ "$CODE" = "00000" ]; then
    pass "check 8: the restricted role can create and drop inside its own schema"
else
    fail "check 8: the restricted role cannot work inside blnk (SQLSTATE $CODE)"
fi

# Check 9 — the cross-schema query ADR-0002 trades C-1 for. If the admin
# connection cannot read Blnk's balances, the continuous zero-sum check is
# not a plain SQL query and the whole co-location argument collapses.
CROSS=$(admin "SELECT count(*) FROM blnk.balances")
if [ -n "$CROSS" ]; then
    pass "check 9: Apivo's own role can read blnk.balances (rows=$CROSS), so the C-1 zero-sum check is one query"
else
    fail "check 9: Apivo's own role cannot read the ledger's balances across the schema boundary"
fi

admin "DROP TABLE IF EXISTS public.spike_s1_probe" > /dev/null

echo
if [ "$FAILURES" -eq 0 ]; then
    echo "S1 VERDICT: PASS — Blnk migrates into the blnk schema under a restricted role and never touches public."
    echo "S1 POSTURE: $POSTURE"
    exit 0
fi

echo "S1 VERDICT: FAIL — $FAILURES check(s) failed." >&2
echo "S1: this triggers the ADR-0002 fallback (a Blnk-owned Postgres plus a periodic reconciliation job)." >&2
echo "S1: that is a founder decision. Do not work around it here." >&2
exit 1
