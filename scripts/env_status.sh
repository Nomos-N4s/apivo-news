#!/bin/sh
# What is actually running in every environment, right now.
#
# Usage: sh scripts/env_status.sh [--json]
#
# This is the answer to "did my change reach QA yet", and it is deliberately
# the cheapest possible thing to ask: one HTTPS request per environment, no
# credentials, no SSH key, no registry token, no VPS access. An agent that
# has merged a pull request can run this from a checkout and know whether the
# environment has caught up, without being given anything that could break
# one.
#
# It reports what each environment SERVES, which is the only account of a
# deployment that cannot be wrong. A channel tag says what should be running
# and a host's state file says what it believes it started; /healthz says
# what answered.
#
# The URLs come from deploy/hetzner/environments.env — committed, no secrets,
# and the same file the release pipeline reads to know where to probe.
set -eu

JSON=false
[ "${1:-}" = "--json" ] && JSON=true

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
ENVIRONMENTS="$ROOT/deploy/hetzner/environments.env"

[ -r "$ENVIRONMENTS" ] || {
    echo "env_status: $ENVIRONMENTS is missing" >&2
    exit 2
}
# The committed file supplies the defaults; the environment overrides it.
#
# That order matters in both directions. The file is the source of truth, so
# it is what is read when nobody says otherwise — but an environment that
# does not have a URL committed yet still has to be checkable, which is how
# a host is verified between provisioning it and opening the pull request
# that records it.
_from_env_qa="${APIVO_QA_URL:-}"
_from_env_staging="${APIVO_STAGING_URL:-}"
_from_env_prod="${APIVO_PROD_URL:-}"
# shellcheck source=/dev/null
. "$ENVIRONMENTS"
[ -n "$_from_env_qa" ] && APIVO_QA_URL="$_from_env_qa"
[ -n "$_from_env_staging" ] && APIVO_STAGING_URL="$_from_env_staging"
[ -n "$_from_env_prod" ] && APIVO_PROD_URL="$_from_env_prod"

# probe <base-url> — writes "status<TAB>version<TAB>ready" to stdout.
#
# Every failure is a value, never an exit: an environment that is down is a
# result this command reports, not an error that stops it looking at the
# others. Short timeouts and no retries, because this answers "what is true
# now" rather than "wait until it is true".
probe() {
    _base="${1%/}"
    _health=$(curl --fail --silent --show-error --max-time 10 "$_base/healthz" 2>/dev/null || true)
    if [ -z "$_health" ]; then
        printf 'down\t\tno\n'
        return
    fi
    # jq is not assumed: this runs on a laptop as often as in CI, and the one
    # field being read is a flat string.
    _version=$(printf '%s' "$_health" | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
    if curl --fail --silent --output /dev/null --max-time 10 "$_base/readyz" 2>/dev/null; then
        _ready=yes
    else
        # /healthz answers but /readyz does not: the process is up and the
        # database is not. Worth distinguishing — it is the difference
        # between "the deploy failed" and "the deploy worked and Supabase is
        # unreachable", which have entirely different fixes.
        _ready=no
    fi
    printf 'up\t%s\t%s\n' "${_version:-unknown}" "$_ready"
}

emit_json() {
    printf '{"environments":['
    _first=true
    for _row in $ROWS; do
        _env=$(printf '%s' "$_row" | cut -d'|' -f1)
        _url=$(printf '%s' "$_row" | cut -d'|' -f2)
        _st=$(printf '%s' "$_row" | cut -d'|' -f3)
        _ver=$(printf '%s' "$_row" | cut -d'|' -f4)
        _rdy=$(printf '%s' "$_row" | cut -d'|' -f5)
        [ "$_first" = true ] || printf ','
        _first=false
        printf '{"env":"%s","url":"%s","status":"%s","version":"%s","ready":%s}' \
            "$_env" "$_url" "$_st" "$_ver" "$([ "$_rdy" = yes ] && echo true || echo false)"
    done
    printf ']}\n'
}

ROWS=""
DOWN=0

for env_name in qa staging prod; do
    case "$env_name" in
    qa) url="${APIVO_QA_URL:-}" ;;
    staging) url="${APIVO_STAGING_URL:-}" ;;
    prod) url="${APIVO_PROD_URL:-}" ;;
    esac

    if [ -z "$url" ]; then
        # Not a failure. An empty URL means that environment has not been
        # provisioned, which today is the truth about production.
        ROWS="$ROWS ${env_name}|-|absent|-|no"
        continue
    fi

    result=$(probe "$url")
    status=$(printf '%s' "$result" | cut -f1)
    version=$(printf '%s' "$result" | cut -f2)
    ready=$(printf '%s' "$result" | cut -f3)
    [ "$status" = up ] && [ "$ready" = yes ] || DOWN=1
    ROWS="$ROWS ${env_name}|${url}|${status}|${version}|${ready}"
done

if [ "$JSON" = true ]; then
    emit_json
else
    printf '%-11s %-36s %-7s %-18s %s\n' ENVIRONMENT URL STATUS VERSION READY
    for row in $ROWS; do
        printf '%-11s %-36s %-7s %-18s %s\n' \
            "$(printf '%s' "$row" | cut -d'|' -f1)" \
            "$(printf '%s' "$row" | cut -d'|' -f2)" \
            "$(printf '%s' "$row" | cut -d'|' -f3)" \
            "$(printf '%s' "$row" | cut -d'|' -f4)" \
            "$(printf '%s' "$row" | cut -d'|' -f5)"
    done
    if printf '%s' "$ROWS" | grep -q '|absent|'; then
        echo
        echo "'absent' means that environment has no URL in deploy/hetzner/environments.env"
        echo "and has not been provisioned. See docs/ENVIRONMENTS.md."
    fi
fi

# Non-zero if any environment that is supposed to exist is not fully serving,
# so this composes into a shell chain without anything having to parse it.
exit "$DOWN"
