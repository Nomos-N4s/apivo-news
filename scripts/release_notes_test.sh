#!/bin/sh
# Unit-style tests for release_notes.sh, in the same shape as
# release_guard_test.sh: throwaway repositories under mktemp, the operator's
# git configuration isolated, one line per verdict. What is under test is
# the choice of the PREVIOUS RELEASE - the lower bound of the notes - because
# choosing it wrongly silently omits shipped commits from what the release
# says it contains.
set -eu

NOTES=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/release_notes.sh

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
export HOME="$TMP"
export GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@invalid

# Fixture history (oldest first):
#   feat: one    <- v0.1.0 (annotated)
#   fix: two     <- v0.1.1 (LIGHTWEIGHT), v1junk (annotated, not semver)
#   feat: three  <- v0.2.0 (annotated)
#   feat: four   <- v0.3.0 and v0.3.1 (both annotated, same commit)
REPO="$TMP/repo"
git init -q -b main "$REPO"
cd "$REPO"
echo one > f
git add f
git commit -q -m "feat: one"
git tag -a v0.1.0 -m "release v0.1.0"
git commit -q --allow-empty -m "fix: two"
git tag v0.1.1
git tag -a v1junk -m "not a version"
git commit -q --allow-empty -m "feat: three"
git tag -a v0.2.0 -m "release v0.2.0"
git commit -q --allow-empty -m "feat: four"
git tag -a v0.3.0 -m "release v0.3.0"
git tag -a v0.3.1 -m "release v0.3.1"

FAILS=0

# expect_contains <description> <tag> <required-fragment>
expect_contains() {
    desc="$1"
    tag="$2"
    fragment="$3"
    if ! out=$(sh "$NOTES" "$tag" 2>&1); then
        echo "FAIL: $desc - notes failed: $out"
        FAILS=1
    elif ! printf '%s' "$out" | grep -q -F -e "$fragment"; then
        echo "FAIL: $desc - '$fragment' missing from: $out"
        FAILS=1
    else
        echo "ok: $desc"
    fi
}

# expect_absent <description> <tag> <forbidden-fragment>
expect_absent() {
    desc="$1"
    tag="$2"
    fragment="$3"
    if ! out=$(sh "$NOTES" "$tag" 2>&1); then
        echo "FAIL: $desc - notes failed: $out"
        FAILS=1
    elif printf '%s' "$out" | grep -q -F -e "$fragment"; then
        echo "FAIL: $desc - '$fragment' present in: $out"
        FAILS=1
    else
        echo "ok: $desc"
    fi
}

# expect_fail <description> <tag> <required-message-fragment>
expect_fail() {
    desc="$1"
    tag="$2"
    fragment="$3"
    if out=$(sh "$NOTES" "$tag" 2>&1); then
        echo "FAIL: $desc - notes succeeded: $out"
        FAILS=1
    elif ! printf '%s' "$out" | grep -q -F -e "$fragment"; then
        echo "FAIL: $desc - wrong message: $out"
        FAILS=1
    else
        echo "ok: $desc"
    fi
}

expect_contains "first release covers the whole history" v0.1.0 "First release"
expect_contains "first release lists its commit"         v0.1.0 "feat: one"

# The lightweight v0.1.1 and the malformed v1junk both sit closer to v0.2.0
# than v0.1.0 does. Neither could ever have been released, so neither may
# bound the notes - and "fix: two" belongs in them.
expect_contains "measured from the previous RELEASE"     v0.2.0 "Changes since v0.1.0."
expect_contains "commits under a lightweight tag kept"   v0.2.0 "fix: two"
expect_contains "commits under a non-semver tag kept"    v0.2.0 "feat: three"

# v0.3.0 and v0.3.1 name the same commit: v0.3.1 succeeds v0.3.0 and adds
# nothing. Saying so is the honest answer; reaching past it to v0.2.0 and
# re-listing "feat: four" would claim it shipped twice.
expect_contains "same-commit predecessor is found"       v0.3.1 "Changes since v0.3.0."
expect_absent   "nothing re-listed across it"            v0.3.1 "feat: four"

expect_fail     "missing tag is refused"                 v9.9.9 "does not exist"

exit "$FAILS"
