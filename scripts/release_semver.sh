#!/bin/sh
# The one definition of "a release version" (issue #119), sourced by
# release_guard.sh (which refuses anything else) and release_notes.sh (which
# measures the notes from the previous one). Two copies of this rule would
# drift, and drift here means the notes are measured from a tag the guard
# would never have released.
#
# The pattern is SemVer 2.0.0's own grammar with the repository's mandatory
# "v" prefix: no leading zeroes in the numeric identifiers (v01.2.3 and
# v1.2.3-01 are not versions), no empty dot-separated identifiers
# (v1.2.3-alpha..1 is not a version), build metadata allowed after "+".
#
# This file only defines functions; sourcing it runs nothing.

# release_semver_ok <name>: true when <name> is a strict semver release tag.
release_semver_ok() {
    printf '%s' "$1" | grep -q -E '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
}

# release_semver_prerelease <name>: true when <name> carries a pre-release
# part (v0.2.0-rc.1). Build metadata is stripped first, because the "-" that
# matters is the one before "+": v1.2.3+build-7 is a stable release.
release_semver_prerelease() {
    case "${1%%+*}" in
    *-*) return 0 ;;
    *) return 1 ;;
    esac
}
