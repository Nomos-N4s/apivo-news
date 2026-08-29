#!/bin/sh
# Unit-style tests for lint-commit-authors.sh. CI runs these before trusting
# the lint's verdict, for the reason the release guard and the migration lint
# have their own suites: a gate is only as good as the proof that it closes.
#
# The lint it proves exists because a message-body check cannot see a commit's
# author. So every case here asserts the EXIT CODE, and every accepting case
# also asserts how many commits were actually examined — the two ways this
# lint could report success while judging nothing are an empty range and a
# git that failed, and both look exactly like a clean branch from the outside.
#
# Everything happens in throwaway repositories under mktemp; the repository
# this script lives in is only ever read. HOME points into the scratch area
# and GIT_CONFIG_NOSYSTEM is set, which matters more here than anywhere else
# in this repository: the machine that produced the failure this lint closes
# carries the offending identity in its GLOBAL git configuration, and a
# fixture that inherited it would be proving the wrong thing.
set -eu

LINT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/lint-commit-authors.sh
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
export HOME="$TMP"
export GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@invalid

FAILS=0
CASES=0
SEQ=0
REPO=""
BASE=""
OUT=""
STATUS=0

HUMAN_NAME="Ordinary Contributor"
HUMAN_MAIL="contributor@example.invalid"
CR=$(printf '\r')

# fixture — a fresh repository holding one ordinary commit, so each case is
# judged only on what it adds after BASE.
fixture() {
    CASES=$((CASES + 1))
    REPO="$TMP/case$CASES"
    SEQ=0
    git init -q -b main "$REPO"
    commit_as "$HUMAN_NAME" "$HUMAN_MAIL" "$HUMAN_NAME" "$HUMAN_MAIL" "chore: fixture base"
    BASE=$(git -C "$REPO" rev-parse HEAD)
}

# commit_as <author-name> <author-mail> <committer-name> <committer-mail> <subject>
#
# Each commit touches a file of its own, so a branch and the trunk can be
# merged later without a conflict getting in the way of the case.
commit_as() {
    SEQ=$((SEQ + 1))
    printf 'change %d\n' "$SEQ" > "$REPO/change-$SEQ.txt"
    (
        cd "$REPO"
        git add -A
        GIT_AUTHOR_NAME="$1" GIT_AUTHOR_EMAIL="$2" \
            GIT_COMMITTER_NAME="$3" GIT_COMMITTER_EMAIL="$4" \
            git commit -q -m "$5"
    )
}

# merge_as <name> <mail> — a real merge commit made by the given identity,
# with both sides written by an ordinary one. Three commits land: the branch
# side, the trunk side, and the merge itself.
merge_as() {
    (cd "$REPO" && git checkout -q -b side "$BASE")
    commit_as "$HUMAN_NAME" "$HUMAN_MAIL" "$HUMAN_NAME" "$HUMAN_MAIL" "feat(side): a change on the branch (#1)"
    (cd "$REPO" && git checkout -q main)
    commit_as "$HUMAN_NAME" "$HUMAN_MAIL" "$HUMAN_NAME" "$HUMAN_MAIL" "feat(trunk): a change on the trunk (#1)"
    (
        cd "$REPO"
        GIT_AUTHOR_NAME="$1" GIT_AUTHOR_EMAIL="$2" \
            GIT_COMMITTER_NAME="$1" GIT_COMMITTER_EMAIL="$2" \
            git merge -q --no-ff -m "chore: merge the branch (#1)" side
    )
}

# run_case <lint-argument>... — runs the lint in REPO and records both what it
# said and what it returned. The exit code is the verdict, so it is captured
# rather than inferred from the output.
run_case() {
    set +e
    OUT=$(cd "$REPO" && sh "$LINT" "$@" 2>&1)
    STATUS=$?
    set -e
}

indent() {
    printf '%s\n' "$OUT" | sed 's/^/    /'
}

examined() {
    printf '%s' "$OUT" | grep -q -F "examined $1 commit(s)"
}

# expect_accepted <description> <commits-examined> <lint-argument>...
expect_accepted() {
    _desc="$1"
    _want="$2"
    shift 2
    run_case "$@"
    if [ "$STATUS" -ne 0 ]; then
        echo "FAIL: $_desc — the lint refused history it must accept (exit $STATUS):"
        indent
        FAILS=1
    elif ! examined "$_want"; then
        echo "FAIL: $_desc — expected it to examine $_want commit(s):"
        indent
        FAILS=1
    else
        echo "ok: $_desc"
    fi
}

# expect_rejected <description> <commits-examined> <required-fragment> <lint-argument>...
expect_rejected() {
    _desc="$1"
    _want="$2"
    _frag="$3"
    shift 3
    run_case "$@"
    if [ "$STATUS" -ne 1 ]; then
        echo "FAIL: $_desc — expected exit 1, got $STATUS:"
        indent
        FAILS=1
    elif ! printf '%s' "$OUT" | grep -q -F -e "$_frag"; then
        echo "FAIL: $_desc — refused without naming \"$_frag\":"
        indent
        FAILS=1
    elif ! examined "$_want"; then
        echo "FAIL: $_desc — expected it to examine $_want commit(s):"
        indent
        FAILS=1
    else
        echo "ok: $_desc"
    fi
}

# expect_refused_to_judge <description> <required-fragment> <lint-argument>...
#
# Exit 2, not 1: the lint reached no verdict at all. This is the status that
# separates "nothing wrong here" from "nothing was read", and the whole point
# of having it is that the second must never be reported as the first.
expect_refused_to_judge() {
    _desc="$1"
    _frag="$2"
    shift 2
    run_case "$@"
    if [ "$STATUS" -ne 2 ]; then
        echo "FAIL: $_desc — expected exit 2, got $STATUS:"
        indent
        FAILS=1
    elif ! printf '%s' "$OUT" | grep -q -F -e "$_frag"; then
        echo "FAIL: $_desc — stopped without saying why:"
        indent
        FAILS=1
    else
        echo "ok: $_desc"
    fi
}

# ---------------------------------------------------------------------------
# The failure this lint exists to close: a clean Conventional Commit message,
# a commit header written by the assistant's identity.

fixture
commit_as "Claude" "noreply@anthropic.com" "Claude" "noreply@anthropic.com" \
    "feat(ingestion): capture provenance at retrieval (#12)"
expect_rejected "the identity a development container ships" 1 \
    'author "Claude <noreply@anthropic.com>"' "$BASE..HEAD"

# ---------------------------------------------------------------------------
# Both fields, separately. The committer is the one a message check cannot
# reach and the one a rebase quietly rewrites.

fixture
commit_as "$HUMAN_NAME" "$HUMAN_MAIL" "Claude" "noreply@anthropic.com" \
    "fix(api): correct the pagination cursor (#42)"
expect_rejected "a clean author with the assistant as committer" 1 \
    'committer "Claude <noreply@anthropic.com>"' "$BASE..HEAD"

fixture
commit_as "Claude" "noreply@anthropic.com" "$HUMAN_NAME" "$HUMAN_MAIL" \
    "fix(api): correct the pagination cursor (#42)"
expect_rejected "a clean committer with the assistant as author" 1 \
    'author "Claude <noreply@anthropic.com>"' "$BASE..HEAD"

# ---------------------------------------------------------------------------
# Merge commits. The trailer step includes them deliberately; so does this
# one, and for the same reason — a merge is a commit with two identities of
# its own.

fixture
merge_as "Claude" "noreply@anthropic.com"
expect_rejected "a merge commit made by an AI identity" 3 \
    "is an AI attribution identity" "$BASE..HEAD"

# ---------------------------------------------------------------------------
# The blocklist over-matches on purpose: case, domain, subdomain, address
# without the name, name without the address.

fixture
commit_as "CLAUDE" "dev@example.invalid" "CLAUDE" "dev@example.invalid" \
    "chore: shout it (#1)"
expect_rejected "an upper-case name" 1 'author "CLAUDE' "$BASE..HEAD"

fixture
commit_as "Anthropic" "dev@example.invalid" "Anthropic" "dev@example.invalid" \
    "chore: the vendor by name (#1)"
expect_rejected "a mixed-case vendor name" 1 'author "Anthropic' "$BASE..HEAD"

fixture
commit_as "$HUMAN_NAME" "NOREPLY@ANTHROPIC.COM" "$HUMAN_NAME" "NOREPLY@ANTHROPIC.COM" \
    "chore: an upper-case address (#1)"
expect_rejected "an upper-case address" 1 "AI attribution identity" "$BASE..HEAD"

fixture
commit_as "A Person" "person@anthropic.com" "A Person" "person@anthropic.com" \
    "chore: an ordinary name at the vendor domain (#1)"
expect_rejected "any address at the vendor domain, whatever the name" 1 \
    "AI attribution identity" "$BASE..HEAD"

fixture
commit_as "A Person" "person@mail.anthropic.com" "A Person" "person@mail.anthropic.com" \
    "chore: a subdomain of the vendor (#1)"
expect_rejected "a subdomain of the vendor domain" 1 \
    "AI attribution identity" "$BASE..HEAD"

fixture
commit_as "Copilot" "198982749+Copilot@users.noreply.github.com" \
    "Copilot" "198982749+Copilot@users.noreply.github.com" \
    "chore: another assistant's bot identity (#1)"
expect_rejected "an assistant whose address is at a legitimate host" 1 \
    "AI attribution identity" "$BASE..HEAD"

fixture
commit_as "claude-code" "agent@example.invalid" "claude-code" "agent@example.invalid" \
    "chore: a suffixed identity (#1)"
expect_rejected "a blocked token carrying a suffix" 1 \
    "AI attribution identity" "$BASE..HEAD"

# A .mailmap can rewrite what git log DISPLAYS. The lint reads the raw fields
# on purpose, so the commit that carries the bad identity cannot also carry
# the file that hides it.
fixture
printf '%s <%s> Claude <noreply@anthropic.com>\n' "$HUMAN_NAME" "$HUMAN_MAIL" > "$REPO/.mailmap"
commit_as "Claude" "noreply@anthropic.com" "Claude" "noreply@anthropic.com" \
    "chore: bring a mailmap along (#1)"
expect_rejected "a .mailmap that launders the identity" 1 \
    'author "Claude <noreply@anthropic.com>"' "$BASE..HEAD"

# An identity is written by whoever opened the pull request, and GitHub
# Actions reads workflow commands off a step's stdout. Git accepts a carriage
# return inside a name, and a carriage return is enough to have the tail of a
# quoted identity read as a new command - ::stop-commands:: switches command
# parsing off for the rest of the job.
fixture
INJECTED=$(printf 'Claude\r::stop-commands::deadbeef')
commit_as "$INJECTED" "agent@example.invalid" "$INJECTED" "agent@example.invalid" \
    "chore: an identity that tries to write a workflow command (#1)"
run_case "$BASE..HEAD"
if [ "$STATUS" -ne 1 ]; then
    echo "FAIL: the injected identity was not even caught as an AI identity (exit $STATUS):"
    indent
    FAILS=1
elif ! printf '%s' "$OUT" | grep -q -F -e "stop-commands"; then
    echo "FAIL: the offending identity was not quoted back at all:"
    indent
    FAILS=1
elif printf '%s' "$OUT" | grep -q -F -e "::stop-commands::"; then
    echo "FAIL: a workflow command survived into the lint's output:"
    indent
    FAILS=1
elif printf '%s' "$OUT" | grep -q -e "$CR"; then
    echo "FAIL: a carriage return survived into the lint's output"
    FAILS=1
else
    echo "ok: a quoted identity cannot forge a workflow command"
fi

# ---------------------------------------------------------------------------
# What must NOT be refused. These are the cases that decide whether the gate
# stays switched on: a lint that fails a legitimate merge gets disabled, and
# then nothing checks anything.

fixture
commit_as "$HUMAN_NAME" "$HUMAN_MAIL" "$HUMAN_NAME" "$HUMAN_MAIL" \
    "feat(reader): show the place name in the header (#7)"
expect_accepted "an ordinary contributor" 1 "$BASE..HEAD"

# GitHub records the squash and merge commits it creates as its own. An
# allowlist of one address would refuse every one of them, which is most of
# this repository's history.
fixture
merge_as "GitHub" "noreply@github.com"
expect_accepted "a merge commit created by GitHub itself" 3 "$BASE..HEAD"

# Delimited matching, tested where it earns its keep: these are real given
# names that a substring blocklist would refuse.
fixture
commit_as "Claudette Bernard" "claudette@example.invalid" \
    "Claudette Bernard" "claudette@example.invalid" "docs: a look-alike name (#1)"
commit_as "Claudia Rossi" "c.rossi@example.invalid" \
    "Claudia Rossi" "c.rossi@example.invalid" "docs: another look-alike (#1)"
expect_accepted "contributors whose names merely resemble a blocked token" 2 "$BASE..HEAD"

# The reason "ai" is not on the list: it lives inside the most common mail
# host there is.
fixture
commit_as "Second Reviewer" "second.reviewer@gmail.com" \
    "Second Reviewer" "second.reviewer@gmail.com" "docs: an ordinary mail host (#1)"
expect_accepted "an ordinary address at a common mail host" 1 "$BASE..HEAD"

# The commit-hygiene job falls back to `<sha> -1` on the first push to a ref,
# where there is no earlier commit to measure from. That shape has to work,
# or the fallback path checks nothing.
fixture
commit_as "$HUMAN_NAME" "$HUMAN_MAIL" "$HUMAN_NAME" "$HUMAN_MAIL" \
    "chore: the single-commit shape (#1)"
expect_accepted "the '<sha> -1' argument shape the job falls back to" 1 HEAD -1

fixture
commit_as "Claude" "noreply@anthropic.com" "Claude" "noreply@anthropic.com" \
    "chore: the single-commit shape, offending (#1)"
expect_rejected "the '<sha> -1' shape still judges" 1 \
    "AI attribution identity" HEAD -1

# ---------------------------------------------------------------------------
# The ways this lint could report success without judging anything. Each of
# these is a green step in a message-only world, and each must be a failure
# here.

fixture
expect_refused_to_judge "an empty range" "holds no commits" "$BASE..$BASE"

fixture
expect_refused_to_judge "a range git cannot resolve" "could not read the range" \
    "no-such-ref..HEAD"

fixture
expect_refused_to_judge "no range at all" "no commit range given"

CASES=$((CASES + 1))
REPO="$TMP/not-a-repository"
mkdir -p "$REPO"
expect_refused_to_judge "a directory that is not a repository" "could not read the range" \
    "HEAD..HEAD"

# A blocklist the runner's grep cannot compile must stop the run. It is the
# same failure mode the brand lint guards: a search that did not happen and a
# search that found nothing produce the same silence.
fixture
commit_as "Claude" "noreply@anthropic.com" "Claude" "noreply@anthropic.com" \
    "chore: an offending commit behind a broken search (#1)"
BROKEN="$TMP/broken-lint.sh"
sed 's/^anthropic$/a{2,1}/' "$LINT" > "$BROKEN"
set +e
OUT=$(cd "$REPO" && sh "$BROKEN" "$BASE..HEAD" 2>&1)
STATUS=$?
set -e
if [ "$STATUS" -eq 0 ]; then
    echo "FAIL: a blocklist that cannot be searched reported a clean range:"
    indent
    FAILS=1
elif ! printf '%s' "$OUT" | grep -q -F -e "could not search"; then
    echo "FAIL: a broken search stopped the run without saying why:"
    indent
    FAILS=1
else
    echo "ok: a search that cannot run fails the lint rather than passing it"
fi

# ---------------------------------------------------------------------------
# The history that actually exists. The expected count is taken from git
# rather than written down, so this asserts the summary is a real count of
# real commits and not a number the lint made up — and it does not go red on
# a shallow clone, where there is simply less to read.

CASES=$((CASES + 1))
REPO="$ROOT"
WANT=$(git -C "$ROOT" rev-list --count -20 HEAD)
run_case HEAD -20
if [ "$STATUS" -ne 0 ]; then
    echo "FAIL: this repository's own recent history was refused (exit $STATUS)."
    echo "      If that is right, the branch itself carries an AI attribution identity:"
    indent
    FAILS=1
elif ! examined "$WANT"; then
    echo "FAIL: this repository's own recent history — expected $WANT commit(s) examined:"
    indent
    FAILS=1
else
    echo "ok: this repository's own recent history ($WANT commits examined)"
fi

if [ "$FAILS" -ne 0 ]; then
    echo "commit author lint tests FAILED"
    exit 1
fi
echo "all $CASES commit author lint cases passed"
