#!/bin/sh
# Generates markdown release notes for a tag: every commit since the previous
# semver tag, grouped by Conventional Commit type (issue #119). The commit
# subjects are the notes - the commit-hygiene CI gate guarantees the format,
# so the grouping never meets a subject it cannot place.
#
# Usage: release_notes.sh <tag>   (markdown on stdout)
#
# Needs the full history and tags in the checkout (fetch-depth: 0).
set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: $0 <tag>" >&2
    exit 2
fi

TAG="$1"
if ! git rev-parse -q --verify "refs/tags/$TAG^{commit}" >/dev/null; then
    echo "::error::release notes: tag '$TAG' does not exist in this checkout" >&2
    exit 1
fi

# The previous release is the nearest semver tag reachable from the commit
# BEFORE this tag ("$TAG^" walks past a tag placed on the same commit twice).
# No previous tag means this is the first release: the notes cover the whole
# history, honestly labelled as such.
PREV=$(git describe --tags --abbrev=0 --match 'v[0-9]*' "$TAG^" 2>/dev/null || true)
if [ -n "$PREV" ]; then
    RANGE="$PREV..$TAG"
else
    RANGE="$TAG"
fi

LOG=$(mktemp)
trap 'rm -f "$LOG"' EXIT
# %h then the subject; --no-merges because a merge subject describes the
# act of merging, and its content arrives as the merged commits themselves.
git log --no-merges --format='%h %s' "$RANGE" > "$LOG"

echo "## $TAG"
echo
if [ -n "$PREV" ]; then
    echo "Changes since $PREV."
else
    echo "First release: the notes cover the full history."
fi

# section <heading> <type-pattern>: emits a heading and the matching
# subjects, most recent first, or nothing at all when the group is empty.
section() {
    heading="$1"
    pattern="$2"
    matches=$(grep -E "^[0-9a-f]+ ($pattern)(\([^)]*\))?!?: " "$LOG" || true)
    [ -z "$matches" ] && return 0
    echo
    echo "### $heading"
    echo
    printf '%s\n' "$matches" | while IFS=' ' read -r hash subject; do
        echo "- $subject ($hash)"
    done
}

# Breaking changes first and loudest: any subject whose type carries the
# Conventional Commit "!" marker. They also appear in their own group below,
# deliberately - a breaking fix is still a fix.
breaking=$(grep -E '^[0-9a-f]+ [a-z]+(\([^)]*\))?!: ' "$LOG" || true)
if [ -n "$breaking" ]; then
    echo
    echo "### Breaking changes"
    echo
    printf '%s\n' "$breaking" | while IFS=' ' read -r hash subject; do
        echo "- $subject ($hash)"
    done
fi

section "Features" 'feat'
section "Fixes" 'fix'
section "Performance" 'perf'
section "Refactoring" 'refactor'
section "Documentation" 'docs'
section "Tests" 'test'
section "Build" 'build'
section "CI" 'ci'
section "Chores" 'chore|style|revert'
