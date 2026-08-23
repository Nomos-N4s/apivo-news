#!/bin/sh
# Unit-style tests for apivo-seed-editors.
#
# This script writes to a database and creates users in an auth project, so
# what matters is not only that it succeeds but WHAT it sends: the account
# row's id has to be the one Supabase issued, and a re-run has to reach the
# SAME id rather than orphaning the row it wrote last time.
#
# No Supabase, no daemon, no database. `curl` and `docker` are both stubs,
# and every call either records what it was given or answers from a fixture.
set -eu

SEED=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/apivo-seed-editors

command -v jq >/dev/null 2>&1 || {
    echo "SKIP: jq is not installed, and apivo-seed-editors requires it"
    exit 0
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

STUB_DIR="$TMP/stub"
mkdir -p "$STUB_DIR"

# ---------------------------------------------------------------------------
# The stub Supabase.
#
# It reads the curl CONFIG from stdin, exactly as the real curl does with
# `-K -`, and records it. That is the only way to assert on the credential
# and the body, because the script deliberately keeps both out of argv.
# ---------------------------------------------------------------------------
cat > "$TMP/curl" <<'STUB'
#!/bin/sh
cfg=$(cat)
printf '%s\n--\n' "$cfg" >> "$STUB_DIR/curl_calls"

url=$(printf '%s' "$cfg" | sed -n 's/^url = "\(.*\)"$/\1/p')
method=$(printf '%s' "$cfg" | sed -n 's/^request = "\(.*\)"$/\1/p')

case "$url" in
*/auth/v1/admin/users\?*)
    # The listing the script uses to find an existing user, PAGINATED the way
    # GoTrue's is. A stub that answered the same body to every page could
    # never fail an unpaginated lookup: the bug is that a user past page 1 is
    # invisible, so only a stub willing to put one there can see it.
    page=$(printf '%s' "$url" | sed -n 's/.*[?&]page=\([0-9][0-9]*\).*/\1/p')
    [ -n "$page" ] || page=1
    if [ -e "$STUB_DIR/users_page_$page" ]; then
        cat "$STUB_DIR/users_page_$page"
    elif [ -e "$STUB_DIR/users_list" ] && [ "$page" -eq 1 ]; then
        cat "$STUB_DIR/users_list"
    else
        echo '{"users":[]}'
    fi
    ;;
*/auth/v1/admin/users)
    [ -e "$STUB_DIR/create_fails" ] && { echo '{"code":422,"msg":"email exists"}'; exit 0; }
    printf '{"id":"%s"}\n' "$(cat "$STUB_DIR/next_id" 2>/dev/null || echo 11111111-1111-1111-1111-111111111111)"
    ;;
*/auth/v1/admin/users/*)
    # A password reset on an existing user echoes the id back.
    printf '{"id":"%s"}\n' "${url##*/}"
    ;;
esac
exit 0
STUB
chmod +x "$TMP/curl"

# ---------------------------------------------------------------------------
# The stub docker: records the SQL it was handed on stdin.
# ---------------------------------------------------------------------------
cat > "$TMP/docker" <<'STUB'
#!/bin/sh
echo "$*" >> "$STUB_DIR/docker_calls"
cat >> "$STUB_DIR/sql"
[ -e "$STUB_DIR/sql_fails" ] && exit 1
exit 0
STUB
chmod +x "$TMP/docker"

export STUB_DIR
export APIVO_DOCKER="$TMP/docker"
export APIVO_CURL="$TMP/curl"
export APIVO_ETC="$TMP/etc"

FAILS=0

reset() {
    rm -rf "$APIVO_ETC" "$STUB_DIR"
    mkdir -p "$APIVO_ETC/qa" "$APIVO_ETC/staging" "$APIVO_ETC/prod" "$STUB_DIR"

    # A production environment that WOULD seed. Without it the prod refusals
    # below pass for the wrong reason - the run dying on a missing stack.env
    # rather than on the guard - and a bypass would look refused here while
    # sailing through on a real host, which is the only host that has this
    # directory.
    cat > "$APIVO_ETC/prod/stack.env" <<'EOF'
APIVO_ENV=prod
APIVO_PG_USER=apivo
APIVO_PG_DB=apivo
COMPOSE_FILE=/opt/apivo/compose/docker-compose.yml:/opt/apivo/compose/docker-compose.local-db.yml
EOF
    printf 'PUBLIC_SUPABASE_URL=https://prod.supabase.co\n' > "$APIVO_ETC/prod/web.env"
    printf 'DATABASE_URL=postgres://apivo:x@postgres:5432/apivo?sslmode=require\n' > "$APIVO_ETC/prod/api.env"

    cat > "$APIVO_ETC/qa/stack.env" <<'EOF'
APIVO_ENV=qa
APIVO_PG_USER=apivo
APIVO_PG_DB=apivo
COMPOSE_FILE=/opt/apivo/compose/docker-compose.yml:/opt/apivo/compose/docker-compose.local-db.yml
EOF
    # The shape a WIRED host actually has. provision.sh writes every key
    # present and empty, and RUNBOOK step 5 appends the real value, so the key
    # appears twice - empty, then real. Docker resolves an env_file
    # last-wins; a fixture with one occurrence per key cannot tell whether
    # this script agrees with Docker or not.
    printf 'PUBLIC_SUPABASE_URL=\nPUBLIC_SUPABASE_ANON_KEY=\nPUBLIC_SUPABASE_URL=https://stub.supabase.co\n' \
        > "$APIVO_ETC/qa/web.env"
    printf 'DATABASE_URL=\nDATABASE_URL=postgres://apivo:x@postgres:5432/apivo?sslmode=require\n' \
        > "$APIVO_ETC/qa/api.env"

    # Staging: no local database, so the SQL has to go over a throwaway client.
    cat > "$APIVO_ETC/staging/stack.env" <<'EOF'
APIVO_ENV=staging
COMPOSE_FILE=/opt/apivo/compose/docker-compose.yml
EOF
    printf 'PUBLIC_SUPABASE_URL=https://stub.supabase.co\n' > "$APIVO_ETC/staging/web.env"
    printf 'DATABASE_URL=postgres://postgres.ref:x@aws.pooler.supabase.com:5432/postgres?sslmode=require\n' > "$APIVO_ETC/staging/api.env"
}

run() {
    set +e
    # ${KEY-...}, NOT ${KEY:-...}: the empty-key test sets KEY to the empty
    # string deliberately, and the colon form would substitute the default
    # for it and quietly test nothing.
    OUT=$(SUPABASE_SERVICE_ROLE_KEY="${KEY-stub-service-key}" sh "$SEED" "$@" 2>&1)
    RC=$?
    set -e
}

check() {
    # check <description> <expected-rc> <required-fragment>
    if [ "$RC" -ne "$2" ]; then
        echo "FAIL: $1 - expected exit $2, got $RC: $OUT"
        FAILS=1
        return
    fi
    if [ -n "$3" ] && ! printf '%s' "$OUT" | grep -q -F -e "$3"; then
        echo "FAIL: $1 - exit $2 as expected, but never said '$3': $OUT"
        FAILS=1
        return
    fi
    echo "ok: $1"
}

check_file() {
    # check_file <description> <file> <fragment>
    got=$(cat "$STUB_DIR/$2" 2>/dev/null || true)
    if printf '%s' "$got" | grep -q -F -e "$3"; then
        echo "ok: $1"
    else
        echo "FAIL: $1 - $2 does not contain '$3': ${got:-<empty>}"
        FAILS=1
    fi
}

check_absent() {
    # check_absent <description> <file> <fragment>
    got=$(cat "$STUB_DIR/$2" 2>/dev/null || true)
    if printf '%s' "$got" | grep -q -F -e "$3"; then
        echo "FAIL: $1 - $2 contains '$3', and must not: $got"
        FAILS=1
    else
        echo "ok: $1"
    fi
}

check_count() {
    # check_count <description> <file> <fragment> <expected-lines>
    got=$(grep -c -F -e "$3" "$STUB_DIR/$2" 2>/dev/null || true)
    got=${got:-0}
    if [ "$got" -eq "$4" ]; then
        echo "ok: $1"
    else
        echo "FAIL: $1 - expected $4 lines matching '$3' in $2, got $got"
        FAILS=1
    fi
}

# ===========================================================================
# Production is refused. First, because nothing else matters if this fails.
#
# The name is refused before it is used, not merely compared: it goes on to
# build $ETC/$ENV_NAME/stack.env, and POSIX resolves /etc/apivo/prod//stack.env
# and /etc/apivo/prod/stack.env to one file. A guard a trailing slash walks
# through is decoration.
# ===========================================================================

reset
run prod
check "prod is refused outright" 2 "refusing to seed 'prod'"
check_absent "and nothing was sent to any auth project" curl_calls "admin/users"
check_absent "and no SQL was run" sql "insert into account"

reset
run prod/
check "and so is 'prod/', which resolves to the same directory" 2 \
    "is not an environment name"
check_absent "with nothing sent to any auth project" curl_calls "admin/users"
check_absent "and no SQL run" sql "insert into account"

reset
run ../prod
check "a traversal in the environment name is refused" 2 \
    "is not an environment name"
check_absent "before reading anything" curl_calls "admin/users"

# ===========================================================================
# The happy path.
# ===========================================================================

reset
printf '%s' '22222222-2222-2222-2222-222222222222' > "$STUB_DIR/next_id"
run qa 1
check "a new editor is created" 0 "created"

# The account row must carry the id SUPABASE issued. Inventing one here would
# produce a user who signs in and is then unknown to the api - authenticated
# and unauthorised, with nothing in the logs saying why.
check_file "the account row uses the id Supabase issued" sql \
    "'22222222-2222-2222-2222-222222222222'"
check_file "and marks the account an editor" sql "'editor'"
check_file "and addresses it @example.com" sql "'editor1@example.com'"
# `docker exec` on the container by name, NOT `docker compose exec`: compose
# renders the whole configuration first, and docker-compose.yml hard-requires
# APIVO_API_IMAGE and APIVO_WEB_IMAGE (and the local-db overlay
# APIVO_PG_PASSWORD) with `:?`. This script sets only COMPOSE_FILE, so compose
# would have died before psql ever ran - on QA, the environment it exists for.
check_file "written to QA's own Postgres" docker_calls "exec -i apivo-qa-postgres"
check_absent "not through compose, which would demand image digests this has none of" \
    docker_calls "compose exec"

# ===========================================================================
# The service-role key never reaches argv.
# ===========================================================================

check_absent "the service-role key is not passed as a command argument" docker_calls "stub-service-key"
check_file "it reaches curl through a config on stdin" curl_calls "apikey: stub-service-key"

# ===========================================================================
# Re-running. An existing user keeps its id.
#
# This is the assertion that matters most for idempotency: if a re-run
# created a second user, or wrote a different id, the account row from the
# first run would be orphaned and the editor would silently stop working.
# ===========================================================================

reset
cat > "$STUB_DIR/users_list" <<'EOF'
{"users":[{"id":"33333333-3333-3333-3333-333333333333","email":"editor1@example.com"}]}
EOF
run qa 1
check "an existing editor is reused, not duplicated" 0 "password reset"
check_file "and the account row keeps that same id" sql \
    "'33333333-3333-3333-3333-333333333333'"
check_absent "no user was created for an address that already exists" curl_calls \
    'request = "POST"'

# Matching is case-insensitive: Supabase lowercases addresses, and the
# account table has a unique index on lower(email), so a mismatch here would
# create a duplicate that the database then refuses.
reset
cat > "$STUB_DIR/users_list" <<'EOF'
{"users":[{"id":"44444444-4444-4444-4444-444444444444","email":"EDITOR1@EXAMPLE.COM"}]}
EOF
run qa 1
check "an address differing only in case is the same editor" 0 "password reset"
check_file "and reuses its id" sql "'44444444-4444-4444-4444-444444444444'"

# An existing user is found however deep in the listing it sits. GoTrue's
# /admin/users returns one page at a time, and a create that collides reports
# "email exists" WITHOUT the id it collided with - so a lookup that gives up
# after page 1 does not degrade, it dies, on an account this script created
# itself the week before.
reset
cat > "$STUB_DIR/users_page_1" <<'EOF'
{"users":[{"id":"aaaaaaaa-0000-0000-0000-000000000000","email":"someone@example.com"}]}
EOF
cat > "$STUB_DIR/users_page_2" <<'EOF'
{"users":[{"id":"66666666-6666-6666-6666-666666666666","email":"editor1@example.com"}]}
EOF
run qa 1
check "an editor on the second page is found, not created again" 0 "password reset"
check_file "and the account row carries the id from that page" sql \
    "'66666666-6666-6666-6666-666666666666'"
check_absent "no duplicate user was created" curl_calls 'request = "POST"'
check_count "and the walk stopped at the page that had the answer" curl_calls \
    "/auth/v1/admin/users?page=" 2

# The end of the list is an empty page, and reaching it must end the walk
# rather than start a second lap.
reset
run qa 1
check "an address that is nowhere in the listing is created" 0 "created"
check_count "after one page, because an empty page is the end of the list" curl_calls \
    "/auth/v1/admin/users?page=" 1

# ===========================================================================
# Staging has no Postgres of its own.
# ===========================================================================

reset
printf '%s' '55555555-5555-5555-5555-555555555555' > "$STUB_DIR/next_id"
run staging 1
check "staging seeds too" 0 "created"
check_file "over a throwaway client, since it has no local Postgres" docker_calls "run --rm -i"
check_absent "and not by execing a Postgres container, which staging has none of" \
    docker_calls "exec -i apivo-"

# ===========================================================================
# Refusals that save an operator from a confusing environment.
# ===========================================================================

reset
: > "$APIVO_ETC/qa/web.env"
run qa 1
check "an environment with no auth configured is refused" 2 "PUBLIC_SUPABASE_URL is empty"
check_absent "before creating anything" curl_calls "admin/users"

# Reading the LAST occurrence must not turn into reading any non-empty one:
# a key appended back to empty is an operator switching auth OFF, and the
# refusal is the correct answer.
reset
printf 'PUBLIC_SUPABASE_URL=https://stub.supabase.co\nPUBLIC_SUPABASE_URL=\n' \
    > "$APIVO_ETC/qa/web.env"
run qa 1
check "a key appended back to empty is empty, as Docker would read it" 2 \
    "PUBLIC_SUPABASE_URL is empty"

reset
KEY="" run qa 1
check "a missing service-role key is refused" 2 "SUPABASE_SERVICE_ROLE_KEY is not set"

reset
run nosuchenv 1
check "an environment that does not exist here is refused" 2 "is not an environment on this host"

reset
run qa 0
check "a count of zero is refused" 2 "at least 1"

reset
run qa two
check "a count that is not a number is refused" 2 "whole number"

# ===========================================================================
# A failing database write must fail the run.
#
# The credentials are printed at the end. Printing them after a failed insert
# would hand somebody a password for an account that cannot authorise
# anything, and they would have no reason to doubt it.
# ===========================================================================

reset
: > "$STUB_DIR/sql_fails"
run qa 1
if [ "$RC" -eq 0 ]; then
    echo "FAIL: a failed account insert still exited 0"
    FAILS=1
else
    echo "ok: a failed account insert fails the run"
fi

if [ "$FAILS" -ne 0 ]; then
    echo "apivo-seed-editors: FAILURES"
    exit 1
fi
echo "apivo-seed-editors: all tests passed"
