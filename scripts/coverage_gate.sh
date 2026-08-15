#!/bin/sh
# Fails the build when total statement coverage is below the minimum.
#
# Usage: coverage_gate.sh <min-percent> <coverage-profile>
#
# Machine-generated code (the sqlc output, one generated package per module
# under internal/*/store) is excluded from the computation: coverage there
# measures the generator, not our tests. The generated queries are exercised
# by integration tests either way.
set -eu

MIN="$1"
PROFILE="$2"

grep -v -E '/internal/(content|editorial)/store/' "$PROFILE" > "$PROFILE.filtered"

# Two steps, not a pipeline: POSIX sh has no pipefail, and a failing
# `go tool cover` piped into awk would otherwise fail open with an empty total.
FUNC_OUT=$(go tool cover -func="$PROFILE.filtered")
TOTAL=$(printf '%s\n' "$FUNC_OUT" | awk '/^total:/ { gsub(/%/, "", $3); print $3 }')

if [ -z "$TOTAL" ]; then
    echo "coverage gate ERROR: no total line in cover output" >&2
    exit 1
fi

echo "total statement coverage: ${TOTAL}% (minimum: ${MIN}%)"

if ! awk -v total="$TOTAL" -v min="$MIN" 'BEGIN { exit !(total >= min) }'; then
    echo "coverage gate FAILED: ${TOTAL}% < ${MIN}%" >&2
    exit 1
fi
