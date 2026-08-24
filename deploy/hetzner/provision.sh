#!/bin/sh
# Turn a fresh Hetzner VPS into a deployment host. Run once, as root, from a
# checkout of this repository. Safe to re-run: every step checks before it
# acts, and NOTHING here ever overwrites a file that already holds a secret.
#
#   APIVO_HOST_ROLE=preprod \
#   APIVO_QA_HOST=ra1ze.com APIVO_STAGING_HOST=reapie.com \
#   APIVO_PREVIEW_DOMAIN=ra1ze.com \
#   APIVO_ORIGIN_CERT=/root/origin.pem APIVO_ORIGIN_KEY=/root/origin.key \
#   APIVO_STAGING_ORIGIN_CERT=/root/origin-staging.pem \
#   APIVO_STAGING_ORIGIN_KEY=/root/origin-staging.key \
#   GHCR_USER=<github-user> GHCR_TOKEN=<read:packages PAT> \
#   APIVO_CONFIGURE_FIREWALL=yes \
#   sh deploy/hetzner/provision.sh
#
# It leaves the host in a state where nothing else has to be done to it,
# ever, to ship code: the environments reconcile themselves from the
# registry every minute, so the next deploy — and every one after it — is a
# merge or a tag, not a visit here.
#
# ---------------------------------------------------------------------------
# What it does NOT do
# ---------------------------------------------------------------------------
#
# It does not write DATABASE_URL, the Supabase keys or the translation
# credential. Those are the operator's to put in /etc/apivo/<env>/api.env and
# web.env, which this script creates as commented templates and then never
# touches again. A provisioning script that could fill them in would be a
# provisioning script that had to be given them.
#
# It does not choose for you whether to configure the firewall. Both answers
# are dangerous in different directions, so APIVO_CONFIGURE_FIREWALL has no
# default and the script refuses to run until it is set to yes or no.
set -eu

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

PREFIX=/opt/apivo
ETC=/etc/apivo
STATE=/var/lib/apivo
UNITS=/etc/systemd/system

APIVO_HOST_ROLE="${APIVO_HOST_ROLE:-}"
APIVO_REGISTRY="${APIVO_REGISTRY:-ghcr.io/nomos-n4s/apivo-news}"
# No default. Both answers are dangerous in different directions - `yes`
# mis-set locks you out of the box, `no` leaves the origin reachable by
# anyone who learns its address, which makes every protection configured at
# the Cloudflare edge one DNS lookup away from irrelevant. A default would
# pick one of those hazards on the operator's behalf and, being a default,
# would be the one nobody thought about. So the script refuses to run until
# somebody has answered.
APIVO_CONFIGURE_FIREWALL="${APIVO_CONFIGURE_FIREWALL:-}"
APIVO_SSH_PORT="${APIVO_SSH_PORT:-22}"

say() { printf '\n=== %s ===\n' "$1"; }
note() { printf '  %s\n' "$1"; }
die() {
    printf '\nprovision: %s\n' "$1" >&2
    exit 1
}

# ---------------------------------------------------------------------------
# 0. Preflight
# ---------------------------------------------------------------------------
say "Preflight"

[ "$(id -u)" = 0 ] || die "run as root"
command -v systemctl >/dev/null 2>&1 || die "no systemd on this host"

case "$APIVO_CONFIGURE_FIREWALL" in
yes | no) ;;
*)
    die "set APIVO_CONFIGURE_FIREWALL to 'yes' or 'no' - there is no default.

  yes  admit 443 from Cloudflare's published ranges only, and ssh on port
       $APIVO_SSH_PORT. This is what you want: the edge publishes Caddy on
       80/443 to every interface, so without it anyone who learns this host's
       address reaches the origin directly and Cloudflare's WAF, rate limits
       and bot rules protect nothing. Check APIVO_SSH_PORT first if you do not
       use 22 - a wrong value here locks you out of the box.

  no   change nothing. Only correct if a firewall is already managed
       elsewhere (Hetzner Cloud Firewall, a hardened image, or by hand)."
    ;;
esac

case "$APIVO_HOST_ROLE" in
preprod)
    ENVS="qa staging"
    CADDYFILE=Caddyfile.preprod
    EDGE_OVERLAY=docker-compose.edge.preprod.yml
    [ -n "${APIVO_QA_HOST:-}" ] || die "set APIVO_QA_HOST (this project: ra1ze.com)"
    [ -n "${APIVO_STAGING_HOST:-}" ] || die "set APIVO_STAGING_HOST (this project: reapie.com)"
    ;;
prod)
    ENVS="prod"
    CADDYFILE=Caddyfile.prod
    EDGE_OVERLAY=docker-compose.edge.prod.yml
    [ -n "${APIVO_PROD_HOST:-}" ] || die "set APIVO_PROD_HOST (this project: apivo.com)"
    [ -n "${APIVO_PROD_ALT_HOST:-}" ] || die "set APIVO_PROD_ALT_HOST (the name that redirects to it, e.g. www.example.com)"
    ;;
*)
    die "set APIVO_HOST_ROLE to 'preprod' (QA + Staging) or 'prod' (production alone)"
    ;;
esac
note "role: $APIVO_HOST_ROLE, environments: $ENVS"

if ! command -v docker >/dev/null 2>&1; then
    say "Installing Docker"
    # The official convenience script. Pinned to nothing on purpose: Docker
    # is the platform here, not a dependency of the application, and the
    # application is pinned by digest regardless of what runs it.
    curl -fsSL https://get.docker.com | sh
fi
docker compose version >/dev/null 2>&1 || die "this Docker has no compose plugin; install docker-compose-plugin"
note "docker: $(docker --version)"

# ---------------------------------------------------------------------------
# 1. The programs
#
# Copied rather than symlinked into the checkout: a host must not depend on a
# git working tree that someone might move, dirty, or check out to another
# branch while investigating something at 3am.
# ---------------------------------------------------------------------------
say "Installing to $PREFIX"

mkdir -p "$PREFIX/bin" "$PREFIX/compose" "$PREFIX/caddy" "$STATE"
install -m 0755 "$HERE/bin/apivo-reconcile" "$PREFIX/bin/apivo-reconcile"
install -m 0755 "$HERE/bin/apivo-previews" "$PREFIX/bin/apivo-previews"
install -m 0755 "$HERE/bin/apivoctl" "$PREFIX/bin/apivoctl"
install -m 0755 "$HERE/bin/apivo-seed-editors" "$PREFIX/bin/apivo-seed-editors"
# Both of the programs a person runs by hand go on PATH. apivo-reconcile and
# apivo-previews do not: they are started by systemd with an absolute path,
# and a host where somebody runs the reconciler by hand is a host where the
# timer and the hand are fighting over the same environment.
ln -sf "$PREFIX/bin/apivoctl" /usr/local/bin/apivoctl
ln -sf "$PREFIX/bin/apivo-seed-editors" /usr/local/bin/apivo-seed-editors
cp "$HERE/compose/"*.yml "$PREFIX/compose/"
cp "$HERE/caddy/"* "$PREFIX/caddy/"
note "installed apivo-reconcile, apivoctl, apivo-seed-editors (on PATH), compose files, Caddy config"

# ---------------------------------------------------------------------------
# 2. Per-environment configuration
#
# Created ONCE. On a re-run the existing files are left exactly as they are,
# because by then they hold the database credential and this script has no
# way to know a better value than the one already there.
# ---------------------------------------------------------------------------
say "Configuring environments"

for env_name in $ENVS; do
    mkdir -p "$ETC/$env_name"

    # Both QA and Staging are given a Postgres container HERE, and staging is
    # then pointed at the nonprod Supabase project by hand (docs/RUNBOOK.md
    # step 5). That is deliberate: a container is the shape that works on a
    # box whose operator has no Supabase project yet, so provisioning never
    # depends on an account existing, and the switch is two edits that survive
    # a re-run because this script never rewrites an existing stack.env.
    #
    # This comment used to justify the container by saying the Supabase free
    # tier was a single project which had to be production. That was wrong -
    # it allows two per organisation - and the model built on it has changed.
    # See docs/ENVIRONMENTS.md for what staging gains by being on a real
    # Supabase project, and why QA keeps its container regardless.
    case "$env_name" in
    qa | staging)
        compose_files="$PREFIX/compose/docker-compose.yml:$PREFIX/compose/docker-compose.local-db.yml"
        ;;
    *)
        compose_files="$PREFIX/compose/docker-compose.yml"
        ;;
    esac

    if [ -e "$ETC/$env_name/stack.env" ]; then
        note "$env_name: stack.env exists, left alone"
    else
        # The Postgres password for the environments that have a Postgres.
        # Generated, never typed: this is the one credential on the host that
        # nobody needs to know.
        #
        # Written ONLY for those environments. Written unconditionally, it put
        # a generated APIVO_PG_PASSWORD into production's stack.env too -
        # which made apivoctl's "this environment uses Supabase" guard dead
        # code on every provisioned host, so `apivoctl psql prod` reached for
        # a postgres service that stack does not define and failed with
        # docker's error instead of the message written for exactly that case.
        pg_block=""
        if [ "$env_name" = qa ] || [ "$env_name" = staging ]; then
            pg_password=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
            pg_block="APIVO_PG_PASSWORD=$pg_password
APIVO_PG_DB=apivo
APIVO_PG_USER=apivo
APIVO_PG_CPUS=1.0
APIVO_PG_MEMORY=768M"
        fi
        cat > "$ETC/$env_name/stack.env" <<EOF
# Written by provision.sh. Structural configuration only — no application
# secrets (those live in api.env). Documented in
# deploy/hetzner/env/stack.env.example.
APIVO_ENV=$env_name
APIVO_CHANNEL=$env_name
APIVO_REGISTRY=$APIVO_REGISTRY
COMPOSE_FILE=$compose_files
$pg_block
# How long a rollout may take before it is called failed. The api migrates the
# schema on boot, so raise this before a release carrying a long migration.
APIVO_WAIT_TIMEOUT=180
# How long an unused image is kept. It must outlast any release you might want
# to roll back to by hand: every image here is digest-pulled and untagged, so
# anything older than this is prunable.
APIVO_IMAGE_RETENTION=168h
APIVO_API_CPUS=1.0
APIVO_API_MEMORY=768M
APIVO_WEB_CPUS=1.0
APIVO_WEB_MEMORY=512M
APIVO_LOG_MAX_SIZE=10m
APIVO_LOG_MAX_FILE=3
EOF
        chmod 0640 "$ETC/$env_name/stack.env"
        note "$env_name: wrote stack.env"
    fi

    if [ -e "$ETC/$env_name/api.env" ]; then
        note "$env_name: api.env exists, left alone (it holds the database credential)"
    else
        # A template with every key present and empty. Empty is a documented,
        # safe state for all of them: no JWKS_URL unmounts the editorial
        # routes and keeps serving readers, and no translation keys leave the
        # pipeline off. A placeholder would be worse than nothing — the
        # binary would treat it as a real value and crash-loop.
        sed -e "s#^DATABASE_URL=.*#DATABASE_URL=#" \
            "$HERE/env/api.env.example" > "$ETC/$env_name/api.env"
        chmod 0600 "$ETC/$env_name/api.env"
        note "$env_name: wrote api.env TEMPLATE — DATABASE_URL is empty and must be filled in"
    fi

    if [ -e "$ETC/$env_name/web.env" ]; then
        note "$env_name: web.env exists, left alone"
    else
        cp "$HERE/env/web.env.example" "$ETC/$env_name/web.env"
        chmod 0640 "$ETC/$env_name/web.env"
        note "$env_name: wrote web.env template"
    fi

    # The ledger's configuration (ADR-0002). Written for every environment,
    # even though none of them loads docker-compose.cashback.yml yet: the
    # overlay reads this file through `env_file`, and a missing env_file fails
    # the whole compose render. Writing it now means enabling cashback on an
    # environment is editing two files that already exist, rather than
    # discovering a third from an error message on a host.
    #
    # Empty is the safe state, exactly as it is for DATABASE_URL: Blnk with no
    # BLNK_DATA_SOURCE_DNS does not start, and an environment whose
    # COMPOSE_FILE does not name the overlay never starts it.
    #
    # TWO files, because the ledger runs as two roles: the database owner
    # migrates, blnk_app serves. Blnk reads the DSN from the same variable
    # either way, so a file per role is the only way to hand each container a
    # different one through `env_file` - and it keeps the owner's credential
    # out of the long-lived server's environment rather than merely unused by
    # it.
    for _blnk_env in blnk.env blnk-migrate.env; do
        if [ -e "$ETC/$env_name/$_blnk_env" ]; then
            note "$env_name: $_blnk_env exists, left alone (it holds a database credential)"
        else
            cp "$HERE/env/$_blnk_env.example" "$ETC/$env_name/$_blnk_env"
            chmod 0600 "$ETC/$env_name/$_blnk_env"
            note "$env_name: wrote $_blnk_env TEMPLATE - BLNK_DATA_SOURCE_DNS is empty"
        fi
    done
    note "$env_name: cashback stays off until both blnk env files are filled in, the blnk_app role is created with scripts/spikes/ledger_schema/bootstrap.sql, and the overlay is added to COMPOSE_FILE"
done

# ---------------------------------------------------------------------------
# 3. Postgres certificates for the environments that run their own Postgres
#
# QA and Staging, on this host. So that both can run APP_ENV=prod like every
# other environment: the api refuses a cleartext DATABASE_URL under
# APP_ENV=prod, and the alternative — running them at APP_ENV=dev — would
# cost JSON logging and, more importantly, the Secure attribute on every
# cookie the app writes. They would become the environments where a cookie
# bug cannot reproduce, which is the opposite of what a staging host is for.
#
# The uid is read out of the image rather than assumed. Postgres refuses to
# start on a key it does not own, and which uid it runs as is an
# implementation detail of the image that has changed before. It is read once
# and reused, because `docker run` per environment is a second of provisioning
# spent proving the same fact twice.
# ---------------------------------------------------------------------------
if [ "$APIVO_HOST_ROLE" = preprod ]; then
    say "Postgres certificates (QA and Staging)"
    pg_uid=""
    for env_name in qa staging; do
        certs="$ETC/$env_name/pg-certs"
        if [ -e "$certs/postgres.key" ]; then
            note "$env_name: already present, left alone"
            continue
        fi
        mkdir -p "$certs"
        openssl req -new -x509 -days 3650 -nodes \
            -out "$certs/postgres.crt" -keyout "$certs/postgres.key" \
            -subj "/CN=apivo-$env_name-postgres" 2>/dev/null
        [ -n "$pg_uid" ] || pg_uid=$(docker run --rm postgres:17-alpine id -u postgres)
        chown "$pg_uid:$pg_uid" "$certs/postgres.key" "$certs/postgres.crt"
        chmod 0600 "$certs/postgres.key"
        chmod 0644 "$certs/postgres.crt"
        note "$env_name: generated, owned by uid $pg_uid (read from the postgres image)"
        note "$env_name: DATABASE_URL should be:"
        note "  postgres://apivo:\$APIVO_PG_PASSWORD@postgres:5432/apivo?sslmode=require"
        note "  (the password is in $ETC/$env_name/stack.env)"
    done
fi

# ---------------------------------------------------------------------------
# 3a. Web origin certificates, so that Astro.url carries the right scheme.
#
# EVERY environment, on every host role, unlike the Postgres certificates
# above: production serves the frontend too, and this is what makes its form
# posts work.
#
# @astrojs/node builds the request URL from the socket and the Host header
# (astro/dist/core/app/node.js):
#
#     const isEncrypted = 'encrypted' in req.socket && req.socket.encrypted;
#     const protocol = isEncrypted ? 'https' : 'http';
#
# No x-forwarded-proto, in either adapter mode. Behind a TLS terminator the
# frontend therefore believes it is on http://, and Astro's own CSRF
# middleware compares that against the browser's https:// Origin with === -
# so it refuses every form post the site makes of itself. Caddy used to
# rewrite the header back to http:// to compensate, which needs the hostname
# at PARSE time and so could never work for the wildcard preview host.
#
# A certificate makes the socket genuinely encrypted, and then nothing has
# to lie. It is self-signed and the only client that sees it - Caddy, one
# hop away on a private network - does not verify it; the same trade this
# deployment already makes for Postgres.
# ---------------------------------------------------------------------------
say "Web origin certificates"
web_uid=""
# Staged in a temporary directory and moved into place only once the pair is
# complete, owned and permissioned.
#
# Writing straight into $certs would make a half-done pair permanent. This
# script runs under `set -e`, openssl writes both files before the uid lookup
# and the chown, and a readiness test of "the key exists" would then be
# satisfied by a key the server cannot read - so every later run would skip
# the repair and every container would fail to start, on a host that reports
# itself provisioned. The move is the last step, so an interrupted run leaves
# the directory as it found it and the next run does the whole job.
write_web_cert() {
    # write_web_cert <destination-directory> <common-name>
    _dest="$1"
    _cn="$2"
    _stage=$(mktemp -d)
    openssl req -new -x509 -days 3650 -nodes \
        -out "$_stage/web.crt" -keyout "$_stage/web.key" \
        -subj "/CN=$_cn" 2>/dev/null
    # Read from the image rather than assumed, for the same reason the
    # Postgres uid is: a base image that renumbers its unprivileged user
    # would otherwise leave a key the server cannot read, and the symptom
    # would be a container that will not start rather than one that says so.
    [ -n "$web_uid" ] || web_uid=$(docker run --rm node:24-slim id -u node)
    chown "$web_uid:$web_uid" "$_stage/web.key" "$_stage/web.crt"
    chmod 0600 "$_stage/web.key"
    chmod 0644 "$_stage/web.crt"
    mkdir -p "$_dest"
    mv "$_stage/web.crt" "$_stage/web.key" "$_dest/"
    rmdir "$_stage"
}

for env_name in $ENVS; do
    certs="$ETC/$env_name/web-certs"
    # BOTH, not just the key: half a pair is not a provisioned host, and the
    # container reads both at startup.
    if [ -e "$certs/web.key" ] && [ -e "$certs/web.crt" ]; then
        note "$env_name: already present, left alone"
        continue
    fi
    write_web_cert "$certs" "apivo-$env_name-web"
    note "$env_name: generated, owned by uid $web_uid (read from the node image)"
done

# ---------------------------------------------------------------------------
# 3b. Previews - one environment per open pull request.
#
# Pre-production only. Production does not run other people's branches.
# ---------------------------------------------------------------------------
if [ "$APIVO_HOST_ROLE" = preprod ]; then
    say "Previews"
    mkdir -p "$ETC/preview/pg-certs"

    # One web certificate for every preview on this host. Self-signed and
    # never verified, so a name per pull request would buy nothing: what it
    # buys is a socket that reports itself encrypted (see 3a).
    if [ -e "$ETC/preview/web-certs/web.key" ] && [ -e "$ETC/preview/web-certs/web.crt" ]; then
        note "preview web certificate already present"
    else
        write_web_cert "$ETC/preview/web-certs" "apivo-preview-web"
        note "generated the preview web certificate"
    fi

    if [ -e "$ETC/preview/stack.env" ]; then
        note "stack.env exists, left alone"
    else
        preview_pg_password=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
        cat > "$ETC/preview/stack.env" <<EOF
# Written by provision.sh. Previews are created and destroyed by
# apivo-previews, from the pr-<n> tags CI publishes and deletes.
APIVO_REGISTRY=$APIVO_REGISTRY
COMPOSE_FILE=$PREFIX/compose/docker-compose.preview.yml
APIVO_PREVIEW_PG_PASSWORD=$preview_pg_password
APIVO_PREVIEW_PG_USER=apivo
# How many previews run at once. Each is two containers plus a database on a
# box that also carries QA and Staging, so this is the line between "reviewers
# can click a link" and "the environments that matter fell over".
APIVO_PREVIEW_MAX=${APIVO_PREVIEW_MAX:-5}
APIVO_PREVIEW_API_MEMORY=384M
APIVO_PREVIEW_WEB_MEMORY=384M
EOF
        chmod 0640 "$ETC/preview/stack.env"
        note "wrote stack.env (cap: ${APIVO_PREVIEW_MAX:-5} concurrent previews)"
    fi

    if [ -e "$ETC/preview/pg-certs/postgres.key" ]; then
        note "preview Postgres certificate already present"
    else
        openssl req -new -x509 -days 3650 -nodes \
            -out "$ETC/preview/pg-certs/postgres.crt" \
            -keyout "$ETC/preview/pg-certs/postgres.key" \
            -subj "/CN=apivo-preview-postgres" 2>/dev/null
        pv_uid=$(docker run --rm postgres:17-alpine id -u postgres)
        chown "$pv_uid:$pv_uid" "$ETC/preview/pg-certs/postgres.key" "$ETC/preview/pg-certs/postgres.crt"
        chmod 0600 "$ETC/preview/pg-certs/postgres.key"
        chmod 0644 "$ETC/preview/pg-certs/postgres.crt"
        note "generated the preview Postgres certificate"
    fi

    docker network create apivo-preview-edge >/dev/null 2>&1 || true
    docker network create apivo-preview-data >/dev/null 2>&1 || true
    note "preview networks ready"
fi

# ---------------------------------------------------------------------------
# 4. The edge
# ---------------------------------------------------------------------------
say "Configuring the edge"

mkdir -p "$ETC/edge/certs"
cp "$PREFIX/caddy/$CADDYFILE" "$ETC/edge/Caddyfile"
cp "$PREFIX/caddy/snippets.caddy" "$ETC/edge/snippets.caddy"

cat > "$ETC/edge/stack.env" <<EOF
# Written by provision.sh.
APIVO_HOST_ROLE=$APIVO_HOST_ROLE
COMPOSE_FILE=$PREFIX/compose/docker-compose.edge.yml:$PREFIX/compose/$EDGE_OVERLAY
APIVO_EDGE_CPUS=1.0
APIVO_EDGE_MEMORY=256M
APIVO_LOG_MAX_SIZE=10m
APIVO_LOG_MAX_FILE=3
EOF
chmod 0640 "$ETC/edge/stack.env"

if [ "$APIVO_HOST_ROLE" = preprod ]; then
    cat > "$ETC/edge/caddy.env" <<EOF
APIVO_QA_HOST=$APIVO_QA_HOST
APIVO_STAGING_HOST=$APIVO_STAGING_HOST
# The wildcard previews are served under. The origin certificate must cover
# *.this or Caddy will not start.
APIVO_PREVIEW_DOMAIN=${APIVO_PREVIEW_DOMAIN:-$APIVO_QA_HOST}
EOF
else
    cat > "$ETC/edge/caddy.env" <<EOF
APIVO_PROD_HOST=$APIVO_PROD_HOST
APIVO_PROD_ALT_HOST=$APIVO_PROD_ALT_HOST
EOF
fi
chmod 0640 "$ETC/edge/caddy.env"
note "installed $CADDYFILE and its hostnames"

# The Cloudflare Origin Certificate: trusted by Cloudflare and by nothing
# else, which is exactly right for a certificate only ever presented to
# Cloudflare. Set the zone to Full (strict) so it is actually verified.
#
# ONE PER CLOUDFLARE ZONE, because the Cloudflare dashboard cannot issue a
# certificate spanning zones — only its API can. A zone's certificate covers
# its apex and its first-level wildcard by default, so `origin` covers the QA
# hostname AND the preview wildcard that lives beside it, while staging's
# separate zone needs `origin-staging`. A production host has one zone and
# therefore only ever needs `origin`.
install_origin_pair() {
    _base=$1
    _cert=$2
    _key=$3
    _what=$4
    _hint=$5
    if [ -n "$_cert" ] && [ -n "$_key" ]; then
        [ -r "$_cert" ] || die "$_hint=$_cert cannot be read"
        [ -r "$_key" ] || die "the key for $_what ($_key) cannot be read"
        install -m 0644 "$_cert" "$ETC/edge/certs/$_base.pem"
        install -m 0640 "$_key" "$ETC/edge/certs/$_base.key"
        note "installed $_what"
    # BOTH files, not just the certificate. Caddy names the pair and will not
    # start HTTPS without either, so treating a lone .pem as "already
    # present" reports a provisioned host that cannot serve — and it reports
    # it on the re-run someone does precisely to check the first run worked.
    elif [ -e "$ETC/edge/certs/$_base.pem" ] && [ -e "$ETC/edge/certs/$_base.key" ]; then
        note "$_what already present, left alone"
    elif [ -e "$ETC/edge/certs/$_base.pem" ] || [ -e "$ETC/edge/certs/$_base.key" ]; then
        die "$_what at $ETC/edge/certs/ is HALF installed - Caddy needs both $_base.pem and $_base.key and will not start with one. Remove the stray file and re-run with both halves set."
    else
        note "MISSING: $_what. Caddy will not start until it is installed:"
        note "  Cloudflare dashboard -> the right zone -> SSL/TLS -> Origin Server"
        note "  -> Create Certificate, then copy the pair to"
        note "  $ETC/edge/certs/$_base.pem and $_base.key"
    fi
}

install_origin_pair origin \
    "${APIVO_ORIGIN_CERT:-}" "${APIVO_ORIGIN_KEY:-}" \
    "the origin certificate" APIVO_ORIGIN_CERT

# Staging is its own Cloudflare zone, so its own certificate. Only the
# pre-production host serves it.
if [ "$APIVO_HOST_ROLE" = preprod ]; then
    install_origin_pair origin-staging \
        "${APIVO_STAGING_ORIGIN_CERT:-}" "${APIVO_STAGING_ORIGIN_KEY:-}" \
        "the staging origin certificate" APIVO_STAGING_ORIGIN_CERT
fi

# ---------------------------------------------------------------------------
# 5. The registry credential
#
# Read-only. This host pulls images and can do nothing else to the registry —
# it cannot publish, cannot move a channel, and cannot delete anything. The
# deployment mechanism is one-directional by construction: CI writes to the
# registry, hosts read from it, and neither can reach the other.
# ---------------------------------------------------------------------------
say "Registry credential"
if [ -n "${GHCR_TOKEN:-}" ]; then
    printf '%s' "$GHCR_TOKEN" | docker login ghcr.io -u "${GHCR_USER:?set GHCR_USER alongside GHCR_TOKEN}" --password-stdin
    note "logged in to ghcr.io as $GHCR_USER"
elif docker pull -q "$APIVO_REGISTRY/api:qa" >/dev/null 2>&1; then
    note "already able to pull from $APIVO_REGISTRY"
else
    note "NOT LOGGED IN. The reconciler cannot resolve a channel until it is:"
    note "  echo <PAT with read:packages> | docker login ghcr.io -u <user> --password-stdin"
fi

# ---------------------------------------------------------------------------
# 6. systemd
# ---------------------------------------------------------------------------
say "Installing systemd units"

install -m 0644 "$HERE/systemd/apivo-reconcile@.service" "$UNITS/apivo-reconcile@.service"
install -m 0644 "$HERE/systemd/apivo-reconcile@.timer" "$UNITS/apivo-reconcile@.timer"
install -m 0644 "$HERE/systemd/apivo-edge.service" "$UNITS/apivo-edge.service"
if [ "$APIVO_HOST_ROLE" = preprod ]; then
    install -m 0644 "$HERE/systemd/apivo-previews.service" "$UNITS/apivo-previews.service"
    install -m 0644 "$HERE/systemd/apivo-previews.timer" "$UNITS/apivo-previews.timer"
fi
systemctl daemon-reload

for env_name in $ENVS; do
    systemctl enable --now "apivo-reconcile@$env_name.timer"
    note "apivo-reconcile@$env_name.timer enabled — $env_name reconciles every minute"
done
systemctl enable apivo-edge.service
note "apivo-edge.service enabled"

# An already-running edge is RESTARTED, because provisioning has just
# installed a Caddy configuration and a configuration that is not the one
# running is the disagreement nothing reports.
#
# apivo-edge deliberately has no timer: the proxy's config is this
# repository's and changes when a person changes it, and a proxy that rolled
# itself forward could take every environment on the host down at once. That
# argument is about AUTOMATIC rollout. This is a person running provision.sh,
# and applying what it just wrote is the same act.
#
# Only when active. On a first provision the edge cannot start yet - the
# environment networks belong to the application stacks and do not exist
# until each has reconciled once - so a blind restart would fail the run at
# the last step. The summary at the end names the manual start for that case.
#
# The frontend upstream became TLS in this change, so there IS a window on an
# existing host: between this restart and each environment reconciling onto
# an image that serves TLS, the proxy speaks TLS to a container that does not
# yet. Named rather than left to be discovered, and closed by reconciling
# immediately - which the note below asks for.
if systemctl is-active --quiet apivo-edge.service; then
    systemctl restart apivo-edge.service
    note "apivo-edge restarted, so the configuration just installed is the one running"
    note "the frontend is DOWN until each environment reconciles onto a TLS-serving image; do that now:"
    for env_name in $ENVS; do
        note "  systemctl start apivo-reconcile@$env_name.service"
    done
else
    note "apivo-edge is not running; start it once the environments have reconciled"
fi

if [ "$APIVO_HOST_ROLE" = preprod ]; then
    # The shared preview database comes up now: apivo-previews creates a
    # database inside it per pull request and cannot do that against a
    # Postgres that is not running.
    APIVO_PREVIEW_PG_PASSWORD=$(sed -n 's/^APIVO_PREVIEW_PG_PASSWORD=//p' "$ETC/preview/stack.env" | head -n 1)
    export APIVO_PREVIEW_PG_PASSWORD
    APIVO_ETC="$ETC"
    export APIVO_ETC
    docker compose -f "$PREFIX/compose/docker-compose.preview-db.yml" up -d --wait --wait-timeout 120 >/dev/null 2>&1 ||
        note "NOTE: the shared preview Postgres did not come up; previews will not start until it does"
    systemctl enable --now apivo-previews.timer
    note "apivo-previews.timer enabled - open pull requests appear within a minute"
fi

# The edge declares each environment's network `external: true`, so the
# network must exist before Caddy can start. Creating them here rather than
# relying on the application stacks to do it first removes a real reboot
# hazard: apivo-edge.service and the reconcilers all start at boot, and if
# Caddy wins the race it fails on a missing network and stays failed.
# `docker network create` on an existing network is an error, not a
# surprise - ignore it.
for env_name in $ENVS; do
    docker network create "apivo-$env_name-edge" >/dev/null 2>&1 || true
    note "network apivo-$env_name-edge ready"
done

# ---------------------------------------------------------------------------
# 7. The firewall — opt-in, because getting it wrong locks you out
#
# Only Cloudflare reaches this origin. Without this, anyone who learns the
# VPS address talks straight to Caddy, and every protection configured at the
# edge — the WAF, the rate limits, the bot rules — is one DNS lookup away
# from being irrelevant.
# ---------------------------------------------------------------------------
if [ "$APIVO_CONFIGURE_FIREWALL" = yes ]; then
    say "Firewall (Cloudflare ranges only)"
    command -v ufw >/dev/null 2>&1 || die "ufw is not installed (apt-get install -y ufw)"

    # SSH FIRST, ALWAYS, and before anything is enabled. The order of these
    # two blocks is the difference between a configured host and a host
    # nobody can log in to again.
    ufw allow "$APIVO_SSH_PORT/tcp" comment 'ssh' >/dev/null
    note "allowed ssh on $APIVO_SSH_PORT/tcp"

    # Replace previous rules rather than stacking duplicates on every re-run.
    ufw status numbered 2>/dev/null | grep -F 'apivo-edge' |
        sed -n 's/^\[ *\([0-9]*\).*/\1/p' | sort -rn |
        while read -r n; do ufw --force delete "$n" >/dev/null; done

    cf_v4=""
    for family in 4 6; do
        if ! ranges=$(curl -fsS --max-time 20 "https://www.cloudflare.com/ips-v$family"); then
            die "could not fetch Cloudflare's IPv$family ranges; no firewall rules were changed for that family, and the host may now admit ssh only"
        fi
        [ "$family" = 4 ] && cf_v4="$ranges"
        for cidr in $ranges; do
            ufw allow from "$cidr" to any port 443 proto tcp comment 'apivo-edge' >/dev/null
        done
        note "allowed 443/tcp from Cloudflare's IPv$family ranges"
    done

    ufw default deny incoming >/dev/null
    ufw --force enable >/dev/null
    note "ufw enabled: ssh, and 443 from Cloudflare only"

    # ufw ALONE DOES NOT COVER THIS EDGE, and believing otherwise is the
    # dangerous part. Docker publishes a container port by writing its own
    # DNAT and FORWARD rules, which are consulted before ufw's INPUT chain
    # ever sees the packet - so `ufw deny 443` leaves Caddy wide open and
    # the ufw status output says the opposite.
    #
    # DOCKER-USER is the chain Docker documents for exactly this: it is
    # consulted first on forwarded traffic and Docker never flushes it. The
    # rules below are rebuilt from scratch each run, and they restrict only
    # 443 - everything else RETURNs untouched, so nothing here can cut off
    # ssh or inter-container traffic.
    #
    # THE FIRST RULE IS NOT OPTIONAL, and leaving it out cost an afternoon on
    # the first host provisioned. DOCKER-USER hangs off FORWARD, which carries
    # container traffic in BOTH directions - so a bare `--dport 443 -j DROP`
    # matches a container dialling OUT to port 443 just as readily as the
    # internet dialling in. Every rule below it is written for inbound
    # (`-s <cloudflare>`), so nothing rescues the outbound packet.
    #
    # The symptom is nasty because nothing says "firewall": the api fetches
    # its JWKS at boot, that fetch times out rather than being refused, and
    # NewVerifier fails construction, so the api crash-loops and the site
    # answers 502. Meanwhile the same URL fetched from the HOST works
    # perfectly, because host traffic never traverses FORWARD. Nothing needed
    # outbound HTTPS from a container until auth was configured, so this
    # stayed invisible for as long as the box had no auth.
    #
    # Matching on the interface rather than on the source is what fixes it:
    # anything not arriving from the internet is container-originated and is
    # returned before the DROP is reached. Inbound still arrives on $ext_if
    # and still meets the Cloudflare-only rules, so nothing is loosened.
    ext_if=$(ip route show default 2>/dev/null | awk '{print $5; exit}')
    [ -n "$ext_if" ] || die "cannot determine the default-route interface, so the firewall cannot tell inbound traffic from container-originated traffic. Set it by hand and re-run, or use APIVO_CONFIGURE_FIREWALL=no and manage the firewall elsewhere."

    iptables -N DOCKER-USER 2>/dev/null || true
    iptables -F DOCKER-USER
    iptables -A DOCKER-USER ! -i "$ext_if" -j RETURN
    for cidr in $cf_v4; do
        iptables -A DOCKER-USER -i "$ext_if" -s "$cidr" -p tcp --dport 443 -j RETURN
    done
    iptables -A DOCKER-USER -i "$ext_if" -p tcp --dport 443 -j DROP
    iptables -A DOCKER-USER -j RETURN
    note "DOCKER-USER rules installed on $ext_if (ufw alone does not cover published container ports)"

    if command -v netfilter-persistent >/dev/null 2>&1; then
        netfilter-persistent save >/dev/null 2>&1 || true
        note "iptables rules persisted across reboot"
    else
        note "NOTE: install iptables-persistent, or the DOCKER-USER rules are lost on reboot."
    fi
    note "Better still, put a Hetzner Cloud Firewall in front of this host: it"
    note "sits outside the machine, so no container runtime can route around it."
else
    say "Firewall (declined)"
    note "APIVO_CONFIGURE_FIREWALL=no, so nothing was changed here."
    note "Something else must restrict inbound 443 to Cloudflare's ranges — a"
    note "Hetzner Cloud Firewall, or rules already on this host. If nothing"
    note "does, the origin is reachable directly and Cloudflare's WAF, rate"
    note "limits and bot rules are protecting a path attackers need not take."
fi

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------
say "Provisioned"
cat <<EOF

This host is configured. What is left is the part a script must not do:

EOF
for env_name in $ENVS; do
    printf '  %s\n' "$ETC/$env_name/api.env   — set DATABASE_URL (and JWKS_URL once auth is wired)"
    printf '  %s\n' "$ETC/$env_name/web.env   — set PUBLIC_SUPABASE_URL and PUBLIC_SUPABASE_ANON_KEY"
done
cat <<EOF

Then:

  systemctl start apivo-reconcile@${ENVS%% *}   # first rollout
  systemctl start apivo-edge                    # Caddy, once a network exists
  apivoctl status                               # what is running

And record the public URLs in deploy/hetzner/environments.env, in a pull
request: that file is what the release pipeline probes and what
scripts/env_status.sh reports on. An environment nobody wrote down is an
environment no release can verify.
EOF
