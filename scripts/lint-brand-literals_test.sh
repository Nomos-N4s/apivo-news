#!/bin/sh
# Unit-style tests for lint-brand-literals.sh. A lint nobody proves is a
# lint that quietly stops finding things: every case below is either a
# literal that MUST be caught or a look-alike that must NOT be, and the
# look-alikes are the ones that decide whether the lint stays switched on.
#
# Everything happens in throwaway repositories under mktemp - the repository
# this script lives in is never touched. HOME points into the scratch area
# and GIT_CONFIG_NOSYSTEM is set, so the operator's global git configuration
# (signing, hooks, default branch) cannot leak into the fixtures.
set -eu

LINT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/lint-brand-literals.sh

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
export HOME="$TMP"
export GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@invalid

FAILS=0
CASES=0
REPO=""

# fixture - a fresh repository with one committed file and nothing else, so
# each case is judged only on what it adds.
fixture() {
    CASES=$((CASES + 1))
    REPO="$TMP/case$CASES"
    git init -q -b main "$REPO"
    printf 'nothing to declare\n' > "$REPO/placeholder.txt"
    (cd "$REPO" && git add placeholder.txt && git commit -q -m "chore: fixture")
}

# write <path> <line>... - a file in the fixture repository, left untracked
# unless the case commits it.
write() {
    path="$REPO/$1"
    shift
    mkdir -p "$(dirname "$path")"
    : > "$path"
    for line in "$@"; do
        printf '%s\n' "$line" >> "$path"
    done
}

commit_all() {
    (cd "$REPO" && git add -A && git commit -q -m "chore: fixture content")
}

# expect_clean <description>
expect_clean() {
    if ! out=$(cd "$REPO" && sh "$LINT" 2>&1); then
        echo "FAIL: $1 - the lint refused a tree it should accept:"
        printf '%s\n' "$out" | sed 's/^/    /'
        FAILS=1
    elif printf '%s' "$out" | grep -q '^FAIL '; then
        echo "FAIL: $1 - the lint reported a hit but exited 0:"
        printf '%s\n' "$out" | sed 's/^/    /'
        FAILS=1
    else
        echo "ok: $1"
    fi
}

# expect_caught <description> <required-message-fragment>
expect_caught() {
    if out=$(cd "$REPO" && sh "$LINT" 2>&1); then
        echo "FAIL: $1 - the lint accepted a brand literal:"
        printf '%s\n' "$out" | sed 's/^/    /'
        FAILS=1
    elif ! printf '%s' "$out" | grep -q -F -e "$2"; then
        echo "FAIL: $1 - caught, but without saying \"$2\":"
        printf '%s\n' "$out" | sed 's/^/    /'
        FAILS=1
    else
        echo "ok: $1"
    fi
}

# ---------------------------------------------------------------------------
# A tree with nothing to find

fixture
expect_clean "a tree with no brand literals passes"

# ---------------------------------------------------------------------------
# The four rules, each catching what it exists for

fixture
write "web/src/pages/wallet.astro" '<h1>Welcome to epiloYES</h1>'
commit_all
expect_caught "the product name in a new template is caught" "product name"

fixture
write "internal/cashback/mail.go" 'const from = "support@apivo.com"'
commit_all
expect_caught "a support address on a brand domain is caught" "brand domain or address"

fixture
write "web/src/components/Card.astro" '<style>.card { color: #ec3013; }</style>'
commit_all
expect_caught "a brand colour is caught" "colour"

fixture
write "web/src/styles/cashback.css" '.wallet { color: #abc; }'
commit_all
expect_caught "a short hex colour inside a stylesheet is caught" "colour"

# ---------------------------------------------------------------------------
# The look-alikes. Each of these was a real false positive at some point,
# and each would have cost the lint its credibility.

fixture
write "internal/ingestion/poll.go" 'const lockName = "apivo.poll"'
write "web/src/lib/tour/progress.ts" 'const key = `apivo.tour.${id}`;'
commit_all
expect_clean "a namespaced identifier is not a domain"

fixture
write "internal/cashback/entry.go" '// Attribution rules live in issue #161, see also #134.'
commit_all
expect_clean "an issue reference is not a colour"

fixture
write "web/src/lib/cashback/api.ts" 'const issue = "#134";'
commit_all
expect_clean "three-digit hex outside a stylesheet is not a colour"

# ---------------------------------------------------------------------------
# What is deliberately out of scope

fixture
write "docs/adr/0004-white-label-rebranding.md" 'The current brand is epiloYES at apivo.com.'
write "specs/002-apivo-cashback-alpha/spec.md" 'epiloYES renders #ec3013.'
write ".github/workflows/example.yml" '# deploys epiloYES to apivo.com'
write "deploy/hetzner/environments.env" 'APIVO_PROD_HOST=apivo.com'
commit_all
expect_clean "documentation, history, CI and deployment configuration are out of scope"

fixture
write "internal/platform/brand/brands/live/brand.json" '{ "name": "epiloYES", "primary": "apivo.com", "bg": "#ec3013" }'
write "web/src/lib/brand/fixtures.ts" "export const host = 'apivo.com';"
commit_all
expect_clean "the brand configuration and its readers are where the literals belong"

# ---------------------------------------------------------------------------
# The budget table is a ratchet, not an amnesty

fixture
write "web/src/pages/404.astro" '<title>epiloYES — 404</title>' '<a>epiloYES</a>'
commit_all
expect_clean "a budgeted file at its budget passes"

fixture
write "web/src/pages/404.astro" '<title>epiloYES — 404</title>' '<a>epiloYES</a>' '<p>epiloYES</p>'
commit_all
expect_caught "one literal more than budgeted fails" "3 brand literals, 2 accounted for"

# ---------------------------------------------------------------------------
# A literal that has not been committed yet is still a literal: a
# contributor must not get a clean run and then push the hit.

fixture
write "web/src/pages/wallet.astro" '<h1>epiloYES</h1>'
expect_caught "an uncommitted file is scanned too" "product name"

if [ "$FAILS" -ne 0 ]; then
    echo "brand-literal lint tests FAILED"
    exit 1
fi
echo "all $CASES brand-literal lint cases passed"
