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
        echo "up ${APIVO_PREVIEW:-?}" >> "$STUB_DIR/ups"
        ;;
    down) echo "down ${APIVO_PREVIEW:-?}" >> "$STUB_DIR/downs" ;;
    esac
    ;;
exec)
    # psql against the shared preview Postgres. The statement is the last arg.
    for a in "$@"; do last="$a"; done
    echo "$last" >> "$STUB_DIR/sql"
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
    # What `docker login ghcr.io` leaves behind.
    printf '{"auths":{"ghcr.io":{"auth":"c3R1YjpzdHVi"}}}' > "$DOCKER_CONFIG/config.json"
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
