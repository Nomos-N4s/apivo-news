#!/bin/sh
# Pulls a container image, retrying transient registry failures.
#
# Usage: pull_retry.sh <image-ref>
#
# CI jobs that pull at run time fail on registry rate limits (Docker Hub and
# AWS ECR Public both throttle anonymous pulls) with an exit code that looks
# exactly like the job's real failure - a drift check reporting "exit 125"
# reads as schema drift when nothing drifted. Retrying with backoff turns
# nearly all of those into a slower success, and the message on the way out
# names the registry so a genuine failure is never mistaken for one.
set -eu

IMAGE="$1"
ATTEMPTS=${PULL_ATTEMPTS:-5}

attempt=1
while [ "$attempt" -le "$ATTEMPTS" ]; do
    if docker pull "$IMAGE"; then
        exit 0
    fi
    if [ "$attempt" -eq "$ATTEMPTS" ]; then
        break
    fi
    delay=$((attempt * 15))
    echo "::warning::pulling $IMAGE failed (attempt $attempt of $ATTEMPTS); retrying in ${delay}s"
    sleep "$delay"
    attempt=$((attempt + 1))
done

echo "::error::could not pull $IMAGE after $ATTEMPTS attempts - this is a registry failure (rate limit or outage), not a problem with the code under test" >&2
exit 1
