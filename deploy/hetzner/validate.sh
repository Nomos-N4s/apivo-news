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
#
# It PROVISIONS the modules rather than only adapting the Caddyfile to JSON,
# which is what makes it worth running — a directive whose arguments are wrong
# fails here, not at the first request. The cost is that it does everything
# starting Caddy would do short of listening, including opening the
# certificate the `tls` directive names. So a throwaway pair is generated
# below and mounted where the real Cloudflare Origin Certificate lives on a
# host. Validating against `caddy adapt` instead would dodge the certificate
# and check materially less.
# ---------------------------------------------------------------------------
CERTS="$TMP/certs"
mkdir -p "$CERTS"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
    -keyout "$CERTS/origin.key" -out "$CERTS/origin.pem" \
    -subj "/CN=validate.invalid" >/dev/null 2>&1 ||
    fail "could not generate a throwaway certificate for the Caddyfile checks"

if ! docker image inspect "$CADDY_IMAGE" >/dev/null 2>&1; then
    docker pull -q "$CADDY_IMAGE" >/dev/null 2>&1 ||
        echo "note: could not pull $CADDY_IMAGE; the Caddyfile checks may fail for that reason alone"
fi

caddy_run() {
    # caddy_run <caddyfile> <env assignments...> -- <caddy args...>
    _file="$1"
    shift
    _envs=""
    while [ "$1" != "--" ]; do
        _envs="$_envs -e $1"
        shift
    done
    shift
    # shellcheck disable=SC2086 # deliberate word splitting: -e pairs and args
    docker run --rm $_envs \
        -v "$CADDY_DIR/$_file:/etc/caddy/Caddyfile:ro" \
        -v "$CADDY_DIR/snippets.caddy:/etc/caddy/snippets.caddy:ro" \
        -v "$CERTS:/etc/caddy/certs:ro" \
        "$CADDY_IMAGE" "$@" 2>&1
}

check_caddyfile() {
    # check_caddyfile <file> <env assignments...>
    file="$1"
    shift

    if out=$(caddy_run "$file" "$@" -- caddy validate --config /etc/caddy/Caddyfile); then
        echo "ok: $file parses"
    else
        fail "$file does not parse: $out"
    fi

    # Formatting is checked separately because `caddy validate` only WARNS
    # about it, on a line nobody reads in a passing run — and a warning that
    # never fails anything is a warning that accumulates. `caddy fmt` without
    # --overwrite exits non-zero on drift, which is the assertion.
    if out=$(caddy_run "$file" "$@" -- caddy fmt /etc/caddy/Caddyfile); then
        echo "ok: $file is formatted"
    else
        # The output is carried into the message rather than summarised away:
        # this command exits non-zero both for real drift and for a docker
        # daemon it could not reach, and a failure that names the wrong cause
        # sends someone to reformat a file that was never the problem.
        fail "$file failed the formatting check (run: caddy fmt --overwrite deploy/hetzner/caddy/$file): $out"
    fi
}

check_caddyfile Caddyfile.preprod \
    APIVO_QA_HOST=qa.validate.invalid \
    APIVO_STAGING_HOST=staging.validate.invalid
check_caddyfile Caddyfile.prod \
    APIVO_PROD_HOST=validate.invalid \
    APIVO_PROD_ALT_HOST=www.validate.invalid

# ---------------------------------------------------------------------------
# The same-origin rewrite, asserted by RUNNING it.
#
# `caddy validate` cannot catch a broken rewrite. It accepts
# `header_up Origin https://{http.request.host} ...` quite happily, and that
# form silently rewrites nothing — header_up does not expand placeholders in
# its search argument. A config that parses is not a config that works, and
# the failure here is invisible until an editor cannot sign in.
#
# So the real snippet is loaded, with its upstreams pointed at echo sites in
# the same Caddy process, and asked what the container would actually
# receive. Both directions matter and both are asserted: the site's own
# https origin MUST be rewritten (or every editorial form post is refused),
# and a foreign origin MUST NOT be (or the CSRF check is defeated).
# ---------------------------------------------------------------------------
REWRITE="$TMP/rewrite"
mkdir -p "$REWRITE"
cp "$CADDY_DIR/snippets.caddy" "$REWRITE/snippets.caddy"
cat > "$REWRITE/Caddyfile" <<'EOF'
{
	auto_https off
	admin off
}

import /rw/snippets.caddy

# The upstreams the routes snippet expects, answering with what they were
# handed rather than with content.
:8080 {
	respond "api"
}

:4321 {
	respond "origin={http.request.header.Origin}|referer={http.request.header.Referer}"
}

:9081 {
	import apivo-routes 127.0.0.1 127.0.0.1 site.validate.invalid
}
EOF

rewrite_probe() {
    # rewrite_probe <header-line> — what the web container receives.
    docker run --rm -v "$REWRITE:/rw:ro" --entrypoint sh "$CADDY_IMAGE" -c "
        caddy start --config /rw/Caddyfile >/dev/null 2>&1
        for _ in 1 2 3 4 5 6 7 8 9 10; do
            wget -q -O- --header='$1' http://127.0.0.1:9081/ 2>/dev/null && exit 0
            sleep 1
        done
        exit 1
    " 2>/dev/null || true
}

got=$(rewrite_probe "Origin: https://site.validate.invalid")
case "$got" in
"origin=http://site.validate.invalid"*)
    echo "ok: this site's own https origin is rewritten (editorial form posts work)"
    ;;
"origin=https://site.validate.invalid"*)
    fail "the same-origin rewrite does NOT fire: the web container still sees 'https://', so csrf.ts compares it against its own 'http://' origin and refuses every editorial form post — sign-in, approval, withdrawal, source registration"
    ;;
*)
    fail "could not probe the same-origin rewrite (got: ${got:-nothing}); the Caddy image may be unavailable"
    ;;
esac

got=$(rewrite_probe "Origin: https://evil.validate.invalid")
case "$got" in
"origin=https://evil.validate.invalid"*)
    echo "ok: a foreign origin is left alone (the CSRF check still refuses it)"
    ;;
"")
    fail "could not probe the same-origin rewrite with a foreign origin"
    ;;
*)
    fail "a FOREIGN origin was rewritten to '$got' — the rewrite is not host-exact and the CSRF check can be defeated"
    ;;
esac

# The snippets file is imported by both Caddyfiles, so its syntax is already
# proved above — but it has no site addresses of its own and would never be
# fmt-checked without this.
if out=$(docker run --rm -v "$CADDY_DIR/snippets.caddy:/etc/caddy/snippets.caddy:ro" \
    "$CADDY_IMAGE" caddy fmt /etc/caddy/snippets.caddy 2>&1); then
    echo "ok: snippets.caddy is formatted"
else
    fail "snippets.caddy is not 'caddy fmt' clean: $out"
fi

if [ "$FAILS" -ne 0 ]; then
    echo "hetzner config: FAILURES"
    exit 1
fi
echo "hetzner config: all checks passed"
