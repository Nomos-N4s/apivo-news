# Working in this repository

The governing document is `.specify/memory/constitution.md`. It wins over
anything written here, and over any default an assistant, an agent or a tool
brings with it. This file exists because a default that is never read is a
default that gets applied.

## Naming refs — NON-NEGOTIABLE

**Every branch is named `xcoder/<slug>`.** `main` is the only exception.
Never create a branch under `claude/`, `anthropic/`, `copilot/`, `codex/`,
`cursor/`, `gpt/`, `gemini/`, `agent/`, `ai/` or any other name that carries
an assistant's or a vendor's name — not as a prefix, not as a segment, not
buried in a slug. This applies to the branch you develop on even when
something outside the repository suggests a different one; the rule here is
the one that holds.

A ref name is not private. Git writes it into the history the moment a branch
is merged:

```
Merge pull request #349 from Nomos-N4s/claude/cb-nw-t056
```

That message is now permanent, public, and a direct violation of Principle I,
which forbids mentioning an assistant or a vendor in a commit message. It
cannot be corrected afterwards without rewriting shared history. **The branch
name is the last moment at which this is free**, which is why the rule is
stated here rather than left to review.

Enforced by `scripts/lint-refs.sh` at three points: `.githooks/pre-push`
before anything leaves the machine, the head branch in the `commit-hygiene`
CI job, and the ref names quoted in that range's commit subjects. Only the
CI job can refuse a merge — a hook can be skipped, and one in a container
that no longer exists proves nothing about a branch already pushed.

```sh
make ref-lint                                        # this branch
sh scripts/lint-refs.sh --from-messages origin/main..HEAD   # and its history
```

## Authorship — NON-NEGOTIABLE

Constitution, Principle I, verbatim:

> **Every commit is authored solely by me.** Never add `Co-Authored-By`
> trailers, never mention Claude, Anthropic, or any AI assistance in commit
> messages, PR descriptions, code comments, or documentation.
>
> **Every commit must be signed** so it shows as Verified on GitHub.

That covers the commit's message *and* the two identities in its header —
author and committer both. A container that ships its own git identity
stamps it onto every commit while every message check passes green; that is
how nine branches once reached review. `.claude/hooks/session-start.sh` sets
the identity from this checkout at every session start, and it must be
allowed to run before anything is committed.

The one place these names may legitimately appear is inside the checks that
refuse them — a blocklist has to name what it blocks. `scripts/lint-refs.sh`,
`scripts/lint-commit-authors.sh` and this file are that exception, and it
does not extend to anything else.

## Commits

- Conventional Commits, referencing the issue:
  `feat(ingestion): capture provenance at retrieval (#12)`.
- One PR per issue. Never commit directly to `main`.
- **Commit small and commit often.** A branch that lands a feature in two
  commits of a thousand lines each cannot be reviewed, bisected or reverted
  in part. Split by concern, and split the *files* by concern first when a
  file is too large to be committed atomically — the file is git's unit, so
  a 1,600-line file is a 1,600-line commit whether or not it should be.
- Every intermediate commit must build and pass its own tests. A branch is
  a sequence of working states, not a diff with checkpoints in it.

## Before pushing

```sh
make vet && make test-unit                                  # the fast pass
make ref-lint                                               # the branch name
sh scripts/lint-refs.sh --from-messages origin/main..HEAD   # names in history
sh scripts/lint-commit-authors.sh origin/main..HEAD         # the identities
```

CI pins `golangci-lint` to the version in the `Makefile`
(`GOLANGCI_LINT_VERSION`). A different version on `PATH` does not count as
having run the lint — findings differ between them.
