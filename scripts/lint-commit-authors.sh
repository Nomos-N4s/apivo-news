#!/bin/sh
# Sole authorship, checked on the commit's IDENTITY rather than on its message
# (constitution, Principle I — NON-NEGOTIABLE).
#
# Principle I is enforced in two places today, and both of them read the
# commit MESSAGE: .githooks/commit-msg strips attribution trailers before the
# commit is written, and the commit-hygiene job greps the message bodies of
# every commit in a range. Neither has ever looked at the two identities git
# records in the commit header itself — the author and the committer.
#
# That gap is not hypothetical. A development container that ships a global
# git identity for an assistant stamps that identity onto every commit made
# on it - author AND committer alike - while every message-body check passes.
# Nine branches reached review that way, all green; a person reading the
# commit list caught what CI could not. The messages were clean. The commit
# headers were not.
#
# ---------------------------------------------------------------------------
# WHY A BLOCKLIST AND NOT AN ALLOWLIST
#
# An allowlist of one address is the obvious reading of "every commit is
# authored solely by me", and it would be wrong three times over. GitHub
# records its own squash and merge commits as `GitHub <noreply@github.com>`,
# so an allowlist would refuse the merges this repository's history is made
# of. A second machine, a second address, or a future collaborator would be
# refused for existing. And a personal address would become a value CI
# asserts — published as policy, in a public repository, needing an edit here
# every time it changes.
#
# The blocklist names what must never appear instead. That set is far smaller
# and far more stable than the set of people who may legitimately commit, and
# per the rule .githooks/commit-msg already states, it over-matches on
# purpose: a name that merely resembles an assistant's costs one rename,
# while a name that slips through costs a rewritten branch after review.
#
# ---------------------------------------------------------------------------
# WHAT A GREEN RUN MEANS, AND WHAT IT DOES NOT
#
# It means no commit in the range records an assistant, an agent or one of
# their vendors as its author or its committer, in either the name or the
# address.
#
# It does NOT mean a human typed the code. Nothing in a commit records that,
# and a check that reads commit headers cannot learn it — an identity is
# whatever the machine writing the commit was configured to claim. What this
# closes is the exact mechanical failure above: a tool writing its own name
# into the commit header, unnoticed, all the way to review. Stated plainly
# because a green check is trusted for more than it can carry.
#
# ---------------------------------------------------------------------------
# Usage: lint-commit-authors.sh <git-log-argument>...
#
# The arguments are handed to `git log` as given, so both shapes the
# commit-hygiene job produces work: a `base..head` range, and `<sha> -1` for
# the first push to a ref, where there is no earlier commit to measure from.
# There is deliberately no default — a run that examines nothing is an error,
# never a pass.
#
# Output is one ::error:: line per offending identity, naming the commit, the
# field and the identity, which surfaces in the GitHub Actions annotations UI
# and reads as plain text anywhere else, plus one summary line stating how
# many commits were actually examined. The exit code is the verdict: 1 for a
# violation, 2 for a run that could not judge.
set -eu

# The identities that must never author or commit here.
#
# Each entry is matched case-insensitively and delimited by non-letters, so
# `claude` catches `Claude`, `CLAUDE`, `claude-code`, `claude.bot@…` and
# `claude2@…`, while leaving `Claudette` and `Claudia` alone. Digits count as
# a delimiter on purpose — a versioned identity is the same identity.
#
# The comment on each entry is the argument for keeping it. Anything added
# here needs the same: an entry nobody can justify is an entry the next
# person deletes.
blocklist() {
    cat <<'EOF'
# The vendor. One entry covers noreply@anthropic.com, every other address at
# that domain, its subdomains, and the bare name in an author field.
anthropic
# The assistant, and the identity the development container actually shipped —
# this is the exact string that reached review.
claude
# GitHub's assistant. Its bot identity is `Copilot <…+Copilot@users.noreply
# .github.com>`, which is why the address alone is not enough to judge by.
copilot
# The vendor behind the assistants below, as it appears in agent identities
# and in @openai.com addresses.
openai
# The assistant by name. `gpt` on its own cannot catch it: the delimiter rule
# means the `chat` in front of it is not a boundary.
chatgpt
# The model family as agents write it — GPT-4, gpt-5-codex, Auto-GPT.
gpt
# Google's assistant.
gemini
# The coding agent, which commits under its own name when driven headlessly.
codex
# The editor's background agent, which commits as itself rather than as the
# person driving it.
cursor
# The assistant and the editor built around it.
codeium
# The terminal coding agent, which sets its own committer identity by default.
aider
EOF
}

# DELIBERATELY NOT ON THE LIST, because each would refuse a real person or a
# legitimate machine, and a gate that cries wolf is a gate someone switches
# off:
#
#   ai          `Ai` is a given name, and `ai` appears inside `gmail.com`.
#   bot         GitHub's own automation is legitimate here; so is any future
#               release bot. This lint judges AI attribution, not automation.
#   assistant   A human mailbox at a company, plausibly.
#   devin, cody, bard
#               All three are ordinary human names before they are agents.
#
# The right instrument for a machine identity that is not an AI identity is
# branch protection, not this list.

if [ "$#" -eq 0 ]; then
    echo "::error::no commit range given; nothing was examined. Usage: $0 <range> (for example origin/main..HEAD, or '<sha> -1')" >&2
    exit 2
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

# One line per identity, two lines per commit, in a shape that survives being
# read back: the commit id first, then the field, then the identity, which is
# the only part that can hold spaces.
#
# %an/%ae and %cn/%ce are the RAW recorded identities. Their %aN/%aE
# counterparts apply .mailmap, and a .mailmap is a file in the tree — reading
# through it would let the same commit that carries a bad identity carry the
# rewrite that hides it.
#
# Merge commits are included, exactly as the trailer step above this one
# includes them: a merge is a commit with an author and a committer of its
# own, and a merge made by the wrong identity is the same violation.
#
# THIS FAILS CLOSED. The output is materialized to a file rather than piped,
# because POSIX sh has no pipefail: in `git log … | grep …` the status is
# grep's, and a git that could not resolve the range would produce an empty
# pipe, no matches, and a green step. The trailing `--` keeps a range from
# being read as a path.
if ! git log --format='%H author %an <%ae>%n%H committer %cn <%ce>' "$@" -- > "$work/identities"; then
    echo "::error::git log could not read the range: $*. Nothing was examined, so this lint cannot report success." >&2
    exit 2
fi

# How many commits were actually read. The field name is generated by the
# format above, never by a contributor, so it is safe to count on.
commits=$(awk '$2 == "author" { n++ } END { print n + 0 }' "$work/identities")
if [ "$commits" -eq 0 ]; then
    echo "::error::the range $* holds no commits; this lint would have reported success without examining anything." >&2
    exit 2
fi

pattern=$(blocklist | grep -vE '^#|^[[:space:]]*$' | tr '\n' '|' | sed 's/|$//')
# The delimiter is written out as a negated bracket expression rather than as
# \b: \b is a GNU extension, and a gate whose verdict depends on how the
# runner's grep was built is a gate nobody can act on. Letters only, so that
# a digit beside a blocked token still matches.
expression='(^|[^a-zA-Z])('"$pattern"')([^a-zA-Z]|$)'

# Same fail-closed reading as the scan in lint-brand-literals.sh: grep exits 1
# when it finds nothing, which is the normal case, and anything above that
# means a pattern this grep cannot compile or a file it cannot read. A gate
# that reports "clean" because its search failed is worse than no gate,
# because everyone believes it.
found=0
grep -n -i -E "$expression" "$work/identities" > "$work/offenders" || found=$?
if [ "$found" -gt 1 ]; then
    echo "::error::the commit author lint could not search the identities it read (grep exited $found); $commits commit(s) went unjudged." >&2
    exit 2
fi

# Anything echoed back out of a commit header goes through here first.
#
# A name and an address are chosen by whoever opened the pull request. GitHub
# Actions parses workflow commands out of a step's stdout, and a carriage
# return inside a quoted identity is enough to have the rest of it read as the
# start of a new one — which forges an annotation, or switches command parsing
# off for the rest of the job with ::stop-commands::. Control characters are
# dropped and the :: introducer is broken up, so the only workflow commands
# this script emits are the ones it writes itself.
quote_from_history() {
    printf '%s' "$1" | tr -d '\000-\010\013-\037\177' | sed 's/::/: :/g'
}

status=0
offenders=0
previous=''
while IFS= read -r hit; do
    # Set before a single line is printed, so nothing that happens while
    # reporting can turn the verdict green again.
    status=1

    entry=${hit#*:}
    commit=${entry%% *}
    rest=${entry#* }
    field=${rest%% *}
    identity=${rest#* }

    # Both fields of one commit are adjacent in the file, so remembering the
    # last one is enough to count offending COMMITS rather than offending
    # fields.
    if [ "$commit" != "$previous" ]; then
        offenders=$((offenders + 1))
        previous=$commit
    fi

    printf '::error::%s: %s "%s" is an AI attribution identity. Every commit is authored solely by the founder (constitution, Principle I).\n' \
        "$commit" "$field" "$(quote_from_history "$identity")" >&2
done < "$work/offenders"

printf 'commit author lint: examined %d commit(s) over %s — the author and the committer of each; %d carrying an AI attribution identity\n' \
    "$commits" "$*" "$offenders"

if [ "$status" -ne 0 ]; then
    cat >&2 <<'EOF'

A commit records two identities, and Principle I applies to both. If a
container or a tool wrote its own identity into these commits, the commits
have to be rewritten — the header cannot be edited in place:

  git config user.name  "<your name>"
  git config user.email "<your address>"
  git rebase --exec 'git commit --amend --no-edit --reset-author' <base>

--reset-author rewrites the author of each commit; the rebase itself rewrites
the committer. Re-sign as you go: Principle I requires that too.
EOF
    echo "::error::commits recording an AI attribution identity"
    exit 1
fi
