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
# WHAT THIS DOES NOT DO
#
# It does not read git. It judges the names it is handed, which is what lets
# it run before a branch has any commits on it - in .githooks/pre-push, and in
# CI against the head branch of a pull request. A check that needed history
# could only fire after the history it was meant to prevent existed.
#
# ---------------------------------------------------------------------------
# Usage: lint-refs.sh <ref-name>...
#
# Exit 0 when every name is acceptable, 1 when one is refused, and 2 when the
# lint could not judge - no arguments, or an empty name. A run that examines
# nothing is an error, never a pass: on a push event `github.head_ref` is the
# empty string, and a gate that reports success on it is a gate that is off.
set -eu

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

    # grep exits 1 when it finds nothing, which is the normal case; anything
    # above that means a pattern this grep cannot compile. A gate that reports
    # "clean" because its search failed is worse than no gate, because
    # everyone believes it.
    found=0
    printf '%s\n' "$ref" | grep -q -i -E "$expression" || found=$?
    if [ "$found" -gt 1 ]; then
        echo "::error::the ref name lint could not search the name it was given (grep exited $found); it went unjudged." >&2
        exit 2
    fi
    if [ "$found" -eq 0 ]; then
        status=1
        refused=$((refused + 1))
        printf '::error::the ref name "%s" carries an assistant or vendor name. A merged branch name is written verbatim into the merge commit, and Principle I forbids naming one there.\n' \
            "$(quote_untrusted "$ref")" >&2
    fi
done

printf 'ref name lint: examined %d ref name(s); %d refused\n' "$examined" "$refused"

if [ "$status" -ne 0 ]; then
    cat >&2 <<'EOF'

Rename the branch before it is merged - afterwards the name is in `main`'s
history and only a rewrite removes it:

  git branch -m xcoder/<slug>
  git push -u origin xcoder/<slug>
  git push origin --delete <old-name>

If a pull request is already open for the old branch, close it and open one
from the renamed branch. Naming a vendor because the work integrates its API
is not an exception: name the branch for the capability instead.
EOF
    echo "::error::ref names carrying an assistant or vendor name"
    exit 1
fi
