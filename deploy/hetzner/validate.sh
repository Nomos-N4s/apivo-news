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
    # The cashback overlay's env_files - BOTH of them, one per database role.
    # Written for every environment rather than only the cashback ones:
    # compose resolves env_file paths for the whole file list it was given, so
    # a missing one fails the render with an error about a file instead of an
    # error about the configuration.
    : > "$APIVO_ETC/$env_name/blnk.env"
    : > "$APIVO_ETC/$env_name/blnk-migrate.env"

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

    # The contract topology, in every environment: nothing in an application
    # stack is publicly routable. `docker compose config` renders host port
    # publishing under a `published:` key, and NO service here may have one —
    # only the edge stack publishes ports, and it is checked separately.
    #
    # Asserted over the whole rendered document rather than over an awk range
    # scoped to the api service. `awk '/^  api:/,/^  [a-z]/'` looks like it
    # scopes to that service and does not: the start pattern also matches the
    # end pattern, so the range closes on its own first line and the grep
    # sees only "  api:". It could never fail. The document-wide form is both
    # stronger and honest about what it checks.
    if printf '%s' "$rendered" | grep -q 'published:'; then
        fail "$env_name: a service publishes a host port; the application stacks must be reachable only through Caddy"
    else
        echo "ok: $env_name publishes no host ports at all"
    fi

    # The data network carries the database and must never route outward.
    if ! printf '%s' "$rendered" | grep -q 'internal: true'; then
        fail "$env_name: the data network is not marked internal"
    else
        echo "ok: $env_name keeps the data network internal"
    fi
}

# QA layers the local Postgres on; production does not.
#
# Staging is checked in BOTH shapes, because it has both. provision.sh still
# composes it with the local database — the safe default for a box whose
# operator has no Supabase project yet — while the documented model drops that
# file and points staging at the nonprod project (docs/ENVIRONMENTS.md).
# Whichever shape a given host is running, its configuration has to parse, and
# checking only one leaves the other to be discovered by a host that will not
# start.
check_env qa docker-compose.yml docker-compose.local-db.yml
check_env staging docker-compose.yml docker-compose.local-db.yml
check_env staging docker-compose.yml
check_env prod docker-compose.yml

# ---------------------------------------------------------------------------
# The cashback overlay (ADR-0002): Blnk and Redis beside the api.
#
# Checked in BOTH database shapes, because the overlay is orthogonal to which
# Postgres an environment uses and both combinations will exist: QA layers it
# over the local container, production over Supabase. The trap it guards is
# the one docker-compose.local-db.yml documents — a `depends_on` naming a
# service that does not exist fails the WHOLE configuration, so an overlay
# that quietly assumed a `postgres` service would render fine on QA and take
# production's stack out entirely.
#
# check_env carries the generic properties (it parses, nothing publishes a
# host port, the data network is internal, every name is namespaced).
# check_cashback adds the ones only this overlay can get wrong.
# ---------------------------------------------------------------------------
check_cashback() {
    # check_cashback <env> <compose-files...>
    env_name="$1"
    shift

    check_env "$env_name" "$@"

    args=""
    for f in "$@"; do
        args="$args -f $COMPOSE_DIR/$f"
    done

    # shellcheck disable=SC2086 # deliberate word splitting: -f pairs
    if ! rendered=$(docker compose $args config 2>&1); then
        # check_env has already reported the parse failure; nothing below
        # could say anything true about a configuration that did not render.
        return
    fi

    for expected in "apivo-$env_name-blnk" "apivo-$env_name-blnk-worker" "apivo-$env_name-redis"; do
        printf '%s' "$rendered" | grep -q -F -e "$expected" ||
            fail "$env_name cashback: nothing in the rendered configuration is named '$expected'"
    done
    echo "ok: $env_name namespaces its ledger containers"

    # The api must be pointed at THIS environment's ledger. Two environments
    # on one host each run a Blnk, and an api addressing the other one would
    # post real money into the wrong ledger while every container sat healthy
    # and every log stayed quiet.
    printf '%s' "$rendered" | grep -q -F "http://apivo-$env_name-blnk:5001" ||
        fail "$env_name cashback: the api's BLNK_URL does not name apivo-$env_name-blnk; an api pointed at another environment's ledger posts money into it and nothing fails visibly"
    printf '%s' "$rendered" | grep -q -F "redis://apivo-$env_name-redis:6379" ||
        fail "$env_name cashback: the api's REDIS_URL does not name apivo-$env_name-redis"
    echo "ok: $env_name points the api at its own ledger and queue"

    # Loading the overlay IS the decision to run cashback. If that stopped
    # being true, an environment could carry a ledger and serve none of the
    # routes that use it — or, worse, the reverse.
    #
    # Matched tolerantly of quoting: `docker compose config` quotes a value
    # that would otherwise parse as a boolean and leaves a plain word alone,
    # so "true" arrives quoted and blnk does not. Pinning either form would
    # make this check a hostage to a YAML emitter's style.
    printf '%s' "$rendered" | grep -q 'CASHBACK_ENABLED: *"\{0,1\}true' ||
        fail "$env_name cashback: the overlay does not set CASHBACK_ENABLED=true, so the stack would run a ledger with the cashback routes unmounted"
    printf '%s' "$rendered" | grep -q 'LEDGER_DRIVER: *"\{0,1\}blnk' ||
        fail "$env_name cashback: the overlay does not select the blnk ledger driver"
    echo "ok: $env_name enables cashback against the blnk driver"

    # The rollout gate is only real if the ledger has a healthcheck: the
    # reconciler's `up -d --wait` waits on services that declare one and
    # treats the rest as ready the moment they are running. A ledger that had
    # crashed on its first database query would otherwise be rolled out,
    # verified and declared serving.
    #
    # Asserted on the ROUTE, not merely on the presence of a healthcheck. A
    # probe of some other path is a probe that passes while /health is the
    # thing an operator will read.
    printf '%s' "$rendered" | grep -q -F ':5001/health' ||
        fail "$env_name cashback: the ledger has no healthcheck against its /health route, so the rollout gate would pass a Blnk that never reached its database"
    # And the WORKER, on its own monitoring port. The server answering says
    # nothing about the process that drains the queue: a worker dead on its
    # first Redis call lets a deploy report success while every transaction
    # sits QUEUED - money not moving, and nothing anywhere saying so.
    printf '%s' "$rendered" | grep -q -F ':5004/health' ||
        fail "$env_name cashback: the blnk worker has no healthcheck against its /health route on 5004, so a dead worker would pass the rollout gate while transactions queued up behind it"
    echo "ok: $env_name gates the rollout on both the ledger and its worker answering /health"

    # ---------------------------------------------------------------------
    # The founder's split posture (2026-08-24): migrations as the database
    # owner, the server and worker as blnk_app, which owns nothing.
    #
    # This is asserted rather than reviewed because it is invisible when it
    # regresses. A single-role overlay works perfectly - it migrates, it
    # serves, every container is healthy - and the only difference is that the
    # process exposed every second of every day can reshape the tables holding
    # members' balances. Nothing fails until something already has.
    # ---------------------------------------------------------------------
    printf '%s' "$rendered" | grep -q -F "apivo-$env_name-blnk-migrate" ||
        fail "$env_name cashback: there is no blnk-migrate container; the ledger would be migrating with whatever role it serves with"

    # The migration container reads the OWNER's file and the runtime
    # containers read blnk_app's. Same variable inside, different file: that
    # difference IS the split.
    printf '%s' "$rendered" | grep -q -F "/$env_name/blnk-migrate.env" ||
        fail "$env_name cashback: blnk-migrate does not read blnk-migrate.env, so it is not running as the database owner"
    printf '%s' "$rendered" | grep -q -F "/$env_name/blnk.env" ||
        fail "$env_name cashback: nothing reads blnk.env, so the runtime role is not configured"
    echo "ok: $env_name gives the migration and the runtime their own env files"

    # The owner's credential must be ABSENT from the long-lived processes, not
    # merely unused by them. If blnk or blnk-worker ever gained the migration
    # file, a compromised ledger could read the owner DSN out of its own
    # environment and the split would buy nothing.
    _runtime_files=$(printf '%s' "$rendered" | awk '
        /^  (blnk|blnk-worker):$/ { svc = 1; next }
        /^  [a-z]/ { svc = 0 }
        svc && /blnk-migrate\.env/ { print "LEAK" }
    ')
    if [ -n "$_runtime_files" ]; then
        fail "$env_name cashback: the ledger server or worker reads blnk-migrate.env; the owner's credential must not be present in a long-lived container, only absent from it"
    else
        echo "ok: $env_name keeps the owner credential out of the running ledger"
    fi

    # `blnk start` does not migrate, so the ordering has to be real rather
    # than hopeful - a server answering before its tables exist fails per
    # request instead of refusing to start.
    printf '%s' "$rendered" | grep -q -F 'service_completed_successfully' ||
        fail "$env_name cashback: nothing waits for blnk-migrate to complete; the ledger would answer before its schema exists"
    echo "ok: $env_name waits for the migration before serving"

    # And the runtime containers must not migrate. `blnk start` and
    # `blnk workers` only - a migrate slipped back into an entrypoint would
    # run DDL as blnk_app and fail, or worse, be 'fixed' by widening the role.
    printf '%s' "$rendered" | grep -q -F 'blnk migrate up &&' &&
        fail "$env_name cashback: a runtime container still chains 'blnk migrate up'; migrations belong to the one-shot owner container"
    echo "ok: $env_name migrates only in the one-shot container"

    # Redis is a queue and a cache with no persistence and no source of truth.
    # `noeviction` is what makes that safe: a full Redis must REFUSE a write,
    # visibly, rather than evict a queued transfer and leave no trace.
    printf '%s' "$rendered" | grep -q -F 'noeviction' ||
        fail "$env_name cashback: redis is not configured with maxmemory-policy noeviction; a full queue would silently drop transfers instead of refusing them"
    echo "ok: $env_name refuses writes to a full Redis rather than evicting queued work"
}

# Every environment, in every database shape it has — matching the coverage
# the plain checks above already give. Staging is the one with two shapes and
# it was the one left out: provision.sh composes it with the local Postgres,
# the documented model drops that file and points it at the nonprod Supabase
# project, and the cashback overlay is orthogonal to both. Checking qa and
# prod covered one shape each and left staging's pair unvalidated entirely —
# which is precisely the environment a release candidate meets first, and the
# only one that exists in two shapes for a breakage to hide in.
check_cashback qa docker-compose.yml docker-compose.local-db.yml docker-compose.cashback.yml
check_cashback staging docker-compose.yml docker-compose.local-db.yml docker-compose.cashback.yml
check_cashback staging docker-compose.yml docker-compose.cashback.yml
check_cashback prod docker-compose.yml docker-compose.cashback.yml

# ---------------------------------------------------------------------------
# The preview stacks.
#
# A preview is one pull request, named by a registry tag. Both files are
# rendered here so a typo in either is a red pull request rather than a
# preview that never appears and never says why.
# ---------------------------------------------------------------------------
APIVO_PREVIEW=pr-1
APIVO_PREVIEW_API_IMAGE=ghcr.io/nomos-n4s/apivo-news/api:pr-1
APIVO_PREVIEW_WEB_IMAGE=ghcr.io/nomos-n4s/apivo-news/web:pr-1
APIVO_PREVIEW_PG_PASSWORD=validate-only
APIVO_STATE_DIR="$TMP/state"
export APIVO_PREVIEW APIVO_PREVIEW_API_IMAGE APIVO_PREVIEW_WEB_IMAGE
export APIVO_PREVIEW_PG_PASSWORD APIVO_STATE_DIR
mkdir -p "$APIVO_STATE_DIR/previews/pr-1" "$APIVO_ETC/preview/pg-certs"
# Both env files, because the preview stack now reads both: api.env for the
# database and the JWKS endpoint, web.env for the auth project the editorial
# screens sign in to. The reconciler writes them when it creates a preview.
: > "$APIVO_STATE_DIR/previews/pr-1/api.env"
: > "$APIVO_STATE_DIR/previews/pr-1/web.env"

if preview=$(docker compose -f "$COMPOSE_DIR/docker-compose.preview.yml" config 2>&1); then
    echo "ok: the preview stack parses"
    printf '%s' "$preview" | grep -q -F 'apivo-pr-1-api' ||
        fail "the preview stack does not name its containers after the pull request"
    printf '%s' "$preview" | grep -q 'published:' &&
        fail "a preview publishes a host port; previews are reachable only through Caddy"
    # A preview must not poll feeds or spend translation budget: it exists to
    # be looked at, and both of those cost money or bandwidth per open pull
    # request.
    printf '%s' "$preview" | grep -q 'POLL_INTERVAL: *"0"' ||
        fail "a preview does not disable feed polling"
    printf '%s' "$preview" | grep -q 'TRANSLATION_INTERVAL: *"0"' ||
        fail "a preview does not disable the translation pipeline"
    echo "ok: the preview is unpublished, and polls and translates nothing"
else
    fail "the preview stack does not parse: $preview"
fi

if out=$(docker compose -f "$COMPOSE_DIR/docker-compose.preview-db.yml" config 2>&1); then
    echo "ok: the shared preview database parses"
else
    fail "the shared preview database does not parse: $out"
fi

# ---------------------------------------------------------------------------
# The edge, in both host roles.
# ---------------------------------------------------------------------------
: > "$APIVO_ETC/edge/caddy.env"
for role in preprod prod; do
    if out=$(docker compose \
        -f "$COMPOSE_DIR/docker-compose.edge.yml" \
        -f "$COMPOSE_DIR/docker-compose.edge.$role.yml" config 2>&1); then
        echo "ok: the $role edge configuration parses"

        # Caddy must be ON every network it proxies into. Parsing proves
        # nothing about that: the first provisioned host had a valid edge
        # configuration, healthy preview containers, and 502 on every preview,
        # because the preprod overlay attached Caddy to QA and Staging and not
        # to the preview network. Nothing fails at that seam - the name simply
        # does not resolve, and only a request finds out.
        if [ "$role" = preprod ]; then
            for net in apivo-qa-edge apivo-staging-edge apivo-preview-edge; do
                if printf '%s' "$out" | grep -q -F "$net"; then
                    echo "ok: the preprod edge joins $net"
                else
                    fail "the preprod edge does not join $net, so Caddy cannot resolve the containers it proxies to on that network and they answer 502 while sitting healthy"
                fi
            done
        fi
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
# One per certificate the Caddyfiles name, because `caddy validate` opens
# every one of them: a missing origin-staging.pem fails the whole check with
# an error about a file rather than about the config. Deriving the list from
# the Caddyfiles themselves means adding a zone cannot leave this behind.
CERT_NAMES=$(grep -ho 'import origin-tls [a-z-]*' "$CADDY_DIR"/Caddyfile.* |
    awk '{print $3}' | sort -u)
[ -n "$CERT_NAMES" ] ||
    fail "no 'import origin-tls <name>' found in any Caddyfile; the certificate names could not be derived"
for _name in $CERT_NAMES; do
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
        -keyout "$CERTS/$_name.key" -out "$CERTS/$_name.pem" \
        -subj "/CN=validate.invalid" >/dev/null 2>&1 ||
        fail "could not generate the throwaway '$_name' certificate for the Caddyfile checks"
done
echo "ok: throwaway certificates for $(echo "$CERT_NAMES" | tr '\n' ' ')"

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

# Both preview-domain depths, because the shape of the name changes what the
# preview site block has to cope with, and validating only one depth is how
# the {labels.3} bug reached main: qa.validate.invalid happens to be exactly
# the depth that makes a right-counted label index look correct.
check_caddyfile Caddyfile.preprod \
    APIVO_QA_HOST=qa.validate.invalid \
    APIVO_STAGING_HOST=staging.validate.invalid \
    APIVO_PREVIEW_DOMAIN=qa.validate.invalid
check_caddyfile Caddyfile.preprod \
    APIVO_QA_HOST=validate.invalid \
    APIVO_STAGING_HOST=staging.validate.invalid \
    APIVO_PREVIEW_DOMAIN=validate.invalid
check_caddyfile Caddyfile.prod \
    APIVO_PROD_HOST=validate.invalid \
    APIVO_PROD_ALT_HOST=www.validate.invalid

# ---------------------------------------------------------------------------
# The scheme the frontend sees, asserted by RUNNING it.
#
# This is the whole reason the frontend is reached over TLS. @astrojs/node
# builds Astro.url from the SOCKET and the Host header
# (astro/dist/core/app/node.js):
#
#     const isEncrypted = 'encrypted' in req.socket && req.socket.encrypted;
#     const protocol = isEncrypted ? 'https' : 'http';
#
# It never consults X-Forwarded-Proto. So a frontend reached over plain http
# believes it is on http://, Astro's own CSRF middleware compares that with
# === against the browser's https:// Origin, and every form post the site
# makes of itself is refused - sign-in, approval, withdrawal, source
# registration. `caddy validate` cannot see any of that: it is a property of
# the connection, not of the syntax.
#
# What used to be here instead was a probe of the Origin REWRITE, which
# compensated by editing the browser's header down to http://. That rewrite
# is gone: it needed the hostname at parse time and so could never cover the
# wildcard preview host, and it made the CSRF property depend on three
# proxies each getting a find-and-replace exactly right.
#
# So the real snippet is loaded, its upstream is a TLS site in the same
# Caddy process, and it is asked what the container would actually receive.
# Both halves are asserted: the hop MUST be encrypted, and the browser's
# Origin MUST now arrive untouched.
# ---------------------------------------------------------------------------
REWRITE="$TMP/rewrite"
mkdir -p "$REWRITE"
cp "$CADDY_DIR/snippets.caddy" "$REWRITE/snippets.caddy"
# The upstream's certificate: self-signed, one day, exactly as throwaway as
# the one provision.sh writes is long-lived. The proxy does not verify it.
openssl req -new -x509 -days 1 -nodes \
    -out "$REWRITE/up.crt" -keyout "$REWRITE/up.key" \
    -subj "/CN=apivo-validate-web" 2>/dev/null
chmod 0644 "$REWRITE/up.key"
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

# TLS, like the real frontend container, so the scheme this reports is the
# scheme @astrojs/node would derive from its own socket.
:4321 {
	tls /rw/up.crt /rw/up.key
	respond "scheme={http.request.scheme}|origin={http.request.header.Origin}|referer={http.request.header.Referer}"
}

:9081 {
	import apivo-routes 127.0.0.1 127.0.0.1
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
"scheme=https|origin=https://site.validate.invalid"*)
    echo "ok: the frontend is reached over TLS and sees the browser's own origin unaltered"
    ;;
"scheme=http|"*)
    fail "the frontend is reached over PLAIN HTTP, so Astro.url would say http:// while the browser says https:// — Astro's origin check refuses every form post on the site, sign-in included"
    ;;
"scheme=https|origin=http://"*)
    fail "something is still rewriting Origin down to http:// ($got); with a TLS upstream that turns a same-origin post into a cross-origin one and refuses it"
    ;;
*)
    fail "could not probe the frontend hop (got: ${got:-nothing}); the Caddy image may be unavailable"
    ;;
esac

got=$(rewrite_probe "Origin: https://evil.validate.invalid")
case "$got" in
"scheme=https|origin=https://evil.validate.invalid"*)
    echo "ok: a foreign origin arrives as itself, so the origin check refuses it"
    ;;
"")
    fail "could not probe the frontend hop with a foreign origin"
    ;;
*)
    fail "a FOREIGN origin arrived as '$got' — anything that edits it here can defeat the origin check"
    ;;
esac

# ---------------------------------------------------------------------------
# Preview routing, asserted by RUNNING it — at TWO preview-domain depths.
#
# `caddy validate` cannot catch this either, and the first version of this
# deployment shipped the bug: the preview upstream was derived from
# {labels.3}, a label index counted from the RIGHT. That is the leftmost label
# of pr-1.qa.example.com and the EMPTY STRING for pr-1.example.com. Point
# previews at a one-level domain and every preview 403s while the config stays
# perfectly valid — and this file did not notice, because it validated with a
# two-label preview domain, which is exactly the depth that makes the wrong
# index look right.
#
# So both depths are probed, and the matcher is LIFTED from the real
# Caddyfile rather than restated here. Restating it would only ever prove
# that a copy works.
# ---------------------------------------------------------------------------
PREVIEW="$TMP/preview"
mkdir -p "$PREVIEW"
cp "$CADDY_DIR/snippets.caddy" "$PREVIEW/snippets.caddy"
# The preview frontend is reached over TLS too (see the scheme probe above),
# so the echo standing in for it has to speak TLS or every assertion below
# fails as a 502 and says nothing about routing.
cp "$REWRITE/up.crt" "$PREVIEW/up.crt"
cp "$REWRITE/up.key" "$PREVIEW/up.key"

# Matched on `https://*.` rather than on the variable name: the site address
# is the only wildcard site in the file, and naming the variable here would
# put a `$` inside single quotes for no gain.
preview_site_count=$(grep -c '^https://\*\.' "$CADDY_DIR/Caddyfile.preprod" || true)
[ "$preview_site_count" = 1 ] ||
    fail "expected exactly one wildcard site in Caddyfile.preprod, found $preview_site_count; the preview checks below lift their matcher from the first one and may be testing the wrong block"
preview_matcher=$(sed -n '/^https:\/\/\*\./,/^}/p' \
    "$CADDY_DIR/Caddyfile.preprod" |
    sed -n 's/^[[:space:]]*\(@preview .*\)$/\1/p' | head -n 1)
if [ -z "$preview_matcher" ]; then
    fail "could not lift the @preview matcher out of Caddyfile.preprod; the preview routing checks below would prove nothing"
    preview_matcher="@preview expression false"
fi

# The upstreams resolve because the container is given host entries for them,
# so a request that reaches the echo proves the placeholder expanded to
# exactly `pr-7` — `apivo--api` or a literal placeholder does not resolve and
# comes back 502. The catch-all answers 200 rather than aborting like the real
# file: this test only needs to tell "the matcher did not fire" apart from
# "it fired", and an aborted connection is indistinguishable from a Caddy that
# never started.
cat > "$PREVIEW/Caddyfile" <<EOF
{
	auto_https off
	admin off
}

import /pv/snippets.caddy

:8080 {
	respond "api-upstream"
}

:4321 {
	tls /pv/up.crt /pv/up.key
	respond "web-upstream"
}

:9082 {
	$preview_matcher
	handle @preview {
		import apivo-preview-routes
	}
	handle {
		respond "REFUSED"
	}
}
EOF

preview_probe() {
    # preview_probe <host-header> <path> — what the preview site routes to.
    docker run --rm \
        --add-host apivo-pr-7-api:127.0.0.1 \
        --add-host apivo-pr-7-web:127.0.0.1 \
        -v "$PREVIEW:/pv:ro" --entrypoint sh "$CADDY_IMAGE" -c "
        caddy start --config /pv/Caddyfile >/dev/null 2>&1
        for _ in 1 2 3 4 5 6 7 8 9 10; do
            wget -q -O- --header='Host: $1' 'http://127.0.0.1:9082$2' 2>/dev/null && exit 0
            sleep 1
        done
        exit 1
    " 2>/dev/null || true
}

# A ONE-LEVEL preview domain (pr-7.example.com). This is the case the label
# index got wrong, and the case this deployment actually uses.
got=$(preview_probe pr-7.example.com /)
case "$got" in
web-upstream)
    echo "ok: a preview on a one-level domain reaches its own web container"
    ;;
REFUSED)
    fail "a preview on a one-level domain (pr-7.example.com) is REFUSED by the @preview matcher. The matcher depends on how deep the preview domain is - almost certainly a {labels.N} index, which is empty at this depth. Every preview would 403."
    ;;
*)
    fail "a preview on a one-level domain did not reach its web container (got: ${got:-nothing}). If this is a 502 the upstream placeholder did not expand to 'pr-7'."
    ;;
esac

# A TWO-LEVEL preview domain (pr-7.qa.example.com), so the fix is not merely
# the old bug moved one label along.
got=$(preview_probe pr-7.qa.example.com /)
case "$got" in
web-upstream)
    echo "ok: and so does a preview on a two-level domain"
    ;;
*)
    fail "a preview on a two-level domain (pr-7.qa.example.com) did not reach its web container (got: ${got:-nothing}); the preview routing works at one depth only"
    ;;
esac

got=$(preview_probe pr-7.example.com /api/x)
case "$got" in
api-upstream)
    echo "ok: and /api/* reaches that preview's api container, not its frontend"
    ;;
*)
    fail "a preview's /api/* did not reach its api container (got: ${got:-nothing})"
    ;;
esac

# The security property: no label but pr-<n> may become a container name.
for hostile in apivo-qa-api.example.com pr-.example.com pr-7x.example.com; do
    got=$(preview_probe "$hostile" /)
    case "$got" in
    REFUSED)
        echo "ok: $hostile is refused rather than proxied at"
        ;;
    *)
        fail "$hostile was NOT refused (got: ${got:-nothing}); a hostname that is not pr-<n> can be turned into a container name to proxy at"
        ;;
    esac
done

# ---------------------------------------------------------------------------
# The crawler fence list, against the copy it was taken from.
#
# FR-013's deny list now lives in three places: deploy/cloudflare/routing.js,
# web/src/middleware.ts, and the (crawler-fence) snippet. Three copies drift,
# and the way this one would drift is silently — a signature added to the
# application copies and not here means the JSON API serves that crawler the
# corpus while the HTML pages refuse it, which no test would otherwise notice.
# ---------------------------------------------------------------------------
js_sigs=$(sed -n "/^export const CRAWLER_SIGNATURES/,/^];/p" \
    "$HERE/../cloudflare/routing.js" | sed -n "s/^\t'\([^']*\)',$/\1/p" | sort)
caddy_sigs=$(sed -n 's/.*@crawler header_regexp User-Agent (?i)(\(.*\))$/\1/p' \
    "$CADDY_DIR/snippets.caddy" | tr '|' '\n' | sed 's/\\//g' | sort)
if [ -z "$caddy_sigs" ]; then
    fail "the (crawler-fence) snippet has no User-Agent list; FR-013 is not enforced at the edge"
elif [ "$js_sigs" = "$caddy_sigs" ]; then
    echo "ok: the crawler deny list matches deploy/cloudflare/routing.js ($(printf '%s\n' "$caddy_sigs" | wc -l | tr -d ' ') signatures)"
else
    fail "the crawler deny list has drifted from deploy/cloudflare/routing.js. Only in one of them: $(printf '%s\n' "$js_sigs" "$caddy_sigs" | sort | uniq -u | tr '\n' ' ')"
fi

# The snippets file is imported by both Caddyfiles, so its syntax is already
# proved above — but it has no site addresses of its own and would never be
# fmt-checked without this.
if out=$(docker run --rm -v "$CADDY_DIR/snippets.caddy:/etc/caddy/snippets.caddy:ro" \
    "$CADDY_IMAGE" caddy fmt /etc/caddy/snippets.caddy 2>&1); then
    echo "ok: snippets.caddy is formatted"
else
    fail "snippets.caddy is not 'caddy fmt' clean: $out"
fi

# ---------------------------------------------------------------------------
# The DOCKER-USER chain must not block traffic LEAVING a container.
#
# iptables cannot be exercised here - there is no privileged netfilter in CI -
# so this asserts the shape of the rules provision.sh emits. That is worth
# doing anyway, because the bug it guards is invisible until a container needs
# the internet: DOCKER-USER hangs off FORWARD and sees both directions, so a
# bare `--dport 443 -j DROP` also drops a container dialling out. The first
# host provisioned answered 502 with the api crash-looping on a JWKS fetch
# that TIMED OUT, while the same URL fetched from the host worked perfectly -
# host traffic never traverses FORWARD.
#
# Two things make it safe, and both are asserted: the DROP is scoped to the
# external interface, and a RETURN for everything arriving on any other
# interface comes FIRST.
# ---------------------------------------------------------------------------
fw=$(sed -n '/iptables -N DOCKER-USER/,/DOCKER-USER rules installed/p' "$HERE/provision.sh")

# Matched without a `$`, deliberately: the pattern would otherwise contain a
# literal '$ext_if' and shellcheck's SC2016 fires on it. CI runs shellcheck
# with no severity filter, so an info-level finding fails the job.
return_line=$(printf '%s\n' "$fw" | grep -n -- '! -i .* -j RETURN' | cut -d: -f1 | head -n 1)
drop_line=$(printf '%s\n' "$fw" | grep -n -- '-p tcp --dport 443 -j DROP' | cut -d: -f1 | head -n 1)

if [ -z "$return_line" ]; then
    fail "provision.sh does not RETURN traffic arriving on an interface other than the external one, so the DOCKER-USER 443 DROP will also drop container-originated HTTPS - the api cannot fetch its JWKS and every environment answers 502"
elif [ -z "$drop_line" ]; then
    fail "provision.sh no longer drops inbound 443 in DOCKER-USER; ufw does not cover published container ports, so the origin would be reachable by anyone who learns its address"
elif [ "$return_line" -ge "$drop_line" ]; then
    fail "provision.sh emits the DOCKER-USER 443 DROP (line $drop_line of that block) at or before the RETURN for non-external interfaces (line $return_line); the DROP is evaluated first and container-originated HTTPS dies"
else
    echo "ok: DOCKER-USER returns container-originated traffic before dropping inbound 443"
fi

if printf '%s\n' "$fw" | grep -q -- '-p tcp --dport 443 -j DROP' &&
    ! printf '%s\n' "$fw" | grep -q -- '-i .* -p tcp --dport 443 -j DROP'; then
    fail "the DOCKER-USER 443 DROP in provision.sh is not scoped to the external interface, so it matches traffic in both directions"
else
    echo "ok: the DOCKER-USER 443 DROP is scoped to the external interface"
fi

if [ "$FAILS" -ne 0 ]; then
    echo "hetzner config: FAILURES"
    exit 1
fi
echo "hetzner config: all checks passed"
