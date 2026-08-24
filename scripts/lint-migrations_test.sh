#!/bin/sh
# Unit-style tests for lint-migrations.sh. CI runs these before trusting the
# lint's verdict, for the reason the release guard has its own suite: a gate
# is only as good as the proof that it closes.
#
# EVERY case asserts how much the lint actually READ, not only what it
# concluded. That is the difference between this suite and the one it
# replaces. The earlier version asserted exit status alone, and a reader
# mutated to never close a string literal - which blanks a file from its first
# apostrophe onward - scored 26 out of 26 green while the real migrations
# reported zero foreign keys instead of fourteen and exited 0. Four separate
# reader holes hid behind that. A lint's silence is trusted, so silence is
# what has to be proved.
#
# Every case is a throwaway migration directory under mktemp. The repository's
# own migrations are checked in the last case - the lint has to be true about
# the schema that exists today, not only about fixtures.
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

# fixture2 writes two migrations, applied in filename order.
fixture2() {
    N=$((N + 1))
    DIR="$TMP/case$N"
    mkdir -p "$DIR"
    printf '%s\n' "$1" >"$DIR/0010_first.up.sql"
    printf '%s\n' "$2" >"$DIR/0011_second.up.sql"
}

# read_count fails the case unless the lint reports reading exactly N foreign
# keys. This is the assertion that makes a blind reader visible: a reader that
# saw nothing concludes nothing, and "concluded nothing" and "found nothing
# wrong" are the same exit status.
read_count() {
    if ! printf '%s' "$3" | grep -q -F "and $2 foreign key(s)"; then
        echo "FAIL: $1 - expected the lint to read $2 foreign key(s): $3"
        FAILS=1
        return 1
    fi
    return 0
}

# expect_pass <description> <foreign-keys-read> <sql>
expect_pass() {
    fixture "$3"
    if out=$(sh "$LINT" "$DIR" 2>&1); then
        if read_count "$1" "$2" "$out"; then
            echo "ok: $1"
        fi
    else
        echo "FAIL: $1 - the lint refused SQL that respects the boundary: $out"
        FAILS=1
    fi
}

# expect_fail <description> <foreign-keys-read> <fragment> <sql>
expect_fail() {
    fixture "$4"
    expect_refusal "$1" "$2" "$3"
}

# expect_refusal judges whatever DIR currently holds.
expect_refusal() {
    if out=$(sh "$LINT" "$DIR" 2>&1); then
        echo "FAIL: $1 - the lint allowed it: $out"
        FAILS=1
    elif ! printf '%s' "$out" | grep -q -F "$3"; then
        echo "FAIL: $1 - refused with the wrong message: $out"
        FAILS=1
    elif read_count "$1" "$2" "$out"; then
        echo "ok: $1"
    fi
}

################################################################################
# The rule itself.
################################################################################

expect_fail "cashback referencing a news table" 1 \
    "crosses the product schema boundary" \
    'create table cashback.merchant (
    id uuid primary key,
    article_id uuid not null references public.article (id)
);'

expect_fail "cashback referencing an unqualified news table" 1 \
    "crosses the product schema boundary" \
    'create table cashback.merchant (
    id uuid primary key,
    article_id uuid not null references article (id)
);'

expect_fail "cashback referencing another product schema" 1 \
    "from one product schema into another" \
    'create table cashback.merchant (
    id uuid primary key,
    listing_id uuid not null references marketplace.listing (id)
);'

expect_fail "a shared table depending on a product schema" 1 \
    "points the wrong way" \
    'alter table public.account
    add column preferred_merchant uuid references cashback.merchant (id);'

expect_fail "a foreign key added as a table constraint" 1 \
    "crosses the product schema boundary" \
    'alter table cashback.click
    add constraint click_article_fk foreign key (article_id) references public.article (id);'

expect_pass "cashback referencing the shared reference data" 3 \
    'create table cashback.member (
    account_id uuid primary key references public.account (id),
    place_id uuid not null references public.place (id),
    lang text not null references public.language (code)
);'

expect_pass "cashback referencing its own tables" 1 \
    'create table cashback.offer (
    id uuid primary key,
    merchant_id uuid not null references cashback.merchant (id)
);'

expect_pass "news referencing news" 2 \
    'create table public.article_place (
    article_id uuid not null references public.article (id),
    place_id uuid not null references place (id)
);'

################################################################################
# Reading SQL correctly. Every one of these would be a false verdict from a
# grep, and a lint is trusted for its silence as much as for its noise.
################################################################################

expect_pass "a cross-schema foreign key written only in a line comment" 0 \
    '-- cashback.merchant used to have: references public.article (id)
create table cashback.merchant (id uuid primary key);'

expect_pass "a cross-schema foreign key written only in a block comment" 0 \
    '/* draft:
   create table cashback.merchant (a uuid references public.article (id));
*/
create table cashback.merchant (id uuid primary key);'

expect_pass "the word references inside a string literal" 0 \
    "create table cashback.merchant (id uuid primary key);
comment on table cashback.merchant is 'references public.article; historical note';"

# shellcheck disable=SC2016 # the dollar quoting is SQL, not shell expansion.
expect_pass "the word references inside a dollar-quoted message" 0 \
    'create function cashback.guard() returns trigger
language plpgsql
as $$
begin
    raise exception $x$references public.article (id)$x$;
end;
$$;
create table cashback.merchant (id uuid primary key);'

expect_pass "grant references, which is a privilege and not a foreign key" 0 \
    'create table cashback.merchant (id uuid primary key);
grant references on public.article to cashback_app;'

expect_pass "a column whose name begins with references" 0 \
    'create table cashback.merchant (
    id uuid primary key,
    references_count integer not null default 0
);'

expect_fail "a foreign key split across lines" 1 \
    "crosses the product schema boundary" \
    'create table cashback.click (
    id uuid primary key,
    article_id uuid not null
        references
        public.article (id)
);'

expect_fail "quoted identifiers" 1 \
    "crosses the product schema boundary" \
    'create table "cashback"."click" (
    article_id uuid not null references "public"."article" ("id")
);'

expect_fail "upper case SQL" 1 \
    "crosses the product schema boundary" \
    'CREATE TABLE CASHBACK.CLICK (
    ARTICLE_ID UUID NOT NULL REFERENCES PUBLIC.ARTICLE (ID)
);'

expect_fail "a foreign key with no space before the parenthesis" 1 \
    "crosses the product schema boundary" \
    'create table cashback.click (
    article_id uuid not null references public.article(id)
);'

# A quoted name needs no separator at all: references"public"."article" is
# legal, and the separator can only be whitespace or the opening quote.
expect_fail "a foreign key with no separator before a quoted name" 1 \
    "crosses the product schema boundary" \
    'create table cashback.click (
    article_id uuid not null references"public"."article"("id")
);'

expect_fail "a statement that never met its semicolon" 1 \
    "crosses the product schema boundary" \
    'create table cashback.click (
    article_id uuid not null references public.article (id)
)'

################################################################################
# The reader losing its place. Each of these blanks the rest of the file for a
# reader that tracks quoting naively, and each hides a real crossing behind it.
################################################################################

expect_fail "a foreign key after a string literal" 1 \
    "cashback.click references public.article" \
    "create table cashback.merchant (id uuid primary key);
comment on table cashback.merchant is 'a merchant, for example a retailer';
create table cashback.click (
    article_id uuid not null references public.article (id)
);"

expect_fail "an apostrophe inside a quoted identifier" 1 \
    "cashback.click references public.article" \
    "create table cashback.\"don't\" (id uuid primary key);
create table cashback.click (
    article_id uuid not null references public.article (id)
);"

expect_fail "a backslash-escaped quote inside an E-string" 1 \
    "cashback.click references public.article" \
    "create table cashback.merchant (id uuid primary key);
comment on table cashback.merchant is E'a merchant\\'s row';
create table cashback.click (
    article_id uuid not null references public.article (id)
);"

# shellcheck disable=SC2016 # the dollar quoting is SQL, not shell expansion.
expect_fail "conditional DDL inside a do block" 1 \
    "cashback.click references public.article" \
    'create table cashback.click (id uuid primary key, article_id uuid);
do $$
begin
    if not exists (select 1 from pg_constraint where conname = $q$click_article_fk$q$) then
        alter table cashback.click
            add constraint click_article_fk foreign key (article_id) references public.article (id);
    end if;
end $$;'

################################################################################
# search_path: a migration written from inside its own schema must be read the
# way Postgres would read it, or the lint blames the wrong schema.
################################################################################

expect_fail "an unqualified table under search_path = cashback" 1 \
    "cashback.click references public.article" \
    'set search_path = cashback;
create table click (
    article_id uuid not null references public.article (id)
);'

expect_fail "no space around the equals sign" 1 \
    "cashback.click references public.article" \
    'set search_path=cashback;
create table click (
    article_id uuid not null references public.article (id)
);'

expect_fail "SET LOCAL" 1 \
    "cashback.click references public.article" \
    'set local search_path=cashback;
create table click (
    article_id uuid not null references public.article (id)
);'

expect_fail "a quoted schema name, which the cleaner blanks" 1 \
    "cashback.click references public.article" \
    "set search_path = 'cashback';
create table click (
    article_id uuid not null references public.article (id)
);"

# Postgres resolves an unqualified name to the first schema on the path that
# actually CONTAINS the table, not simply the first schema on the path. Under
# the idiomatic two-entry path this is the difference between seeing a real
# crossing and blessing it as same-schema.
expect_fail "a news table reached through a two-entry search path" 1 \
    "cashback.click references public.article" \
    'create table public.article (id uuid primary key);
set search_path = cashback, public;
create table click (
    article_id uuid not null references article (id)
);'

expect_pass "a cashback table reached through a two-entry search path" 1 \
    'set search_path = cashback, public;
create table merchant (id uuid primary key);
create table offer (
    merchant_id uuid not null references merchant (id)
);'

expect_fail "an unqualified name no schema on the path is known to hold" 1 \
    "cannot be decided here" \
    'set search_path = cashback, public;
create table offer (
    merchant_id uuid not null references merchant (id)
);'

expect_pass "RESET search_path" 1 \
    'set search_path = cashback;
reset search_path;
create table article_place (
    article_id uuid not null references public.article (id)
);'

# A plain SET is session state, and golang-migrate holds one connection for
# the whole run, so migration N+1 really does execute under migration N's
# search_path. SET LOCAL ends with its transaction, which is the migration.
fixture2 'set search_path = cashback;
create table click (id uuid primary key);' \
    'create table receipt (
    article_id uuid not null references public.article (id)
);'
expect_refusal "a plain SET reaching the next migration" 1 "cashback.receipt references public.article"

fixture2 'set local search_path = cashback;
create table click (id uuid primary key);' \
    'create table receipt (
    article_id uuid not null references public.article (id)
);'
if out=$(sh "$LINT" "$DIR" 2>&1); then
    if read_count "SET LOCAL not reaching the next migration" 1 "$out"; then
        echo "ok: SET LOCAL not reaching the next migration"
    fi
else
    echo "FAIL: SET LOCAL not reaching the next migration - SET LOCAL leaked past its transaction: $out"
    FAILS=1
fi

################################################################################
# The ways this lint could report success without reading anything.
################################################################################

expect_fail "a literal that is never closed" 0 \
    "still inside a string literal" \
    "create table cashback.merchant (id uuid primary key);
comment on table cashback.merchant is 'never closed;
create table cashback.click (
    article_id uuid not null references public.article (id)
);"

expect_fail "a block comment that is never closed" 0 \
    "still inside a block comment" \
    'create table cashback.merchant (id uuid primary key);
/* never closed
create table cashback.click (
    article_id uuid not null references public.article (id)
);'

expect_fail "statement terminators that produced no statement" 0 \
    "produced no SQL statement" \
    '-- everything here is commented out, semicolons and all;
-- create table cashback.click (a uuid references public.article (id));'

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
# The schema that actually exists. The count is asserted as a floor rather
# than an equality: migrations only accumulate, so this can never be satisfied
# by a reader that stopped reading, and it does not break when 0010 and its
# successors land.
################################################################################

if out=$(sh "$LINT" "$MIGRATIONS" 2>&1); then
    read=$(printf '%s' "$out" | sed -n 's/.*and \([0-9]*\) foreign key(s).*/\1/p')
    if [ -z "$read" ]; then
        echo "FAIL: the repository's own migrations - no count in the summary: $out"
        FAILS=1
    elif [ "$read" -lt 14 ]; then
        echo "FAIL: the repository's own migrations - read only $read foreign key(s), and there were 14 before 0010 existed: $out"
        FAILS=1
    else
        echo "ok: the repository's own migrations ($read foreign keys read)"
    fi
else
    echo "FAIL: the repository's own migrations were refused: $out"
    FAILS=1
fi

exit "$FAILS"
