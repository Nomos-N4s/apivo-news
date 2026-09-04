#!/bin/sh
# Prove the cashback Makefile targets guard, rather than merely fail.
#
# This file exists because of a specific bug and the specific way it hid.
#
# The `missing` macro was written as `{ printf ...; } >&2; exit 1`. After a
# `||` the shell reads that as `(A || B); C` — the `exit 1` is a separate
# command in the list and runs unconditionally. Every cashback target failed,
# always, and would have gone on failing after its dependency landed.
#
# It was verified by running all six targets and watching all six fail, which
# was read as correct because every dependency genuinely is missing right now.
# That is the trap: while a dependency is absent, a target that guards
# correctly and a target that fails unconditionally produce exactly the same
# output and the same exit code. The negative case cannot tell them apart, so
# on its own it proves nothing at all.
#
# What separates them is a case where the dependency is PRESENT and the target
# must SUCCEED. That is case 2 below, and it is the reason this file exists.
#
# Run locally with `sh scripts/make_targets_test.sh`; CI runs it in the
# `make-targets` workflow, which is the only thing anywhere that exercises
# this Makefile.
set -eu

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$HERE"

MAKE="${MAKE:-make}"

if ! command -v "$MAKE" >/dev/null 2>&1; then
    echo "SKIP: no '$MAKE' on PATH; this suite needs GNU make"
    exit 0
fi

FAILS=0

fail() {
    echo "FAIL: $1"
    FAILS=1
}

# ---------------------------------------------------------------------------
# 1. A target whose dependency is ABSENT refuses, and says why.
#
# Conditional on the dependency actually being absent, so this stays true as
# the other tasks land rather than turning red the day they do.
# ---------------------------------------------------------------------------
check_guard() {
    # check_guard <dependency-path> <target> [make-args...]
    _dep="$1"
    _target="$2"
    shift 2

    if [ -e "$_dep" ]; then
        echo "skip: $_target's dependency $_dep exists now, so the guard no longer applies"
        return
    fi

    set +e
    _out=$("$MAKE" "$_target" "$@" 2>&1)
    _status=$?
    set -e

    if [ "$_status" -eq 0 ]; then
        fail "$_target SUCCEEDED with $_dep missing; a target that quietly passes here reports a result nobody produced"
        return
    fi

    case "$_out" in
    *"has not landed yet"*)
        case "$_out" in
        *"$_dep"*)
            echo "ok: $_target refuses and names $_dep"
            ;;
        *)
            fail "$_target refused but did not name the missing file; it said: $_out"
            ;;
        esac
        ;;
    *)
        fail "$_target failed WITHOUT the guard message, so it failed obscurely rather than reporting an unbuilt dependency: $_out"
        ;;
    esac
}

check_guard cmd/apivo/seed_cashback.go cashback-seed
check_guard internal/cashback/scenarios cashback-scenario NAME=earn-confirm
check_guard internal/cashback/wallet/zerosum.go cashback-verify-ledger
check_guard scripts/lint-brand-literals.sh cashback-brand-check
check_guard scripts/lint-migrations.sh migration-lint
check_guard scripts/lint-migration-numbering.sh migration-numbering-lint

# ---------------------------------------------------------------------------
# 2. A target whose dependency is PRESENT succeeds.
#
# THE ASSERTION THAT MATTERS. Without it the whole suite is satisfied by a
# Makefile whose every target exits 1 unconditionally — which is exactly the
# bug this file was written for.
#
# The two lint targets are used because their bodies are `sh <script>` and
# nothing else: satisfying the dependency with a script that exits 0 makes the
# whole target's success depend only on the guard letting it through. The
# other four shell out to Docker or to `go`, which cannot be settled in a unit
# test without proving something else instead.
# ---------------------------------------------------------------------------
STUBS=""

cleanup() {
    for _s in $STUBS; do
        rm -f "$_s"
    done
}
trap cleanup EXIT

check_passthrough() {
    # check_passthrough <dependency-script> <target>
    _dep="$1"
    _target="$2"

    if [ -e "$_dep" ]; then
        # The real thing has landed. Use it: the target must still run it and
        # must not be blocked by a guard that outlived its purpose.
        _note="its real implementation"
    else
        printf '#!/bin/sh\nexit 0\n' > "$_dep"
        chmod +x "$_dep"
        STUBS="$STUBS $_dep"
        _note="a stub that exits 0"
    fi

    set +e
    _out=$("$MAKE" "$_target" 2>&1)
    _status=$?
    set -e

    if [ "$_status" -eq 0 ]; then
        echo "ok: $_target SUCCEEDS with $_note in place, so the guard is a guard and not an unconditional exit"
    else
        fail "$_target FAILED with $_dep present ($_note). If the message mentions a dependency that has not landed, the guard is exiting unconditionally - check that the exit in the 'missing' macro is INSIDE the braces, or the shell reads it as a separate command after the ||. It said: $_out"
    fi
}

check_passthrough scripts/lint-migrations.sh migration-lint
check_passthrough scripts/lint-migration-numbering.sh migration-numbering-lint
check_passthrough scripts/lint-brand-literals.sh cashback-brand-check

# ---------------------------------------------------------------------------
# 3. Every cashback target is well-formed.
#
# `make -n` expands each recipe without running it, which is what catches a
# malformed $(call missing,...) - the argument list splits on commas, and the
# messages carry parentheses and `#`.
# ---------------------------------------------------------------------------
set +e
dry=$("$MAKE" -n cashback-up cashback-seed cashback-scenario NAME=earn-confirm \
    cashback-verify-ledger cashback-brand-check migration-lint \
    migration-numbering-lint 2>&1)
dry_status=$?
set -e

if [ "$dry_status" -eq 0 ]; then
    echo "ok: every cashback recipe expands"
else
    fail "at least one cashback recipe does not expand: $dry"
fi

# NAME is claimed as a makefile variable so only the command line can set it.
# Unclaimed, make lets the ENVIRONMENT supply it, and `NAME` is the hostname on
# some systems - `make cashback-scenario` with no argument then silently ran
# `-run TestScenario/<hostname>` instead of saying NAME was required.
set +e
noname=$(NAME=from-the-environment "$MAKE" cashback-scenario 2>&1)
noname_status=$?
set -e

if [ "$noname_status" -eq 0 ]; then
    fail "cashback-scenario succeeded with no NAME on the command line"
elif printf '%s\n' "$noname" | grep -q 'NAME is required'; then
    echo "ok: cashback-scenario ignores NAME from the environment and asks for it"
else
    fail "cashback-scenario with no NAME did not ask for one; a NAME leaking in from the environment would run a scenario nobody named. It said: $noname"
fi

if [ "$FAILS" -ne 0 ]; then
    echo "make targets: FAILURES"
    exit 1
fi
echo "make targets: all checks passed"
