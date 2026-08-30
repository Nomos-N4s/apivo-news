<!--
Sync Impact Report
- Version change: 1.1.1 → 1.2.0 (MINOR: a rule is added - branches are named
  `xcoder/<slug>` - and Principle I is extended to cover ref names. No
  principle is removed or redefined, and nothing already permitted becomes
  forbidden except the naming of a ref after an assistant or a vendor, which
  the quoted authorship rule already forbade in the commit message a ref name
  ends up inside.)
- Modified sections:
  * Principle I: ref names are brought under the principle explicitly. A
    merged branch name is written verbatim into the merge commit, so
    "never mention ... in commit messages" already reached it - but only
    through a step nobody took, which is how `main` came to carry 32 commits
    reading "Merge pull request #N from <owner>/<assistant>/<slug>".
  * Principle I: the branch naming rule is stated - `xcoder/<slug>`, with
    `main` and the refs GitHub and the dependency bots generate as the
    exceptions. The repository had followed it 85 times without it being
    written down anywhere, which is exactly how a tool's default prefix
    leaked in 24 more times without anyone having to overrule anything.
  * Principle I, "Enforcement": the pre-push hook and scripts/lint-refs.sh
    are added to the list, in the order they bite. The note's closing
    sentence already said only the CI job can refuse a commit; it now also
    says which of these is the only one that can refuse a MERGE.
  * Principle I: the carve-out is stated - a check may name what it refuses.
    scripts/lint-commit-authors.sh has carried the blocked names since it was
    written, and lint-refs.sh and CLAUDE.md now do too. A blocklist that
    could not name what it blocks would be an empty file.
- Rationale: the violation was recorded in shared history and cannot be
  corrected there. What remains is to stop the next one, and the only moment
  at which a ref name is free to change is before it is pushed - which is
  earlier than anything this document previously described.
- Follow-up: the 32 merge commits on `main` stay as they are. Rewriting them
  would rewrite every commit after each one, and this repository's history is
  shared. They are recorded as known violations rather than corrected ones.

Previous amendment (1.1.0 → 1.1.1, PATCH: Principle I's enforcement note now
  describes the checks that exist; no principle added, removed or redefined,
  and no rule changed for anyone)
- Modified sections:
  * Principle I, "Enforcement": the note listed the commit-msg hook and the
    commit-hygiene job, and was written before either the author/committer
    check or the session-start identity hook existed. It described neither,
    so it understated what a commit is checked against and named none of the
    ordering. It now lists all four in the order they bite, and says plainly
    that only the CI job can refuse a commit - a local hook proves nothing
    about a commit that is already pushed.
- Rationale: authorship was violated in practice while this note was
  accurate about the checks it named, because the identity fields, not the
  trailers, were what carried the attribution. Recording what actually
  guards the principle is the point of the note.

Amendment before that (1.0.0 → 1.1.0, MINOR: principles added, constraints
expanded; none removed or redefined)
- Added principles: IX. Money Is Double Entry, Evidence-Backed and Exactly
  Once (invariants C-1 to C-7)
- Added sections: "Products" and "Rebrandability" under Architecture
  Constraints; "Cashback alpha" under Product Scope & Delivery Rules
- Modified sections:
  * "Alpha Scope & Delivery Rules" → "Product Scope & Delivery Rules",
    split per product; the news alpha text is carried verbatim
  * Architecture Constraints: the single-binary / no-microservices line now
    reads "a modular monolith per product domain, composed into one binary";
    a self-hosted open-source ledger is permitted as a sidecar with the C-1
    trade named explicitly
  * Product Scope: cashback moves from "out of scope" to a named second
    product with its own scope block; the remaining ecosystem mini-apps
    stay out of scope
  * Governance: founder-level open questions extended with the cashback
    questions Q1–Q9; Q2 recorded as decided
- Removed sections: none
- Follow-up TODOs: founder answers to cashback questions Q1, Q3–Q9
-->

# Apivo Constitution

Apivo is a super app for Greek communities abroad. Its first surface is
**epiloYES**: a multilingual local newspaper — a Greek speaker in Munich
reads Munich news in Greek. Its second is **cashback**: members earn a share
of the affiliate commission their purchases generate.

Both carry real legal exposure — content licensing for news, money handling
for cashback. This constitution exists to make the protections against that
exposure structural rather than habitual.

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

**The rule reaches ref names.** A merged branch name is written verbatim
into the merge commit — `Merge pull request #349 from
Nomos-N4s/<branch>` — so a branch named after an assistant or a vendor
puts that name in a commit message, permanently and publicly. The name
is the last moment at which this is free to fix: afterwards only a
rewrite of shared history removes it, and shared history is not rewritten
here. Tags are covered for the same reason.

**Every branch is `xcoder/<slug>`.** `main` is the only exception, along
with the refs nobody here chooses — GitHub's revert button, the
dependency bots. The convention is stated because it was followed 85
times without being written down anywhere, and an unwritten convention
overrules nothing: a tool arriving with a default prefix of its own
simply used it, 24 times, and nobody had to decide to allow it. A
blocklist can only refuse what somebody thought to write down; requiring
the prefix refuses the next default too.

**A check may name what it refuses.** `scripts/lint-refs.sh`,
`scripts/lint-commit-authors.sh` and `CLAUDE.md` carry the assistant and
vendor names this principle forbids, because a blocklist that could not
name what it blocks would be an empty file. The exception is theirs
alone; it does not extend to any other code, comment or document.

Enforcement, in the order it bites:

- `.claude/hooks/session-start.sh` sets the commit identity from the
  checkout at every session start. An agent session runs in a container
  that carries a git identity of its own and restores it on restart, so an
  identity set by hand holds only until the next one; setting it from the
  repository is what makes authorship survive a restart rather than depend
  on somebody noticing.
- `.githooks/commit-msg` (wired via `core.hooksPath`) strips disallowed
  trailers locally.
- `.githooks/pre-push` refuses a ref name before it reaches the remote,
  which is the earliest point at which a rename costs nothing. Deletions
  pass: removing a badly named branch is the remedy, not another
  offence.
- `scripts/lint-commit-authors.sh` checks the **author and the committer**
  of every commit on the branch against a blocklist of AI attribution
  identities. A trailer is not the only way attribution reaches a commit,
  and the identity fields are the way it reached one here.
- `scripts/lint-refs.sh` refuses a ref that names an assistant or a
  vendor, and a branch that is not `xcoder/<slug>`. It also reads the ref
  names git has already written into commit subjects, which is the shape
  the violation actually took.
- The `commit-hygiene` CI job runs the message check, the author check
  and both halves of the ref check on pull requests and on pushes to
  `main`.

The local hooks are conveniences that keep a mistake from being made; only
the CI job can refuse one. A local check that has been skipped, and a hook
in a container that no longer exists, prove nothing about a commit that is
already pushed — or about a branch name that is, since the merge commit
quoting it is written server-side where no local hook runs at all.

Every check here is proved by a suite of its own that CI runs before
trusting its verdict. A gate is only as good as the proof that it closes,
and each of these was written after a green gate had already let the thing
it names through.

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

This principle applies to every product. The cashback catalogue takes
`lang` and `place` as separate parameters for the same reason.

### VIII. Database-Enforced Invariants over Application Discipline

Application code is never trusted with a legal guarantee. Every
invariant above is enforced by the database (constraints, triggers,
views) and carries an explicit test asserting the DATABASE rejects the
illegal state, by SQLSTATE, against a real Postgres. Coverage numbers
are necessary but not sufficient; a passing gate without these tests
means nothing.

Where an adopted component takes an invariant outside our own schema, the
exception must be named in an ADR, the invariant must be verified
continuously by an automated check that fails loudly, and an in-repository
implementation that would restore full enforcement must be kept working.
See Principle IX, C-1.

### IX. Money Is Double Entry, Evidence-Backed and Exactly Once (NON-NEGOTIABLE)

Cashback owes real money to real people out of money a third party says it
will pay. The same discipline that defends the news product against
licensing exposure defends the cashback product against money exposure.
Seven invariants, enforced by the database, each with a test asserting the
DATABASE rejects the illegal state:

- **C-1 (Double entry)**: A member balance is never stored as a settable
  number. It exists only as the sum of immutable ledger postings, and every
  posting belongs to a transfer whose postings sum to zero per currency.
- **C-2 (Attribution)**: A cashback credit cannot exist without a reference
  to exactly one network transaction record and, through it, at most one
  click record. Credits with no evidence are unrepresentable.
- **C-3 (Immutable network evidence)**: Network transaction records, click
  records and imported statements reject UPDATE, DELETE and TRUNCATE. A
  status change is a new superseding record, never an edit.
- **C-4 (No payout without a named approver)**: A payout row cannot exist
  without a non-null named human approver. The row IS the approval, exactly
  as `article.approved_by` is for news.
- **C-5 (Exactly-once money movement)**: Every outbound payout carries a
  unique idempotency key with a database uniqueness constraint, derived
  deterministically from the withdrawal request. A retry cannot create a
  second payout.
- **C-6 (Integer money)**: All monetary amounts are integer minor units with
  an explicit ISO-4217 currency code. Floating point in a money column, or a
  posting without a currency, is rejected by the schema. No decimal ever
  crosses an API boundary.
- **C-7 (Traceability)**: For any member payout, one query returns the full
  chain — payout, approver, ledger postings, cashback entries, network
  transaction evidence, click, and the offer rate at click time — in under
  five minutes.

**C-1 exception, named as required by Principle VIII**: the double-entry
guarantee is carried by the adopted open-source ledger rather than by a
constraint in our own schema (ADR-0002). It is verified continuously by a
zero-sum check over real rows, that check failing is treated as an incident,
no member-facing number is ever computed outside the ledger, and a Postgres
implementation of the ledger port is kept working as the exit route.

## Architecture Constraints

Decided through an ADR process; implement, do not re-litigate. Concrete
blockers stop work and go to the founder. Records live in `docs/adr/`.

- Frontend: **Astro** (v6+), TypeScript strict, `@astrojs/node` adapter.
- Backend: **Go** — a modular monolith per product domain, composed into
  one binary for the alpha. **No microservices.** Any future extraction of
  a product into its own deployable must be a deployment change, not a
  redesign, and only against the split triggers documented in ADR-0001.
- Database: **Supabase** (Postgres), EU region.
- Auth: Supabase Auth; Astro uses the JS SDK, Go validates the JWT.
- Types are generated from the Postgres schema — `sqlc` for Go,
  `supabase gen types` for TypeScript. **Never hand-write types on both
  sides.** CI fails on drift between schema and generated code.
- Module boundaries under `internal/` — `platform/` may be imported by
  anyone; `identity/` may be imported by any product domain; no other
  module imports a sibling's internals. Modules communicate through
  interfaces defined by the consumer, wired in `cmd`. An architecture test
  fails the build on violations.
- Deployment is container-first: Cloudflare Containers and the Hetzner
  compose host today (EU jurisdiction), Kubernetes-ready by construction.
  Nothing platform-specific leaks into application code; platform bindings
  stay behind interfaces in `internal/platform`.
- Every external dependency sits behind a consumer-defined interface,
  swappable in under five engineer-days, proved by a second working
  implementation in the repository. This applies to the LLM translation
  adapter (with a per-article cost ceiling and a monthly cap that halts the
  pipeline rather than overspending), the ledger, affiliate network
  adapters and payout rails alike.
- A **self-hosted open-source ledger may run as a sidecar service** beside
  the binary, with its supporting infrastructure, where it carries a
  correctness burden we would otherwise write ourselves. This is the single
  permitted exception to one-process-per-application, it must be
  Apache/MIT-class licensed with no user or revenue cap, and the invariant
  it takes out of our schema must be handled per Principle IX, C-1.

### Products

- A **product domain** owns its own Postgres schema. Shared reference data
  (`account`, `place`, `language`, `domain_event`) is the only thing both
  products read.
- **No foreign key crosses a product schema boundary.** A migration lint
  fails the build on one.
- **A product domain may not import another product domain, at any depth.**
  Cross-product communication is asynchronous only, through the
  transactional outbox into the append-only `domain_event` stream. There is
  no synchronous call from one product into another.
- Adding a product means adding a schema, a domain package and event
  subscriptions — never modifying another product.

### Rebrandability

- No product name, legal entity, domain, colour, logo, support address or
  currency default is hardcoded in application code, templates or
  migrations. All of it resolves from one brand configuration, and a CI
  lint fails on a literal outside it.
- All member-facing text lives in translation catalogues keyed by BCP-47
  primary language subtag.
- A fixture brand renders every member-facing surface in CI. Rebrandability
  is a test that goes red, not a claim in a document.
- Simultaneous multi-tenancy is **not** built. Forward compatibility costs
  one brand id on the records where a tenant boundary would fall, and no
  global brand singleton.

## Product Scope & Delivery Rules

### epiloYES (news) v1.0.0-alpha

In scope: Greek and German; Munich as reader locale; Greek national and
Munich local sources; RSS/Atom feeds only — no scraping; text only — no
images; translated headline and extract linking back to the source — not
full-text translation; human approval on every item; full provenance;
reader front page and article pages, locale-scoped, attribution rendered;
registration and consent capture in the schema (UI only if time allows).

Individual sources are upgraded to full text only with recorded written
permission; every new source defaults to `extract_and_link`.

Cut order under time pressure: registration UI → locale switching →
editorial polish. **Never cut**: provenance capture, the approval gate.
Those cannot be added afterwards.

### Cashback alpha

In scope: affiliate publishing with revenue share — merchant catalogue with
place scope and per-language copy; tracked click-out with a click-time rate
snapshot; polled ingestion of network transactions as immutable evidence;
attribution to clicks; a member wallet whose totals derive from ledger
postings; withdrawal with a named approver and exactly-once payout;
operator queues for unattributed transactions, held entries, reconciliation
differences and withdrawal approvals; reconciliation against network
statements.

Member balances are a **claim on a future rebate**, not stored value: no
member-to-member transfers, no spending inside Apivo, payouts only to a
destination the member owns and has verified. *(Founder decision,
2026-08-24.)*

Polling is the only thing that creates a credit. A webhook or push
notification may shorten latency by triggering a targeted poll; it never
moves money on its own.

Cut order under time pressure: operator polish → catalogue breadth →
reconciliation automation. **Never cut**: the C-1..C-7 invariant suite,
evidence immutability, the approval gate on payouts, the exactly-once
payout tests. Those cannot be added afterwards.

### Out of scope for both products

Do not build, do not scaffold, do not leave TODOs for: images, scraping,
full-text translation, search, comments, newsletter, social login, the
ebest.gr user migration, price comparison, fuel saver, loyalty points and
tiers, reviews, TV and radio listings, referral bonuses, in-app spending of
balances, member-to-member transfers, browser extension, mobile apps,
card-linked offers, in-store cashback, multi-currency wallets, automated
fraud scoring, simultaneous multi-brand tenancy, or any further ecosystem
mini-app.

### Quality bar (both products)

Go minimum 90% statement coverage and TypeScript minimum 80%, both
CI-enforced; integration tests run against a real Postgres in CI;
table-driven tests in Go; strict `golangci-lint` and clean `go vet`; every
exported Go symbol documented. For cashback, a green coverage number
without a passing C-1..C-7 invariant suite means nothing.

## Governance

This constitution supersedes all other practices in this repository.

- Founder-level open questions are decided by the founder alone. Specs,
  plans and code must not silently resolve them; until answered, the
  recorded safe defaults apply.
  - **News**: indexing/crawler posture (default: block all crawlers at the
    edge in one place), data retention periods (default: no automated
    deletion), LLM translation provider, per-source usage rules (default:
    `extract_and_link`).
  - **Cashback**: which affiliate networks to join (Q1), clawback posture
    after payout (Q3, default: absorb the loss), revenue share and rounding
    (Q4), payout rails and threshold (Q5), KYC and sanctions posture (Q6),
    tax treatment and member reporting (Q7), click-log retention (Q8),
    repository and brand naming (Q9).
  - **Decided**: the regulatory posture on member balances (Q2) — the
    rebate-claim posture recorded under "Cashback alpha", founder decision
    of 2026-08-24, for the alpha/MVP. A change to stored value is a new
    founder decision taken with legal advice.
- Amendments arrive as a signed, sole-authored PR that updates this
  document with a Sync Impact Report and a semantic version bump:
  MAJOR for removed or redefined principles, MINOR for new or
  materially expanded principles, PATCH for clarifications.
- Every PR review verifies compliance: invariants keep their
  database-level rejection tests, boundaries hold, scope stays inside
  the declared product scope, and commit hygiene is intact.
- Complexity must justify itself against the declared scope; when in
  doubt, the simpler structure that preserves the invariants wins.

**Version**: 1.2.0 | **Ratified**: 2026-08-14 | **Last Amended**: 2026-08-30
