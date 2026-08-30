#!/bin/bash
# Sets the commit identity this repository requires, on every session start.
#
# Constitution Principle I is NON-NEGOTIABLE: every commit is authored solely
# by the founder, and never carries an AI attribution identity. In a remote
# session the container ships with a global git identity of its own, and it
# comes back on every restart - so an identity set by hand earlier in a
# session is silently gone after one, and the next commit is authored wrongly
# without anything having changed in the repository.
#
# Setting it here makes the identity a property of the checkout rather than of
# whoever remembered. The local config is used rather than the global one so
# this cannot leak into another repository sharing the container, and it is
# written unconditionally: a stale value is exactly the failure being fixed,
# so "already set" is not a reason to leave it alone.
#
# `scripts/lint-commit-authors.sh` and the `commit-hygiene` CI job stay the
# enforcement. This hook is what stops them having to fail in the first place.
set -euo pipefail

readonly AUTHOR_NAME='xcoder-es'
readonly AUTHOR_EMAIL='capintobe@gmail.com'

cd "${CLAUDE_PROJECT_DIR:-$(pwd)}"

if ! git rev-parse --git-dir >/dev/null 2>&1; then
    echo 'session-start: not a git checkout, leaving the identity alone' >&2
    exit 0
fi

git config --local user.name "$AUTHOR_NAME"
git config --local user.email "$AUTHOR_EMAIL"

# The commit-msg hook that strips AI attribution trailers only runs when git
# has been pointed at it, and core.hooksPath is repository-local config that a
# fresh clone does not carry.
if [ -d .githooks ]; then
    git config --local core.hooksPath .githooks
fi

echo "session-start: commits will be authored by $AUTHOR_NAME <$AUTHOR_EMAIL>" >&2
