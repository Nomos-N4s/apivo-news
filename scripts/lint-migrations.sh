#!/bin/sh
# The product schema boundary, enforced in SQL text (issue #186).
#
# ADR-0001 gives each product its own Postgres schema and states the rule
# that makes the boundary real rather than decorative: NO FOREIGN KEY CROSSES
# A PRODUCT SCHEMA BOUNDARY. A cashback row that refers to an account holds
# the account id and validates it through the identity module - it does not
# reach across the boundary with referential integrity.
#
# The one exception is the shared reference data both products read:
# public.account, public.place and public.language. Nothing else in public -
# and in particular no news table - may be the target of a cashback foreign
# key, and no news or shared table may depend on a product schema.
#
# Why this is a lint and not a database rule: Postgres is perfectly happy to
# create a cross-schema foreign key, and the first one written becomes
# permanent. It survives review because it is one word in a column
# definition, and it is discovered years later when the products have to be
# separated - the moment when it is most expensive and least recoverable.
#
# Usage: lint-migrations.sh [path...]
#
# Paths may be directories or .sql files; the default is the migrations
# directory. Every .sql file found is checked - up migrations and down
# migrations alike.
#
# Output is one ::error:: line per violation, naming the file and line, which
# surfaces in the GitHub Actions annotations UI and reads as plain text
# anywhere else. The exit code is the verdict.
set -eu

# The shared reference data a product schema may point at. Deliberately NOT
# the whole of public: account, place and language are reference data both
# products read (constitution, "Products"), and everything else in public
# belongs to news.
SHARED_TABLES="account place language"

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
if [ "$#" -eq 0 ]; then
    set -- "$ROOT/internal/platform/db/migrations"
fi

for path in "$@"; do
    if [ ! -e "$path" ]; then
        echo "::error::$path does not exist; nothing was checked" >&2
        exit 2
    fi
done

# Collect the .sql files first: a lint that silently checks nothing is worse
# than no lint, so an empty set is a failure rather than a pass.
FILES=$(
    for path in "$@"; do
        if [ -d "$path" ]; then
            find "$path" -type f -name '*.sql'
        else
            printf '%s\n' "$path"
        fi
    done | sort
)
if [ -z "$FILES" ]; then
    echo "::error::no .sql files found under: $*; this lint would have passed without reading anything" >&2
    exit 2
fi

# The SQL reader. It is a small state machine rather than a grep because
# every shortcut here fails the same way - by passing. Comments, string
# literals and dollar-quoted function bodies all contain the word
# "references", and a foreign key is routinely written across several lines.
PROGRAM=$(
    cat <<'AWK'
BEGIN {
    parts = split(allow, allowed, " ")
    for (p = 1; p <= parts; p++) shared[allowed[p]] = 1
    fails = 0
    files = 0
    checked = 0
    statements = 0
    buf = ""
    curfile = ""
}

# Per-file reset. State never leaks across files: an unterminated string in
# one migration must not blind the next.
FNR == 1 {
    endfile()
    curfile = FILENAME
    files++
    filestatements = 0
    filesemis = 0
    openline = 0
    state = "sql"
    depth = 0
    tag = ""
    buf = ""
    bufstart = 1
    defschema = "public"
}

{
    filesemis += gsub(/;/, ";")
    line = strip($0)
    if (buf == "") bufstart = FNR
    buf = buf line "\n"
    while ((semi = index(buf, ";")) > 0) {
        statement(substr(buf, 1, semi - 1), bufstart)
        eaten = substr(buf, 1, semi)
        bufstart += gsub(/\n/, "\n", eaten)
        buf = substr(buf, semi + 1)
    }
}

END {
    endfile()
    if (files == 0) exit 2
    # Always, pass or fail: the summary states how much was READ, not only
    # what was concluded. A reader that saw nothing concludes nothing, and
    # "found nothing wrong" and "looked at nothing" are otherwise the same
    # sentence and the same exit status.
    printf "migration lint: read %d statement(s) and %d foreign key(s) across %d migration file(s); %d crossing a product schema boundary\n", statements, checked, files, fails
    if (fails > 0) exit 1
}

# strip blanks out everything the SQL grammar says is not code - line
# comments, block comments (which nest in Postgres), string literals and
# dollar-quoted bodies - one character at a time, preserving length so that
# offsets still point where they did. State carries across lines because all
# but the line comment may.
function strip(text,   out, i, n, c, two, rest) {
    out = ""
    i = 1
    n = length(text)
    while (i <= n) {
        c = substr(text, i, 1)
        two = substr(text, i, 2)
        if (state == "sql") {
            if (two == "--") { break }
            if (two == "/*") { state = "block"; depth = 1; openline = FNR; i += 2; continue }
            if (c == "'") { state = "string"; openline = FNR; i++; continue }
            if (c == "$") {
                rest = substr(text, i)
                if (match(rest, /^\$[A-Za-z_0-9]*\$/)) {
                    tag = substr(rest, 1, RLENGTH)
                    state = "dollar"
                    openline = FNR
                    i += RLENGTH
                    continue
                }
            }
            out = out c
            i++
        } else if (state == "block") {
            if (two == "/*") { depth++; i += 2; continue }
            if (two == "*/") { depth--; i += 2; if (depth == 0) state = "sql"; continue }
            i++
        } else if (state == "string") {
            if (c == "'") state = "sql"
            i++
        } else {
            if (substr(text, i, length(tag)) == tag) { state = "sql"; i += length(tag); continue }
            i++
        }
    }
    return out
}

# endfile judges a trailing statement that never met its semicolon - so a
# missing terminator cannot hide a foreign key - and then asks the only two
# questions that tell a clean file apart from a reader that went blind on it.
#
# Both are failures rather than silence. A reader that loses track of a quote
# blanks the file from that point on, finds nothing, and says so in exactly
# the words it uses for a file that is genuinely clean.
function endfile(   rest) {
    if (curfile == "") return

    rest = buf
    gsub(/[ \t\n]/, "", rest)
    if (rest != "") statement(buf, bufstart)
    buf = ""

    if (state != "sql") {
        report(openline, sprintf("the reader reached the end of this file still inside %s, so everything after it was blanked and never checked. Either the file has an unterminated literal or comment, or this lint has a hole in it - both mean nothing below that point was read.", inside()))
    }
    # A file carrying statement terminators that yielded no statement was not
    # read, whatever the reason. Conditioning on the semicolon is what keeps a
    # deliberately empty down migration - "-- irreversible", no SQL at all -
    # from being called a failure.
    if (filestatements == 0 && filesemis > 0) {
        report(1, sprintf("this file contains %d statement terminator(s) but produced no SQL statement, so nothing in it was checked; either the file is not what it looks like or this lint could not read it.", filesemis))
    }
}

function inside() {
    if (state == "string") return "a string literal"
    if (state == "block") return "a block comment"
    if (state == "dollar") return "a " tag " quoted body"
    return state
}

# statement judges one SQL statement. Newlines become spaces for matching -
# character for character, so an offset in the flattened text still names a
# position in the original and the line number survives.
function statement(stmt, startline,   flat, head, words, count, i, at, src, srcschema, target, seg, pos, rest, padded, mark) {
    flat = stmt
    gsub(/\n/, " ", flat)
    flat = tolower(flat)

    head = flat
    sub(/^[ \t]+/, "", head)
    if (head == "") return
    statements++
    filestatements++

    # search_path decides what an unqualified name means from here on, so a
    # migration written entirely inside its own schema is read correctly
    # rather than being blamed for referencing public.
    if (head ~ /^set[ \t]+(local[ \t]+|session[ \t]+)?search_path[ \t]*(=|to)[ \t]/) {
        sub(/^set[ \t]+(local[ \t]+|session[ \t]+)?search_path[ \t]*(=|to)[ \t]*/, "", head)
        split(head, words, ",")
        head = words[1]
        gsub(/[" \t]/, "", head)
        if (head != "") defschema = head
        return
    }

    # Only CREATE TABLE and ALTER TABLE can carry a foreign key. Restricting
    # the search to them is what keeps GRANT REFERENCES, comments about
    # references and a column named references_count out of the results.
    count = split(head, words, /[ \t]+/)
    at = 0
    if (words[1] == "create") {
        for (i = 2; i <= count && i <= 4; i++) if (words[i] == "table") { at = i; break }
    } else if (words[1] == "alter" && words[2] == "table") {
        at = 2
    }
    if (at == 0) return

    i = at + 1
    while (i <= count && (words[i] == "if" || words[i] == "not" || words[i] == "exists" || words[i] == "only")) i++
    src = identifier(words[i])
    if (src == "") return
    srcschema = schemaof(src)

    # A foreign key may be a column constraint (col uuid references t (id))
    # or a table constraint (foreign key (col) references t (id)); both end
    # in the same two words, so both are found by looking for them.
    padded = " " flat
    pos = 1
    while (1) {
        rest = substr(padded, pos)
        if (!match(rest, /[^a-z0-9_]references[ \t]+[a-z0-9_."]+/)) break
        seg = substr(rest, RSTART, RLENGTH)
        mark = pos + RSTART - 1 + index(seg, "references") - 2
        pos = pos + RSTART + RLENGTH - 1
        sub(/^[^a-z]*references[ \t]+/, "", seg)
        target = identifier(seg)
        if (target == "") continue
        checked++
        judge(srcschema, src, target, startline + newlines(stmt, mark))
    }
}

# judge applies the boundary rule to one foreign key.
function judge(srcschema, src, target, at,   dstschema, dstbare, dsttable) {
    dstschema = schemaof(target)
    dstbare = bare(target)
    dsttable = qualify(dstschema, dstbare)
    src = qualify(srcschema, bare(src))

    if (srcschema == dstschema) return

    if (srcschema != "public" && dstschema == "public") {
        if (dstbare in shared) return
        report(at, sprintf("%s references %s, which crosses the product schema boundary: a product table may reference only the shared reference data (%s). Hold the id and validate it through the module that owns the row - referential integrity stops at the boundary (ADR-0001).", src, dsttable, allowlist))
        return
    }
    if (srcschema == "public" && dstschema != "public") {
        report(at, sprintf("%s references %s: shared and news tables must never depend on a product schema. The dependency points the wrong way - it would make %s undroppable and would let a product's data hold the shared schema hostage.", src, dsttable, dstschema))
        return
    }
    report(at, sprintf("%s references %s: a foreign key must not cross from one product schema into another. Products integrate asynchronously through the domain event stream, never through referential integrity (ADR-0001).", src, dsttable))
}

function report(at, message) {
    printf "::error file=%s,line=%d::%s\n", curfile, at, message > "/dev/stderr"
    fails++
}

# identifier cleans one token into a bare table name: quotes removed, an
# opening parenthesis or trailing punctuation cut off.
function identifier(token,   paren) {
    gsub(/"/, "", token)
    paren = index(token, "(")
    if (paren > 0) token = substr(token, 1, paren - 1)
    sub(/[,;.]+$/, "", token)
    return token
}

function schemaof(name,   dot) {
    dot = index(name, ".")
    if (dot > 0) return substr(name, 1, dot - 1)
    return defschema
}

function bare(name,   dot) {
    dot = index(name, ".")
    if (dot > 0) return substr(name, dot + 1)
    return name
}

function qualify(schema, table) {
    return schema "." table
}

function newlines(text, offset,   head) {
    head = substr(text, 1, offset)
    return gsub(/\n/, "\n", head)
}
AWK
)

# One awk invocation over the whole set, so the summary counts every file.
# FILES is split on newlines alone: migration filenames are repository
# controlled, but a directory above them may well contain a space.
IFS='
'
# shellcheck disable=SC2086
awk \
    -v allow="$SHARED_TABLES" \
    -v allowlist="public.account, public.place, public.language" \
    "$PROGRAM" $FILES
