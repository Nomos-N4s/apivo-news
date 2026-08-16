#!/bin/sh
# The release gate (issue #119): a release may be cut ONLY from an annotated
# semver tag whose commit is already merged to main. Anything else - a
# lightweight tag, a tag on unmerged work, a name that is not semver - is
# refused with a message naming exactly what to fix.
#
# Usage: release_guard.sh <tag> [main-ref]
#
# main-ref defaults to origin/main (what the release workflow checks
# against); the tests pass a local ref instead. Output is one line stating
# the verdict; the exit code is the verdict. ::error:: markers surface the
# reason in the GitHub Actions annotations UI and are plain text anywhere
# else.
set -eu

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
    echo "usage: $0 <tag> [main-ref]" >&2
    exit 2
fi

TAG="$1"
MAIN_REF="${2:-origin/main}"

# Semver shape first: vMAJOR.MINOR.PATCH with optional pre-release/build
# metadata (v1.2.3, v1.2.3-rc.1, v1.2.3+exp). The push trigger pattern is
# looser than real semver, so the guard re-checks it strictly.
if ! printf '%s' "$TAG" | grep -q -E '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'; then
    echo "::error::'$TAG' is not a semver tag (vMAJOR.MINOR.PATCH, optional -pre/+build); releases are cut only from semver tags" >&2
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

echo "release guard: '$TAG' is an annotated tag on $COMMIT, reachable from $MAIN_REF"
