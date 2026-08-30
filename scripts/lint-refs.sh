#!/bin/sh
# A ref name is part of the commit history, and Principle I applies to it
# (constitution, Principle I — NON-NEGOTIABLE).
#
# Principle I is enforced in four places today and every one of them reads a
# commit: .claude/hooks/session-start.sh sets the identity, .githooks/commit-msg
# strips attribution trailers, the commit-hygiene job greps the message bodies,
# and scripts/lint-commit-authors.sh reads the author and committer fields.
# None of them has ever read the name of a branch.
#
# That is not a missing pattern, it is a missing subject, and it cost this
# repository 32 commits on `main` reading
#
#     Merge pull request #349 from Nomos-N4s/<assistant>/cb-nw-t056
#
# where the branch prefix was a tool's default that nobody had written a rule
# against. Every gate was green while they landed, and correctly so:
#
#   - the commit-msg hook never runs, because GitHub writes a merge commit
#     server-side;
#   - the trailer grep matches three shapes, all of them trailers, and
#     `Merge pull request #N from owner/branch` is none of them;
#   - the author lint reads identities, and the identities on those merges
#     are right. A branch name is not an identity.
#
# The merge commit's message is generated FROM the branch name at merge time.
# Once written it is permanent, public, and correctable only by rewriting
# shared history. The branch name is the last moment at which this is free,
# which is why this lint judges the name and not the commit.
#
# ---------------------------------------------------------------------------
# TWO WAYS IN
#
# By default it judges the names it is handed and does not read git at all,
# which is what lets it run before a branch has any commits on it - in
# .githooks/pre-push, and in CI against the head branch of a pull request. A
# check that needed history could only fire after the history it was meant to
# prevent existed.
#
# With --from-messages it reads the ref names git has already written into
# commit subjects, which is the shape this violation actually takes. It is
# the second half of the same gate: the first half stops a name from being
# merged, and this one notices if the first half was skipped, bypassed, or
# never ran on the path a commit took.
#
# ---------------------------------------------------------------------------
# TWO RULES
#
#   1. No name carries an assistant's or a vendor's name. This is Principle I
#      and it applies to every ref, branch or tag.
#   2. Every branch is `xcoder/<slug>`, with `main` and the refs GitHub and
#      the dependency bots generate as the only exceptions. This is the
#      repository's own convention, followed 85 times before it was ever
#      written down - which is exactly how a tool's default prefix leaked in
#      unnoticed 24 more.
#
# Rule 2 is not a milder restatement of rule 1. Rule 1 refuses a name nobody
# may use; rule 2 refuses a name nobody has USED, and that is the one that
# catches the next tool's default - a prefix this blocklist has never heard
# of. A blocklist can only refuse what somebody thought to write down.
#
# Rule 2 is about branches, so a name given as `refs/tags/...` is judged by
# rule 1 alone: a release tag is `v1.2.3` and always will be.
#
# ---------------------------------------------------------------------------
# Usage: lint-refs.sh <ref-name>...
#        lint-refs.sh --from-messages <git-log-argument>...
#
# Names may be given bare (`xcoder/cb-money`, which is what
# `github.head_ref` holds) or fully qualified (`refs/heads/xcoder/cb-money`,
# which is what a pre-push hook is handed). Both are read the same way.
#
# --from-messages takes the arguments `git log` takes, so both shapes the
# commit-hygiene job produces work: a `base..head` range, and `<sha> -1` for
# the first push to a ref. It reads the subject of every commit in the range
# and pulls the ref names out of the three subjects that carry one:
#
#     Merge pull request #349 from <owner>/<branch>   (GitHub, server-side)
#     Merge branch '<branch>' into <branch>           (git, locally)
#     Merge remote-tracking branch '<remote>/<branch>'
#
# Only rule 1 applies there. In a commit subject the name is already
# permanent, so refusing it for being off convention would refuse history
# that cannot be changed and offer no remedy; the convention is enforced
# where a rename is still free.
#
# The range must be what a branch ADDS, not what it inherits - `base..head`,
# or `$(git merge-base base head)..head` on a branch that has merged its base
# in. This repository's `main` already carries 32 merge commits this lint
# refuses, and they cannot be corrected without rewriting shared history; a
# range that reaches them reports a violation nobody can act on.
#
# Exit 0 when every name is acceptable, 1 when one is refused, and 2 when the
# lint could not judge - no arguments, or an empty name. A run that examines
# nothing is an error, never a pass: on a push event `github.head_ref` is the
# empty string, and a gate that reports success on it is a gate that is off.
set -eu

# The one prefix this repository's branches use. A constant rather than a
# pattern because there is exactly one, and a list of prefixes invites a
# second.
BRANCH_PREFIX='xcoder/'

# The tokens that must never appear in a ref name.
#
# This is the same set scripts/lint-commit-authors.sh refuses in an author or
# committer field, and lint-refs_test.sh asserts the two lists stay identical:
# a token added to one and not the other is the next blind spot. It is
# duplicated rather than shared because that script is a proved gate and its
# suite reaches into its text; the drift check buys the same protection at no
# risk to it.
#
# Each entry is matched case-insensitively and delimited by non-letters, so
# `claude` catches `claude/x`, `xcoder/claude-fix`, `CLAUDE_2` and
# `xcoder/cb-claude` while leaving `claudette` and `claudia` alone. Digits
# count as a delimiter: a versioned name is the same name.
blocklist() {
    cat <<'EOF'
# The vendor.
anthropic
# The assistant, and the prefix a development container defaults to - this is
# the exact token that reached `main` 32 times.
claude
# GitHub's assistant.
copilot
# The vendor behind the assistants below.
openai
# The assistant by name. `gpt` alone cannot catch it: the delimiter rule means
# the `chat` in front of it is not a boundary.
chatgpt
# The model family as tools write it - gpt-4, gpt-5-codex, auto-gpt.
gpt
# Google's assistant.
gemini
# The coding agent.
codex
# The editor's background agent.
cursor
# The assistant and the editor built around it.
codeium
# The terminal coding agent.
aider
EOF
}

# DELIBERATELY NOT ON THE LIST. Each would refuse a legitimate branch, and a
# gate that cries wolf is a gate someone switches off:
#
#   ai        `ai` is a syllable, not a word - it is inside `retail`,
#             `domain`, `email` and `available`, all plausible in a slug.
#   bot       An ordinary noun here, and GitHub's own automation is welcome.
#   agent     This repository has agents that are not AI: a user agent, a
#             polling agent. The word describes half the codebase.
#   devin, cody, bard
#             Human names before they are agents.
#
# A branch that genuinely integrates a vendor's API is named for the
# capability rather than the vendor - `xcoder/machine-translation`, not
# `xcoder/openai-translation`. That costs one rename and keeps the vendor's
# name out of a merge commit, which is the whole point.

if [ "$#" -eq 0 ]; then
    echo "::error::no ref name given; nothing was examined. Usage: $0 <ref-name>... (for example \"\$(git branch --show-current)\")" >&2
    exit 2
fi

pattern=$(blocklist | grep -vE '^#|^[[:space:]]*$' | tr '\n' '|' | sed 's/|$//')
# Written as a negated bracket expression rather than \b: \b is a GNU
# extension, and a verdict that depends on how the runner's grep was built is
# a verdict nobody can act on. Letters only, so a digit beside a blocked token
# still matches.
expression='(^|[^a-zA-Z])('"$pattern"')([^a-zA-Z]|$)'

# A ref name reaches CI from `github.head_ref`, which on a pull request from a
# fork is chosen by whoever opened it. GitHub Actions parses workflow commands
# off a step's stdout, so nothing from a ref name is echoed back without
# passing through here first: control characters are dropped and the ::
# introducer is broken up, leaving the only workflow commands this script
# emits the ones it writes itself.
quote_untrusted() {
    printf '%s' "$1" | tr -d '\000-\010\013-\037\177' | sed 's/::/: :/g'
}

status=0
examined=0
refused=0

# carries_blocked_token <name> — 0 when the name carries one, 1 when it does
# not. Exits 2 rather than answering when the search itself could not run:
# grep exits 1 on no match, which is the normal case, and anything above that
# means a pattern this grep cannot compile. A gate that reports "clean"
# because its search failed is worse than no gate, because everyone believes
# it.
carries_blocked_token() {
    _found=0
    printf '%s\n' "$1" | grep -q -i -E "$expression" || _found=$?
    if [ "$_found" -gt 1 ]; then
        echo "::error::the ref name lint could not search the name it was given (grep exited $_found); it went unjudged." >&2
        exit 2
    fi
    return "$_found"
}

# judge_name <ref> <apply-rule-2: yes|no> <prefix>
#
# The prefix is written in front of every message, so a name read out of a
# commit subject can name the commit it came from while a name given as an
# argument says nothing extra. Sets `refused` and `status`; a name that breaks
# both rules is one refusal reported twice, because it is one rename.
judge_name() {
    _ref=$1
    _rule2=$2
    _prefix=$3
    _bad=0

    if carries_blocked_token "$_ref"; then
        _bad=1
        printf '::error::%sthe ref name "%s" carries an assistant or vendor name. A merged branch name is written verbatim into the merge commit, and Principle I forbids naming one there.\n' \
            "$_prefix" "$(quote_untrusted "$_ref")" >&2
    fi

    # Rule 2, branches only. `refs/tags/` is skipped rather than accepted:
    # tags have their own naming rule (release_semver.sh owns it), and rule 1
    # has already judged this one.
    case "$_ref" in
        refs/tags/*) _name='' ;;
        refs/heads/*) _name=${_ref#refs/heads/} ;;
        *) _name=$_ref ;;
    esac
    [ "$_rule2" = yes ] || _name=''

    case "$_name" in
        # Nothing to judge: a tag, or a name read out of a commit subject.
        '') ;;
        # The trunk.
        main) ;;
        # The convention. `?*` requires a slug: `xcoder/` alone names nothing.
        "$BRANCH_PREFIX"?*) ;;
        # Refs whose names nobody here chooses. GitHub's revert button builds
        # `revert-<pr>-<branch>`, and the dependency bots build their own
        # paths; refusing them would mean refusing a button in the UI.
        revert-*|dependabot/*|renovate/*) ;;
        *)
            _bad=1
            printf '::error::%sthe branch "%s" is not named "%s<slug>". Every branch here is, and "main" is the only exception.\n' \
                "$_prefix" "$(quote_untrusted "$_ref")" "$BRANCH_PREFIX" >&2
            ;;
    esac

    if [ "$_bad" -ne 0 ]; then
        status=1
        refused=$((refused + 1))
    fi
}

if [ "$1" = --from-messages ]; then
    shift
    if [ "$#" -eq 0 ]; then
        echo "::error::--from-messages needs a commit range; nothing was examined. Usage: $0 --from-messages <range> (for example origin/main..HEAD, or '<sha> -1')" >&2
        exit 2
    fi

    work=$(mktemp -d)
    trap 'rm -rf "$work"' EXIT INT TERM

    # THIS FAILS CLOSED. The output is materialized to a file rather than
    # piped, because POSIX sh has no pipefail: in `git log … | awk …` the
    # status is awk's, and a git that could not resolve the range would
    # produce an empty pipe, no names, and a green step. The trailing `--`
    # keeps a range from being read as a path.
    if ! git log --format='%H %s' "$@" -- > "$work/subjects"; then
        echo "::error::git log could not read the range: $*. Nothing was examined, so this lint cannot report success." >&2
        exit 2
    fi
    commits=$(wc -l < "$work/subjects" | tr -d ' ')
    if [ "$commits" -eq 0 ]; then
        echo "::error::the range $* holds no commits; this lint would have reported success without examining anything." >&2
        exit 2
    fi

    # Written to a file rather than inlined so the program can use plain
    # single quotes: the ref names git writes are quoted with them, and an awk
    # program inside a shell string cannot say so without becoming unreadable.
    cat > "$work/extract.awk" <<'AWK'
# Pull ref names out of the three merge subjects that carry one. A squash
# merge's subject is the pull request title and names no branch, which is why
# nothing here looks for one.
BEGIN { Q = "\047" }
{
    commit = $1
    line = $0
    sub(/^[^ ]+ /, "", line)

    # GitHub, server-side: "Merge pull request #349 from <owner>/<branch>".
    # The owner is a GitHub account, not part of the ref, so it is dropped -
    # otherwise every fork's owner name would be judged as a branch prefix.
    if (line ~ /^Merge pull request #[0-9]+ from [^ ]+/) {
        ref = line
        sub(/^Merge pull request #[0-9]+ from /, "", ref)
        sub(/ .*$/, "", ref)
        sub("^[^/]+/", "", ref)
        if (ref != "") print commit " " ref
        next
    }

    # git, locally: "Merge branch 'a'", optionally "… into b", and the
    # remote-tracking form whose first segment is the remote rather than the
    # ref. Both sides are printed: a merge names two branches and either can
    # be the offending one.
    if (line ~ /^Merge (remote-tracking )?branch /) {
        remote = (line ~ /^Merge remote-tracking branch /)
        rest = line
        sub(/^Merge (remote-tracking )?branch /, "", rest)
        if (substr(rest, 1, 1) == Q) {
            rest = substr(rest, 2)
            close_quote = index(rest, Q)
            if (close_quote > 1) {
                ref = substr(rest, 1, close_quote - 1)
                if (remote) sub("^[^/]+/", "", ref)
                if (ref != "") print commit " " ref
                rest = substr(rest, close_quote + 1)
            }
        }
        if (match(rest, / into .+$/)) {
            into = substr(rest, RSTART + 6)
            if (into != "") print commit " " into
        }
    }
}
AWK

    if ! awk -f "$work/extract.awk" "$work/subjects" > "$work/refs"; then
        echo "::error::the ref names could not be read out of $commits commit subject(s); they went unjudged." >&2
        exit 2
    fi

    while IFS=' ' read -r commit ref; do
        [ -n "$ref" ] || continue
        examined=$((examined + 1))
        judge_name "$ref" no "$commit: "
    done < "$work/refs"

    printf 'ref name lint: read %d commit subject(s) over %s, naming %d ref(s); %d refused\n' \
        "$commits" "$*" "$examined" "$refused"
else
    for ref in "$@"; do
        # An empty name is the way this lint goes quiet. On a push event
        # `github.head_ref` is empty, and a workflow that passes it through
        # unguarded would run this script with one blank argument, examine
        # nothing, and report green.
        if [ -z "$ref" ]; then
            echo "::error::an empty ref name was given; there is nothing to judge, so this lint cannot report success. Pass the branch under test, or do not run this step." >&2
            exit 2
        fi
        examined=$((examined + 1))
        judge_name "$ref" yes ''
    done

    printf 'ref name lint: examined %d ref name(s); %d refused\n' "$examined" "$refused"
fi

if [ "$status" -ne 0 ]; then
    cat >&2 <<'EOF'

Rename the branch before it is merged - afterwards the name is in `main`'s
history and only a rewrite removes it:

  git branch -m xcoder/<slug>
  git push -u origin xcoder/<slug>
  git push origin --delete <old-name>

If a pull request is already open for the old branch, close it and open one
from the renamed branch. Neither rule has an exception worth taking: naming a
vendor because the work integrates its API is answered by naming the branch
for the capability instead, and a prefix that is not `xcoder/` is answered by
the rename above.

A name reported against a commit is already in history and cannot be renamed.
If that commit is one this branch ADDS, the branch has to be rebuilt on a
correctly named branch; if it is one the branch merely inherits, the range is
wrong - this lint judges what a branch adds, not what it was cut from.
EOF
    printf '::error::%d of %d ref name(s) refused\n' "$refused" "$examined"
    exit 1
fi
