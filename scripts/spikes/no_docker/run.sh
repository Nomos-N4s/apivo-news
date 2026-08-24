#!/bin/sh
# Spike S3 (ADR-0002, task T008) — the suite half.
#
# Question: with no Docker, no Postgres, no Redis and no ledger, is
# `go test ./...` GREEN — every container-backed suite skipping rather than
# failing — and is the cashback product still a valid configuration?
#
# This is the founder's actual machine, reproduced deliberately: every key
# that would let a test reach a container is unset before anything runs, so
# a suite that quietly depended on one fails here instead of on the day the
# stack is unavailable.
#
# It is safe and useful to run anywhere. In CI it runs in a job with no
# service containers at all, which is what makes the result evidence rather
# than assertion.
#
# Usage:
#
#   sh scripts/spikes/no_docker/run.sh
#
# The other half of S3 — that the full cashback job passes in CI with Blnk
# and Redis as service containers — is answered by that job's own run, and
# nothing here can stand in for it.
set -eu

echo "S3: verifying the no-Docker path"

# Everything that lets a test reach a container. Unsetting a variable that
# was never set is not an error, so this is safe under `set -u`.
unset DATABASE_URL
unset BLNK_URL
unset BLNK_SECRET_KEY
unset REDIS_URL
unset SPIKE_ADMIN_DATABASE_URL
unset SPIKE_BLNK_ROLE_DSN
unset S2_WORKER_MODE

# The documented local shape: the product on, the in-process ledger, the
# fixture network. No sidecar is named because none can be reached.
CASHBACK_ENABLED=true
LEDGER_DRIVER=memory
NETWORK_DRIVER=fixture
export CASHBACK_ENABLED LEDGER_DRIVER NETWORK_DRIVER

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAILURES=0

pass() {
    echo "S3 PASS  $1"
}

fail() {
    echo "S3 FAIL  $1" >&2
    FAILURES=$((FAILURES + 1))
}

# Check 1 — it builds.
if go build ./...; then
    pass "check 1: go build ./... with the cashback environment set"
else
    fail "check 1: go build ./... failed"
fi

# Check 2 — it vets.
if go vet ./...; then
    pass "check 2: go vet ./... with the cashback environment set"
else
    fail "check 2: go vet ./... failed"
fi

# Check 3 — the whole suite is green with nothing to connect to. This is the
# claim: a contributor with no Docker gets a green run, not a wall of
# connection errors.
if go test -count=1 ./... > "$WORK/test.out" 2>&1; then
    pass "check 3: go test ./... is green with no database, no ledger and no Redis"
else
    fail "check 3: go test ./... failed without containers"
    cat "$WORK/test.out" >&2
fi

# Check 4 — green for the right reason. A suite that reached nothing because
# it no longer exists would also be green, so the container-keyed tests must
# be observed SKIPPING, and each skip must say which key it is waiting for.
#
# The exit status is CAPTURED, not discarded. An earlier revision ended this
# line with `|| true` and then judged the run by grepping for "--- FAIL",
# which is a check that cannot fail: a package that does not compile, fails
# to build or panics in init prints `FAIL <pkg> [build failed]` and `# <pkg>`
# and never emits a single "--- FAIL" marker, so the spike declared PASS over
# a suite that had not run at all. This is the evidence job for S3; it fails
# closed now.
if go test -count=1 -v ./scripts/spikes/... ./internal/platform/db/ > "$WORK/skips.out" 2>&1; then
    VERBOSE_STATUS=0
else
    VERBOSE_STATUS=$?
fi

for key in DATABASE_URL BLNK_URL; do
    if grep -q -- "--- SKIP" "$WORK/skips.out" && grep -q "$key is unset" "$WORK/skips.out"; then
        pass "check 4: a container-backed suite skipped, naming $key"
    else
        fail "check 4: no suite skipped naming $key - a Docker-dependent test may be passing vacuously, or failing"
        grep -E -- "--- (SKIP|FAIL)|^(ok|FAIL)" "$WORK/skips.out" >&2 || true
    fi
done

# Check 5 — and nothing in that verbose run actually failed, for any of the
# ways a Go test run can fail.
#
# The exit status is the primary signal, because it is the only one that
# covers every failure mode at once. The grep is kept as a second, narrower
# net: it names WHICH package went wrong in the output, which a bare status
# cannot. Both have to be clean.
if [ "$VERBOSE_STATUS" -ne 0 ]; then
    fail "check 5: the verbose run exited $VERBOSE_STATUS - the suite did not pass"
    grep -E -- '^(--- FAIL|FAIL|panic:|# )' "$WORK/skips.out" >&2 || tail -20 "$WORK/skips.out" >&2
elif grep -qE -- '^(--- FAIL|FAIL|panic:)' "$WORK/skips.out"; then
    fail "check 5: the verbose run exited 0 but reported a failure - trust the report"
    grep -E -- '^(--- FAIL|FAIL|panic:|# )' "$WORK/skips.out" >&2
else
    pass "check 5: no test failed without containers"
fi

SKIPPED=$(grep -c -- "--- SKIP" "$WORK/skips.out" || true)
echo "S3: $SKIPPED container-keyed test(s) skipped, as designed"

echo
if [ "$FAILURES" -eq 0 ]; then
    echo "S3 VERDICT: PASS — the repository builds, vets and tests green with no Docker, and LEDGER_DRIVER=memory is a complete cashback configuration."
    echo "S3 NOTE: the memory ledger removes the ledger and Redis from the local loop, not Postgres. The binary still requires DATABASE_URL, so running the SERVER without Docker needs a database somewhere else; running the SUITE does not."
    exit 0
fi

echo "S3 VERDICT: FAIL — $FAILURES check(s) failed." >&2
exit 1
