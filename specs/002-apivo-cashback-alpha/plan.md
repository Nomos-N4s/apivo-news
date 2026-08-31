# Implementation Plan: Apivo Cashback Alpha — Member Cashback for the Apivo Super App

**Branch**: `002-apivo-cashback-alpha` | **Date**: 2026-08-24 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/002-apivo-cashback-alpha/spec.md`

> **GATE PASSED.** The founder ratified constitution **v1.1.0** on
> 2026-08-24, bringing cashback into scope, reworking the single-binary
> constraint, and adding Principle IX (invariants C-1 to C-7). Founder
> question **Q2** is decided: the rebate-claim posture. ADR-0001 to
> ADR-0005 are **Accepted**. Implementation may proceed — see
> [Constitution Check](#constitution-check) and [research.md](research.md)
> §8.

## Summary

Apivo becomes a super app by adding cashback as its second product. A member
clicks out to a merchant through a tracked affiliate deeplink; the network
reports the resulting commission; Apivo credits the member an agreed share,
and pays it out once confirmed. The money is carried by an adopted
open-source double-entry ledger — **Blnk, Apache-2.0**
([ADR-0002](../../docs/adr/0002-cashback-money-substrate.md)) — because the
survey found no production-grade, unrestricted open-source cashback platform
exists; the closest, Open Loyalty, caps commercial use at 200 customers.

Everything cashback-specific is built in the existing Go monolith as a peer
product domain: one repository, one binary for the alpha, schema isolation,
and asynchronous event integration
([ADR-0001](../../docs/adr/0001-super-app-architecture.md)). No second stack
([ADR-0005](../../docs/adr/0005-cashback-stack.md)). Affiliate networks sit
behind one consumer-defined port with fixture-first development, so the
founder's network decision blocks go-live but not engineering
([ADR-0003](../../docs/adr/0003-affiliate-network-integration.md)).
Rebrandability is a configuration boundary proved by a CI test, not a claim
([ADR-0004](../../docs/adr/0004-white-label-rebranding.md)).

The legal-defensibility discipline the news product applies to content
licensing is applied here to money: seven invariants, C-1 to C-7, enforced
by the database with tests that assert **the database** rejects the illegal
state.

## Technical Context

**Language/Version**: Go 1.26 (backend); TypeScript 5.9 strict / Node 24
(frontend) — unchanged (ADR-0005)

**Primary Dependencies**: pgx/v5, golang-migrate, sqlc 1.31.1 (existing);
**Blnk Go SDK** (pinned) and **Redis** (new, required by Blnk); Astro 7 +
`@astrojs/node`, Supabase Auth JS SDK

**Storage**: Postgres — Supabase (EU) in production, `postgres:17` via
compose and as a CI service container. New `cashback` schema; Blnk in a
`blnk` schema in the same instance, **subject to spike S1**

**Testing**: `go test` (table-driven; C-1..C-7 invariant tests, contract
tests, port conformance suites, integration against real Postgres, 90%
gate); vitest + `astro check` (80% gate); adapter live contract tests
skipped without credentials

**Target Platform**: Linux containers — the existing Hetzner compose host
and Cloudflare Containers (`jurisdiction: eu`), plain Kubernetes manifests
kept valid in parallel

**Project Type**: web application — Go modular monolith API (two product
domains, one binary) + Astro SSR frontend + Blnk ledger sidecar

**Performance Goals**: wallet and catalogue render < 2 s (SC-009); API p95
< 200 ms at alpha scale; poller respects 6 req/s and 31-day windows for the
reference network

**Constraints**: C-1..C-7 database-enforced (C-1 in the ledger, verified
continuously — see Complexity Tracking); money is integer minor units
everywhere, no float, no decimal; no anonymous click can ever be credited;
webhooks never move money; cashback surfaces are non-indexable behind the
existing single crawler gate; nothing platform-specific in application code

**Scale/Scope**: alpha — hundreds of members, tens of merchants, low
thousands of clicks/month, one affiliate network. No horizontal-scale work.

**Unknowns**: all resolved in [research.md](research.md) D1–D12. The nine
founder-only questions (spec Q1–Q9) are **not** unknowns to resolve here —
each has a recorded safe default and a stated blast radius; only Q2 (PSD2
posture) must be answered before public launch, and none blocks build.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Status | Evidence in this plan |
|---|---|---|
| I. Sole authorship & signed commits | PASS | Workflow unchanged: signed sole-author commits, atomic commits, one PR per issue, `gh stack` for dependent chains |
| II. I-1 human approval | PASS (extended) | Unaffected for news; the same pattern is reused as **C-4** — `payout.approved_by NOT NULL`, the row *is* the approval |
| III. I-2 provenance at retrieval | PASS (extended) | Network transactions store retrieval time, account, query window and raw payload in the same transaction as the content — no deferred path exists |
| IV. I-3 immutable evidence | PASS (extended) | `raise_immutable()` reused on `network_transaction`, `click` and `reconciliation_run`; reversals insert, never edit |
| V. I-4 licence snapshot | N/A | No licensed content in this feature; the analogue is the click-time **rate snapshot** (FR-013) |
| VI. I-5 traceability | PASS (extended) | `cashback.provenance` view answers C-7 in one query |
| VII. Language ⊥ place | PASS | Catalogue takes `lang` and `place` as separate parameters; `merchant_copy` keyed by language, `merchant_place` by place; no combined locale anywhere |
| VIII. DB-enforced invariants | PASS (with named exception) | C-2..C-7 are database-enforced with SQLSTATE-asserting tests. **C-1 (double-entry zero sum) lives in the adopted ledger** — permitted by v1.1.0's exception clause, which requires the ADR, the continuous check, and a working in-repository implementation. All three are in place |
| **IX. Money is double entry, evidence-backed, exactly once** | PASS | C-1..C-7 mapped to constraints, triggers, unique indexes and views in [data-model.md](data-model.md) §4, each with a database-rejection test |
| Architecture: modular monolith per product, **no microservices** | PASS (under v1.1.0) | One Go binary serving both products. **Blnk runs as a sidecar** under the v1.1.0 clause permitting a self-hosted open-source ledger; Redis supports it and holds no source of truth |
| Architecture: module boundaries | PASS (strengthened) | Product domains may not import each other at any depth; enforced by the existing architecture test plus a new migration lint on cross-schema FKs |
| Architecture: generated types both sides | PASS | sqlc + `supabase gen types` extended to the `cashback` schema, drift-checked in CI |
| Architecture: adapters swappable in < 5 engineer-days | PASS | Three ports (`Ledger`, `Network`, `PayoutRail`), each with a second working implementation in-repo as proof |
| Architecture: containers first, platform-agnostic | PASS | Blnk and Redis added to compose, k8s manifests and the Hetzner stack alike; no platform specifics in application code |
| Architecture: Products (schema per product, no cross-product FK or import, events only) | PASS | New in v1.1.0; enforced by the architecture test and the migration lint |
| Architecture: Rebrandability | PASS | New in v1.1.0; brand configuration, brand-literal lint, fixture-brand render in CI |
| **Product scope** | PASS (under v1.1.0) | Cashback is now a named second product with its own scope block and cut order; nothing in this plan reaches outside it |
| Quality bar (90% Go / 80% TS, real Postgres, strict lint) | PASS | Unchanged, plus the C-1..C-7 invariant suite as a hard gate |

**Verdict**: **PASS under constitution v1.1.0** (ratified 2026-08-24). The
two violations against v1.0.0 — cashback being out of scope, and the ledger
sidecar against "single binary" — are resolved by the amendment rather than
by exception. Principle VIII carries one named exception (C-1), which the
amendment's own clause permits and constrains. History is kept in
[Complexity Tracking](#complexity-tracking).

**Post-Phase-1 re-check**: the design introduces **no new violation**. The
data model, contracts and ports keep every other principle intact, and the
C-1 partial is narrowed as far as the adoption decision allows — Blnk's
schema is co-located in the same database precisely so the check is a real
SQL query over real rows, and the `postgres` implementation of the `Ledger`
port is kept as an exit route that would restore VIII in full.

## Project Structure

### Documentation (this feature)

```text
specs/002-apivo-cashback-alpha/
├── spec.md              # feature specification (+ founder questions Q1–Q9)
├── plan.md              # this file
├── research.md          # Phase 0: decisions D1–D12, ADR summaries, amendment draft
├── data-model.md        # Phase 1: cashback schema, state machine, invariant map
├── quickstart.md        # Phase 1: run + validation scenarios V1–V10, spikes S1–S3
├── contracts/
│   ├── http-api.md      # member + operator endpoints
│   ├── ports.md         # Ledger, Network, PayoutRail
│   └── events.md        # cross-product event contract
└── tasks.md             # Phase 2 — created by /speckit-tasks, NOT by this command

docs/adr/                # repository-wide decisions (new)
├── README.md
├── 0001-super-app-architecture.md
├── 0002-cashback-money-substrate.md
├── 0003-affiliate-network-integration.md
├── 0004-white-label-rebranding.md
└── 0005-cashback-stack.md
```

### Source Code (repository root)

```text
cmd/apivo/                       # composition root: both products, ports wired here
internal/
├── platform/                    # kernel — extended, not forked
│   ├── brand/                   # NEW: one brand configuration (ADR-0004)
│   ├── money/                   # NEW: Amount{Minor int64; Currency}
│   ├── events/                  # NEW: outbox + dispatcher over domain_event
│   ├── scheduler/               # NEW: in-process jobs, Postgres advisory lock
│   ├── config/ db/ http/ logging/ text/   # existing
├── identity/                    # existing, shared by both products
├── ingestion/ translation/ editorial/ content/   # news — UNCHANGED, not moved
└── cashback/                    # NEW product domain
    ├── catalogue/               # merchants, offers, rate bands, localisation
    ├── clickout/                # tracked redirects, click records
    ├── networks/                # Network port + fixture/ + <network>/ adapters
    ├── earnings/                # entries, state machine, attribution
    ├── wallet/                  # Ledger port + blnk/ memory/ postgres/
    ├── payout/                  # PayoutRail port + manual/ stub/; withdrawal flow
    └── ops/                     # operator queues, reconciliation
web/
├── src/pages/{lang}/{place}/cashback/   # catalogue, merchant, wallet, withdrawals
├── src/pages/ops/                       # operator queues (role-gated)
└── src/lib/brand/                       # brand tokens shared with Go config
internal/platform/db/migrations/         # 0010–0017 (see data-model.md §5)
deploy/                                  # blnk + redis added to compose, k8s, hetzner
```

**Structure Decision**: cashback is a **peer product domain inside the
existing repository and binary**, not a service and not a second repo
(ADR-0001). Existing news packages are **not moved** — grouping them under
`internal/news/` is a later pure rename, deliberately deferred so in-flight
news work stays valid. The only new processes are Blnk and Redis, both
sidecars in every deployment target.

## Phases

Every phase leaves CI green and the coverage gates satisfied. Invariant work
always lands with its database-rejection test in the same PR. Small PRs, one
per issue; dependent chains go through `gh stack`.

### Phase 0 — Ratification and spikes (blocking)

Nothing else starts until these land.

1. **Constitution amendment PR** (v1.0.0 → v1.1.0) from the draft in
   research.md §8 — founder-authored, signed, with its Sync Impact Report.
2. **Spike S1**: Blnk against Supabase Postgres in a dedicated schema and
   role.
3. **Spike S2**: outbox + shared idempotency key across Apivo and the
   ledger, with a kill-between-the-two crash test.
4. **Spike S3**: the full cashback job green in CI with Blnk and Redis as
   service containers, and `LEDGER_DRIVER=memory` working without Docker.

Exit: amendment merged, all three spikes passing or their ADR-0002
fallbacks adopted explicitly.

### Phase A — Platform extensions

`platform/money`, `platform/brand` (+ fixture brand and the brand-literal
lint), `platform/events` (outbox, dispatcher, dead-letter), and
`platform/scheduler` (advisory-lock jobs). No cashback domain code yet;
each lands with its own tests and is useful to news as well.

### Phase B — Schema and invariants

Migrations 0010–0017, the `cashback.provenance` and
`cashback.ledger_zero_sum` views, the migration lint for cross-schema FKs,
sqlc generation for the new schema, and the **C-1..C-7 invariant suite**.
This phase is where the legal guarantees become structural; it precedes
every feature that depends on them.

### Phase C — Ledger port and money movement

The `Ledger` port with all three implementations (`blnk`, `memory`,
`postgres`) and the shared conformance suite. Wallet totals computed from
postings. The continuous zero-sum check wired into the deployed
environment.

### Phase D — Catalogue and click-out (US5, US1 first half)

Catalogue queries with language fallback and place scoping, merchant pages,
the click-out endpoint with the rate snapshot, and the per-member and
per-context rate limits.

### Phase E — Network ingestion (US2, US1 second half)

The `Network` port, the `fixture` adapter with its full lifecycle, the
poller with dual cursors and trailing re-reads, evidence persistence, the
attribution path, and the unattributed queue. The first real adapter lands
here **if** the founder has answered Q1; if not, the phase completes on
fixtures and the real adapter is a later, self-contained PR. *Resolved
2026-08-31: the phase completed on fixtures, and Q1 answered Awin — so the
Awin adapter is that later self-contained PR.*

### Phase F — Wallet and entry lifecycle (US3, US7)

Entry state machine with a ledger transfer behind every transition, hold
rules, wallet endpoints and pages, reversal handling.

### Phase G — Withdrawal and payout (US4)

Destinations and verification, reservation on request, the approval gate
carrying `approved_by`, the `PayoutRail` port with `manual` and `stub`, and
the exactly-once suite (concurrency, replay, timeout, terminal failure).

### Phase H — Operations and reconciliation (US6)

Operator queues, statement import, difference detection and resolution,
accounting exports, and the promotion rule that only confirms entries whose
commission is reconciled as received.

### Phase I — Rebrand proof and hardening

Fixture-brand rendering across every member-facing surface in CI, the
brand-literal lint at zero exceptions, the architecture and migration lints
at zero exceptions, and the V1–V10 validation scenarios wired as automated
tests.

**Cut order under time pressure**: operator polish → catalogue breadth →
reconciliation automation (manual reconciliation carries the first weeks).
**Never cut**: the invariant suite, evidence immutability, the approval gate
on payouts, or the exactly-once payout tests. Those cannot be added
afterwards.

## Complexity Tracking

> Retained as the justification record for what the v1.1.0 amendment
> changed, and for the one exception the amendment permits rather than
> removes.

| Trade | Why needed | Simpler alternative rejected because |
|---|---|---|
| **Cashback brought into scope** *(resolved by amendment)* | The founder is expanding the MVP to a super app; cashback is the second product and the first revenue line | There was no simpler alternative — the constraint was a scope statement and the founder changed the scope. Amended, not worked around. |
| **A second runtime service (Blnk) + Redis** *(resolved by amendment)* | The founder's directive is to adopt an open-source offering, and the only production-grade unrestricted options are ledgers, which are servers. Blnk carries double entry, idempotency and reconciliation — the most expensive things to get wrong | Building the ledger in Postgres inside the monolith **would** have kept C-1 in our schema, but it contradicts the explicit directive and puts money correctness back on us. Kept as a fully-specified exit route behind the `Ledger` port, so the decision is reversible in days. |
| **C-1 enforced outside our database** *(live exception under Principle VIII)* | Balances live where the ledger lives | Mirroring balances into our schema would create two truths — precisely the failure mode Principle VIII exists to prevent. Instead: Blnk's schema is co-located in the same Postgres so the zero-sum check is a real query, the check runs continuously and a failure is treated as an incident, and no member-facing number is ever computed outside the ledger. Named, not hidden. |
| **A new product domain, schema and event spine** | Two products with different legal exposure sharing one database need a real boundary; the event spine reuses `domain_event`, which already exists | A single shared schema is simpler on day one and unrecoverable on day two — the first cross-product join becomes permanent. The boundary costs one schema and two lints. |

## Next Steps

1. ~~Ratify the constitution amendment.~~ **Done 2026-08-24 — v1.1.0.**
2. ~~Decide Q2 (PSD2 / e-money posture).~~ **Done 2026-08-24 —
   rebate-claim.**
3. ~~**Founder action — apply for an affiliate network publisher account
   (Q1).**~~ **Done 2026-08-31 — Awin**, publisher account approved and API
   credentials held. It gated go-live and revenue, not engineering, exactly
   as planned: Phase E completed on the `fixture` adapter, and the Awin
   adapter is now the later self-contained PR that was always intended.
   Per-merchant programme approval remains a per-programme business process
   and still gates what can actually be earned on.
4. Remaining founder questions Q3–Q9 all have working defaults and block
   nothing.
5. Run `/speckit-tasks` to generate `tasks.md` from these artifacts, then
   `/speckit-taskstoissues` for the milestone backlog — the same route the
   news alpha took. Phase 0 (spikes S1–S3) is the first milestone.
