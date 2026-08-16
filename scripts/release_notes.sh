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
TAG_COMMIT=$(git rev-parse "refs/tags/$TAG^{commit}")

. "$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/release_semver.sh"

LOG=$(mktemp)
CANDIDATES=$(mktemp)
trap 'rm -f "$LOG" "$CANDIDATES"' EXIT

# The previous RELEASE, not merely the previous tag: only annotated tags
# (the guard refuses lightweight ones, so a lightweight tag could never have
# been released) whose names pass the same strict semver check the guard
# applies. A stray v1junk or a lightweight v0.1.1 left lying around must not
# become the lower bound and silently drop commits from the notes.
git tag --list 'v*' --sort=-v:refname --format='%(objecttype) %(refname:short)' |
    while read -r objtype name; do
        [ "$objtype" = "tag" ] || continue
        release_semver_ok "$name" || continue
        printf '%s\n' "$name"
    done > "$CANDIDATES"

# Walk that descending list: everything after this tag is a lower version,
# and the first one whose commit is an ancestor of this tag's commit is the
# release this one succeeds. --is-ancestor is reflexive, so a predecessor
# sitting on the SAME commit is found rather than walked past (the old
# "$TAG^" lookup skipped it and measured from one release too far back).
# No candidate means this is the first release: the notes cover the whole
# history, honestly labelled as such.
PREV=""
if grep -q -x -F -e "$TAG" "$CANDIDATES"; then
    past_tag=0
else
    # This tag is not itself a release tag (the guard would refuse it);
    # nothing to skip, so consider every candidate.
    past_tag=1
fi
while read -r name; do
    if [ "$name" = "$TAG" ]; then
        past_tag=1
        continue
    fi
    [ "$past_tag" -eq 1 ] || continue
    if git merge-base --is-ancestor "refs/tags/$name^{commit}" "$TAG_COMMIT"; then
        PREV="$name"
        break
    fi
done < "$CANDIDATES"

if [ -n "$PREV" ]; then
    RANGE="$PREV..$TAG"
else
    RANGE="$TAG"
fi
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
