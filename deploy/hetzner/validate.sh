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

# QA and Staging layer the local Postgres on; production does not, and that
# single difference is the whole difference between them. Staging is here
# because the Supabase free tier is one project and it has to be production
# — see docs/ENVIRONMENTS.md for what that costs.
check_env qa docker-compose.yml docker-compose.local-db.yml
check_env staging docker-compose.yml docker-compose.local-db.yml
check_env prod docker-compose.yml

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
: > "$APIVO_STATE_DIR/previews/pr-1/api.env"

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

if [ "$FAILS" -ne 0 ]; then
    echo "hetzner config: FAILURES"
    exit 1
fi
echo "hetzner config: all checks passed"
