#!/bin/sh
# Brand-literal lint — ADR-0004, FR-070, constitution "Rebrandability".
#
# A rebrand must be a configuration change plus an asset swap. That is only
# true if no product name, domain, support address or colour is written into
# application code, templates or migrations, so this greps the tracked tree
# for the current brand's literals and fails on a hit that is not accounted
# for.
#
# ---------------------------------------------------------------------------
# WHAT IS OUT OF SCOPE, AND WHY
#
# Whole path classes are skipped because FR-070 is about application code,
# templates and migrations:
#
#   docs/ specs/ .specify/ README.md   Documentation and history. The ADR
#                                      names these as the genuine references
#                                      an allowlist has to make room for: a
#                                      changelog cannot be rebranded, it
#                                      records what was true at the time.
#   .github/ deploy/                   Deployment and CI configuration. Host
#                                      names there address infrastructure,
#                                      not members, and a rebrand changes
#                                      them by changing the deployment.
#   internal/platform/brand/           The brand configuration itself and its
#   web/src/lib/brand/                 two readers. This is where the
#                                      literals are SUPPOSED to be.
#   scripts/lint-brand-literals.sh     This file, which has to name what it
#   scripts/lint-brand-literals_test.sh  forbids in order to forbid it, and
#                                      its tests, which have to write the
#                                      forbidden thing down to prove it is
#                                      caught.
#
# ---------------------------------------------------------------------------
# THE BUDGET TABLE, AND WHAT IT IS NOT
#
# The tree already carries brand literals: this repository shipped a news
# product before it had a brand configuration, and its surfaces render the
# product name directly. Driving those to zero is issue #275. Until then the
# table below records, per file, exactly how many hits are known and
# accepted — frozen at the state of the tree on the day this lint landed.
#
# That is a ratchet, not an amnesty. A file over its budget FAILS, so a new
# literal cannot be added to an already-listed file; a file with no budget
# fails on its first hit, so a new file cannot introduce one at all. A file
# under its budget only warns, so removing a literal never breaks somebody
# else's pull request — it just leaves a note asking for the number to come
# down with it.
#
# The patterns are deliberately not weakened to make the table shorter.
set -eu

cd "$(git rev-parse --show-toplevel)" || exit 1

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT INT TERM
hits="$work/hits"
: > "$hits"

# The brand's proper nouns, domains, addresses and colours. Every pattern is
# matched case-insensitively: an upper-case hex colour is still a colour, and
# a product name is still the product name however it is capitalised.
#
# Word boundaries are written out as POSIX ERE - "followed by something that
# is not more of the same, or by the end of the line" - rather than with \b.
# \b is a GNU extension that git's regex engine may or may not provide
# depending on how it was built, and a lint whose verdict depends on the
# contributor's machine is a lint nobody can act on.
NAME_PATTERN='epiloyes'
HOST_PATTERN='[a-z0-9._%+-]*@?(apivo|epiloyes)\.(com|net|org|news|app|io|eu|de|gr|example)([^a-z0-9]|$)'
# Six hex digits, or eight for the #rrggbbaa form. Seven is neither, and
# stays unmatched.
COLOUR_PATTERN='#[0-9a-f]{6}([0-9a-f]{2})?([^0-9a-f]|$)'
# Three-digit hex is only read as a colour inside a stylesheet. Everywhere
# else "#134" is an issue reference, and this repository is full of them.
SHORT_COLOUR_PATTERN='#[0-9a-f]{3}([^0-9a-f]|$)'

# scan RULE PATTERN [PATHSPEC...] — record every hit, tagged with the rule
# that found it.
#
# THIS FAILS CLOSED. git grep exits 1 when it finds nothing, which is the
# normal case; any other non-zero status means a broken pattern, an
# unsupported flag or a repository state this lint cannot read, and it stops
# the run. A merge gate that reports "clean" because its search failed is
# worse than no gate at all, because everyone believes it.
#
# The grep is deliberately NOT part of a pipeline. POSIX sh has no pipefail,
# so the status of `git grep | sed` is sed's, and git grep's failure would be
# invisible however carefully it was inspected afterwards. It writes to a
# file; sed reads the file.
scan() {
    rule="$1"
    pattern="$2"
    shift 2

    found=0
    git grep --untracked -nIiE "$pattern" -- "$@" \
        ':(exclude)docs/' \
        ':(exclude)specs/' \
        ':(exclude).specify/' \
        ':(exclude)README.md' \
        ':(exclude).github/' \
        ':(exclude)deploy/' \
        ':(exclude)internal/platform/brand/' \
        ':(exclude)web/src/lib/brand/' \
        ':(exclude)scripts/lint-brand-literals.sh' \
        ':(exclude)scripts/lint-brand-literals_test.sh' \
        > "$work/raw" || found=$?

    if [ "$found" -gt 1 ]; then
        echo "brand lint: git grep failed with status $found while scanning for $rule" >&2
        echo "::error::the brand-literal lint could not search the tree" >&2
        exit 2
    fi

    sed "s/^/$rule|/" "$work/raw" >> "$hits"
}

# Anything echoed back out of the tree goes through here first.
#
# This lint runs on pull requests, so the lines it quotes were written by
# whoever opened one. GitHub Actions parses workflow commands out of a step's
# stdout, and a carriage return inside a quoted line is enough to have the
# rest of it read as the beginning of a new one - which forges an annotation,
# or switches command parsing off for the remainder of the job with
# ::stop-commands::. Control characters are dropped (tab and newline
# excepted, the second being the record separator) and the :: introducer is
# broken up. The only workflow commands this script emits are the ones it
# writes itself.
quote_from_tree() {
    tr -d '\000-\010\013-\037\177' | sed 's/::/: :/g'
}

scan "product name" "$NAME_PATTERN" '*'
scan "brand domain or address" "$HOST_PATTERN" '*'
scan "colour" "$COLOUR_PATTERN" '*'
scan "colour" "$SHORT_COLOUR_PATTERN" '*.css'

# A CITATION of a spec directory is a reference to a document, not a brand
# literal, and it is the same exemption specs/ itself already has: a
# specification records what was true when it was written, and its directory
# is named after the feature - so `specs/001-epiloyes-alpha/contracts/` is
# the address of a file that exists. Renaming it to satisfy this lint would
# break every reference to it and rewrite history to say something that was
# never true.
#
# Only the product-name rule is filtered, and only where the name appears
# solely inside such a path. A spec directory cannot contain a domain or a
# colour, so the other rules are untouched; and a line that cites a spec AND
# names the brand separately still fails, because the test below removes the
# path first and then looks again at what is left.
uncited="$work/uncited"
: > "$uncited"
while IFS= read -r hit; do
    case "$hit" in
        "product name|"*) ;;
        *) printf '%s\n' "$hit" >> "$uncited"; continue ;;
    esac
    # RULE|path:line:content - strip the two positions, keep the text.
    text=${hit#*|}
    text=${text#*:}
    text=${text#*:}
    if printf '%s\n' "$text" \
        | sed -E 's#specs/[0-9]{3}-[A-Za-z0-9._-]+##g' \
        | grep -qiE "$NAME_PATTERN"; then
        printf '%s\n' "$hit" >> "$uncited"
    fi
done < "$hits"
mv "$uncited" "$hits"

# budgets — the hits this lint already knows about, per file.
#
# Every entry is a real literal that predates the brand configuration.
# Nothing may be added here to make a new change pass: a new literal is the
# thing this lint exists to stop.
budgets() {
    cat <<'EOF'
# The news product's member-facing surfaces. Each renders the product name
# directly instead of interpolating it from a brand token (#275).
web/src/pages/404.astro 2
web/src/pages/500.astro 2
web/src/pages/503.astro 2
web/src/pages/index.astro 1
web/src/pages/[lang]/about.astro 2
web/src/pages/[lang]/register.astro 1
web/src/pages/[lang]/setup.astro 1
web/src/pages/[lang]/[place]/index.astro 1
web/src/pages/[lang]/[place]/a/[id].astro 1
web/src/pages/[lang]/editor/audit.astro 1
web/src/pages/[lang]/editor/index.astro 1
web/src/pages/[lang]/editor/signin.astro 1
web/src/pages/[lang]/editor/sources.astro 2
web/src/components/SiteFooter.astro 1
web/src/components/SiteFooter.test.ts 3

# Translation catalogues and the copy that quotes the product name inside a
# translated sentence — FR-071's "interpolated token, never part of a
# translated string" is exactly what these have not done yet.
web/src/lib/reader/strings.ts 5
web/src/lib/reader/fixtures.ts 4
web/src/lib/editorial/fixtures.ts 2

# Comments and error text naming the product rather than the brand.
web/src/lib/usage.ts 1

# Test data on the brand's own domain: a rebrand should move these mailboxes
# and origins with it, so they are literals like any other.
web/src/lib/csrf.test.ts 3
web/src/lib/editorial/session.test.ts 5
web/src/middleware.test.ts 1

# The design system, vendored with its palette written out. Every colour
# here is a brand value that a brand configuration should supply; this is
# the single largest item in #275.
web/src/styles/modernist.css 39
EOF
}

# Files with at least one hit, and how many LINES carry one. Counting lines
# rather than pattern matches keeps a budget readable: one line quoting the
# product name at a brand domain is one literal to remove, not two.
counts="$work/counts"
cut -d'|' -f2- "$hits" | cut -d: -f1,2 | sort -u | cut -d: -f1 | uniq -c \
    | awk '{ count = $1; $1 = ""; sub(/^ /, ""); print $0 "\t" count }' > "$counts"

budgets | grep -vE '^#|^[[:space:]]*$' > "$work/budgets"

# One pass over both tables: what is over budget fails, what is under it (or
# gone entirely) leaves a note so the table cannot outlive what it describes.
awk -F'\t' '
    NR == FNR {
        split($0, entry, " ")
        budget[entry[1]] = entry[2]
        next
    }
    {
        path = $1
        count = $2 + 0
        allowed = (path in budget) ? budget[path] + 0 : 0
        seen[path] = 1
        if (count > allowed) {
            printf "FAIL\t%s\t%d\t%d\n", path, count, allowed
        } else if (count < allowed) {
            printf "NOTE\t%s\t%d\t%d\n", path, count, allowed
        }
    }
    END {
        for (path in budget) {
            if (!(path in seen)) {
                printf "NOTE\t%s\t0\t%d\n", path, budget[path]
            }
        }
    }
' "$work/budgets" "$counts" | sort > "$work/verdict"

status=0
while IFS="$(printf '\t')" read -r verdict path count budget; do
    if [ "$verdict" = "FAIL" ]; then
        # Set before a single line is printed, so nothing that happens
        # while reporting can turn the verdict green again.
        status=1

        matched=0
        grep -F "|$path:" "$hits" > "$work/matched" || matched=$?
        if [ "$matched" -gt 1 ]; then
            echo "brand lint: could not re-read the recorded hits" >&2
            exit 2
        fi

        {
            echo "FAIL $path: $count brand literals, $budget accounted for"
            sed 's/^\([^|]*\)|\(.*\)$/    \2   <- \1/' "$work/matched"
        } | quote_from_tree
    else
        echo "note $path: $count of the $budget budgeted brand literals remain — lower the budget in $0" | quote_from_tree
    fi
done < "$work/verdict"

if [ "$status" -ne 0 ]; then
    cat >&2 <<'EOF'

A brand literal must not live in application code, a template or a
migration (ADR-0004, FR-070). Resolve the value from the brand
configuration instead:

  Go          internal/platform/brand — a Brand passed from the
              composition root, never a package-level value.
  TypeScript  web/src/lib/brand — brandCustomProperties() for colours and
              type, the loaded Brand for names, domains and addresses.
  Copy        a translation catalogue, with the brand name interpolated as
              a token rather than written into the translated string.
EOF
    echo "::error::brand literals found outside the brand configuration"
    exit 1
fi

echo "no unaccounted brand literals"
