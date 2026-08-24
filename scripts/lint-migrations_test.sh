#!/bin/sh
# Unit-style tests for lint-migrations.sh. CI runs these before trusting the
# lint's verdict, for the reason the release guard has its own suite: a gate
# is only as good as the proof that it closes.
#
# Every case is a throwaway migration directory under mktemp. The repository's
# own migrations are checked too, in the last case - the lint has to be true
# about the schema that exists today, not only about fixtures.
set -eu

LINT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/lint-migrations.sh
MIGRATIONS=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)/internal/platform/db/migrations

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

FAILS=0
N=0
DIR=

# fixture writes one migration into a fresh directory and points DIR at it.
fixture() {
    N=$((N + 1))
    DIR="$TMP/case$N"
    mkdir -p "$DIR"
    printf '%s\n' "$1" >"$DIR/0010_fixture.up.sql"
}

# expect_pass <description> <sql>
expect_pass() {
    fixture "$2"
    if out=$(sh "$LINT" "$DIR" 2>&1); then
        echo "ok: $1"
    else
        echo "FAIL: $1 - the lint refused SQL that respects the boundary: $out"
        FAILS=1
    fi
}

# expect_fail <description> <required-message-fragment> <sql>
expect_fail() {
    fixture "$3"
    if out=$(sh "$LINT" "$DIR" 2>&1); then
        echo "FAIL: $1 - the lint allowed a cross-boundary foreign key: $out"
        FAILS=1
    elif ! printf '%s' "$out" | grep -q -F "$2"; then
        echo "FAIL: $1 - refused with the wrong message: $out"
        FAILS=1
    else
        echo "ok: $1"
    fi
}

################################################################################
# The rule itself.
################################################################################

expect_fail "cashback referencing a news table" \
    "crosses the product schema boundary" \
    'create table cashback.merchant (
    id uuid primary key,
    article_id uuid not null references public.article (id)
);'

expect_fail "cashback referencing an unqualified news table" \
    "crosses the product schema boundary" \
    'create table cashback.merchant (
    id uuid primary key,
    article_id uuid not null references article (id)
);'

expect_fail "cashback referencing another product schema" \
    "from one product schema into another" \
    'create table cashback.merchant (
    id uuid primary key,
    listing_id uuid not null references marketplace.listing (id)
);'

expect_fail "a shared table depending on a product schema" \
    "points the wrong way" \
    'alter table public.account
    add column preferred_merchant uuid references cashback.merchant (id);'

expect_fail "a foreign key added as a table constraint" \
    "crosses the product schema boundary" \
    'alter table cashback.click
    add constraint click_article_fk foreign key (article_id) references public.article (id);'

expect_pass "cashback referencing the shared reference data" \
    'create table cashback.member (
    account_id uuid primary key references public.account (id),
    place_id uuid not null references public.place (id),
    lang text not null references public.language (code)
);'

expect_pass "cashback referencing its own tables" \
    'create table cashback.offer (
    id uuid primary key,
    merchant_id uuid not null references cashback.merchant (id)
);'

expect_pass "news referencing news" \
    'create table public.article_place (
    article_id uuid not null references public.article (id),
    place_id uuid not null references place (id)
);'

################################################################################
# Reading SQL correctly. Every one of these would be a false verdict from a
# grep, and a lint is trusted for its silence as much as for its noise.
################################################################################

expect_pass "a cross-schema foreign key written only in a line comment" \
    '-- cashback.merchant used to have: references public.article (id)
create table cashback.merchant (id uuid primary key);'

expect_pass "a cross-schema foreign key written only in a block comment" \
    '/* draft:
   create table cashback.merchant (a uuid references public.article (id));
*/
create table cashback.merchant (id uuid primary key);'

expect_pass "the word references inside a string literal" \
    "create table cashback.merchant (id uuid primary key);
comment on table cashback.merchant is 'references public.article; historical note';"

# shellcheck disable=SC2016 # the dollar quoting is SQL, not shell expansion.
expect_pass "the word references inside a dollar-quoted function body" \
    'create function cashback.guard() returns trigger
language plpgsql
as $$
begin
    raise exception $x$references public.article (id)$x$;
end;
$$;
create table cashback.merchant (id uuid primary key);'

expect_pass "grant references, which is a privilege and not a foreign key" \
    'create table cashback.merchant (id uuid primary key);
grant references on public.article to cashback_app;'

expect_pass "a column whose name begins with references" \
    'create table cashback.merchant (
    id uuid primary key,
    references_count integer not null default 0
);'

expect_fail "a foreign key split across lines" \
    "crosses the product schema boundary" \
    'create table cashback.click (
    id uuid primary key,
    article_id uuid not null
        references
        public.article (id)
);'

expect_fail "quoted identifiers" \
    "crosses the product schema boundary" \
    'create table "cashback"."click" (
    article_id uuid not null references "public"."article" ("id")
);'

expect_fail "upper case SQL" \
    "crosses the product schema boundary" \
    'CREATE TABLE CASHBACK.CLICK (
    ARTICLE_ID UUID NOT NULL REFERENCES PUBLIC.ARTICLE (ID)
);'

expect_fail "a foreign key with no space before the parenthesis" \
    "crosses the product schema boundary" \
    'create table cashback.click (
    article_id uuid not null references public.article(id)
);'

expect_fail "a statement that never met its semicolon" \
    "crosses the product schema boundary" \
    'create table cashback.click (
    article_id uuid not null references public.article (id)
)'

################################################################################
# search_path: a migration written from inside its own schema must be read
# the way Postgres would read it, or the lint blames the wrong schema.
################################################################################

expect_fail "an unqualified table under search_path = cashback" \
    "cashback.click references public.article" \
    'set search_path = cashback;
create table click (
    article_id uuid not null references public.article (id)
);'

expect_pass "an unqualified reference resolved inside cashback" \
    'set search_path to cashback, public;
create table offer (
    merchant_id uuid not null references merchant (id)
);'

################################################################################
# The report, and the ways this lint could pass without checking anything.
################################################################################

expect_fail "the violation names the line it is on" \
    "line=3" \
    'create table cashback.click (
    id uuid primary key,
    article_id uuid not null references public.article (id)
);'

expect_fail "every violation is reported, not just the first" \
    "cashback.click references public.source_item" \
    'create table cashback.click (
    article_id uuid not null references public.article (id),
    source_item_id uuid not null references public.source_item (id)
);'

N=$((N + 1))
mkdir -p "$TMP/empty"
if out=$(sh "$LINT" "$TMP/empty" 2>&1); then
    echo "FAIL: a directory holding no migrations - the lint passed without reading anything: $out"
    FAILS=1
elif ! printf '%s' "$out" | grep -q -F "without reading anything"; then
    echo "FAIL: a directory holding no migrations - refused with the wrong message: $out"
    FAILS=1
else
    echo "ok: a directory holding no migrations"
fi

if out=$(sh "$LINT" "$TMP/does-not-exist" 2>&1); then
    echo "FAIL: a path that does not exist - the lint passed: $out"
    FAILS=1
else
    echo "ok: a path that does not exist"
fi

################################################################################
# The schema that actually exists.
################################################################################

if out=$(sh "$LINT" "$MIGRATIONS" 2>&1); then
    echo "ok: the repository's own migrations"
else
    echo "FAIL: the repository's own migrations were refused: $out"
    FAILS=1
fi

exit "$FAILS"
