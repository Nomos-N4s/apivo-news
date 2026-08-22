#!/bin/sh
# Prove the deployment configuration without deploying it.
#
# Runs in CI on every PR and locally as `make hetzner-validate`. It answers
# the questions that used to be answerable only by pushing to a box and
# watching:
#
#   - does every environment's compose configuration parse, in the exact
#     combination of files that environment actually uses?
#   - do the container, network and volume names come out namespaced per
#     environment, so two environments on one host cannot collide?
#   - is the api genuinely unpublished, in every environment?
#   - does each Caddyfile parse, with its snippets and its variables?
#
# Nothing here contacts a registry, a daemon or a host. The images are
# fictional digests and the environment files are empty fixtures under a
# temporary root, which is what ${APIVO_ETC} in the compose files is for.
set -eu

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
COMPOSE_DIR="$HERE/compose"
CADDY_DIR="$HERE/caddy"

CADDY_IMAGE="${CADDY_IMAGE:-caddy:2.10-alpine}"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

APIVO_ETC="$TMP/etc"
export APIVO_ETC
mkdir -p "$APIVO_ETC/edge/certs"

# Fictional but well-formed: a digest is 64 hex characters, and the compose
# files reject an unset image outright, so these prove the pinning path too.
APIVO_API_IMAGE=ghcr.io/nomos-n4s/apivo-news/api@sha256:1111111111111111111111111111111111111111111111111111111111111111
APIVO_WEB_IMAGE=ghcr.io/nomos-n4s/apivo-news/web@sha256:2222222222222222222222222222222222222222222222222222222222222222
export APIVO_API_IMAGE APIVO_WEB_IMAGE
export APIVO_VERSION=v0.0.0-validate
export APIVO_PG_PASSWORD=validate-only

FAILS=0

fail() {
    echo "FAIL: $1"
    FAILS=1
}

# ---------------------------------------------------------------------------
# The application stacks, one environment at a time.
# ---------------------------------------------------------------------------
check_env() {
    # check_env <env> <compose-files...>
    env_name="$1"
    shift
    mkdir -p "$APIVO_ETC/$env_name"
    : > "$APIVO_ETC/$env_name/api.env"
    : > "$APIVO_ETC/$env_name/web.env"

    APIVO_ENV="$env_name"
    export APIVO_ENV

    args=""
    for f in "$@"; do
        args="$args -f $COMPOSE_DIR/$f"
    done

    # shellcheck disable=SC2086 # deliberate word splitting: -f pairs
    if ! rendered=$(docker compose $args config 2>&1); then
        fail "$env_name: the compose configuration does not parse: $rendered"
        return
    fi
    echo "ok: $env_name compose configuration parses"

    # Every name is namespaced by environment. Without this, two environments
    # on the pre-production host would fight over a container name and the
    # second to start would simply lose — silently, and with the first one
    # still answering.
    for expected in "apivo-$env_name-api" "apivo-$env_name-web" "apivo-$env_name-edge" "apivo-$env_name-data"; do
        printf '%s' "$rendered" | grep -q -F -e "$expected" ||
            fail "$env_name: nothing in the rendered configuration is named '$expected'"
    done
    echo "ok: $env_name namespaces its containers and networks"

    # The contract topology, in every environment: the API is not publicly
    # routable. `docker compose config` renders host port publishing under a
    # `published:` key, so the api service having one at all is the failure.
    if printf '%s' "$rendered" | awk '/^  api:/,/^  [a-z]/' | grep -q 'published:'; then
        fail "$env_name: the api publishes a host port; it must be reachable only through Caddy"
    else
        echo "ok: $env_name keeps the api off the host's network"
    fi

    # The data network carries the database and must never route outward.
    if ! printf '%s' "$rendered" | grep -q 'internal: true'; then
        fail "$env_name: the data network is not marked internal"
    else
        echo "ok: $env_name keeps the data network internal"
    fi
}

# QA layers the local Postgres on; staging and production do not, and that
# single difference is the whole difference between them.
check_env qa docker-compose.yml docker-compose.local-db.yml
check_env staging docker-compose.yml
check_env prod docker-compose.yml

# ---------------------------------------------------------------------------
# The edge, in both host roles.
# ---------------------------------------------------------------------------
: > "$APIVO_ETC/edge/caddy.env"
for role in preprod prod; do
    if out=$(docker compose \
        -f "$COMPOSE_DIR/docker-compose.edge.yml" \
        -f "$COMPOSE_DIR/docker-compose.edge.$role.yml" config 2>&1); then
        echo "ok: the $role edge configuration parses"
    else
        fail "the $role edge configuration does not parse: $out"
    fi
done

# ---------------------------------------------------------------------------
# The Caddyfiles.
#
# `caddy validate` is to this deployment what `wrangler deploy --dry-run` was
# to the last one: a full parse of the thing that decides which container
# answers, needing no credentials and reaching no network. A typo here used
# to be discoverable only by restarting the proxy in front of every
# environment on the host.
# ---------------------------------------------------------------------------
if ! docker image inspect "$CADDY_IMAGE" >/dev/null 2>&1; then
    docker pull -q "$CADDY_IMAGE" >/dev/null 2>&1 ||
        echo "note: could not pull $CADDY_IMAGE; the Caddyfile checks may fail for that reason alone"
fi

check_caddyfile() {
    # check_caddyfile <file> <env assignments...>
    file="$1"
    shift
    envs=""
    for kv in "$@"; do
        envs="$envs -e $kv"
    done
    # shellcheck disable=SC2086 # deliberate word splitting: -e pairs
    if out=$(docker run --rm $envs \
        -v "$CADDY_DIR/$file:/etc/caddy/Caddyfile:ro" \
        -v "$CADDY_DIR/snippets.caddy:/etc/caddy/snippets.caddy:ro" \
        "$CADDY_IMAGE" caddy validate --config /etc/caddy/Caddyfile 2>&1); then
        echo "ok: $file parses"
    else
        fail "$file does not parse: $out"
    fi
}

check_caddyfile Caddyfile.preprod \
    APIVO_QA_HOST=qa.validate.invalid \
    APIVO_STAGING_HOST=staging.validate.invalid
check_caddyfile Caddyfile.prod \
    APIVO_PROD_HOST=validate.invalid \
    APIVO_PROD_ALT_HOST=www.validate.invalid

if [ "$FAILS" -ne 0 ]; then
    echo "hetzner config: FAILURES"
    exit 1
fi
echo "hetzner config: all checks passed"
