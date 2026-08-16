#!/bin/sh
# The release gate (issue #119): a release may be cut ONLY from an annotated
# semver tag whose commit is already merged to main. Anything else - a
# lightweight tag, a tag on unmerged work, a name that is not semver - is
# refused with a message naming exactly what to fix.
#
# A tag that has already been released may be released again - that is the
# rollback - but only if it still names the same commit.
#
# Usage: release_guard.sh <tag> [main-ref] [released-commit]
#
# main-ref defaults to origin/main (what the release workflow checks
# against); the tests pass a local ref instead.
#
# released-commit is what the already-published GitHub Release for this tag
# records as the commit it was cut from, and it is the one thing here that
# git alone cannot answer. The workflow looks it up (gh release view) and
# passes it in, so this script stays pure git and stays testable:
#
#   ""         no Release exists for this tag yet
#   <sha>      the commit the published Release names
#   unknown    a Release exists but records no commit
#
# Output is one line stating the verdict; the exit code is the verdict.
# ::error:: markers surface the reason in the GitHub Actions annotations UI
# and are plain text anywhere else.
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 3 ]; then
    echo "usage: $0 <tag> [main-ref] [released-commit]" >&2
    exit 2
fi

TAG="$1"
MAIN_REF="${2:-origin/main}"
RELEASED_COMMIT="${3:-}"

. "$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/release_semver.sh"

# Semver shape first: vMAJOR.MINOR.PATCH with optional pre-release/build
# metadata (v1.2.3, v1.2.3-rc.1, v1.2.3+exp), by SemVer 2.0.0's own grammar
# (release_semver.sh). The push trigger pattern is looser than real semver,
# so the guard re-checks it strictly.
if ! release_semver_ok "$TAG"; then
    echo "::error::'$TAG' is not a semver tag (vMAJOR.MINOR.PATCH, optional -pre/+build, no leading zeroes, no empty identifiers); releases are cut only from semver tags" >&2
    exit 1
fi

# The tag must exist here and be annotated: an annotated tag is its own git
# object of type "tag" carrying tagger and message - the deliberate release
# act. A lightweight tag resolves straight to type "commit".
if ! TYPE=$(git cat-file -t "refs/tags/$TAG" 2>/dev/null); then
    echo "::error::tag '$TAG' does not exist in this checkout; the release workflow needs a full fetch (fetch-depth: 0)" >&2
    exit 1
fi
if [ "$TYPE" != "tag" ]; then
    echo "::error::tag '$TAG' is lightweight (points straight at a $TYPE); a release tag must be annotated: git tag -d $TAG && git tag -a $TAG -m 'release $TAG' <commit>" >&2
    exit 1
fi

# The tagged commit must be reachable from main: a release of unmerged work
# is refused. merge-base --is-ancestor answers exactly that question.
COMMIT=$(git rev-parse "refs/tags/$TAG^{commit}")
if ! git merge-base --is-ancestor "$COMMIT" "$MAIN_REF"; then
    echo "::error::tag '$TAG' points at $COMMIT, which is not reachable from $MAIN_REF; merge the work to main first - unmerged work is never released" >&2
    exit 1
fi

# A version, once published, names one commit forever. Re-releasing the same
# tag is legitimate and documented - it is how a rollback works - but only
# while the tag still points where the published Release says it was cut
# from. A force-moved tag would otherwise ship different code under a version
# the world has already seen, with release notes describing the old commit.
case "$RELEASED_COMMIT" in
"")
    : # No Release for this tag yet: nothing published to contradict.
    ;;
unknown)
    echo "::error::a GitHub Release for '$TAG' exists but records no commit, so nothing here can prove the tag still names the code that was released; this pipeline stamps every Release with its commit, so the Release was made by hand - check it and re-create it (gh release delete '$TAG'), or cut a new version. Nothing was deployed." >&2
    exit 1
    ;;
"$COMMIT")
    echo "release guard: '$TAG' is an annotated tag on $COMMIT, reachable from $MAIN_REF; re-releasing the commit its published Release already names (rollback or re-run)"
    exit 0
    ;;
*)
    echo "::error::tag '$TAG' now points at $COMMIT, but the published GitHub Release for '$TAG' was cut from $RELEASED_COMMIT: the tag has moved. A released version names one commit forever - deploying this would ship different code under a version that is already public, described by release notes written for the old commit. Cut a new version instead. Nothing was deployed." >&2
    exit 1
    ;;
esac

echo "release guard: '$TAG' is an annotated tag on $COMMIT, reachable from $MAIN_REF"
