# ADR-0001: Super-app architecture — monorepo, product-scoped modular monoliths, event integration

- **Status**: Accepted (2026-08-24, constitution v1.1.0)
- **Date**: 2026-08-24
- **Deciders**: founder
- **Supersedes**: none
- **Related**: [0002](0002-cashback-money-substrate.md), [0005](0005-cashback-stack.md)

## Context

Apivo is now two products, not one: epiloYES (news) and cashback. More are
named in the founder's roadmap — price comparison, fuel saver, loyalty,
reviews, listings. The question raised is how the super app should be
structured so it stays maintainable by a **solo founder** while it grows:
microservices, several modular monoliths, events, multi-repo or monorepo.

Constraints that make this decision, rather than taste:

1. **One person operates it.** Every additional deployable is a release
   pipeline, an on-call surface, a version skew and a debugging hop that one
   person pays for forever. Operational cost dominates.
2. **The products have genuinely different legal exposure.** News carries
   content-licensing liability; cashback carries money-handling liability.
   Bleeding one into the other is the failure mode worth engineering
   against.
3. **The constitution already enforces module boundaries** under
   `internal/` with an architecture test, and already has an append-only
   `domain_event` stream. The structure that answers this question is
   half-built.
4. **Alpha volume is tiny** — hundreds of members, low thousands of clicks
   per month. Nothing here is a scaling problem. Every argument for
   microservices at this size is an argument from anticipation.
5. **News is mid-delivery.** Four agents are working inside the existing
   package layout. A restructure that moves those packages now would
   invalidate open work.

## Decision

**One repository. One Go module. One deployable for the alpha. Product
domains as peer modular monoliths inside it, isolated by schema, connected
only by asynchronous events, with the boundaries enforced by tests.**

Concretely:

### Repository — monorepo

The existing repository stays and absorbs cashback. No second repository, no
submodules, no Go workspace with multiple modules. Renaming the GitHub
repository from `apivo-news` to `apivo` is cosmetic and is founder question
Q9; the internal structure is designed so the rename changes nothing.

### Code — product domains as peers under `internal/`

```text
internal/
├── platform/        # kernel: config, db, http, logging, events, brand, i18n
├── identity/        # shared: accounts, JWT validation, roles
├── ingestion/       # news domain  ─┐
├── translation/     # news domain   ├─ existing, NOT moved by this decision
├── editorial/       # news domain   │
├── content/         # news domain  ─┘
└── cashback/        # new product domain
    ├── catalogue/   # merchants, offers, rate bands
    ├── clickout/    # tracked redirects, click records
    ├── networks/    # affiliate network adapters (see ADR-0003)
    ├── earnings/    # cashback entries, state machine, attribution
    ├── wallet/      # ledger projection, balances (see ADR-0002)
    ├── payout/      # withdrawal, approval, rails
    └── ops/         # operator queues, reconciliation
```

Import rules, extending the existing architecture test:

- `platform` may be imported by anyone; it imports no domain.
- `identity` may be imported by any product domain; it imports only
  `platform`.
- **A product domain may not import another product domain, at any depth.**
  `cashback/*` cannot import `content/*`, and vice versa.
- Sub-packages of a product domain may import their siblings.
- Composition happens only in `cmd/`.

Existing news packages are **not moved**. Grouping them under
`internal/news/` later is a pure rename that changes no semantics, and it
waits until the news alpha is delivered.

### Data — one database, one schema per product

One Postgres instance. `cashback` gets its own schema. Shared reference data
(`account`, `place`, `language`, `domain_event`) stays where it is today, in
`public`, readable by both. The news tables are not moved into a `news`
schema by this decision — that is a later, optional tidy-up on the same
footing as the package rename below.

**No foreign key crosses a product schema boundary.** A cashback row that
refers to an account holds the account id and validates it through the
identity module, not through a cross-schema constraint. A migration lint
fails the build on a cross-product FK.

Rationale: one database means one backup, one migration runner, one
transaction when it matters, and one `EXPLAIN` when something is slow.
Schema separation means the boundary is nonetheless real, visible in `\dn`,
and grantable to separate roles — which is what the legal separation
actually needs.

### Integration — asynchronous, through the existing event stream

Products communicate **only** by events, never by direct call:

- A domain writes an event to a **transactional outbox** in its own schema,
  in the same transaction as its state change.
- A dispatcher in `platform/events` moves outbox rows to the append-only
  `domain_event` stream and delivers them to in-process subscribers with
  at-least-once semantics and an idempotency key.
- Subscribers are idempotent by construction; a redelivered event is a
  no-op.

No message broker in the alpha. A Postgres-backed outbox handles this
volume with room to spare, and it removes an entire class of
"the-broker-and-the-database-disagree" failure. Swapping the transport for
NATS or Kafka later is a change inside `platform/events`, invisible to every
domain.

### Deployment — one binary now, extraction as a deployment change later

The alpha ships **one Go binary** serving both products, with cashback
behind a feature flag, plus the existing Astro frontend. Because domains
share nothing but `platform` and `identity`, communicate only by events, and
own separate schemas, extracting `cashback` into its own binary later means:
add a `cmd/apivo-cashback`, point it at the same database, change the event
dispatcher from in-process to transport-backed. No domain code changes.

**Documented split triggers** — extract a product into its own deployable
when *any* of these is true, and not before:

1. The two products need materially different scaling or availability
   (cashback payouts must stay up while a news ingestion storm is running).
2. A compliance requirement demands process-level or network-level isolation
   of money handling.
3. Release cadences conflict — one product's deploys are routinely blocked
   by the other's.
4. More than one engineer works full-time on each product.

None is true today, and #4 is the honest reason microservices are wrong
here.

## Consequences

**Positive**

- One deploy, one log stream, one debugger, one migration path for a solo
  operator.
- Boundaries are enforced by a build-failing test today, so the option to
  split stays open at near-zero cost.
- Cross-product coupling is impossible to add by accident: it would have to
  pass an import test, a migration lint and a code review.
- The event stream that already exists for audit becomes the integration
  spine, so no new infrastructure is introduced for it.

**Negative / accepted costs**

- A cashback bug can take the news site down with it. Mitigated by
  per-product route mounting, panic isolation at the HTTP boundary, and the
  feature flag; not eliminated. This is the central cost and it is accepted
  knowingly.
- Async-only integration means eventual consistency between products.
  Acceptable: nothing cross-product needs a synchronous answer.
- The monorepo's CI grows; a cashback change runs news tests. Mitigated by
  path-filtered jobs, not by splitting the repository.
- Cross-schema FKs being forbidden moves referential integrity for
  account references into application code — a real weakening, deliberately
  scoped to that one relationship.

## Alternatives considered

| Alternative | Why rejected |
|---|---|
| **Microservices per product** | Solo founder. Multiplies deployables, pipelines, secrets, network failure modes and distributed-transaction problems, in exchange for scaling and team-autonomy benefits that a one-person, hundreds-of-users alpha cannot use. The split triggers above are the honest test; none is met. |
| **Separate repository for cashback** | Every shared change (identity, brand, place model, platform) becomes a cross-repo version bump and a two-PR dance. Loses atomic refactors across the boundary, which is precisely what a young codebase needs most. |
| **Separate database per product** | Doubles backup, migration and connection management for a legal separation that a schema plus a role already provides at this scale. Revisit if a compliance requirement demands physical separation. |
| **Message broker (NATS/Kafka) from day one** | Adds a stateful dependency and dual-write consistency problems to replace a Postgres table that is already append-only and already backed up. Kept as the documented upgrade path inside `platform/events`. |
| **Single shared schema for both products** | Nothing then prevents a cross-product join, and the first one written becomes permanent. The schema boundary is cheap; its absence is not recoverable. |
| **Modular monolith with separate binaries now** | Buys process isolation before it is needed and pays for it every release. Deferred behind the split triggers. |

## Revisit triggers

- Any split trigger above becomes true.
- Alpha volume grows by two orders of magnitude.
- A second full-time engineer joins.
- A compliance opinion (founder question Q2) requires isolation of money
  handling at the process or network level.
