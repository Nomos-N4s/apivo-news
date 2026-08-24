#!/bin/sh
# Brings the Blnk ledger up beside an already-running Postgres and Redis,
# and does not return until it answers.
#
# Usage: blnk_up.sh
#
# Blnk is NOT declared as a GitHub Actions service container, and the reason
# is worth stating rather than rediscovering:
#
#   * `blnk start` does not migrate. The schema has to exist first, and a
#     service container's command cannot be overridden - `options` does not
#     accept an entrypoint - so `blnk migrate up && blnk start` has no
#     expression as a service.
#   * Service containers start concurrently and there is no way to say
#     "after Postgres is healthy". Blnk connects at startup and exits when
#     it cannot, so a service-container Blnk would lose that race
#     intermittently and be gone for the rest of the job.
#
# Running it as a step instead makes the order real: Postgres and Redis are
# service containers with health checks, this script runs after them, and it
# fails loudly with the container's logs attached when the ledger does not
# come up.
#
# Environment:
#   BLNK_IMAGE             pinned image reference (required)
#   BLNK_DATA_SOURCE_DNS   Postgres DSN the server connects with (required)
#   BLNK_REDIS_DNS         Redis host:port, e.g. 127.0.0.1:6379 (required)
#   BLNK_MIGRATE_DSN       DSN for `blnk migrate up`; defaults to
#                          BLNK_DATA_SOURCE_DNS. Spike S1 sets it to a
#                          restricted role to prove the migration stays
#                          inside the blnk schema.
#   BLNK_SERVER_PORT       listen port (default 5001)
#   BLNK_CONTAINER         container name (default blnk)
#   BLNK_SERVER_SECRET_KEY when set, the server requires it on every call
#   BLNK_WAIT_SECONDS      how long to wait for the first answer (default 90)
set -eu

require() {
    if [ -z "$2" ]; then
        echo "blnk_up: $1 is required" >&2
        exit 2
    fi
}

require BLNK_IMAGE "${BLNK_IMAGE:-}"
require BLNK_DATA_SOURCE_DNS "${BLNK_DATA_SOURCE_DNS:-}"
require BLNK_REDIS_DNS "${BLNK_REDIS_DNS:-}"

PORT="${BLNK_SERVER_PORT:-5001}"
NAME="${BLNK_CONTAINER:-blnk}"
MIGRATE_DSN="${BLNK_MIGRATE_DSN:-$BLNK_DATA_SOURCE_DNS}"
WAIT_SECONDS="${BLNK_WAIT_SECONDS:-90}"
SECRET_KEY="${BLNK_SERVER_SECRET_KEY:-}"

HERE="$(dirname "$0")"
sh "$HERE/pull_retry.sh" "$BLNK_IMAGE"

# The migration runs in its own throwaway container. Blnk validates its
# whole configuration before any command, so the Redis address has to be
# present even though migrating never touches it.
echo "blnk_up: migrating the ledger schema"
docker run --rm --network host \
    -e "BLNK_DATA_SOURCE_DNS=$MIGRATE_DSN" \
    -e "BLNK_REDIS_DNS=$BLNK_REDIS_DNS" \
    "$BLNK_IMAGE" blnk migrate up

echo "blnk_up: starting the ledger on port $PORT"
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" --network host \
    -e "BLNK_PROJECT_NAME=apivo-cashback" \
    -e "BLNK_DATA_SOURCE_DNS=$BLNK_DATA_SOURCE_DNS" \
    -e "BLNK_REDIS_DNS=$BLNK_REDIS_DNS" \
    -e "BLNK_SERVER_PORT=$PORT" \
    -e "BLNK_SERVER_SECRET_KEY=$SECRET_KEY" \
    "$BLNK_IMAGE" >/dev/null

# Blnk publishes no health route, so readiness is a real read through the
# router and the database connection. 401 counts: it means the server
# answered and is only refusing the credential, which is what a secured
# deployment looks like.
elapsed=0
while [ "$elapsed" -lt "$WAIT_SECONDS" ]; do
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/ledgers" || echo 000)
    if [ "$code" = "200" ] || [ "$code" = "401" ]; then
        echo "blnk_up: ledger answering on port $PORT (HTTP $code) after ${elapsed}s"
        exit 0
    fi
    if [ -z "$(docker ps -q -f "name=^${NAME}\$")" ]; then
        echo "::error::blnk exited before answering; its logs follow" >&2
        docker logs "$NAME" >&2 || true
        exit 1
    fi
    sleep 2
    elapsed=$((elapsed + 2))
done

echo "::error::blnk did not answer on port $PORT within ${WAIT_SECONDS}s; its logs follow" >&2
docker logs "$NAME" >&2 || true
exit 1
