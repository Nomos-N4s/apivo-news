#!/bin/sh
# Unit-style tests for lint-migration-numbering.sh (issue #507).
#
# The sibling suite's lesson applies here unchanged: a gate is only as good as
# the proof that it closes. Every case asserts the exit status AND the counts
# in the summary line, because "read nothing and found nothing wrong" and
# "read everything and found nothing wrong" are the same exit status and very
# nearly the same sentence.
#
# Each case is a throwaway directory under mktemp. The repository's own
# migrations are checked in the last case: the lint has to be true about the
# schema that exists today, not only about fixtures.
set -eu

LINT=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/lint-migration-numbering.sh
MIGRATIONS=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)/internal/platform/db/migrations

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

FAILS=0
N=0
DIR=

# fresh starts a new empty case directory.
fresh() {
    N=$((N + 1))
    DIR="$TMP/case$N"
    mkdir -p "$DIR"
}

# pair writes a complete migration: both halves, same version and name.
pair() {
    printf 'select 1;\n' >"$DIR/$1.up.sql"
    printf 'select 1;\n' >"$DIR/$1.down.sql"
}

# expect runs the lint and asserts the exit status.
# expect <what> <wanted status>
expect() {
    what=$1
    want=$2
    set +e
    OUT=$(sh "$LINT" "$DIR" 2>"$TMP/err")
    got=$?
    set -e
    ERR=$(cat "$TMP/err")
    if [ "$got" -ne "$want" ]; then
        printf 'FAIL: %s: wanted exit %d, got %d\n' "$what" "$want" "$got"
        printf '  stdout: %s\n  stderr: %s\n' "$OUT" "$ERR"
        FAILS=$((FAILS + 1))
        return
    fi
    printf 'ok: %s\n' "$what"
}

# read_count asserts the summary named exactly this many files and versions,
# which is what makes a reader that saw nothing visible.
# read_count <what> <files> <versions>
read_count() {
    what=$1
    wantf=$2
    wantv=$3
    if ! printf '%s' "$OUT" | grep -q "read $wantf file(s) covering $wantv version(s)"; then
        printf 'FAIL: %s: summary did not report %s file(s) and %s version(s)\n' "$what" "$wantf" "$wantv"
        printf '  stdout: %s\n' "$OUT"
        FAILS=$((FAILS + 1))
        return
    fi
    printf 'ok: %s (read %s file(s), %s version(s))\n' "$what" "$wantf" "$wantv"
}

# said asserts the failure named the thing a human needs in order to fix it.
# A lint that refuses without saying which file and why costs more than it saves.
said() {
    what=$1
    needle=$2
    if ! printf '%s' "$ERR" | grep -q "$needle"; then
        printf 'FAIL: %s: the error did not mention "%s"\n' "$what" "$needle"
        printf '  stderr: %s\n' "$ERR"
        FAILS=$((FAILS + 1))
        return
    fi
    printf 'ok: %s said "%s"\n' "$what" "$needle"
}

################################################################################
# Clean cases. These prove the lint passes what it should, which is the half a
# too-eager lint fails.
################################################################################

fresh
pair 0001_init
expect "one complete migration passes" 0
read_count "one complete migration" 2 1

fresh
pair 0001_init
pair 0002_second
pair 0003_third
expect "three contiguous migrations pass" 0
read_count "three contiguous migrations" 6 3

# A version does not have to start at 1 for the run to be contiguous: a
# directory checked in isolation is still internally consistent.
fresh
pair 0031_late
pair 0032_later
expect "contiguity is about gaps, not about starting at one" 0
read_count "a run starting at 31" 4 2

################################################################################
# The three defects the lint exists for.
################################################################################

fresh
pair 0001_init
printf 'select 1;\n' >"$DIR/0002_one_name.up.sql"
printf 'select 1;\n' >"$DIR/0002_one_name.down.sql"
printf 'select 1;\n' >"$DIR/0002_other_name.up.sql"
printf 'select 1;\n' >"$DIR/0002_other_name.down.sql"
expect "a duplicate version is refused" 1
said "the duplicate" "claimed twice"

fresh
pair 0001_init
pair 0005_after_a_gap
expect "a gap is refused" 1
said "the gap" "will never run"
said "the gap names the count" "3 version(s) unused"

fresh
pair 0001_init
printf 'select 1;\n' >"$DIR/0002_no_way_back.up.sql"
expect "an up migration with no down is refused" 1
said "the unpaired up" "no matching down migration"

fresh
pair 0001_init
printf 'select 1;\n' >"$DIR/0002_orphan.down.sql"
expect "a down migration with no up is refused" 1
said "the unpaired down" "no matching up migration"

fresh
pair 0001_init
printf 'select 1;\n' >"$DIR/not_a_migration.sql"
expect "a file that is not a migration is refused" 1
said "the malformed name" "golang-migrate will not run it"

# A version with no leading zeros is legal to golang-migrate and must not be
# read as a different version from its padded form's neighbours.
fresh
pair 1_unpadded
pair 2_also_unpadded
expect "unpadded versions are read as numbers" 0
read_count "unpadded versions" 4 2

################################################################################
# Counting. A lint that finds one defect and stops hides the rest.
################################################################################

fresh
pair 0001_init
pair 0004_gap_one
printf 'select 1;\n' >"$DIR/0005_unpaired.up.sql"
expect "several defects at once are all refused" 1
said "the multi-defect case reports the gap" "will never run"
said "the multi-defect case reports the pairing" "no matching down migration"
if ! printf '%s' "$OUT" | grep -q "2 defect(s)"; then
    printf 'FAIL: the multi-defect case did not count both defects\n  stdout: %s\n' "$OUT"
    FAILS=$((FAILS + 1))
else
    printf 'ok: both defects were counted\n'
fi

################################################################################
# Refusing to check nothing. Silence is the failure mode a lint has to rule
# out about itself.
################################################################################

fresh
expect "an empty directory is not a pass" 2

N=$((N + 1))
DIR="$TMP/case$N-does-not-exist"
expect "a missing directory is not a pass" 2

################################################################################
# The migrations that actually exist.
################################################################################

# Counted independently rather than written down. A literal here would be
# wrong the moment anybody adds a migration - it was, on the very next one -
# and a test that has to be edited by every unrelated pull request is a test
# people learn to edit without reading.
#
# The independence is the point: the lint counts by parsing filenames into
# versions, this counts .sql and .up.sql files. A reader that saw nothing
# still fails, because it would report 0 against a directory holding dozens.
DIR=$MIGRATIONS
REAL_FILES=$(find "$MIGRATIONS" -type f -name '*.sql' | wc -l | tr -d ' ')
REAL_VERSIONS=$(find "$MIGRATIONS" -type f -name '*.up.sql' | wc -l | tr -d ' ')
expect "the repository's own migrations are clean" 0
read_count "the repository's own migrations" "$REAL_FILES" "$REAL_VERSIONS"

# And the floor: a directory that somehow held nothing would make the two
# counts above agree with a lint that read nothing.
if [ "$REAL_VERSIONS" -lt 1 ]; then
    printf 'FAIL: the repository has no migrations, so the case above asserted nothing\n'
    FAILS=$((FAILS + 1))
fi

################################################################################

printf '\n'
if [ "$FAILS" -ne 0 ]; then
    printf '%d case(s) failed\n' "$FAILS"
    exit 1
fi
printf 'all migration numbering lint cases passed\n'
