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
# Exit status is three-valued, and the third value is the one that matters:
#
#   0  PASS            every check passed
#   1  FAIL            a check failed — this is S1's verdict, and it is what
#                      opens the ADR-0002 fallback conversation
#   2  COULD NOT RUN   the harness or its environment is broken. NOT a
#                      verdict on Blnk, and must never be read as one
#
# Conflating 1 and 2 would be the worst outcome here: "the ledger cannot be
# confined" and "psql was not installed" are different sentences, and only one
# of them changes an architecture decision.
#
# Usage:
#
#   SPIKE_ADMIN_DATABASE_URL=postgres://apivo:apivo@localhost:5432/apivo?sslmode=disable \
#   SPIKE_BLNK_ROLE_DSN=postgres://blnk_app:<secret>@localhost:5432/apivo?sslmode=disable \
#   SPIKE_BLNK_ROLE_PASSWORD=<secret> \
#   BLNK_IMAGE=jerryenebeli/blnk:0.15.2@sha256:... \
#   BLNK_REDIS_DNS=127.0.0.1:6379 \
#   sh scripts/spikes/ledger_schema/run.sh
#
# The password is passed separately from the DSN rather than parsed out of
# it: picking a password back out of a URL means guessing about encoding, and
# guessing about a credential is how a role ends up with a password nobody
# meant.
#
# It needs Docker and a Postgres, so it does not run on the founder's machine
# while Docker Desktop is unavailable. It runs in the `cashback` CI job, and
# that run is the evidence.
set -eu

ADMIN_DSN="${SPIKE_ADMIN_DATABASE_URL:-${DATABASE_URL:-}}"
ROLE_DSN="${SPIKE_BLNK_ROLE_DSN:-}"
ROLE_PASSWORD="${SPIKE_BLNK_ROLE_PASSWORD:-}"
REDIS_DNS="${BLNK_REDIS_DNS:-127.0.0.1:6379}"
IMAGE="${BLNK_IMAGE:-}"

# cannot_run reports a harness problem and exits 2. Every early exit goes
# through here so no caller has to tell a broken spike from a failing one.
cannot_run() {
    echo "S1 COULD NOT RUN: $1" >&2
    exit 2
}

[ -n "$ADMIN_DSN" ] || cannot_run "SPIKE_ADMIN_DATABASE_URL (or DATABASE_URL) is required"
[ -n "$ROLE_DSN" ] || cannot_run "SPIKE_BLNK_ROLE_DSN is required"
[ -n "$ROLE_PASSWORD" ] || cannot_run "SPIKE_BLNK_ROLE_PASSWORD is required"
[ -n "$IMAGE" ] || cannot_run "BLNK_IMAGE is required"

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_SCRIPTS="$(cd "$HERE/../.." && pwd)"
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

# admin_value runs a read-only query as the database owner and prints its
# single result.
#
# It is written to be SAFE UNDER `set -e`, which the previous revision was
# not. `VALUE=$(psql ...)` takes its exit status from the substitution, so
# one failing query killed the whole script — past the accumulated FAILURES
# count, past every remaining check, and past the VERDICT line the founder
# reads. Check 9 was the likely trigger: it exists precisely to test whether
# the admin connection CAN read blnk.balances, so the case it was written to
# detect was the case that silenced it.
#
# Callers must guard with `if admin_value ... ; then`, which keeps `set -e`
# out of the way and turns a failed query into a recorded FAIL.
admin_value() {
    if value=$(psql "$ADMIN_DSN" -v ON_ERROR_STOP=1 -tAqc "$1" 2>&1); then
        printf '%s\n' "$value"
        return 0
    fi
    printf 'S1: query failed: %s\n' "$value" >&2
    return 1
}

# admin_exec runs a statement as the database owner and discards its output,
# reporting failure rather than aborting.
admin_exec() {
    if output=$(psql "$ADMIN_DSN" -v ON_ERROR_STOP=1 -tAqc "$1" 2>&1); then
        return 0
    fi
    printf 'S1: statement failed: %s\n' "$output" >&2
    return 1
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

command -v psql >/dev/null 2>&1 || cannot_run "psql is not on PATH"
command -v docker >/dev/null 2>&1 || cannot_run "docker is not on PATH"

DBNAME=$(admin_value "SELECT current_database()") || cannot_run "cannot reach the database as the owner"
echo "S1: database=$DBNAME image=$IMAGE"

echo "S1: creating the restricted role and the blnk schema"
psql "$ADMIN_DSN" -v ON_ERROR_STOP=1 -q -v "blnk_password=$ROLE_PASSWORD" -f "$HERE/bootstrap.sql" \
    || cannot_run "bootstrap.sql failed"

# The image is pulled through the repository's retry wrapper BEFORE any
# `docker run`. Docker Hub throttles anonymous pulls, and without this a rate
# limit surfaces as `docker run` failing, which this script would have read
# as "the migration was refused" — a registry outage reported as a
# schema-confinement failure, and an architecture decision made on it.
sh "$REPO_SCRIPTS/pull_retry.sh" "$IMAGE" || cannot_run "could not pull $IMAGE"

echo "S1: snapshotting public before the ledger migration"
psql "$ADMIN_DSN" -v ON_ERROR_STOP=1 -tAq -f "$HERE/public_snapshot.sql" > "$WORK/public.before" \
    || cannot_run "could not snapshot the public schema"

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
    if admin_exec "GRANT CREATE ON DATABASE \"$DBNAME\" TO blnk_app"; then
        POSTURE="schema owner PLUS CREATE on database (required: Blnk issues CREATE SCHEMA IF NOT EXISTS)"
        if migrate; then
            pass "check 1: \`blnk migrate up\` succeeded once CREATE ON DATABASE was granted"
        else
            fail "check 1: \`blnk migrate up\` failed under the restricted role even with CREATE ON DATABASE"
        fi
    else
        fail "check 1: could not grant CREATE ON DATABASE to retry the migration"
    fi
fi
echo "S1: posture required = $POSTURE"

echo "S1: snapshotting public after the ledger migration"
psql "$ADMIN_DSN" -v ON_ERROR_STOP=1 -tAq -f "$HERE/public_snapshot.sql" > "$WORK/public.after" \
    || cannot_run "could not snapshot the public schema"

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
if BLNK_RELATIONS=$(admin_value "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'blnk' AND c.relkind = 'r'"); then
    if [ "$BLNK_RELATIONS" -ge 10 ]; then
        pass "check 3: the blnk schema holds $BLNK_RELATIONS tables"
    else
        fail "check 3: the blnk schema holds only $BLNK_RELATIONS tables; the migration cannot have landed there"
    fi
else
    fail "check 3: could not count the tables in the blnk schema"
fi

# Check 4 — the migration bookkeeping is inside `blnk` too. This is the one
# object a migration tool most often leaves in the default schema, and one
# stray `public.gorp_migrations` would mean two components writing to the
# same namespace.
if BOOKKEEPING=$(admin_value "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relname = 'gorp_migrations' AND n.nspname = 'public'"); then
    if [ "$BOOKKEEPING" = "0" ]; then
        pass "check 4: no migration bookkeeping table was left in public"
    else
        fail "check 4: the ledger left its migration bookkeeping in public"
    fi
else
    fail "check 4: could not look for migration bookkeeping in public"
fi

# Check 5 — everything in `blnk` is owned by the restricted role, so nothing
# there is silently the property of a superuser.
if FOREIGN_OWNED=$(admin_value "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_roles r ON r.oid = c.relowner WHERE n.nspname = 'blnk' AND r.rolname <> 'blnk_app'"); then
    if [ "$FOREIGN_OWNED" = "0" ]; then
        pass "check 5: every relation in the blnk schema is owned by the restricted role"
    else
        fail "check 5: $FOREIGN_OWNED relations in the blnk schema are owned by another role"
    fi
else
    fail "check 5: could not read the owners of the blnk schema's relations"
fi

# Check 6 — the confinement is enforced by Postgres, not by Blnk's good
# manners. 42501 is insufficient_privilege.
CODE=$(sqlstate "CREATE TABLE public.spike_s1_should_not_exist (id integer)")
if [ "$CODE" = "42501" ]; then
    pass "check 6: the restricted role is refused CREATE in public (SQLSTATE 42501)"
else
    fail "check 6: CREATE in public as the restricted role returned SQLSTATE $CODE, expected 42501"
    admin_exec "DROP TABLE IF EXISTS public.spike_s1_should_not_exist" || true
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
#
# This is the check the old `set -e` bug silenced, and it is worth noticing
# why: a failure here is a real S1 finding, so the one check most likely to
# fail legitimately was the one that took the report down with it.
if CROSS=$(admin_value "SELECT count(*) FROM blnk.balances"); then
    pass "check 9: Apivo's own role can read blnk.balances (rows=$CROSS), so the C-1 zero-sum check is one query"
else
    fail "check 9: Apivo's own role cannot read the ledger's balances across the schema boundary"
fi

admin_exec "DROP TABLE IF EXISTS public.spike_s1_probe" || true

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
