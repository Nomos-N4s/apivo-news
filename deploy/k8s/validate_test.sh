#!/bin/sh
# Prove that deploy/k8s/validate.sh fails the way it is supposed to.
#
# A gate is only worth having if it can both pass and fail, and the two cases
# below are exactly that pair. Running only the passing one proves nothing: a
# script that always exits 0 and a script that is correct look identical from
# there, and so do a script that always exits 1 and a script that caught a
# real defect.
#
# The case that matters most is the EMPTY one, and it has TWO failure modes
# rather than one, because `grep PATTERN` with no file arguments reads STDIN
# and what that does depends on what stdin is:
#
#   - stdin open (a terminal, `make k8s-validate` from a shell): grep BLOCKS.
#     The script hangs. In a required CI job that is not a red X — it is a job
#     sitting on the runner until the timeout kills it, which reads as an
#     infrastructure fault and gets retried rather than fixed.
#   - stdin closed (a GitHub Actions `run:` step): grep reads EOF and matches
#     nothing, so every check REPORTS SUCCESS against a directory with no
#     manifests in it. That is the quieter and worse mode: a gate that says
#     "ok: every Service is ClusterIP" about files that do not exist.
#
# Both were confirmed by hand against the pre-fix script. So the assertions
# below cover both, and the second one is the load-bearing one: a non-zero
# exit alone does NOT distinguish the buggy script from the correct one,
# because the buggy script also ends up non-zero once it reaches a check that
# an empty match cannot satisfy. What separates them is that the buggy script
# prints "ok:" lines on the way there and the correct one prints none.
#
# validate.sh derives the directory it validates from its own location, so
# copying it alone into an empty directory is all it takes to drive that path.
# Nothing is stubbed and nothing is mocked.
set -eu

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

FAILS=0

fail() {
    echo "FAIL: $1"
    FAILS=1
}

# ---------------------------------------------------------------------------
# 1. An empty manifest directory FAILS, and does not hang.
# ---------------------------------------------------------------------------
mkdir -p "$TMP/empty"
cp "$HERE/validate.sh" "$TMP/empty/validate.sh"

set +e
out=$(timeout 30 sh "$TMP/empty/validate.sh" 2>&1)
status=$?
set -e

case "$status" in
0)
    fail "validate.sh PASSED against a directory with no manifests at all; a gate that green-lights an empty manifest set is a gate that would green-light a deleted one"
    ;;
124 | 137)
    fail "validate.sh HUNG against an empty manifest directory (killed by timeout). A grep with no file arguments reads stdin and blocks - in CI this burns the job timeout and looks like an infrastructure fault rather than a broken manifest set"
    ;;
*)
    echo "ok: an empty manifest directory fails fast (exit $status) rather than hanging"
    ;;
esac

case "$out" in
*"no manifests found"*)
    echo "ok: and it says which directory it found nothing in"
    ;;
*)
    fail "the empty-directory failure did not explain itself; it said: $out"
    ;;
esac

# THE ASSERTION THAT ACTUALLY SEPARATES THE TWO SCRIPTS.
#
# With stdin closed - which is how a CI `run:` step invokes this - the pre-fix
# validator did not hang. It let every grep read EOF, printed a run of "ok:"
# lines about manifests that do not exist, and only failed later at a check an
# empty match could not satisfy. A test that accepted any non-zero exit would
# have called that correct.
#
# Nothing may be reported as passing about an empty directory.
if printf '%s\n' "$out" | grep -q '^ok:'; then
    fail "validate.sh reported checks PASSING against a directory with no manifests: $(printf '%s\n' "$out" | grep '^ok:' | tr '\n' ' ')"
else
    echo "ok: and it asserts nothing at all about manifests that do not exist"
fi

# ---------------------------------------------------------------------------
# 2. The REAL manifest directory PASSES.
#
# The other half of the pair. Without it, case 1 is satisfied by a validate.sh
# that fails unconditionally.
# ---------------------------------------------------------------------------
set +e
real_out=$(timeout 60 sh "$HERE/validate.sh" 2>&1)
real_status=$?
set -e

if [ "$real_status" -eq 0 ]; then
    echo "ok: the real manifest set passes, so the failure above was a decision and not a habit"
else
    fail "validate.sh does not pass against deploy/k8s itself (exit $real_status): $real_out"
fi

if [ "$FAILS" -ne 0 ]; then
    echo "k8s validator: FAILURES"
    exit 1
fi
echo "k8s validator: all checks passed"
