#!/bin/sh
# Unit-style tests for lint-refs.sh. CI runs these before trusting the lint's
# verdict, for the reason the release guard, the migration lint and the commit
# author lint have their own suites: a gate is only as good as the proof that
# it closes.
#
# Every case asserts the EXIT CODE, because that is the verdict. The two ways
# this lint could report success while judging nothing are an empty argument
# list and an empty name, and both look exactly like a clean branch from the
# outside, so each is asserted to FAIL rather than pass.
#
# The cases for the default mode touch no git at all - the lint judges names,
# which is what lets it run before a branch has any commits on it. The
# --from-messages cases build throwaway repositories under mktemp; the
# repository this script lives in is only ever read.
#
# The two rules are proved separately. Rule 1 refuses a name nobody may use;
# rule 2 refuses a name nobody has used, and a case that satisfies rule 2
# while breaking rule 1 (`xcoder/cb-claude-fix`) is what keeps the second from
# quietly standing in for the first.
set -eu

LINT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/lint-refs.sh
SCRIPTS=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)

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

CR=$(printf '\r')

# run_case <lint-argument>... — runs the lint and records both what it said
# and what it returned.
REPO="$TMP"
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
    printf '%s' "$OUT" | grep -q -F "examined $1 ref name(s)"
}

refused_count() {
    printf '%s' "$OUT" | grep -q -F "$1 refused"
}

# expect_accepted <description> <ref-name>...
expect_accepted() {
    CASES=$((CASES + 1))
    _desc="$1"
    shift
    _want=$#
    run_case "$@"
    if [ "$STATUS" -ne 0 ]; then
        echo "FAIL: $_desc — the lint refused a name it must accept (exit $STATUS):"
        indent
        FAILS=1
    elif ! examined "$_want"; then
        echo "FAIL: $_desc — expected it to examine $_want name(s):"
        indent
        FAILS=1
    elif ! refused_count 0; then
        echo "FAIL: $_desc — accepted, but did not report zero refusals:"
        indent
        FAILS=1
    else
        echo "ok: $_desc"
    fi
}

# expect_accepted_messages <description> <git-log-argument>...
#
# Separate from expect_accepted because in --from-messages mode the summary
# counts commit subjects read and refs found, not names given, and asserting
# both is what keeps "found nothing" apart from "read nothing".
expect_accepted_messages() {
    CASES=$((CASES + 1))
    _desc="$1"
    shift
    run_case --from-messages "$@"
    if [ "$STATUS" -ne 0 ]; then
        echo "FAIL: $_desc — the lint refused history it must accept (exit $STATUS):"
        indent
        FAILS=1
    elif ! printf '%s' "$OUT" | grep -q -E 'read [1-9][0-9]* commit subject'; then
        echo "FAIL: $_desc — accepted without reading any commit subject:"
        indent
        FAILS=1
    elif ! refused_count 0; then
        echo "FAIL: $_desc — accepted, but did not report zero refusals:"
        indent
        FAILS=1
    else
        echo "ok: $_desc"
    fi
}

# expect_refused <description> <refusals> <ref-name>...
expect_refused() {
    CASES=$((CASES + 1))
    _desc="$1"
    _refused="$2"
    shift 2
    _want=$#
    # In --from-messages mode the names are read out of history rather than
    # given, so there is no argument count to check against.
    if [ "$1" = --from-messages ]; then _want=''; fi
    run_case "$@"
    if [ "$STATUS" -ne 1 ]; then
        echo "FAIL: $_desc — expected exit 1, got $STATUS:"
        indent
        FAILS=1
    elif [ -n "$_want" ] && ! examined "$_want"; then
        echo "FAIL: $_desc — expected it to examine $_want name(s):"
        indent
        FAILS=1
    elif ! refused_count "$_refused"; then
        echo "FAIL: $_desc — expected $_refused refusal(s):"
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
    CASES=$((CASES + 1))
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
# The failure this lint exists to close, in the exact shape it took: a
# tool's default branch prefix, which GitHub then wrote into `main` as
# "Merge pull request #349 from Nomos-N4s/claude/cb-nw-t056".

expect_refused "the prefix that reached main 32 times" 1 "claude/cb-nw-t056"
expect_refused "the same prefix on a longer slug" 1 \
    "claude/github-issues-spec-kit-status-f8n8x5"

# ---------------------------------------------------------------------------
# The token anywhere in the name, not only as the prefix. A merge commit
# quotes the whole name.

expect_refused "a blocked token in the middle of a slug" 1 "xcoder/cb-claude-fix"
expect_refused "a blocked token at the end of a slug" 1 "xcoder/port-for-claude"
expect_refused "a blocked token as a bare name" 1 "anthropic"

# ---------------------------------------------------------------------------
# The blocklist over-matches on purpose: case, separators, digits, suffixes.

expect_refused "an upper-case name" 1 "CLAUDE/cb-nw-t056"
expect_refused "a mixed-case token" 1 "xcoder/Claude-Code-notes"
expect_refused "an underscore as the delimiter" 1 "xcoder/claude_notes"
expect_refused "a dot as the delimiter" 1 "xcoder/v2.copilot.draft"
expect_refused "a digit as the delimiter" 1 "xcoder/gpt-4-summaries"
expect_refused "a versioned assistant" 1 "xcoder/gpt5-codex-run"

# Every vendor and assistant on the list, judged as a prefix. A token nobody
# tests is a token somebody deletes.
for token in anthropic claude copilot openai chatgpt gpt gemini codex cursor codeium aider; do
    expect_refused "the token '$token' as a prefix" 1 "$token/some-work"
done

# ---------------------------------------------------------------------------
# Several names at once: one bad name refuses the run, and the counts stay
# honest about how many were read and how many were refused.

expect_refused "one bad name among good ones" 1 \
    "main" "xcoder/cb-nw-t056" "claude/cb-nw-t057"
expect_refused "two bad names" 2 "claude/a" "xcoder/ok" "copilot/b"

# ---------------------------------------------------------------------------
# Rule 2: the convention. This is the half that catches the NEXT tool's
# default - a prefix no blocklist has heard of yet.

expect_refused "a prefix this repository does not use" 1 "feature/ingest-window"
expect_refused "another plausible prefix" 1 "fix/pagination-cursor"
expect_refused "no prefix at all" 1 "my-working-branch"
expect_refused "the prefix without a slug" 1 "xcoder/"
expect_refused "the prefix as a bare name" 1 "xcoder"
expect_refused "a prefix that merely starts the same way" 1 "xcoder-experiments/thing"

# A name breaking BOTH rules is one refusal, not two, and says both things.
CASES=$((CASES + 1))
run_case "claude/cb-nw-t056"
if [ "$STATUS" -ne 1 ]; then
    echo "FAIL: a name breaking both rules — expected exit 1, got $STATUS:"
    indent
    FAILS=1
elif ! refused_count 1; then
    echo "FAIL: a name breaking both rules was counted more than once:"
    indent
    FAILS=1
elif ! printf '%s' "$OUT" | grep -q -F -e "assistant or vendor name"; then
    echo "FAIL: a name breaking both rules did not report the first:"
    indent
    FAILS=1
elif ! printf '%s' "$OUT" | grep -q -F -e "is not named"; then
    echo "FAIL: a name breaking both rules did not report the second:"
    indent
    FAILS=1
else
    echo "ok: a name breaking both rules is refused once and reported twice"
fi

# ---------------------------------------------------------------------------
# Fully qualified names. A pre-push hook is handed `refs/heads/<branch>` on
# stdin, so both spellings have to be read the same way.

expect_accepted "a fully qualified branch" "refs/heads/xcoder/cb-money"
expect_accepted "the fully qualified trunk" "refs/heads/main"
expect_refused "a fully qualified branch under a banned name" 1 \
    "refs/heads/claude/cb-nw-t056"
expect_refused "a fully qualified branch off convention" 1 \
    "refs/heads/feature/ingest-window"

# A tag is judged by rule 1 alone: `v1.2.3` is not `xcoder/<slug>` and never
# will be. Rule 1 still reaches it, because a tag name is quoted in a release
# just as a branch name is quoted in a merge.
expect_accepted "a release tag" "refs/tags/v1.2.3"
expect_accepted "a prerelease tag" "refs/tags/v2.0.0-rc.1"
expect_refused "a tag carrying a banned name" 1 "refs/tags/v1.2.3-claude"

# ---------------------------------------------------------------------------
# What must NOT be refused. These decide whether the gate stays switched on:
# a lint that refuses a legitimate branch gets disabled, and then nothing
# checks anything.

expect_accepted "the repository's convention" "xcoder/cb-nw-t056"
expect_accepted "the trunk" "main"
expect_accepted "several ordinary names" "main" "xcoder/t001-go-dockerfile" "xcoder/cb-money"

# Delimited matching, tested where it earns its keep.
expect_accepted "a name that merely resembles a blocked token" "xcoder/claudette-profile"
expect_accepted "another look-alike" "xcoder/claudia-onboarding"

# The reasons `ai`, `bot` and `agent` are deliberately absent: each is a
# syllable or an ordinary noun that a real branch in this repository would
# carry.
expect_accepted "'ai' as a syllable inside ordinary words" "xcoder/retail-email-domain"
expect_accepted "'agent' meaning a poller, not an assistant" "xcoder/agent-poller-backoff"
expect_accepted "'bot' meaning automation" "xcoder/release-bot-guard"

# Refs whose names nobody here chooses. Refusing one would mean refusing a
# button in the GitHub UI, or a dependency update nobody can rename.
expect_accepted "a revert branch GitHub generated" "revert-349-xcoder/cb-nw-t056"
expect_accepted "a dependency bot's branch" "dependabot/go_modules/github.com/lib/pq-1.10.9"
expect_accepted "the other dependency bot's branch" "renovate/go-1.x"

# ---------------------------------------------------------------------------
# The ways this lint could report success without judging anything.

expect_refused_to_judge "no arguments at all" "no ref name given"

# On a push event `github.head_ref` is the empty string. A workflow that
# passes it through unguarded runs this script with one blank argument, and a
# lint that shrugged at it would be off on exactly the events it is meant to
# cover.
expect_refused_to_judge "an empty name" "an empty ref name was given" ""
expect_refused_to_judge "an empty name beside a good one" \
    "an empty ref name was given" "xcoder/ok" ""

# A blocklist the runner's grep cannot compile must stop the run. A search
# that did not happen and a search that found nothing produce the same
# silence.
CASES=$((CASES + 1))
BROKEN="$TMP/broken-lint.sh"
sed 's/^anthropic$/a{2,1}/' "$LINT" > "$BROKEN"
set +e
OUT=$(sh "$BROKEN" "claude/cb-nw-t056" 2>&1)
STATUS=$?
set -e
if [ "$STATUS" -eq 0 ]; then
    echo "FAIL: a blocklist that cannot be searched reported a clean name:"
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
# A ref name on a pull request from a fork is chosen by whoever opened it, and
# GitHub Actions reads workflow commands off a step's stdout. ::stop-commands::
# switches command parsing off for the rest of the job.

CASES=$((CASES + 1))
INJECTED=$(printf 'claude\r::stop-commands::deadbeef')
run_case "$INJECTED"
if [ "$STATUS" -ne 1 ]; then
    echo "FAIL: the injected name was not even caught as a blocked name (exit $STATUS):"
    indent
    FAILS=1
elif ! printf '%s' "$OUT" | grep -q -F -e "stop-commands"; then
    echo "FAIL: the offending name was not quoted back at all:"
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
    echo "ok: a quoted name cannot forge a workflow command"
fi

# ---------------------------------------------------------------------------
# --from-messages: the ref names git has already written into history. This
# is the shape the violation actually took, so the fixtures are the literal
# subjects git and GitHub produce.

REPOS=0
SEQ=0

# fixture — a fresh repository. Subjects are written onto empty commits: this
# lint reads a subject, and a subject that names a branch names it whether or
# not the commit has two parents.
fixture() {
    REPOS=$((REPOS + 1))
    REPO="$TMP/repo$REPOS"
    SEQ=0
    git init -q -b main "$REPO"
    subject "chore: fixture base (#1)"
    BASE=$(git -C "$REPO" rev-parse HEAD)
}

subject() {
    SEQ=$((SEQ + 1))
    git -C "$REPO" commit -q --allow-empty -m "$1"
}

# The exact subject GitHub wrote 32 times.
fixture
subject "Merge pull request #349 from Nomos-N4s/claude/cb-nw-t056"
expect_refused "the merge subject GitHub wrote into main" 1 --from-messages "$BASE..HEAD"
CASES=$((CASES + 1))
if ! printf '%s' "$OUT" | grep -q -F -e "claude/cb-nw-t056"; then
    echo "FAIL: the offending merge subject was refused without naming the branch:"
    indent
    FAILS=1
elif ! printf '%s' "$OUT" | grep -q -E "^::error::[0-9a-f]{40}: "; then
    echo "FAIL: the offending merge subject was refused without naming the commit:"
    indent
    FAILS=1
else
    echo "ok: a refused merge subject names both the commit and the branch"
fi

# The owner is a GitHub account, not part of the ref. If it were judged, every
# fork's owner name would be read as a branch prefix.
fixture
subject "Merge pull request #350 from Nomos-N4s/xcoder/principle-i-refs"
expect_accepted_messages "a merge of a correctly named branch" "$BASE..HEAD"

# The two cases that pin the first segment being dropped. Both are contrived
# and both are the point: an account we do not control, and a remote somebody
# named locally, are not refs anybody here can rename - so refusing them would
# report a violation with no remedy, which is how a gate loses its audience.
# Without the strip these read as blocked names and go red.
fixture
subject "Merge pull request #5 from openai-labs/patch-1"
expect_accepted_messages "a fork whose OWNER carries a blocked token" "$BASE..HEAD"
fixture
subject "Merge remote-tracking branch 'codex/xcoder/cb-money' into main"
expect_accepted_messages "a REMOTE named after a blocked token" "$BASE..HEAD"

# And the branch behind such an owner is still judged: dropping the first
# segment must not drop the rest with it.
fixture
subject "Merge pull request #5 from openai-labs/claude/cb-nw-t056"
expect_refused "a banned branch behind an owner that also carries one" 1 \
    --from-messages "$BASE..HEAD"

# Rule 2 must NOT reach a commit subject: the name there is permanent, so
# refusing it for being off convention would refuse history nobody can change.
fixture
subject "Merge pull request #12 from someone-else/patch-1"
expect_accepted_messages "a fork's branch, off this repository's convention" "$BASE..HEAD"
fixture
subject "Merge branch 'feature/ingest-window' into main"
expect_accepted_messages "a local branch off convention" "$BASE..HEAD"

# git's own merge subjects. A merge names two branches and either can be the
# offending one.
fixture
subject "Merge branch 'claude/cb-nw-t056' into main"
expect_refused "a local merge of a banned branch" 1 --from-messages "$BASE..HEAD"
fixture
subject "Merge branch 'xcoder/cb-money' into claude/cb-nw-t056"
expect_refused "a banned branch on the receiving side of a merge" 1 \
    --from-messages "$BASE..HEAD"
fixture
subject "Merge branch 'xcoder/cb-money' into main"
expect_accepted_messages "an ordinary local merge, both sides clean" "$BASE..HEAD"

# The remote-tracking form: the first segment is the remote, not the ref.
fixture
subject "Merge remote-tracking branch 'origin/claude/cb-nw-t056'"
expect_refused "a remote-tracking merge of a banned branch" 1 \
    --from-messages "$BASE..HEAD"
fixture
subject "Merge remote-tracking branch 'origin/xcoder/cb-money' into main"
expect_accepted_messages "a remote-tracking merge of a clean branch" "$BASE..HEAD"

# Subjects that name no branch at all. A squash merge is the common one: its
# subject is the pull request title, and nothing in it can be judged.
fixture
subject "feat(ingestion): capture provenance at retrieval (#12)"
subject "fix(api): correct the pagination cursor (#42)"
expect_accepted_messages "ordinary commits, naming no ref" "$BASE..HEAD"
fixture
subject "chore(scripts): refuse a merge of the assistant's branch (#350)"
expect_accepted_messages "a subject that merely mentions the word merge" "$BASE..HEAD"

# The `<sha> -1` shape the commit-hygiene job falls back to on a first push.
fixture
subject "Merge pull request #349 from Nomos-N4s/claude/cb-nw-t056"
expect_refused "the '<sha> -1' shape still judges" 1 --from-messages HEAD -1

# The ways this mode could report success without judging anything.
fixture
expect_refused_to_judge "an empty range" "holds no commits" \
    --from-messages "$BASE..$BASE"
fixture
expect_refused_to_judge "a range git cannot resolve" "could not read the range" \
    --from-messages "no-such-ref..HEAD"
fixture
expect_refused_to_judge "--from-messages with no range" \
    "needs a commit range" --from-messages

# ---------------------------------------------------------------------------
# The two blocklists must not drift.
#
# This lint carries its own copy of the token list rather than sharing one
# with scripts/lint-commit-authors.sh, because that script is a proved gate
# and its own suite reaches into its text. The copy is only safe while it
# stays a copy: a token added to one list and not the other is precisely the
# kind of half-closed gate that produced this lint in the first place.

CASES=$((CASES + 1))
tokens_of() {
    sed -n '/^blocklist() {$/,/^}$/p' "$1" \
        | grep -vE '^blocklist\(\) \{$|^\}$|^ *cat <<|^EOF$|^#|^[[:space:]]*$' \
        | sed 's/^[[:space:]]*//' | sort
}
tokens_of "$LINT" > "$TMP/refs-tokens"
tokens_of "$SCRIPTS/lint-commit-authors.sh" > "$TMP/author-tokens"
if [ ! -s "$TMP/refs-tokens" ]; then
    echo "FAIL: no tokens could be read out of lint-refs.sh; this check proves nothing."
    FAILS=1
elif [ ! -s "$TMP/author-tokens" ]; then
    echo "FAIL: no tokens could be read out of lint-commit-authors.sh; this check proves nothing."
    FAILS=1
elif ! diff -u "$TMP/author-tokens" "$TMP/refs-tokens" > "$TMP/token-diff"; then
    echo "FAIL: the two blocklists have drifted apart (-author +refs):"
    sed 's/^/    /' "$TMP/token-diff"
    echo "      Add the token to BOTH scripts. An identity and a ref name are"
    echo "      two ways for the same name to reach the same history."
    FAILS=1
else
    echo "ok: the ref lint and the author lint refuse the same tokens ($(wc -l < "$TMP/refs-tokens" | tr -d ' ') of them)"
fi

# ---------------------------------------------------------------------------
# The branch this suite is actually running on. Skipped on a detached HEAD,
# which is what a CI checkout of a pull request merge ref looks like.

CASES=$((CASES + 1))
HERE=$(git -C "$ROOT" branch --show-current 2>/dev/null || true)
if [ -z "$HERE" ]; then
    echo "skip: detached HEAD, so there is no branch name to judge"
else
    run_case "$HERE"
    if [ "$STATUS" -ne 0 ]; then
        echo "FAIL: this suite is running on a branch the lint refuses (exit $STATUS)."
        echo "      If that is right, rename the branch before pushing it:"
        indent
        FAILS=1
    else
        echo "ok: the branch this suite runs on ($HERE)"
    fi
fi

if [ "$FAILS" -ne 0 ]; then
    echo "ref name lint tests FAILED"
    exit 1
fi
echo "all $CASES ref name lint cases passed"
