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
# public.account, public.place, public.language and public.domain_event
# (constitution, "Products"; ADR-0001). Nothing else in public - and in
# particular no news table - may be the target of a cashback foreign key, and
# no news or shared table may depend on a product schema.
#
# Why this is a lint and not a database rule: Postgres is perfectly happy to
# create a cross-schema foreign key, and the first one written becomes
# permanent. It survives review because it is one word in a column
# definition, and it is discovered years later when the products have to be
# separated - the moment when it is most expensive and least recoverable.
#
# WHAT THIS CANNOT SEE, stated plainly because a lint's silence is trusted:
# SQL built as a string and run through EXECUTE. A foreign key assembled at
# runtime is beyond text analysis, and nothing here pretends otherwise.
#
# Usage: lint-migrations.sh [path...]
#
# Paths may be directories or .sql files; the default is the migrations
# directory. Every .sql file found is checked - up migrations and down
# migrations alike - in filename order, which is the order they are applied
# in, because search_path and the set of tables that exist are both carried
# forward from one migration to the next.
#
# Output is one ::error:: line per violation, naming the file and line, which
# surfaces in the GitHub Actions annotations UI and reads as plain text
# anywhere else, plus one summary line stating how much was actually read.
# The exit code is the verdict.
set -eu

# The shared reference data a product schema may point at. Deliberately NOT
# the whole of public: these four are what both products read (constitution,
# "Products"), and everything else in public belongs to news.
SHARED_TABLES="account place language domain_event"

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

# The SQL reader. It is a state machine rather than a grep because every
# shortcut here fails the same way - by passing. Comments, string literals,
# quoted identifiers and dollar-quoted bodies all contain the word
# "references", and a foreign key is routinely written across several lines.
#
# The reader blanks what it suppresses instead of deleting it, so every
# offset in the cleaned text still names the same position in the original.
# That is what lets a violation be reported on its own line, and what lets a
# SET statement read its value back out of the raw text after the cleaner has
# blanked the string literal around it.
PROGRAM=$(
    cat <<'AWK'
BEGIN {
    parts = split(allow, allowed, " ")
    for (p = 1; p <= parts; p++) shared[allowed[p]] = 1

    # Words that can stand where a table name would in a REFERENCES clause
    # without being one. GRANT REFERENCES ON is the case that matters; the
    # rest are here so a plpgsql fragment cannot invent a table called "and".
    words = split("on to from select where and or is not null in default check key using", kw, " ")
    for (p = 1; p <= words; p++) keyword[kw[p]] = 1

    fails = 0
    files = 0
    checked = 0
    statements = 0
    buf = ""
    rawbuf = ""
    curfile = ""

    # search_path survives a file. golang-migrate holds one pinned
    # connection for the whole run, so a plain SET in migration N is still in
    # force in migration N+1; only SET LOCAL is confined to its transaction,
    # and each migration is one transaction.
    sessionn = split("public", sessionpath, " ")
    resetpath()
}

FNR == 1 {
    endfile()
    curfile = FILENAME
    files++
    filestatements = 0
    filesemis = 0
    state = "sql"
    depth = 0
    tag = ""
    esc = 0
    openline = 0
    p1 = ""
    p2 = ""
    buf = ""
    rawbuf = ""
    bufstart = 1
    resetpath()
}

{
    filesemis += gsub(/;/, ";")
    line = strip($0)
    if (buf == "") bufstart = FNR
    buf = buf line "\n"
    rawbuf = rawbuf $0 "\n"
    while ((semi = index(buf, ";")) > 0) {
        statement(substr(buf, 1, semi - 1), substr(rawbuf, 1, semi - 1), bufstart)
        eaten = substr(buf, 1, semi)
        bufstart += gsub(/\n/, "\n", eaten)
        buf = substr(buf, semi + 1)
        rawbuf = substr(rawbuf, semi + 1)
    }
}

END {
    endfile()
    if (files == 0) exit 2
    printf "migration lint: read %d statement(s) and %d foreign key(s) across %d migration file(s); %d crossing a product schema boundary\n", statements, checked, files, fails
    if (fails > 0) exit 1
}

################################################################################
# Reading.
################################################################################

# strip blanks everything the SQL grammar says is not code, preserving length
# so offsets still line up with the original: line comments, block comments
# (which nest in Postgres), string literals and the delimiters of a
# dollar-quoted body. State carries across lines, because all but the line
# comment may.
#
# A dollar-quoted BODY is deliberately kept as code. do $$ ... $$ blocks and
# function bodies are where conditional DDL lives - the idiom is already in
# these migrations - and a foreign key added inside one is a foreign key.
# What that costs is a dollar-quoted string used as a value: its contents are
# read as SQL. Nothing comes of that unless the contents parse as a CREATE or
# ALTER TABLE, and a false alarm is a loud failure a human resolves, where
# the alternative is a whole class of DDL the reader cannot see.
function strip(text,   out, i, n, c, two, rest) {
    out = ""
    i = 1
    n = length(text)
    while (i <= n) {
        c = substr(text, i, 1)
        two = substr(text, i, 2)
        if (state == "sql") {
            if (two == "--") { out = out blanks(n - i + 1); break }
            if (two == "/*") { state = "block"; depth = 1; openline = FNR; out = out "  "; i += 2; continue }
            if (c == "'") {
                state = "string"
                openline = FNR
                # E'...' gives the backslash its escaping power back;
                # ordinary literals in standard-conforming Postgres do not,
                # where the only escape is a doubled quote.
                esc = ((p1 == "e") && (p2 !~ /[a-z0-9_]/))
                out = out " "
                i++
                continue
            }
            if (c == "\"") { state = "dquote"; openline = FNR; out = out "\""; i++; continue }
            if (c == "$") {
                rest = substr(text, i)
                if (match(rest, /^\$[A-Za-z_0-9]*\$/)) {
                    tag = substr(rest, 1, RLENGTH)
                    state = "dollar"
                    openline = FNR
                    out = out blanks(RLENGTH)
                    i += RLENGTH
                    continue
                }
            }
            out = out c
            p2 = p1
            p1 = tolower(c)
            i++
        } else if (state == "block") {
            if (two == "/*") { depth++; out = out "  "; i += 2; continue }
            if (two == "*/") { depth--; out = out "  "; i += 2; if (depth == 0) state = "sql"; continue }
            out = out " "
            i++
        } else if (state == "string") {
            if (esc && c == "\\") { out = out "  "; i += 2; continue }
            if (c == "'") { state = "sql"; p2 = ""; p1 = "'" }
            out = out " "
            i++
        } else if (state == "dquote") {
            # A quoted identifier is code, but nothing inside it may act as
            # syntax: a lone apostrophe in "don't" would otherwise open a
            # string literal that never closes and blind the rest of the file.
            if (c == "\"") { state = "sql"; p2 = ""; p1 = "\""; out = out "\""; i++; continue }
            out = out (c ~ /[A-Za-z0-9_]/ ? c : "_")
            i++
        } else {
            if (substr(text, i, length(tag)) == tag) { state = "sql"; p1 = ""; p2 = ""; out = out blanks(length(tag)); i += length(tag); continue }
            out = out c
            i++
        }
    }
    # A line comment ends at the newline; every other state may span lines.
    return out
}

function blanks(n,   s) {
    s = ""
    while (length(s) < n) s = s "                                        "
    return substr(s, 1, n)
}

# endfile judges what is left over and then asks the only two questions that
# tell a clean file apart from a reader that went blind on it. Both are
# failures rather than silence: a run that read nothing reports success in
# exactly the same words as a run that read everything and found nothing.
function endfile(   rest) {
    if (curfile == "") return

    rest = buf
    gsub(/[ \t\n]/, "", rest)
    if (rest != "") statement(buf, rawbuf, bufstart)
    buf = ""
    rawbuf = ""

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
    if (state == "dquote") return "a quoted identifier"
    if (state == "block") return "a block comment"
    if (state == "dollar") return "a " tag " quoted body"
    return state
}

################################################################################
# Judging.
################################################################################

# statement judges one SQL statement. Newlines become spaces for matching -
# character for character, so an offset in the flattened text still names a
# position in the original and the line number survives. raw is the same span
# of the untouched file, used only where the cleaner has blanked something a
# rule needs to read.
function statement(stmt, raw, startline,   flat, rawflat, lead, head, rawhead, i, src, srcschema, target, seg, pos, rest, padded, mark, sep, tail, anchors, owner) {
    flat = stmt
    gsub(/\n/, " ", flat)
    flat = tolower(flat)
    rawflat = raw
    gsub(/\n/, " ", rawflat)

    if (!match(flat, /[^ \t]/)) return
    lead = RSTART - 1
    head = substr(flat, lead + 1)
    rawhead = substr(rawflat, lead + 1)
    statements++
    filestatements++

    # search_path decides what an unqualified name means from here on, so a
    # migration written inside its own schema is read the way Postgres reads
    # it rather than being blamed for referencing public. Accepts every legal
    # spelling: = or TO, spaced or not, LOCAL or SESSION, quoted or bare.
    if (match(head, /^set[ \t]+(local[ \t]+|session[ \t]+)?search_path([ \t]*=|[ \t]+to)/)) {
        setpath(substr(rawhead, RLENGTH + 1), substr(head, 1, RLENGTH) ~ /[ \t]local[ \t]/)
        return
    }
    if (head ~ /^reset[ \t]+search_path/) {
        setpath("public", 0)
        return
    }

    # Only CREATE TABLE and ALTER TABLE can carry a foreign key, but a
    # statement does not have to BEGIN with one. A do $$ ... $$ block is a
    # single statement to the splitter, and conditional DDL inside one is
    # exactly where a foreign key gets added to a table that already exists.
    # So every CREATE/ALTER TABLE in the statement is an anchor, and each
    # REFERENCES belongs to the nearest anchor before it. A REFERENCES with
    # no anchor before it - GRANT REFERENCES - belongs to nothing and is not
    # a foreign key.
    padded = " " flat
    anchors = 0
    pos = 1
    while (1) {
        rest = substr(padded, pos)
        if (!match(rest, /[^a-z0-9_](create|alter)[ \t]+([a-z]+[ \t]+)?([a-z]+[ \t]+)?table[ \t]+/)) break
        seg = substr(rest, RSTART, RLENGTH)
        mark = pos + RSTART - 1
        pos = pos + RSTART + RLENGTH - 2
        tail = substr(padded, pos + 1)
        while (match(tail, /^(if[ \t]+not[ \t]+exists[ \t]+|if[ \t]+exists[ \t]+|only[ \t]+)/)) tail = substr(tail, RLENGTH + 1)
        if (!match(tail, /^[a-z0-9_."]+/)) continue
        src = identifier(substr(tail, 1, RLENGTH))
        if (src == "") continue

        if (seg ~ /create/) {
            # CREATE always writes into the FIRST schema on the search path,
            # whatever the rest of it holds.
            srcschema = qualified(src) ? schemapart(src) : curpath[1]
            created[srcschema "." bare(src)] = 1
        } else {
            srcschema = resolve(src, startline + newlines(stmt, mark))
            if (srcschema == "") continue
        }
        anchors++
        anchorat[anchors] = mark
        anchorname[anchors] = src
        anchorschema[anchors] = srcschema
    }
    if (anchors == 0) return

    # A foreign key may be a column constraint (col uuid references t (id))
    # or a table constraint (foreign key (col) references t (id)); both end
    # in the same two words, so both are found by looking for them.
    pos = 1
    while (1) {
        rest = substr(padded, pos)
        if (!match(rest, /[^a-z0-9_]references[ \t]*[a-z0-9_."]+/)) break
        seg = substr(rest, RSTART, RLENGTH)
        mark = pos + RSTART - 1 + index(seg, "references") - 2
        pos = pos + RSTART + RLENGTH - 1
        sub(/^[^a-z]*references/, "", seg)
        # The name may follow whitespace, or a quote with nothing between:
        # references"public"."article" is legal. What must NOT follow is a
        # word character, or references_count becomes a foreign key.
        sep = substr(seg, 1, 1)
        if (sep != " " && sep != "\t" && sep != "\"") continue
        target = identifier(seg)
        if (target == "" || (target in keyword)) continue

        owner = 0
        for (i = 1; i <= anchors; i++) if (anchorat[i] < mark) owner = i
        if (owner == 0) continue

        checked++
        judge(anchorschema[owner], anchorname[owner], target, startline + newlines(stmt, mark))
    }
}

# judge applies the boundary rule to one foreign key.
function judge(srcschema, src, target, at,   dstschema, dstbare, dsttable) {
    dstschema = qualified(target) ? schemapart(target) : resolve(target, at)
    if (dstschema == "") return
    dstbare = bare(target)
    dsttable = dstschema "." dstbare
    src = srcschema "." bare(src)

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

################################################################################
# Names and the search path.
################################################################################

# resolve answers which schema an unqualified name lives in, the way Postgres
# answers it: the first schema on the search path that actually CONTAINS the
# table, not simply the first schema on the path. Under the idiomatic
# "set search_path = cashback, public" an unqualified news table is public's,
# and reading it as cashback's would judge a real crossing as same-schema.
#
# Every table this run has seen created is known, and the files are read in
# the order they are applied, so this is the same knowledge Postgres has. If
# a name is unknown and the path offers more than one candidate, the answer
# is refused rather than guessed - a guess here is a silent wrong verdict.
function resolve(name, at,   i, candidate) {
    if (qualified(name)) return schemapart(name)
    for (i = 1; i <= curn; i++) {
        if ((curpath[i] "." bare(name)) in created) return curpath[i]
    }
    if (curn == 1) return curpath[1]
    candidate = curpath[1]
    for (i = 2; i <= curn; i++) candidate = candidate ", " curpath[i]
    report(at, sprintf("%s is unqualified and no schema on the search path (%s) is known to hold it, so which schema it means cannot be decided here - and guessing would decide whether a foreign key crosses a product boundary. Qualify it.", name, candidate))
    return ""
}

# setpath records a new search path. A plain SET outlives the file it is in,
# because the whole migration run shares one connection; SET LOCAL ends with
# its transaction, which is the migration.
function setpath(value, islocal,   raw, n, i, entry, out, count) {
    value = tolower(value)
    sub(/;.*$/, "", value)
    if (value ~ /^[ \t]*default[ \t]*$/) value = "public"

    n = split(value, raw, ",")
    count = 0
    for (i = 1; i <= n; i++) {
        entry = raw[i]
        gsub(/^[ \t]+|[ \t]+$/, "", entry)
        gsub(/^['"]|['"]$/, "", entry)
        # "$user" names a schema per role; migrations never write into one.
        if (entry == "" || entry ~ /^\$user$/) continue
        out[++count] = entry
    }
    if (count == 0) return

    curn = count
    for (i = 1; i <= count; i++) curpath[i] = out[i]
    if (!islocal) {
        sessionn = count
        for (i = 1; i <= count; i++) sessionpath[i] = out[i]
    }
}

function resetpath(   i) {
    curn = sessionn
    for (i = 1; i <= sessionn; i++) curpath[i] = sessionpath[i]
}

# identifier cleans one token into a bare table name: quotes removed, an
# opening parenthesis or trailing punctuation cut off.
function identifier(token,   paren) {
    gsub(/"/, "", token)
    gsub(/^[ \t]+/, "", token)
    paren = index(token, "(")
    if (paren > 0) token = substr(token, 1, paren - 1)
    sub(/[,;.]+$/, "", token)
    return token
}

function qualified(name) {
    return index(name, ".") > 0
}

function schemapart(name) {
    return substr(name, 1, index(name, ".") - 1)
}

function bare(name) {
    if (qualified(name)) return substr(name, index(name, ".") + 1)
    return name
}

function newlines(text, offset,   head) {
    head = substr(text, 1, offset)
    return gsub(/\n/, "\n", head)
}
AWK
)

# One awk invocation over the whole set, in filename order, so search_path
# and the set of known tables carry forward exactly as they do when the
# migrations are applied.
IFS='
'
# shellcheck disable=SC2086
awk \
    -v allow="$SHARED_TABLES" \
    -v allowlist="public.account, public.place, public.language, public.domain_event" \
    "$PROGRAM" $FILES
