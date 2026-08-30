#!/bin/sh
# End-to-end tests for .githooks/pre-push, driven through real `git push`.
#
# The hook is proved by pushing rather than by feeding it stdin, because what
# it has to get right is the protocol git speaks to it: which of the four
# fields on a line is the name that will land on the remote, and which shape a
# deletion takes. A hand-written stdin fixture would encode whatever this
# suite's author believed about that, and believing the wrong thing is the
# only way this hook can fail.
#
# Two cases matter more than the rest and neither is about refusing anything:
# a deletion must go through, because deleting a badly named branch is the
# remedy the lint asks for, and a tag must go through, because a release tag
# is not `xcoder/<slug>` and never will be. A hook that blocked either would
# be removed within a day.
#
# Everything happens in throwaway repositories under mktemp, pushing to a bare
# repository in the same directory. The repository this script lives in is
# only ever read - its .githooks and scripts directories are what the fixtures
# point `core.hooksPath` at, so this suite proves the hook that is committed
# rather than a copy of it.
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
HOOKS="$ROOT/.githooks"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
export HOME="$TMP"
export GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@invalid

FAILS=0
CASES=0
OUT=""
STATUS=0
WORK=""
REMOTE=""

# fixture — a bare remote and a work repository wired to this repository's
# committed hooks. The commit-msg hook lives in the same directory and would
# run on every commit here; the fixtures commit before hooksPath is set, so
# only the push path is under test.
fixture() {
    WORK="$TMP/work"
    REMOTE="$TMP/remote.git"
    rm -rf "$WORK" "$REMOTE"
    git init -q --bare "$REMOTE"
    git init -q -b main "$WORK"
    git -C "$WORK" commit -q --allow-empty -m "chore: fixture base (#1)"
    git -C "$WORK" remote add origin "$REMOTE"
    git -C "$WORK" config core.hooksPath "$HOOKS"
}

# push <git-push-argument>...
push() {
    set +e
    OUT=$(git -C "$WORK" push "$@" 2>&1)
    STATUS=$?
    set -e
}

indent() {
    printf '%s\n' "$OUT" | sed 's/^/    /'
}

# expect_push_allowed <description> <git-push-argument>...
expect_push_allowed() {
    CASES=$((CASES + 1))
    _desc="$1"
    shift
    push "$@"
    if [ "$STATUS" -ne 0 ]; then
        echo "FAIL: $_desc — the hook blocked a push it must allow (exit $STATUS):"
        indent
        FAILS=1
    else
        echo "ok: $_desc"
    fi
}

# expect_push_blocked <description> <required-fragment> <git-push-argument>...
expect_push_blocked() {
    CASES=$((CASES + 1))
    _desc="$1"
    _frag="$2"
    shift 2
    push "$@"
    if [ "$STATUS" -eq 0 ]; then
        echo "FAIL: $_desc — the push went through:"
        indent
        FAILS=1
    elif ! printf '%s' "$OUT" | grep -q -F -e "$_frag"; then
        echo "FAIL: $_desc — blocked without saying \"$_frag\":"
        indent
        FAILS=1
    else
        echo "ok: $_desc"
    fi
}

# on_remote <ref> — 0 when the bare repository carries it.
on_remote() {
    git -C "$REMOTE" show-ref --verify --quiet "$1"
}

# ---------------------------------------------------------------------------
# The name that lands on the remote is the one judged.

fixture
git -C "$WORK" checkout -q -b xcoder/cb-money
expect_push_allowed "a branch following the convention" -u origin xcoder/cb-money

fixture
git -C "$WORK" checkout -q -b claude/cb-nw-t056
expect_push_blocked "the prefix that reached main 32 times" \
    "carries an assistant or vendor name" -u origin claude/cb-nw-t056
CASES=$((CASES + 1))
if on_remote refs/heads/claude/cb-nw-t056; then
    echo "FAIL: the refused branch reached the remote anyway"
    FAILS=1
else
    echo "ok: a refused branch does not reach the remote"
fi

fixture
git -C "$WORK" checkout -q -b feature/ingest-window
expect_push_blocked "a prefix this repository does not use" \
    "is not named" -u origin feature/ingest-window

# A local branch pushed under another name is judged by the name it LANDS
# under, which is the field this hook has to pick out of the four git hands
# it. Both directions are proved: a clean local name cannot smuggle a bad
# remote name in, and a bad local name is not held against a clean push.
fixture
git -C "$WORK" checkout -q -b xcoder/cb-money
expect_push_blocked "a clean local name pushed to a banned remote name" \
    "carries an assistant or vendor name" origin xcoder/cb-money:refs/heads/claude/smuggled

fixture
git -C "$WORK" checkout -q -b claude/cb-nw-t056
expect_push_allowed "a banned local name pushed to a clean remote name" \
    origin claude/cb-nw-t056:refs/heads/xcoder/cb-nw-t056

# ---------------------------------------------------------------------------
# What must NOT be blocked. A hook that stopped either of these would be
# removed, and then nothing runs before CI.

# Deleting a badly named branch is the remedy the lint asks for. The branch is
# seeded with --no-verify, which is also the honest demonstration that this
# hook is a convenience rather than the gate.
fixture
git -C "$WORK" checkout -q -b claude/cb-nw-t056
git -C "$WORK" push -q --no-verify origin claude/cb-nw-t056
CASES=$((CASES + 1))
if on_remote refs/heads/claude/cb-nw-t056; then
    echo "ok: --no-verify bypasses the hook, as a hook always can"
else
    echo "FAIL: the seed push did not reach the remote, so the deletion case proves nothing"
    FAILS=1
fi
expect_push_allowed "deleting a badly named branch" origin --delete claude/cb-nw-t056
CASES=$((CASES + 1))
if on_remote refs/heads/claude/cb-nw-t056; then
    echo "FAIL: the deletion was allowed but the branch is still on the remote"
    FAILS=1
else
    echo "ok: the badly named branch is gone from the remote"
fi

# A release tag is judged by rule 1 alone. `v1.2.3` is not `xcoder/<slug>`,
# and a hook that demanded it would block every release.
fixture
git -C "$WORK" tag v1.2.3
expect_push_allowed "a release tag" origin v1.2.3
fixture
git -C "$WORK" tag v1.2.3-claude
expect_push_blocked "a tag carrying a banned name" \
    "carries an assistant or vendor name" origin v1.2.3-claude

# The trunk, and a push that has nothing to send.
fixture
expect_push_allowed "the trunk" -u origin main
expect_push_allowed "a push with nothing to send" origin main

# Several refs in one push: one bad name refuses the whole push, and git
# sends all or nothing.
fixture
git -C "$WORK" branch xcoder/cb-money
git -C "$WORK" branch claude/cb-nw-t056
expect_push_blocked "one bad name among several refs" \
    "carries an assistant or vendor name" origin main xcoder/cb-money claude/cb-nw-t056
CASES=$((CASES + 1))
if on_remote refs/heads/xcoder/cb-money; then
    echo "FAIL: a refused push still delivered its acceptable refs"
    FAILS=1
else
    echo "ok: one refused ref refuses the whole push"
fi

fixture
git -C "$WORK" branch xcoder/cb-money
expect_push_allowed "several refs, all acceptable" origin main xcoder/cb-money

# ---------------------------------------------------------------------------
# The hook cannot judge without the lint, and must say so rather than wave the
# push through.

CASES=$((CASES + 1))
fixture
BARE_HOOKS="$TMP/hooks-only"
mkdir -p "$BARE_HOOKS"
cp "$HOOKS/pre-push" "$BARE_HOOKS/pre-push"
git -C "$WORK" config core.hooksPath "$BARE_HOOKS"
git -C "$WORK" checkout -q -b xcoder/cb-money
push -u origin xcoder/cb-money
if [ "$STATUS" -eq 0 ]; then
    echo "FAIL: with no lint beside it the hook let the push through:"
    indent
    FAILS=1
elif ! printf '%s' "$OUT" | grep -q -F -e "cannot be judged"; then
    echo "FAIL: the hook refused without saying the lint was missing:"
    indent
    FAILS=1
else
    echo "ok: a hook that cannot find the lint refuses rather than passing"
fi

if [ "$FAILS" -ne 0 ]; then
    echo "pre-push hook tests FAILED"
    exit 1
fi
echo "all $CASES pre-push hook cases passed"
