#!/bin/sh
# Turn a fresh Hetzner VPS into a deployment host. Run once, as root, from a
# checkout of this repository. Safe to re-run: every step checks before it
# acts, and NOTHING here ever overwrites a file that already holds a secret.
#
#   APIVO_HOST_ROLE=preprod \
#   APIVO_QA_HOST=qa.example.com APIVO_STAGING_HOST=staging.example.com \
#   APIVO_ORIGIN_CERT=/root/origin.pem APIVO_ORIGIN_KEY=/root/origin.key \
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
    [ -n "${APIVO_QA_HOST:-}" ] || die "set APIVO_QA_HOST (e.g. qa.example.com)"
    [ -n "${APIVO_STAGING_HOST:-}" ] || die "set APIVO_STAGING_HOST"
    ;;
prod)
    ENVS="prod"
    CADDYFILE=Caddyfile.prod
    EDGE_OVERLAY=docker-compose.edge.prod.yml
    [ -n "${APIVO_PROD_HOST:-}" ] || die "set APIVO_PROD_HOST (e.g. example.com)"
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
install -m 0755 "$HERE/bin/apivoctl" "$PREFIX/bin/apivoctl"
ln -sf "$PREFIX/bin/apivoctl" /usr/local/bin/apivoctl
cp "$HERE/compose/"*.yml "$PREFIX/compose/"
cp "$HERE/caddy/"* "$PREFIX/caddy/"
note "installed apivo-reconcile, apivoctl (on PATH), compose files, Caddy config"

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

    if [ "$env_name" = qa ]; then
        compose_files="$PREFIX/compose/docker-compose.yml:$PREFIX/compose/docker-compose.local-db.yml"
    else
        compose_files="$PREFIX/compose/docker-compose.yml"
    fi

    if [ -e "$ETC/$env_name/stack.env" ]; then
        note "$env_name: stack.env exists, left alone"
    else
        # QA's Postgres password. Generated, never typed: this is the one
        # credential on the host that nobody needs to know.
        pg_password=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
        cat > "$ETC/$env_name/stack.env" <<EOF
# Written by provision.sh. Structural configuration only — no application
# secrets (those live in api.env). Documented in
# deploy/hetzner/env/stack.env.example.
APIVO_ENV=$env_name
APIVO_CHANNEL=$env_name
APIVO_REGISTRY=$APIVO_REGISTRY
COMPOSE_FILE=$compose_files
APIVO_PG_PASSWORD=$pg_password
APIVO_PG_DB=apivo
APIVO_PG_USER=apivo
APIVO_API_CPUS=1.0
APIVO_API_MEMORY=768M
APIVO_WEB_CPUS=1.0
APIVO_WEB_MEMORY=512M
APIVO_PG_CPUS=1.0
APIVO_PG_MEMORY=768M
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
done

# ---------------------------------------------------------------------------
# 3. QA's Postgres certificate
#
# So that QA can run APP_ENV=prod like every other environment. The api
# refuses a cleartext DATABASE_URL under APP_ENV=prod, and the alternative —
# running QA at APP_ENV=dev — would cost JSON logging and, more importantly,
# the Secure attribute on every cookie the app writes. QA would become the
# one environment where a cookie bug cannot reproduce.
#
# The uid is read out of the image rather than assumed. Postgres refuses to
# start on a key it does not own, and which uid it runs as is an
# implementation detail of the image that has changed before.
# ---------------------------------------------------------------------------
if [ "$APIVO_HOST_ROLE" = preprod ]; then
    say "QA Postgres certificate"
    certs="$ETC/qa/pg-certs"
    if [ -e "$certs/postgres.key" ]; then
        note "already present, left alone"
    else
        mkdir -p "$certs"
        openssl req -new -x509 -days 3650 -nodes \
            -out "$certs/postgres.crt" -keyout "$certs/postgres.key" \
            -subj "/CN=apivo-qa-postgres" 2>/dev/null
        pg_uid=$(docker run --rm postgres:17-alpine id -u postgres)
        chown "$pg_uid:$pg_uid" "$certs/postgres.key" "$certs/postgres.crt"
        chmod 0600 "$certs/postgres.key"
        chmod 0644 "$certs/postgres.crt"
        note "generated, owned by uid $pg_uid (read from the postgres image)"
        note "QA's DATABASE_URL should be:"
        note "  postgres://apivo:\$APIVO_PG_PASSWORD@postgres:5432/apivo?sslmode=require"
        note "  (the password is in $ETC/qa/stack.env)"
    fi
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
if [ -n "${APIVO_ORIGIN_CERT:-}" ] && [ -n "${APIVO_ORIGIN_KEY:-}" ]; then
    [ -r "$APIVO_ORIGIN_CERT" ] || die "APIVO_ORIGIN_CERT=$APIVO_ORIGIN_CERT cannot be read"
    [ -r "$APIVO_ORIGIN_KEY" ] || die "APIVO_ORIGIN_KEY=$APIVO_ORIGIN_KEY cannot be read"
    install -m 0644 "$APIVO_ORIGIN_CERT" "$ETC/edge/certs/origin.pem"
    install -m 0640 "$APIVO_ORIGIN_KEY" "$ETC/edge/certs/origin.key"
    note "installed the origin certificate"
# BOTH files, not just the certificate. Caddy names the pair and will not
# start HTTPS without either, so treating a lone origin.pem as "already
# present" reports a provisioned host that cannot serve — and it reports it
# on the re-run someone does precisely to check the first run worked.
elif [ -e "$ETC/edge/certs/origin.pem" ] && [ -e "$ETC/edge/certs/origin.key" ]; then
    note "origin certificate already present, left alone"
elif [ -e "$ETC/edge/certs/origin.pem" ] || [ -e "$ETC/edge/certs/origin.key" ]; then
    die "the origin certificate at $ETC/edge/certs/ is HALF installed - Caddy needs both origin.pem and origin.key and will not start with one. Remove the stray file and re-run with APIVO_ORIGIN_CERT and APIVO_ORIGIN_KEY set to both halves."
else
    note "NO ORIGIN CERTIFICATE. Caddy will not start until one is installed:"
    note "  Cloudflare dashboard -> SSL/TLS -> Origin Server -> Create Certificate"
    note "  then copy them to $ETC/edge/certs/origin.pem and origin.key"
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
systemctl daemon-reload

for env_name in $ENVS; do
    systemctl enable --now "apivo-reconcile@$env_name.timer"
    note "apivo-reconcile@$env_name.timer enabled — $env_name reconciles every minute"
done
systemctl enable apivo-edge.service
note "apivo-edge.service enabled"

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
    iptables -N DOCKER-USER 2>/dev/null || true
    iptables -F DOCKER-USER
    for cidr in $cf_v4; do
        iptables -A DOCKER-USER -s "$cidr" -p tcp --dport 443 -j RETURN
    done
    iptables -A DOCKER-USER -p tcp --dport 443 -j DROP
    iptables -A DOCKER-USER -j RETURN
    note "DOCKER-USER rules installed (ufw alone does not cover published container ports)"

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
