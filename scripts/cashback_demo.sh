#!/bin/sh
# The whole cashback product, running locally, in one command.
#
# It exists because the product was invisible for months while being built.
# Everything it needs was already in the repository - a fixture network, a
# Postgres ledger, four member surfaces and the operator queues - and putting
# them together took an afternoon of archaeology every time somebody asked
# "can I see it".
#
# WHAT IT IS NOT. It is not a deployment, and it is not a test. The scenarios
# under internal/cashback/scenarios/ are what prove the money loop; this
# proves nothing. It puts the product in front of your eyes, which is a
# different job and one nothing else here does.
#
# NO DOCKER REQUIRED. The ledger is LEDGER_DRIVER=postgres - the in-repository
# ledger ADR-0002 keeps as its exit route from Blnk - so there is no Blnk
# container and no Redis. The network is the fixture adapter, which needs no
# credentials at all. What you need is a Postgres this can reach.
#
# THE CONFIGURATION IS THE DEPLOYED ONE, deliberately. Every key below is one
# a real environment sets, including the house-account names and the payout
# threshold that APP_ENV=prod requires. Only APP_ENV differs. So a demo that
# runs here is a rehearsal of the environment file rather than a separate
# arrangement that happens to work.
#
# THE ONE THING IT ADDS is an auth provider, because there is no anonymous
# cashback surface (FR-023) and a demo that cannot issue a token cannot show
# the product at all - only an API answering 404 beside a web app rendering
# its own fixtures. scripts/demo/auth.go stands in for Supabase: it publishes
# a JWKS, signs a token for a member and one for an operator, and writes the
# account rows that `seed cashback` deliberately refuses to invent.
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

DATABASE_URL=${DATABASE_URL:-postgres://apivo:apivo@localhost:5432/apivo?sslmode=disable}
API_PORT=${API_PORT:-8080}
WEB_PORT=${WEB_PORT:-4321}
AUTH_PORT=${AUTH_PORT:-8089}

# Fixed rather than generated, so a second run finds the member it opted in
# last time instead of leaving a trail of one-use accounts behind it. Version
# 4 UUIDs by construction, because account.id is a uuid and the auth provider
# this stands in for issues those.
MEMBER_ID=${MEMBER_ID:-1e11e5f0-0000-4000-8000-00000000cab1}
OPERATOR_ID=${OPERATOR_ID:-1e11e5f0-0000-4000-8000-000000000095}

say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
die() { printf '\n%s\n\n' "$*" >&2; exit 1; }

# show prints one API call and its answer, which is the whole point of the
# walk below: a reader should be able to repeat any line of it by hand.
show() {
    printf '\n  \033[1m%s\033[0m\n' "$1"
    shift
    printf '  %s\n' "$@" | sed 's/^/  /'
}

command -v go >/dev/null 2>&1 || die "cashback-demo: no go on PATH"
command -v curl >/dev/null 2>&1 || die "cashback-demo: no curl on PATH"

# Reachability is checked BEFORE pool_max_conns is added below, because that
# parameter is pgx's and libpq does not know it: psql refuses the whole URL
# over it and the failure reads as an unreachable database. Caught by running
# this script, which is the only way that class of bug is ever caught.
if command -v psql >/dev/null 2>&1; then
    psql "$DATABASE_URL" -c 'select 1' >/dev/null 2>&1 ||
        die "cashback-demo: cannot reach $DATABASE_URL

Start one, then run this again. With Docker:   make db-up
Without:                                       pg_ctlcluster 16 main start"
fi

# The scheduler takes two connections per job plus two reserved, and cashback
# registers six jobs for one network - see the arithmetic in cmd/apivo/main.go.
# pgx defaults MaxConns to max(4, NumCPU), so a URL without this refuses to
# start with a message about 14. Added rather than demanded of the caller: the
# demo's job is to run, not to teach the pool arithmetic before it will.
case $DATABASE_URL in
    *pool_max_conns=*) ;;
    *\?*) DATABASE_URL="$DATABASE_URL&pool_max_conns=20" ;;
    *)    DATABASE_URL="$DATABASE_URL?pool_max_conns=20" ;;
esac
export DATABASE_URL

# Every key a deployed environment sets, so this run rehearses that file.
# APP_ENV is the one difference: dev here, prod on every real host.
export APP_ENV=dev
export CASHBACK_ENABLED=true
export LEDGER_DRIVER=postgres
export NETWORKS=fixture
export NETWORK_FIXTURE_ACCOUNT_ID="${NETWORK_FIXTURE_ACCOUNT_ID:-fixture-publisher}"
export NETWORK_FIXTURE_SOURCE_LANGUAGE="${NETWORK_FIXTURE_SOURCE_LANGUAGE:-en}"
export BRAND_DIR="${BRAND_DIR:-internal/platform/brand/testdata/fixture}"
# House accounts are LABELS an operator chooses, not rows to create: they are
# where members' money meets the business's, and the ledger opens them on
# first use. Three distinct names, because two purposes sharing one would
# merge two figures the design keeps apart - which parseCashback refuses.
export HOUSE_ACCOUNT_ROUNDING="${HOUSE_ACCOUNT_ROUNDING:-house:rounding}"
export HOUSE_ACCOUNT_CLAWBACK="${HOUSE_ACCOUNT_CLAWBACK:-house:clawback}"
export HOUSE_ACCOUNT_NETWORK_RECEIVABLE="${HOUSE_ACCOUNT_NETWORK_RECEIVABLE:-house:network-receivable}"
export PAYOUT_THRESHOLD_MINOR="${PAYOUT_THRESHOLD_MINOR:-1000}"
export PAYOUT_THRESHOLD_CURRENCY="${PAYOUT_THRESHOLD_CURRENCY:-EUR}"

TMP=$(mktemp -d)
API_PID=""
AUTH_PID=""
WEB_STARTED=""
# Children as well as the process itself, because a leaked server holding a
# port is what the guard further down had to be written to survive, and not
# leaking one in the first place is better than detecting it next time.
stop() {
    [ -n "$1" ] || return 0
    pkill -P "$1" 2>/dev/null || true
    kill "$1" 2>/dev/null || true
}
cleanup() {
    if [ -n "$WEB_STARTED" ]; then
        (cd web && npx astro dev stop >/dev/null 2>&1) || true
    fi
    stop "$API_PID"
    stop "$AUTH_PID"
    rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

# waitfor polls a URL until it answers or the attempts run out. Every start
# below needs it and none of them can be raced.
waitfor() {
    i=0
    while [ "$i" -lt "$2" ]; do
        curl -sf -o /dev/null "$1" 2>/dev/null && return 0
        i=$((i + 1))
        sleep 0.5
    done
    return 1
}

# REFUSE A PORT SOMETHING ELSE IS ON, rather than waiting until it answers.
# Every start below waits for its own URL to respond, and a stranger already
# listening answers immediately - so the demo would attach to somebody else's
# process and then report on it. That is not hypothetical: a leftover api from
# an earlier run held :8080, this script waited for it, found no cashback
# routes on it, and blamed the feature flag. Exit 7 is curl's "could not
# connect", which is the only answer that means the port is ours to take.
portfree() {
    code=0
    curl -s -o /dev/null --max-time 2 "http://127.0.0.1:$1/" 2>/dev/null || code=$?
    [ "$code" -eq 7 ]
}
for port in "$API_PORT" "$WEB_PORT" "$AUTH_PORT"; do
    portfree "$port" || die "cashback-demo: something is already listening on 127.0.0.1:$port

This demo would attach to it and report on it as if it were its own. Stop it,
or point this run somewhere else:

    API_PORT=18080 WEB_PORT=14321 AUTH_PORT=18089 sh scripts/cashback_demo.sh"
done

say "1/6  building"
go build -o "$TMP/apivo" ./cmd/apivo
# Built rather than `go run`, which forks: killing `go run` leaves the server
# it started holding the port, and the next run then waits for a process this
# one thought it had stopped. A binary is the process we hold the pid of.
go build -o "$TMP/demoauth" scripts/demo/auth.go

say "2/6  migrating"
# The api migrates on boot and everything below needs the schema, so a
# throwaway boot comes first. Its only job is to run Migrate; it is stopped
# as soon as it is serving. JWKS_URL is deliberately still unset here, which
# is why this boot logs that the authenticated surfaces are unmounted - true
# of this boot, and irrelevant to the one in step 5.
"$TMP/apivo" > "$TMP/migrate.log" 2>&1 &
MIGRATE_PID=$!
waitfor "http://localhost:$API_PORT/healthz" 60 || { tail -20 "$TMP/migrate.log"; die "cashback-demo: the api did not come up to migrate"; }
kill "$MIGRATE_PID" 2>/dev/null || true
wait "$MIGRATE_PID" 2>/dev/null || true

say "3/6  standing in for the auth provider on :$AUTH_PORT"
"$TMP/demoauth" \
    -addr "127.0.0.1:$AUTH_PORT" \
    -token-dir "$TMP" \
    -account "$MEMBER_ID=reader" \
    -account "$OPERATOR_ID=operator" > "$TMP/auth.log" 2>&1 &
AUTH_PID=$!
export JWKS_URL="http://127.0.0.1:$AUTH_PORT/jwks.json"
waitfor "$JWKS_URL" 60 || { cat "$TMP/auth.log"; die "cashback-demo: the auth stand-in did not come up"; }
sed 's/^/     /' "$TMP/auth.log"
MEMBER_TOKEN=$(cat "$TMP/reader.token")

say "4/6  seeding the fixture catalogue and opting the member in"
"$TMP/apivo" seed cashback "$MEMBER_ID" | tee "$TMP/seed.log" | sed 's/^/     /'
# The offer id is read back out of the seed's own report rather than queried
# for: the line exists to be acted on, and a second source for the same fact
# is a second thing that can disagree with it.
OFFER_ID=$(sed -n 's/^live offer *\([0-9a-f-]*\) .*/\1/p' "$TMP/seed.log")
MERCHANT=$(sed -n 's/^live offer *[0-9a-f-]* *(\([^)]*\)).*/\1/p' "$TMP/seed.log")

say "5/6  starting the api on :$API_PORT"
"$TMP/apivo" > "$TMP/api.log" 2>&1 &
API_PID=$!
waitfor "http://localhost:$API_PORT/healthz" 60 ||
    { tail -20 "$TMP/api.log"; die "cashback-demo: the api did not come up"; }

# WHETHER THE CASHBACK ROUTES ARE MOUNTED, AND IF NOT, WHICH OF THE TWO GATES
# CLOSED THEM. This block exists because that question cost days: a 404 on a
# cashback route has two unrelated causes and the difference is invisible from
# outside.
#
#   Gate 1  JWKS_URL. Every cashback route - member AND operator - is built
#           inside newAuthenticatedRoutes (cmd/apivo/main.go), which only runs
#           when a JWKS endpoint is configured. No JWKS, no routes, whatever
#           else is set. Step 3 above is what satisfies it.
#   Gate 2  Mountable() = CASHBACK_ENABLED and every configured network usable.
#
# A deployment can pass either and fail the other, and both look like 404.
# An anonymous request separates a mounted module from an absent one: the
# member gate answers 401 (a bearer token is required), never 404.
CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:$API_PORT/api/v1/cashback/wallet")
case $CODE in
    401) printf '     mounted (an anonymous wallet request answers 401, as a gated surface should)\n' ;;
    404)
        if [ -z "${JWKS_URL:-}" ]; then
            die "cashback-demo: NOT mounted - gate 1, JWKS_URL is unset"
        fi
        # The log goes to stderr here rather than being left in $TMP for
        # somebody to find, because the trap removes $TMP on the way out and
        # a path in a message is no use once it is gone.
        grep -iE 'NOT MOUNTED|cannot poll|UNMOUNTED|ERROR' "$TMP/api.log" >&2 || tail -30 "$TMP/api.log" >&2
        die "cashback-demo: NOT mounted - gate 2, the product is off or a network cannot poll (see above)"
        ;;
    *)
        tail -30 "$TMP/api.log" >&2
        die "cashback-demo: the wallet answered $CODE, which is neither 401 nor 404 (see above)"
        ;;
esac

say "6/6  walking the product as the member"
# Everything below is the real API against the real database: no fixtures,
# no stubs, no test harness. Each call is one a member's browser makes.
AUTH="Authorization: Bearer $MEMBER_TOKEN"
GET() { curl -s -H "$AUTH" "http://localhost:$API_PORT$1"; }

show "GET /api/v1/cashback/participation" "$(GET /api/v1/cashback/participation | jq -c . 2>/dev/null || GET /api/v1/cashback/participation)"
show "GET /api/v1/cashback/wallet" "$(GET /api/v1/cashback/wallet | jq -c . 2>/dev/null || GET /api/v1/cashback/wallet)"
if [ -n "$MERCHANT" ]; then
    show "GET /api/v1/cashback/merchants/$MERCHANT" \
        "$(GET "/api/v1/cashback/merchants/$MERCHANT" | jq -c '{slug, name, rates: (.rates // [] | length)}' 2>/dev/null ||
           GET "/api/v1/cashback/merchants/$MERCHANT")"
fi
if [ -n "$OFFER_ID" ]; then
    # The click-out is the moment the product becomes money: it writes the
    # click, seals its evidence and hands back the tracking URL the member is
    # sent to. Everything the network later reports is matched back to the
    # click_ref printed here.
    CLICK=$(curl -s -H "$AUTH" -H 'Content-Type: application/json' \
        -d "{\"offer_id\":\"$OFFER_ID\"}" \
        "http://localhost:$API_PORT/api/v1/cashback/clickouts")
    show "POST /api/v1/cashback/clickouts  {\"offer_id\":\"$OFFER_ID\"}" \
        "$(printf '%s' "$CLICK" | jq -c . 2>/dev/null || printf '%s' "$CLICK")"
fi
show "GET /api/v1/cashback/wallet/entries" "$(GET /api/v1/cashback/wallet/entries | jq -c . 2>/dev/null || GET /api/v1/cashback/wallet/entries)"

say "and the web app on :$WEB_PORT"
[ -d web/node_modules ] || (cd web && npm ci)
# `astro dev` DAEMONIZES: it spawns a detached server, records it in a lock
# file under web/.astro/, and returns. So it is not backgrounded here and no
# pid is kept for it - both would be wrong. `astro dev stop`, which reads
# that lock file, is the only thing that stops it, and the trap below calls
# it. Killing the process this command started would leave the server it
# spawned holding the port, which is how this script came to need the guard
# at the top.
(cd web && npx astro dev --port "$WEB_PORT" > "$TMP/web.log" 2>&1) || true
WEB_STARTED=yes
waitfor "http://localhost:$WEB_PORT/" 120 || die "cashback-demo: the web app is not on :$WEB_PORT

Astro allows one dev server per project, whatever port is asked for, so a
server already running for web/ would have been reported instead of ours.
Stop it and run this again:

    cd web && npx astro dev stop

$(cat "$TMP/web.log")"

cat <<EOF

  Cashback is running.

    catalogue   http://localhost:$WEB_PORT/el/munich/cashback
    a retailer  http://localhost:$WEB_PORT/el/munich/cashback/agora
    the wallet  http://localhost:$WEB_PORT/el/munich/cashback/wallet
    withdrawal  http://localhost:$WEB_PORT/el/munich/cashback/withdraw

  Those four pages render from the web app's own fixtures, because signing a
  browser in needs a real Supabase project and this stands in for one only as
  far as the API. They are the product's shape and copy, which is what they
  are here to show.

  The API beside them is not a fixture. It ran the migrations, imported a
  catalogue through the same importer the scheduled job uses, opened the
  Postgres ledger, and answered every call in step 6 for a member it had
  authenticated. To keep asking it:

    TOKEN=\$(cat $TMP/reader.token)
    curl -H "Authorization: Bearer \$TOKEN" http://localhost:$API_PORT/api/v1/cashback/wallet

  The operator's token is beside it in $TMP/operator.token, for the queues
  under http://localhost:$API_PORT/api/v1/cashback/ops/.

  Logs: $TMP/api.log, $TMP/auth.log, $TMP/web.log

  To prove the money loop rather than look at it:

    DATABASE_URL="$DATABASE_URL" go test -run TestScenario ./internal/cashback/scenarios/

  Ctrl-C to stop everything.

EOF

wait "$API_PID"
