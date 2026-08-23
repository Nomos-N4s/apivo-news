#!/bin/sh
# Unit-style tests for apivo-previews.
#
# This script CREATES AND DESTROYS environments, and it decides to destroy one
# by observing that something is absent. That is a good design and a dangerous
# implementation to get wrong: a registry that answers with an empty list
# instead of an error would tear down every open pull request's preview at
# once. Several of the tests below exist for exactly that case.
#
# No registry, no daemon, no container. `curl` and `docker` are both stubs.
set -eu

PREVIEWS=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/apivo-previews

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

STUB_DIR="$TMP/stub"
mkdir -p "$STUB_DIR"

# ---------------------------------------------------------------------------
# The stub registry: `curl` answering the two calls the script makes — a token
# request, then a tag list.
# ---------------------------------------------------------------------------
cat > "$TMP/curl" <<'STUB'
#!/bin/sh
# Every invocation is recorded, headers included. Without this the stub
# answers a token request whether or not a credential reached it, and no
# assertion can tell a parsed credential from an unparsed one - which is
# exactly how the pretty-printed config.json bug survived a green suite.
printf '%s\n' "$*" >> "$STUB_DIR/curl_calls"
url=""
for a in "$@"; do
    case "$a" in https://*) url="$a" ;; esac
done
case "$url" in
*/token*)
    [ -e "$STUB_DIR/token_fails" ] && exit 22
    echo '{"token":"stub-token"}'
    ;;
*/api/tags/list)
    [ -e "$STUB_DIR/api_tags_fail" ] && exit 22
    printf '{"name":"x","tags":[%s]}\n' "$(cat "$STUB_DIR/api_tags" 2>/dev/null || echo '')"
    ;;
*/web/tags/list)
    [ -e "$STUB_DIR/web_tags_fail" ] && exit 22
    printf '{"name":"x","tags":[%s]}\n' "$(cat "$STUB_DIR/web_tags" 2>/dev/null || echo '')"
    ;;
esac
exit 0
STUB
chmod +x "$TMP/curl"

# ---------------------------------------------------------------------------
# The stub docker: records what it was asked to do, so the tests assert on
# actions rather than on the script's own logging.
# ---------------------------------------------------------------------------
cat > "$TMP/docker" <<'STUB'
#!/bin/sh
echo "$*" >> "$STUB_DIR/calls"
case "$1" in
compose)
    case "$2" in
    up)
        [ -e "$STUB_DIR/up_fails" ] && exit 1
        echo "up ${APIVO_PREVIEW:-?} ${APIVO_PREVIEW_WEB_IMAGE:-?}" >> "$STUB_DIR/ups"
        ;;
    pull)
        [ -e "$STUB_DIR/pull_fails" ] && exit 1
        echo "pull ${APIVO_PREVIEW:-?} ${APIVO_PREVIEW_WEB_IMAGE:-?}" >> "$STUB_DIR/pulls"
        ;;
    down) echo "down ${APIVO_PREVIEW:-?}" >> "$STUB_DIR/downs" ;;
    esac
    ;;
exec)
    # psql against a Postgres container. The container name tells QA's
    # database from the preview's.
    for a in "$@"; do last="$a"; done
    if [ "$last" = "-" ]; then
        # `psql -f -`: the statements arrive on STDIN, not in argv. Recorded
        # from there, because a stub that only ever looked at the last
        # argument would record a bare "-" and let any SQL through unseen.
        last=$(cat)
        # Answered like psql, because the caller counts the rows it actually
        # inserted rather than assuming the copy did something.
        if [ -e "$STUB_DIR/insert_zero" ]; then
            echo "INSERT 0 0"
        else
            echo "INSERT 0 2"
        fi
    fi
    echo "$last" >> "$STUB_DIR/sql"
    case " $* " in
    *apivo-qa-postgres*) echo "$last" >> "$STUB_DIR/qa_sql" ;;
    esac
    ;;
esac
exit 0
STUB
chmod +x "$TMP/docker"

export STUB_DIR
export APIVO_DOCKER="$TMP/docker"
export APIVO_CURL="$TMP/curl"
export APIVO_ETC="$TMP/etc"
export APIVO_STATE="$TMP/state"
export DOCKER_CONFIG="$TMP/dockercfg"

FAILS=0

reset() {
    rm -rf "$APIVO_ETC" "$APIVO_STATE" "$STUB_DIR" "$DOCKER_CONFIG"
    mkdir -p "$APIVO_ETC/preview" "$APIVO_STATE" "$STUB_DIR" "$DOCKER_CONFIG"
    cat > "$APIVO_ETC/preview/stack.env" <<EOF
APIVO_REGISTRY=ghcr.io/nomos-n4s/apivo-news
APIVO_PREVIEW_PG_PASSWORD=stub-password
APIVO_PREVIEW_PG_USER=apivo
APIVO_PREVIEW_MAX=${1:-5}
COMPOSE_FILE=/opt/apivo/compose/docker-compose.preview.yml
EOF
    # What `docker login ghcr.io` leaves behind - PRETTY-PRINTED, with tabs,
    # exactly as docker writes it.
    #
    # This fixture used to be a single line, and that is the only reason the
    # suite stayed green while previews could not work on any host: sed
    # matches one line at a time, so registry_auth's pattern - which needs
    # `"ghcr.io"` and `"auth"` together - matched the fixture and never the
    # real file. The credential was present, valid and invisible, and the
    # operator was told the registry was unreachable.
    #
    # A fixture in a shape the program will never meet proves nothing. If
    # this is ever "tidied" back onto one line, it stops testing anything.
    cat > "$DOCKER_CONFIG/config.json" <<'EOF'
{
	"auths": {
		"ghcr.io": {
			"auth": "c3R1YjpzdHVi"
		}
	}
}
EOF
    # QA's own configuration, which previews borrow. Each key appears TWICE,
    # empty then real, because that is the shape a wired host has: provision
    # writes every key present and empty, and RUNBOOK step 5 appends. A
    # fixture with one occurrence would let a first-match read pass.
    mkdir -p "$APIVO_ETC/qa"
    cat > "$APIVO_ETC/qa/web.env" <<'EOF'
PUBLIC_SUPABASE_URL=
PUBLIC_SUPABASE_ANON_KEY=
PUBLIC_SUPABASE_URL=https://qaproject.supabase.co
PUBLIC_SUPABASE_ANON_KEY=sb_publishable_stub
EOF
    cat > "$APIVO_ETC/qa/api.env" <<'EOF'
JWKS_URL=
JWKS_URL=https://qaproject.supabase.co/auth/v1/.well-known/jwks.json
EOF
    cat > "$APIVO_ETC/qa/stack.env" <<'EOF'
APIVO_PG_USER=apivo
APIVO_PG_DB=apivo
APIVO_PG_PASSWORD=qa-stub-password
EOF

    printf '%s' '"qa","staging","pr-1","pr-2"' > "$STUB_DIR/api_tags"
    printf '%s' '"qa","staging","pr-1","pr-2"' > "$STUB_DIR/web_tags"
}

run() {
    set +e
    OUT=$(sh "$PREVIEWS" 2>&1)
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

# The env files a preview's containers read live under the state directory,
# not under the stub directory check_file looks in.
check_env() {
    # check_env <description> <path> <expected-fragment>
    if [ ! -f "$2" ]; then
        echo "FAIL: $1 - $2 does not exist"
        FAILS=1
        return
    fi
    if grep -q -F -e "$3" "$2"; then
        echo "ok: $1"
    else
        echo "FAIL: $1 - $2 never says '$3': $(cat "$2")"
        FAILS=1
    fi
}

check_absent_file() {
    # check_absent_file <description> <file>
    if [ -s "$STUB_DIR/$2" ]; then
        echo "FAIL: $1 - $2 exists and should not: $(cat "$STUB_DIR/$2")"
        FAILS=1
    else
        echo "ok: $1"
    fi
}

check_file() {
    # check_file <description> <file> <expected-content-fragment-or-empty>
    got=$(cat "$STUB_DIR/$2" 2>/dev/null || true)
    if [ -z "$3" ]; then
        if [ -n "$got" ]; then
            echo "FAIL: $1 - expected no $2, got: $got"
            FAILS=1
        else
            echo "ok: $1"
        fi
        return
    fi
    if printf '%s' "$got" | grep -q -F -e "$3"; then
        echo "ok: $1"
    else
        echo "FAIL: $1 - $2 does not contain '$3': ${got:-<empty>}"
        FAILS=1
    fi
}

check_live() {
    # check_live <description> <preview> <yes|no>
    if [ -d "$APIVO_STATE/previews/$2" ]; then had=yes; else had=no; fi
    if [ "$had" = "$3" ]; then
        echo "ok: $1"
    else
        echo "FAIL: $1 - $2 present=$had, expected $3"
        FAILS=1
    fi
}

# ===========================================================================
# Creating.
# ===========================================================================

reset
run
check "open pull requests get previews" 0 '"event":"created"'

# The credential has to REACH THE WIRE, not merely exist in the file.
#
# This is the assertion the suite was missing. Everything else here passes
# whether or not registry_auth found anything, because the stub answers a
# token request regardless - which is how a parser that could not read a
# pretty-printed config.json shipped green and left previews broken on every
# host, reported as "the registry is unreachable".
#
# c3R1YjpzdHVi is the fixture's value. Asserting on the header proves the
# whole path: file on disk -> registry_auth -> Authorization.
check_file "the stored credential reaches the registry as a Basic header" \
    curl_calls "Basic c3R1YjpzdHVi"
check_live "pr-1 is up" pr-1 yes
check_live "pr-2 is up" pr-2 yes
check_file "each gets its own database" sql "CREATE DATABASE apivo_pr_1"
check_file "and the other one too" sql "CREATE DATABASE apivo_pr_2"
if [ -r "$APIVO_STATE/previews/pr-1/api.env" ] &&
    grep -q 'dbname\|apivo_pr_1' "$APIVO_STATE/previews/pr-1/api.env"; then
    echo "ok: the preview's DATABASE_URL names its own database"
else
    echo "FAIL: pr-1's api.env does not name apivo_pr_1: $(cat "$APIVO_STATE/previews/pr-1/api.env" 2>/dev/null)"
    FAILS=1
fi
if grep -q 'sslmode=require' "$APIVO_STATE/previews/pr-1/api.env"; then
    echo "ok: and connects with TLS, like every other environment"
else
    echo "FAIL: pr-1 connects without sslmode=require"
    FAILS=1
fi

# ===========================================================================
# ===========================================================================
# A preview must be able to sign an editor in.
#
# Every editorial screen is behind sign-in, and a preview exists to be
# reviewed BEFORE the work reaches QA — so a preview that cannot
# authenticate cannot show a reviewer the thing they opened it for. The
# editorial half of the product was untestable in the one place it was meant
# to be tested first.
#
# Only public values cross: the anon key ships to every browser that loads
# QA, and the JWKS endpoint is published. The service-role key is not here.
# ===========================================================================

reset
run
check "previews are created" 0 '"event":"created"'

if grep -q '^JWKS_URL=https://qaproject.supabase.co/auth/v1/.well-known/jwks.json$' \
    "$APIVO_STATE/previews/pr-1/api.env" 2>/dev/null; then
    echo "ok: the api is given QA's JWKS endpoint"
else
    echo "FAIL: pr-1/api.env has no JWKS_URL: $(cat "$APIVO_STATE/previews/pr-1/api.env" 2>/dev/null)"
    FAILS=1
fi

# The LAST occurrence of each key, because provisioning writes a placeholder
# and step 5 appends the real value — and Docker resolves env_file last-wins.
# Reading the first match would hand the preview an empty string and report
# that QA has no auth while QA's own container serves that project. That
# exact bug shipped once already, in apivo-seed-editors.
if grep -q '^PUBLIC_SUPABASE_URL=https://qaproject.supabase.co$' \
    "$APIVO_STATE/previews/pr-1/web.env" 2>/dev/null &&
    grep -q '^PUBLIC_SUPABASE_ANON_KEY=sb_publishable_stub$' \
        "$APIVO_STATE/previews/pr-1/web.env" 2>/dev/null; then
    echo "ok: the web is given QA's project, taking the real value not the placeholder"
else
    echo "FAIL: pr-1/web.env does not carry QA's auth: $(cat "$APIVO_STATE/previews/pr-1/web.env" 2>/dev/null)"
    FAILS=1
fi

# Sharing an auth project is not sharing an identity: the api looks the
# token's subject up in the database it is pointed at, and a preview's is its
# own. Without the copy an editor signs in and is ErrUnknownAccount —
# authenticated and unauthorised, which reads as a permissions bug.
check_file "QA's editors are read out of QA's own database" qa_sql \
    "from account where role = 'editor'"
check_file "and loaded into the preview's" sql \
    "copy _qa_editors (id, email, display_name, role) from stdin"
check_file "through a conflict-tolerant insert, because every tick runs this" sql \
    "on conflict do nothing"

# A host whose QA has no auth yet gets the documented empty state, and is
# told why rather than left to wonder.
reset
: > "$APIVO_ETC/qa/web.env"
: > "$APIVO_ETC/qa/api.env"
run
check "a host with no QA auth still gets previews" 0 '"event":"created"'
if printf '%s' "$OUT" | grep -q '"event":"no_auth"'; then
    echo "ok: and is told its previews cannot sign anybody in"
else
    echo "FAIL: no auth configured, and nothing said so: $OUT"
    FAILS=1
fi
check_absent_file "with no editors copied from a QA that has none" qa_sql

# ===========================================================================
# A preview that already exists must FOLLOW its pull request.
#
# A push to an open pull request moves its tag. `up -d` alone never notices:
# compose does not fetch a tag it already has a local image for, so it
# compares the running container against the stale image and does nothing.
# The preview then serves the commit it was created from for the life of the
# pull request — healthy, silent, and a week out of date.
#
# Found by a reviewer opening a preview to test work pushed an hour earlier
# and being shown the first commit. The branch that does this was never
# covered here at all, which is how it survived.
# ===========================================================================

reset
run
check "the previews are created" 0 '"event":"created"'
# BOTH cleared, so what follows can only be explained by the second tick.
# Leaving `pulls` in place let creation's own pull satisfy the assertions
# below, and they passed against the unfixed script — a fixture in a shape
# the bug could not fail.
: > "$STUB_DIR/ups"
: > "$STUB_DIR/pulls"

# Second tick, same open pull requests, everything already on disk.
run
check_file "an existing preview pulls its tag again" pulls "pull pr-1"
check_file "and the other one" pulls "pull pr-2"
check_file "with the tag the pull request now points at" pulls "web:pr-1"
check_file "and then converges, so a moved tag recreates the container" ups "up pr-1"

# A registry that will not answer must not take a serving preview down. The
# old version is worse than the new one and far better than none.
reset
run
: > "$STUB_DIR/ups"
: > "$STUB_DIR/pulls"
: > "$STUB_DIR/pull_fails"
run
check "a preview whose pull fails is still reconciled" 0 ""
if printf '%s' "$OUT" | grep -q '"event":"pull_failed"'; then
    echo "ok: a failed refresh is reported"
else
    echo "FAIL: a failed refresh was not reported: $OUT"
    FAILS=1
fi
check_live "the preview keeps serving" pr-1 yes
rm -f "$STUB_DIR/pull_fails"

# Tearing down. The whole point: a closed pull request has no tag, and no tag
# means no preview - without anything having to tell this host so.
# ===========================================================================

reset
run
rm -f "$STUB_DIR/downs" "$STUB_DIR/sql"
# CI deleted pr-1's tags when the pull request merged.
printf '%s' '"qa","pr-2"' > "$STUB_DIR/api_tags"
printf '%s' '"qa","pr-2"' > "$STUB_DIR/web_tags"
run
check "a merged pull request's preview is destroyed" 0 '"event":"destroyed"'
check_live "pr-1 is gone" pr-1 no
check_live "pr-2 is untouched" pr-2 yes
check_file "and its database is dropped" sql "DROP DATABASE IF EXISTS apivo_pr_1"
check_file "only that one came down" downs "down pr-1"
if grep -q "down pr-2" "$STUB_DIR/downs" 2>/dev/null; then
    echo "FAIL: pr-2 was destroyed too"
    FAILS=1
else
    echo "ok: pr-2 was not touched"
fi

# ===========================================================================
# THE DANGEROUS CASE.
#
# Teardown is triggered by absence, so anything that makes the tag list look
# empty destroys every preview at once. A registry that refuses must be an
# error, never an empty list.
# ===========================================================================

reset
run
rm -f "$STUB_DIR/downs"
touch "$STUB_DIR/api_tags_fail"
run
check "a registry that refuses is an error, not an empty list" 1 "cannot list preview tags"
check "and it says nothing was destroyed" 1 "No preview was created or destroyed"
check_live "pr-1 survives a registry outage" pr-1 yes
check_live "pr-2 survives a registry outage" pr-2 yes
check_file "nothing was torn down" downs ""

reset
run
rm -f "$STUB_DIR/downs"
touch "$STUB_DIR/token_fails"
run
check "an expired credential is an error too" 1 "cannot list preview tags"
check_live "and previews survive it" pr-1 yes
check_file "nothing was torn down" downs ""

# A genuinely empty list IS a real state - every pull request closed - and
# must still tear down. The difference between this and the two above is the
# whole safety property.
reset
run
rm -f "$STUB_DIR/downs"
printf '%s' '"qa","staging"' > "$STUB_DIR/api_tags"
printf '%s' '"qa","staging"' > "$STUB_DIR/web_tags"
run
check "no open pull requests means no previews" 0 '"event":"destroyed"'
check_live "pr-1 is gone" pr-1 no
check_live "pr-2 is gone" pr-2 no

# ===========================================================================
# A half-published tag pair.
# ===========================================================================

reset
printf '%s' '"pr-1","pr-9"' > "$STUB_DIR/api_tags"
printf '%s' '"pr-1"' > "$STUB_DIR/web_tags"
run
check_live "a preview whose api tag exists but web does not is not started" pr-9 no
check_live "the complete pair is" pr-1 yes

# ===========================================================================
# The cap. A busy afternoon must not take the host down.
# ===========================================================================

reset 2
printf '%s' '"pr-1","pr-2","pr-7","pr-11"' > "$STUB_DIR/api_tags"
printf '%s' '"pr-1","pr-2","pr-7","pr-11"' > "$STUB_DIR/web_tags"
run
check "over the cap, the host says so rather than silently dropping" 0 '"event":"capped"'
check_live "the newest pull request is served" pr-11 yes
check_live "and the next newest" pr-7 yes
check_live "the oldest is not" pr-1 no
check_live "nor the next oldest" pr-2 no
# Numeric, not lexical: pr-11 must beat pr-7, and a string sort says
# otherwise. Asserted against the "capped" line alone - pr-11 appears in its
# own creation logs, so grepping the whole output proves nothing.
# Compared as an exact token SET, not by substring: `grep pr-1` also matches
# inside pr-11, and a trailing-space pattern depends on where the log line
# happens to end. Both of those made this assertion pass or fail for reasons
# that had nothing to do with the ordering it is supposed to prove.
capped=$(printf '%s' "$OUT" | grep '"event":"capped"' || true)
dropped=$(printf '%s' "$capped" | sed -n 's/.*not starting: \([^"]*\).*/\1/p' |
    tr ' ' '\n' | grep -v '^$' | sort | tr '\n' ' ')
if [ "$dropped" = "pr-1 pr-2 " ]; then
    echo "ok: pull requests are ordered numerically, so pr-11 outranks pr-7"
else
    echo "FAIL: expected exactly pr-1 and pr-2 to be dropped, got: ${dropped:-<none>} (from: $capped)"
    FAILS=1
fi

# ===========================================================================
# A preview that already exists gets the current configuration.
#
# The auth work wrote api.env and web.env in create_preview only. A preview
# is created ONCE and converged every sixty seconds for the rest of the pull
# request's life, so "only on creation" means "only for previews that did not
# exist yet when the change shipped" - which is every preview except the ones
# still being opened, and in particular not the one the change was written to
# be tested on.
#
# On the host, pr-146 had been running since before the change. Installing it
# left that preview with no web.env at all and an api.env from before
# JWKS_URL existed; compose refuses to start a service whose env_file is
# missing; and the converge branch ended in `|| true`, so the failure was
# discarded. Every tick reported success and changed nothing. The operator
# installed the fix, waited, and saw the same page.
#
# So this simulates exactly that: create a preview, delete its env files the
# way a host that predates them would have none, and tick again.
# ===========================================================================

reset
run
check "the preview is created" 0 '"event":"created"'

# A host from before the auth change: no web.env, and an api.env with no
# JWKS_URL in it.
rm -f "$APIVO_STATE/previews/pr-1/web.env"
cat > "$APIVO_STATE/previews/pr-1/api.env" <<'EOF'
DATABASE_URL=postgres://apivo:stub-password@apivo-preview-postgres:5432/apivo_pr_1?sslmode=require
EOF
: > "$STUB_DIR/ups"

run
if [ -f "$APIVO_STATE/previews/pr-1/web.env" ]; then
    echo "ok: converging an existing preview writes the web env it was missing"
else
    echo "FAIL: an existing preview was converged without ever being given a web.env: $OUT"
    FAILS=1
fi
check_env "with QA's project, so its sign-in reaches a real one" \
    "$APIVO_STATE/previews/pr-1/web.env" "https://qaproject.supabase.co"
check_env "and its anon key" \
    "$APIVO_STATE/previews/pr-1/web.env" "sb_publishable_stub"
check_env "and the api is given the JWKS endpoint it had no way to learn" \
    "$APIVO_STATE/previews/pr-1/api.env" "jwks.json"
check_file "and only then is the stack converged" ups "up pr-1"

# ===========================================================================
# A converge that cannot converge says so.
#
# `|| true` on the up branch turned a compose that refused to start into a
# tick indistinguishable from one that had nothing to do. That is the same
# silence this file was written to end for stale images, one branch over.
# ===========================================================================

reset
run
: > "$STUB_DIR/ups"
: > "$STUB_DIR/up_fails"
run
if printf '%s' "$OUT" | grep -q '"event":"converge_failed"'; then
    echo "ok: a converge that fails is reported rather than discarded"
else
    echo "FAIL: a failing converge was silent: $OUT"
    FAILS=1
fi

# ===========================================================================
# The quiet tick.
# ===========================================================================

reset
run
rm -f "$STUB_DIR/ups" "$STUB_DIR/downs" "$STUB_DIR/sql"
run
check "an unchanged tick converges quietly" 0 ""
check_file "and destroys nothing" downs ""
check_file "and creates no database" sql ""
check_file "but still brings the stacks up, so a dead container returns" ups "up pr-1"

# ===========================================================================
# Configuration refusals.
# ===========================================================================

reset
rm -f "$APIVO_ETC/preview/stack.env"
run
check "a host that does not serve previews is refused" 2 "does not serve previews"

reset
sed -i 's/^APIVO_REGISTRY=.*//' "$APIVO_ETC/preview/stack.env"
run
check "a stack.env missing a required key is refused" 2 "does not set APIVO_REGISTRY"

if [ "$FAILS" -ne 0 ]; then
    echo "apivo-previews: FAILURES"
    exit 1
fi
echo "apivo-previews: all tests passed"
