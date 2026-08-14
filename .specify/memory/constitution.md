<!--
Sync Impact Report
- Version change: (template) → 1.0.0
- Modified principles: all placeholders replaced (initial ratification)
- Added sections: Core Principles (I–VIII), Architecture Constraints,
  Alpha Scope & Delivery Rules, Governance
- Removed sections: none (template structure preserved)
- Follow-up TODOs: none
-->

# Apivo News (epiloYES) Constitution

Apivo News builds epiloYES: a multilingual local newspaper for Greek
communities abroad. A Greek speaker in Munich reads Munich news in Greek.
The business carries real legal exposure around content licensing; this
constitution exists to make the protections against that exposure
structural rather than habitual.

## Core Principles

### I. Sole Authorship and Signed Commits (NON-NEGOTIABLE)

The founder's authorship rules apply verbatim:

> **Every commit is authored solely by me.** Never add `Co-Authored-By`
> trailers, never mention Claude, Anthropic, or any AI assistance in
> commit messages, PR descriptions, code comments, or documentation.
>
> **Every commit must be signed** so it shows as Verified on GitHub.

Conventional Commits format is mandatory, referencing the issue
(`feat(ingestion): capture provenance at retrieval (#12)`). One PR per
issue. Never commit directly to `main`.

Enforcement: `.githooks/commit-msg` (wired via `core.hooksPath`) strips
disallowed trailers locally; the `commit-hygiene` CI job re-checks every
commit message on pull requests and pushes to `main`.

### II. Invariant I-1 — Human Approval (NON-NEGOTIABLE)

> **An article cannot exist without a named human approver.**

Enforced with a `NOT NULL` constraint on `article.approved_by`, not in
application code. A test attempts the insert and asserts the database
rejects it. A row in `article` IS the approval; drafts are
unrepresentable there.

### III. Invariant I-2 — Provenance at Retrieval (NON-NEGOTIABLE)

> **Provenance is captured at retrieval, in the same transaction as the
> content.** Never at publish time. A `source_item` with no provenance
> must be impossible to create.

Enforced by `NOT NULL` (and not-blank) constraints on the provenance
columns of `source_item` itself.

### IV. Invariant I-3 — Immutable Evidence (NON-NEGOTIABLE)

> **`source_item` is immutable.** It is legal evidence of what was
> retrieved and under what terms.

Enforced with database triggers rejecting UPDATE, DELETE and TRUNCATE.
The same protection extends to `translation` (lineage) and
`domain_event` (append-only audit stream).

### V. Invariant I-4 — Licence Snapshot (NON-NEGOTIABLE)

> **Licence terms are snapshotted at retrieval.** Publishers change
> terms; the defence rests on what applied at the time.

`source_item.licence_snapshot` is `NOT NULL` and never blank.

### VI. Invariant I-5 — Total Traceability (NON-NEGOTIABLE)

> **Every published sentence is traceable** to source, licence, model,
> prompt version and approver — by query, in under five minutes.

Enforced by the `article_provenance` view: one query answers all of it.
Any schema change that breaks this view fails the invariant test suite.

### VII. Language and Place Are Independent Axes

A Greek speaker in Munich wants Munich news in Greek; that is the entire
product thesis, and a single `el-DE` style tag breaks it. `language`
holds BCP-47 primary language subtags only (the schema rejects combined
tags), `place` is a self-referencing hierarchy, and articles and readers
relate to places many-to-many. No code, schema or UI may fuse language
and place into one locale value.

### VIII. Database-Enforced Invariants over Application Discipline

Application code is never trusted with a legal guarantee. Every
invariant above is enforced by the database (constraints, triggers,
views) and carries an explicit test asserting the DATABASE rejects the
illegal state, by SQLSTATE, against a real Postgres. Coverage numbers
are necessary but not sufficient; a passing gate without these tests
means nothing.

## Architecture Constraints

Decided through an ADR process; implement, do not re-litigate. Concrete
blockers stop work and go to the founder.

- Frontend: **Astro** (v6+), TypeScript strict, `@astrojs/node` adapter.
- Backend: **Go** — a single binary, modular monolith. No microservices.
- Database: **Supabase** (Postgres), EU region.
- Auth: Supabase Auth; Astro uses the JS SDK, Go validates the JWT.
- Types are generated from the Postgres schema — `sqlc` for Go,
  `supabase gen types` for TypeScript. **Never hand-write types on both
  sides.** CI fails on drift between schema and generated code.
- Module boundaries under `internal/` (`ingestion`, `translation`,
  `editorial`, `content`, `identity`, `platform`): `platform/` may be
  imported by anyone; no other module imports a sibling's internals —
  modules communicate through interfaces defined by the consumer, wired
  in `cmd`. An architecture test fails the build on violations.
- Deployment is container-first: Cloudflare Containers today (EU
  jurisdiction), Kubernetes-ready by construction. Nothing
  platform-specific leaks into application code; platform bindings stay
  behind interfaces in `internal/platform`.
- The LLM translation adapter sits behind an interface, swappable in
  under five engineer-days, with a per-article cost ceiling and a
  monthly cap that halts the pipeline rather than overspending.

## Alpha Scope & Delivery Rules

In scope (v1.0.0-alpha): Greek and German; Munich as reader locale;
Greek national and Munich local sources; RSS/Atom feeds only — no
scraping; text only — no images; translated headline and extract
linking back to the source — not full-text translation; human approval
on every item; full provenance; reader front page and article pages,
locale-scoped, attribution rendered; registration and consent capture
in the schema (UI only if time allows).

Out of scope — do not build, do not scaffold, do not leave TODOs for:
images, scraping, full-text translation, search, comments, newsletter,
social login, the ebest.gr user migration, cashback, price comparison,
fuel saver, loyalty, reviews, TV and radio listings, any ecosystem
mini-app. Individual sources are upgraded to full text only with
recorded written permission; every new source defaults to
`extract_and_link`.

Cut order under time pressure: registration UI → locale switching →
editorial polish. **Never cut: provenance capture, the approval gate.**
Those cannot be added afterwards.

Quality bar: Go minimum 90% statement coverage and TypeScript minimum
80%, both CI-enforced; integration tests run against a real Postgres in
CI; table-driven tests in Go; strict `golangci-lint` and clean
`go vet`; every exported Go symbol documented.

## Governance

This constitution supersedes all other practices in this repository.

- Founder-level open questions (indexing/crawler posture, data
  retention periods, LLM translation provider, per-source usage rules)
  are decided by the founder alone. Specs, plans and code must not
  silently resolve them; until answered, the recorded safe defaults
  apply (block all crawlers at the edge in one place; default sources
  to `extract_and_link`).
- Amendments arrive as a signed, sole-authored PR that updates this
  document with a Sync Impact Report and a semantic version bump:
  MAJOR for removed or redefined principles, MINOR for new or
  materially expanded principles, PATCH for clarifications.
- Every PR review verifies compliance: invariants keep their
  database-level rejection tests, boundaries hold, scope stays inside
  the alpha, and commit hygiene is intact.
- Complexity must justify itself against the alpha scope; when in
  doubt, the simpler structure that preserves the invariants wins.

**Version**: 1.0.0 | **Ratified**: 2026-08-14 | **Last Amended**: 2026-08-14
