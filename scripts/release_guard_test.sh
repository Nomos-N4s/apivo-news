#!/bin/sh
# Unit-style tests for release_guard.sh. The release workflow runs this
# before trusting the guard's verdict: a gate is only as good as the proof
# that it closes.
#
# Everything happens in throwaway repositories under mktemp - the repository
# this script lives in is never touched. HOME points into the scratch area
# and GIT_CONFIG_NOSYSTEM is set, so the operator's global git configuration
# (signing, hooks, default branch) cannot leak into the fixtures.
set -eu

GUARD=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/release_guard.sh

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
export HOME="$TMP"
export GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@invalid

# Fixture history:
#   main:     feat: one  <- v0.1.0 (annotated), v0.1.1 (lightweight),
#                           v0.2.0-rc.1 (annotated)
#   feature:  feat: unmerged  <- v0.9.0 (annotated, NOT reachable from main)
REPO="$TMP/repo"
git init -q -b main "$REPO"
cd "$REPO"
echo one > f
git add f
git commit -q -m "feat: one"
git tag -a v0.1.0 -m "release v0.1.0"
git tag v0.1.1
git tag -a v0.2.0-rc.1 -m "release candidate"
git checkout -q -b feature
echo two > f
git commit -q -am "feat: unmerged"
git tag -a v0.9.0 -m "tagged off main"
git checkout -q main

FAILS=0

# expect_pass <description> <guard args...>
expect_pass() {
    desc="$1"
    shift
    if out=$(sh "$GUARD" "$@" 2>&1); then
        echo "ok: $desc"
    else
        echo "FAIL: $desc - guard refused: $out"
        FAILS=1
    fi
}

# expect_fail <description> <required-message-fragment> <guard args...>
expect_fail() {
    desc="$1"
    fragment="$2"
    shift 2
    if out=$(sh "$GUARD" "$@" 2>&1); then
        echo "FAIL: $desc - guard allowed it: $out"
        FAILS=1
    elif ! printf '%s' "$out" | grep -q -F "$fragment"; then
        echo "FAIL: $desc - refused with the wrong message: $out"
        FAILS=1
    else
        echo "ok: $desc"
    fi
}

expect_pass "annotated semver tag on main"            v0.1.0 main
expect_pass "annotated pre-release tag on main"       v0.2.0-rc.1 main
expect_fail "lightweight tag"        "must be annotated"          v0.1.1 main
expect_fail "tag on unmerged work"   "not reachable from"         v0.9.0 main
expect_fail "non-semver name"        "not a semver tag"           release-1 main
expect_fail "semver without v"       "not a semver tag"           0.1.0 main
expect_fail "nonexistent tag"        "does not exist"             v9.9.9 main
expect_fail "trailing garbage"       "not a semver tag"           v0.1.0.. main

# SemVer 2.0.0 forbids leading zeroes in numeric identifiers and empty
# dot-separated identifiers. They are refused for the shape alone, before
# the tag is looked up - none of these exist in the fixture.
expect_fail "leading zero in major"  "not a semver tag"           v01.2.3 main
expect_fail "leading zero in patch"  "not a semver tag"           v1.2.03 main
expect_fail "leading zero in pre"    "not a semver tag"           v1.2.3-01 main
expect_fail "empty pre identifier"   "not a semver tag"           v1.2.3-alpha..1 main
expect_fail "empty build identifier" "not a semver tag"           v1.2.3+build..7 main
expect_fail "pre-release only dash"  "not a semver tag"           v1.2.3- main

exit "$FAILS"
