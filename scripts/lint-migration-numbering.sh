#!/bin/sh
# Migration version numbers, checked as filenames (issue #507).
#
# golang-migrate applies every migration whose version exceeds the version
# recorded in schema_migrations, in ascending order, and then records the
# HIGHEST one it applied. Three filename-level defects follow from that, and
# all three are silent:
#
#   A DUPLICATE version makes the run ambiguous. golang-migrate refuses it,
#   but only at migrate time - which is to say on whichever branch merges
#   second, after review, usually in CI against a real database. Two specs
#   claiming one number is the ordinary way this happens: numbers are written
#   into a spec long before a file exists, and no tool reads a spec.
#
#   A GAP is a migration that will never run. Land 0037 against a database at
#   0032 and the recorded version becomes 37; 0033 through 0036 are now in the
#   past and are skipped forever, without an error, because golang-migrate has
#   no way to know they were meant to be there. This is the expensive one: the
#   schema is wrong and every diagnostic is green.
#
#   An UNPAIRED up migration cannot be rolled back. The down file is the only
#   thing standing between a bad deploy and a restore from backup, and its
#   absence is invisible until the moment it is needed.
#
# WHAT THIS CANNOT SEE, stated plainly because a lint's silence is trusted:
# a version claimed only in prose. A spec that says "0033_cashback_claims"
# has no file, so nothing here can know the number is spoken for. Before
# writing a number down, check it against every UNBUILT spec as well as the
# files on disk - or better, take the number when the pull request opens
# rather than when the spec is written.
#
# This is a separate script from lint-migrations.sh on purpose. That one is a
# SQL reader and takes arbitrary .sql paths, including single files and
# fixtures with no down migration at all; these rules are about a directory
# as a whole and would refuse its test fixtures.
#
# Usage: lint-migration-numbering.sh [dir]
#
# Output is one ::error:: line per defect, which surfaces in the GitHub
# Actions annotations UI and reads as plain text anywhere else, plus one
# summary line stating how much was actually read. The exit code is the
# verdict: 0 clean, 1 defects found, 2 nothing was checked.
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
DIR=${1:-"$ROOT/internal/platform/db/migrations"}

if [ ! -d "$DIR" ]; then
    echo "::error::$DIR is not a directory; nothing was checked" >&2
    exit 2
fi

# Collect first. A lint that silently checks nothing is worse than no lint,
# so an empty directory is a failure rather than a pass.
FILES=$(find "$DIR" -type f -name '*.sql' | sort)
if [ -z "$FILES" ]; then
    echo "::error::no .sql files under $DIR; this lint would have passed without reading anything" >&2
    exit 2
fi

PROGRAM=$(
    cat <<'AWK'
BEGIN {
    fails = 0
    files = 0
}

{
    files++
    path = $0
    name = path
    sub(/^.*\//, "", name)

    # The shape golang-migrate requires: <version>_<name>.<direction>.sql.
    # Anything else is not a migration it will run, which makes it either a
    # stray file or a migration that silently does not exist.
    if (name !~ /^[0-9]+_[A-Za-z0-9_]+\.(up|down)\.sql$/) {
        report(path, sprintf("%s is not <version>_<name>.up.sql or <version>_<name>.down.sql. golang-migrate will not run it, so a migration named this way does not exist as far as the schema is concerned.", name))
        next
    }

    version = name
    sub(/_.*$/, "", version)
    stem = name
    sub(/\.(up|down)\.sql$/, "", stem)
    label = stem
    sub(/^[0-9]+_/, "", label)
    direction = (name ~ /\.up\.sql$/) ? "up" : "down"

    n = version + 0
    seen[n] = seen[n] + 1
    if (!(n in labelof)) {
        labelof[n] = label
        pathof[n] = path
    } else if (labelof[n] != label) {
        report(path, sprintf("version %s is claimed twice, by %s and by %s. golang-migrate refuses a duplicate version, but only when it runs - which is on whichever branch merges second, against a real database.", version, labelof[n], label))
    }
    if (direction == "up") { up[stem] = path; upver[stem] = version }
    else { down[stem] = path }
    if (!(n in known)) { known[n] = 1; versions[++count] = n }
}

END {
    if (files == 0) exit 2

    # Pairing. An up with no down cannot be rolled back; a down with no up
    # runs against nothing.
    for (stem in up) {
        if (!(stem in down)) {
            report(up[stem], sprintf("%s has no matching down migration. The down file is what stands between a bad deploy and a restore from backup, and its absence is invisible until it is needed.", stem ".up.sql"))
        }
    }
    for (stem in down) {
        if (!(stem in up)) {
            report(down[stem], sprintf("%s has no matching up migration, so it undoes something nothing here creates.", stem ".down.sql"))
        }
    }

    # Contiguity. Sort the versions and walk them: any step larger than one
    # is a number that will never run if it is filled in later.
    for (i = 1; i <= count; i++)
        for (j = i + 1; j <= count; j++)
            if (versions[j] < versions[i]) { t = versions[i]; versions[i] = versions[j]; versions[j] = t }

    for (i = 2; i <= count; i++) {
        if (versions[i] != versions[i - 1] + 1) {
            missing = versions[i] - versions[i - 1] - 1
            report(pathof[versions[i]], sprintf("version %d follows %d, leaving %d version(s) unused. A migration written into that gap later will never run: golang-migrate records the highest version it applied, so anything below it is already in the past.", versions[i], versions[i - 1], missing))
        }
    }

    if (count > 0)
        printf "migration numbering lint: read %d file(s) covering %d version(s), %04d to %04d; %d defect(s)\n", files, count, versions[1], versions[count], fails
    else
        printf "migration numbering lint: read %d file(s) covering no version at all; %d defect(s)\n", files, fails

    if (fails > 0) exit 1
}

function report(path, message) {
    printf "::error file=%s::%s\n", path, message > "/dev/stderr"
    fails++
}
AWK
)

printf '%s\n' "$FILES" | awk "$PROGRAM"
