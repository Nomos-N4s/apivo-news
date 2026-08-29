# Phase 0 Research — Apivo Cashback Alpha

**Feature**: `002-apivo-cashback-alpha` | **Date**: 2026-08-24 | **Spec**: [spec.md](spec.md)

This document resolves every NEEDS CLARIFICATION raised in
[plan.md](plan.md) Technical Context. Repository-wide decisions are recorded
as ADRs under [`docs/adr/`](../../docs/adr/README.md) and summarised here;
feature-local decisions are recorded here in full.

Founder-only questions (spec Q1–Q9) are **not** resolved here. Each is
listed in §7 with the safe default the plan proceeds under, and with what
breaks if the founder decides differently.

---

## 1. The open-source survey (the founder's core question)

**Question**: which open-source cashback offering do we adopt?

**Finding**: the category does not exist in usable form. Full evidence and
the evaluation table are in
[ADR-0002](../../docs/adr/0002-cashback-money-substrate.md). The short
version:

| Category | Representative projects | Verdict |
|---|---|---|
| Loyalty engines | **Open Loyalty** | Licence is *"Based on MIT"* but caps commercial use at **200 customers**, with immediate termination on breach. Disqualifying: the cap sits exactly where success begins. PHP/Symfony + Elasticsearch also mismatches the stack. |
| Affiliate tracking software | Refferq, Peerclick, Raider, eLitius, Weferral, Post Affiliate Pro | Wrong side of the market — they let a *merchant run* an affiliate programme. Apivo is the publisher sharing its own commission. No wallet, no commission share, no network reconciliation. |
| "Cashback" GitHub projects | `setxpro/cashback-customer-wallet`, `vippermaeglin/cashflix`, `IvanAlekseevichPopov/cashback` | Demonstration-grade, unmaintained, licence assurance absent. Not a basis for handling members' money. |
| **Financial ledgers** | **Blnk** (Apache-2.0), **Formance** (MIT), **TigerBeetle** (Apache-2.0), Apache Fineract (Apache-2.0) | The one category that is production-grade **and** unrestricted — and it covers the hardest, most expensive-to-get-wrong part of cashback. |

### D1 — Decision: adopt Blnk as the money substrate

- **Decision**: Blnk (Apache-2.0, Go, Postgres + Redis) provides ledger
  accounts, balances, transfers, postings, multi-currency and idempotency.
  The cashback domain — catalogue, click-out, attribution, entry state
  machine, reconciliation, payout orchestration — is built in the Apivo Go
  monolith on top of it, behind a one-package `Ledger` port.
- **Rationale**: unrestricted licence, same language and database as the
  rest of the stack, actively maintained through mid-2026, and it removes
  exactly the correctness burden (double entry, idempotent money movement,
  reconciliation) that is most expensive to carry ourselves. Adopting it
  satisfies the founder's directive with a component that is genuinely
  free rather than free-until-it-works.
- **Alternatives considered**: Open Loyalty (licence cap), Formance
  (production support gated to its k8s operator; Numscript DSL — runner-up),
  TigerBeetle (no queries or strings; needs a full Postgres projection
  anyway — kept as the scale escape hatch), Fineract (core-banking scope on
  a JVM), commerce platforms (model selling goods, not rebate claims),
  building it ourselves (contradicts the directive — kept as the implemented
  exit route behind the port).
- **Stated cost**: invariant C-1 (double-entry zero sum) leaves Apivo's own
  schema. Mitigated by co-locating Blnk's schema in the same Postgres, a
  continuous cross-schema zero-sum check, and never computing a
  member-facing number outside the ledger. This is a real softening of
  constitution principle VIII and the founder should accept it explicitly.

**Blocking spikes before dependent work** (also in ADR-0002):

- **S1**: Blnk runs against Supabase Postgres in a dedicated schema with a
  restricted role and does not touch `public`. Fallback: Blnk-owned Postgres
  on Hetzner plus a periodic reconciliation job.
- **S2**: Apivo transaction + Blnk transfer are reliably consistent via the
  outbox and a shared idempotency key, proved with a kill-between-the-two
  crash test.
- **S3**: The stack starts in CI without Docker Desktop on the founder's
  machine, and a `ledger=stub` mode lets non-ledger work proceed locally.

---

## S-results — spike outcomes

All three blocking spikes **passed**. Recorded 2026-08-29. The spikes are
not one-off experiments: each harness reruns on every CI push — S1 and S2 as
named steps of the `cashback` job, S3 as the `cashback-no-docker` job
(`.github/workflows/ci.yml`) — so the evidence is continuous rather than
archival, and a regression in anything a harness **asserts** — S1's eleven
checks, S2's three crash-and-race tests, S3's five checks — turns a named
step red instead of fading quietly. Five of the facts below sit outside that
guarantee: the harnesses **record** them without gating on them, and each is
marked where it appears. They can stop being true with every named step
still green, so they are observations from the run cited here rather than
continuing promises. Most recent verification at the time of this record:
[ci run 460](https://github.com/Nomos-N4s/apivo-news/actions/runs/33255760615)
(2026-08-29, `22eb232`), both jobs green. The quoted observations are from
that run and from a 2026-08-29 rerun of everything that runs on a machine
without Docker — which is the honest split, and each entry says which half
ran where.

**S1 — passed** (step *"Spike S1 — the ledger is confined to its own
schema"*, `sh scripts/spikes/ledger_schema/run.sh`). `blnk migrate up`, run
as the database owner after `bootstrap.sql` has created the restricted
`blnk_app` role, lands the ledger's tables inside the `blnk` schema and
leaves its migration bookkeeping out of `public`, where a stray
`gorp_migrations` is the one object a migration tool most often leaves
behind; a catalogue snapshot of `public` is byte-identical before and after
the migration. Nineteen tables in the cited run — *recorded, not gated*: the
check asserts at least ten and prints what it found, so a migration that
stopped creating half of them would still pass it. The runtime role owns
nothing, and Postgres itself, not the ledger's good manners, refuses it
`CREATE SCHEMA`, DDL inside `blnk`, and both `CREATE` and `SELECT` in
`public` (SQLSTATE 42501 each time) while leaving it the DML the server
needs; Apivo's own role reads `blnk.balances` across the schema boundary, so
the continuous C-1 zero-sum check is the one plain SQL query the co-location
argument in ADR-0002 depends on. The finding that changed a design on the
way: Blnk's first migration issues `CREATE SCHEMA IF NOT EXISTS`, and
Postgres checks the database-level CREATE privilege *before* taking the IF
NOT EXISTS shortcut, so one role doing both jobs would carry CREATE on the
database permanently — hence the split posture (founder decision,
2026-08-24): migrations as the database owner, the server as `blnk_app` with
USAGE and DML only. Nothing in the confinement is Supabase-specific — it is
plain Postgres privilege, proved against the job's stock Postgres and
applied to the real database by running `bootstrap.sql` there. Re-checked
2026-08-29 without Docker: `bootstrap.sql` plus the four refusal probes,
replayed against a local Postgres 16, returned 42501 in every case; the
migration half needs the pinned container, which is exactly the case the
harness's three-valued exit reserves for CI.

**S2 — passed** (step *"Spike S2 — a commit and a ledger transfer survive a
crash between them"*, `go test -count=1 -v ./scripts/spikes/outbox_crash/`,
verbose so that what the ledger did is on the record of every run). The
crash is a real `os.Exit` in a re-executed worker process, landed in each of
the two windows. After the commit, before the ledger call: the entry row and
its undispatched outbox row survive together, nothing has reached the
ledger, and recovery posts exactly one transfer. After the ledger call,
before the mark — the window that decides S2, because recovery is then
*guaranteed* to replay — the replay adds no second transfer and the
destination balance does not move again; four dispatchers racing the same
undispatched row leave one ledger row between them. Those outcomes are what
the tests assert. *How* the ledger absorbed the replay is **recorded, not
gated**: it refused outright (HTTP 409, `TXN_DUPLICATE_REFERENCE`) in the
cited run, and one of the four concurrent posts was accepted and three
refused — but a ledger that swallowed a duplicate silently would satisfy
every assertion here, so a change in that shape would be visible only in
the logged output. The evidence worth keeping is recorded in the same
deliberate way, by a test that reports which of two enforcement shapes it
finds and passes on either: in the cited run Blnk enforces reference
uniqueness **in the database** — one unique index on
`blnk.transactions.reference` — so exactly-once is held by two independent
unique constraints, the ledger's and C-5's own, not by application
discipline in one process. Losing that index would narrow the guarantee to
C-5 rather than break it, which is why the record states it and CI does not
require it. Re-checked 2026-08-29 without a ledger to reach: all four tests
skip naming `BLNK_URL`, which is the designed outcome on precisely the
machine ADR-0002 describes.

**S3 — passed**, and it is the one spike that ran in full locally: on
2026-08-29, on a machine with no Docker at all,
`sh scripts/spikes/no_docker/run.sh` reported all five checks green — build,
vet, the entire suite with nothing to connect to, the container-keyed suites
observed *skipping* rather than passing vacuously, and no failure anywhere
in the verbose run — ending in `S3 VERDICT: PASS`, with
`LEDGER_DRIVER=memory` a complete cashback configuration. What that fourth
check requires is a skip naming `DATABASE_URL` and a skip naming `BLNK_URL`;
the count it also printed — 321 — is *recorded, not gated*, so the number
moves with the suite and only the naming is enforced. The configuration half
is pinned by tests of its own: the memory ledger needs no endpoint, no Redis
and no network credentials, still requires `DATABASE_URL`, and is refused in
production, where balances that vanish with the process would be a lie. The
other half — the full stack starting in CI — is the `cashback` job itself:
the digest-pinned image migrates, the ledger answers `/health` within
seconds of `blnk_up.sh`, and the whole Go suite then runs against it. The
SDK pin rides along (`scripts/spikes/ledger_sdk/`): the offline
construct-and-field tests run everywhere, the reachability test runs where
the ledger is.

**Consequence**: S1 and S2 passed, so neither ADR-0002 fallback is taken and
no ADR-0006 exists — the ledger stays co-located in one Postgres behind the
cross-schema zero-sum check, the outbox with a shared idempotency key stays
the consistency mechanism, and D1 stands as recorded. What would reopen the
decision is unchanged: the revisit triggers in ADR-0002, none of which has
fired.

---

## 2. Super-app architecture (the founder's second question)

**Question**: microservices, multiple modular monoliths, events, multi-repo,
monorepo?

### D2 — Decision: monorepo, product-scoped modular monoliths, one binary, event integration

Full record: [ADR-0001](../../docs/adr/0001-super-app-architecture.md).

- **Decision**: one repository, one Go module, **one deployable for the
  alpha**. Product domains are peers under `internal/` (`cashback/` beside
  the existing news packages), isolated by Postgres schema, connected only
  by asynchronous events through a transactional outbox into the existing
  append-only `domain_event` stream. Import rules and a migration lint
  enforce the boundary; `cmd/` is the only composition point.
- **Rationale**: the deciding constraint is that **one person operates
  it**. Every extra deployable is a pipeline, a secret set, an on-call
  surface and a debugging hop paid forever. Boundaries enforced by a
  build-failing test give the *option* to split at near-zero cost, which is
  the value microservices are usually bought for. Four explicit split
  triggers are documented so the decision is revisited on evidence rather
  than on mood.
- **Alternatives considered**: microservices per product (no trigger met;
  solo founder), separate repository (every shared change becomes a
  cross-repo version dance), separate database per product (doubles
  operational surface for isolation a schema and a role already give at this
  scale), broker from day one (stateful dependency plus dual-write
  consistency, replacing a table that is already append-only and backed up),
  single shared schema (the first cross-product join becomes permanent),
  separate binaries now (pays for process isolation before it is needed).
- **Explicitly not done**: existing news packages are **not moved**.
  Grouping them under `internal/news/` later is a pure rename; it waits
  until the news alpha ships, so in-flight agent work stays valid.

### D3 — Decision: no second stack

Full record: [ADR-0005](../../docs/adr/0005-cashback-stack.md).

- **Decision**: Go + Astro + Supabase Postgres, with sqlc and
  `supabase gen types` as today. Additions: Redis (required by Blnk), an
  in-process scheduler guarded by a Postgres advisory lock, and a
  `platform/money` integer-minor-unit type.
- **Rationale**: Go fits money arithmetic, network adapters and long-running
  pollers well; Blnk is Go; the generated-types pipeline the constitution
  mandates already exists and is inherited free. The single-operator stack
  tax decides the rest.
- **Alternatives considered**: TypeScript/Node end-to-end (decimal
  arithmetic vigilance; splits backend knowledge across two runtimes),
  Elixir/Phoenix (excellent fit, rejected purely on the second-toolchain
  tax), Java/Kotlin + Spring (operational weight), commerce platforms
  (permanent impedance mismatch), a separate Astro app (splits session,
  design system and the robots gate to solve a directory problem).

---

## 3. Affiliate network integration

### D4 — Decision: consumer-defined adapters, fixtures first, polling authoritative

Full record: [ADR-0003](../../docs/adr/0003-affiliate-network-integration.md).

- **Decision**: one `Network` port owned by the domain; one package per
  network; every adapter emits a normalised transaction **and** the verbatim
  raw payload; polling with a durable per-account cursor is the only thing
  that creates credits; push notifications are a latency hint that triggers
  a targeted poll and never move money by themselves; recorded fixtures make
  the whole chain buildable and testable with no live credentials.
- **Rationale**: the founder's network choice (Q1) is a business process
  with an unknown lead time, and it must not block engineering. Refusing to
  trust webhooks removes forgery and replay from the money path entirely.
  Storing raw payloads means a normalisation bug is fixable without the
  network's cooperation.
- **Concrete constraints absorbed from the reference network (Awin)**:
  6 requests/second rate limit; **31-day maximum query window**, so backfill
  is inherently windowed; `clickref` as the publisher-to-advertiser tracking
  parameter; `pending → approved | declined` status vocabulary with
  validation taking **up to 90 days**, which is why the poller re-reads a
  trailing ~100-day window on a slower schedule.
- **Alternatives considered**: webhook-driven credits (authenticity enters
  the money path), a paid aggregator API (excluded by the free/OSS
  directive), one generic config-driven adapter (networks differ in
  behaviour, not just field names), building against the domain and
  extracting later (the first network's vocabulary silently becomes the
  domain's).

---

## 4. Rebranding

### D5 — Decision: single-brand-per-deployment, configuration-driven, multi-tenancy deferred

Full record: [ADR-0004](../../docs/adr/0004-white-label-rebranding.md).

- **Decision**: one brand configuration loaded once on both sides
  (`internal/platform/brand`, `web/src/lib/brand`); zero brand literals in
  code, templates or migrations, enforced by a CI lint; all member-facing
  text in BCP-47 translation catalogues; a **fixture brand renders every
  member-facing surface in CI**, so rebrandability is a test that goes red.
  Forward compatibility with multi-tenancy costs one brand-id column on the
  records where a tenant boundary would fall, and no global brand singleton.
- **Rationale**: "ready to be rebranded" is satisfied by reading 1 (rename,
  restyle, re-domain with no code change). Reading 2 (many brands live at
  once) is a different product — tenant scoping in every query, test and
  screen — and is speculative before a single paying member exists.
- **Alternatives considered**: full multi-tenancy now (speculative
  complexity), CSS theming only (misses name, domain, legal entity, payout
  descriptor — where the real work is), hostname-based runtime switching
  (multi-tenancy without its safeguards; one shared member table behind two
  brands is a data-protection incident), build-time find-and-replace
  (per-brand artefacts, untestable in one CI run).

---

## 5. Money mechanics

### D6 — Decision: integer minor units, one currency per posting, house account absorbs rounding

- **Decision**: `platform/money` holds `Amount{Minor int64; Currency
  ISO4217}`. No floating point column exists anywhere in the cashback
  schema; `CHECK` constraints and column types make one unrepresentable
  (C-6). Member share = `commission_minor × rate_bps / 10000`, rounded per
  a documented policy, with the remainder posted to a house account so that
  every transfer still sums to zero.
- **Rationale**: rounding that "disappears" is the classic way a ledger
  stops balancing. Making the remainder an explicit posting keeps C-1 true
  by construction rather than by luck.
- **Alternatives considered**: `NUMERIC` in Postgres (correct, but invites
  float handling in Go and JS on the way out), a decimal library (adds a
  dependency to represent something `int64` represents exactly),
  banker's rounding without a remainder posting (silently breaks zero-sum).

### D7 — Decision: entry state machine mirrors the network, balances derive from postings

- **Decision**: entry states `held | pending | confirmed | reserved | paid |
  reversed` (FR-042). Each transition writes a ledger transfer; no state is
  ever recorded without its posting, and no posting is ever written without
  a state. Reversal is a **new pair of postings**, never an edit (SC-010).
  Member-visible totals are computed from postings, never stored.
- **Rationale**: one source of truth. A stored balance is a cache, and a
  cache that disagrees with the ledger is the bug that costs trust.
- **Alternatives considered**: stored balance column with triggers (fast,
  but creates two truths), event-sourced entries with projections (more
  machinery than a ledger already provides).

### D8 — Decision: exactly-once payout via a request-derived idempotency key

- **Decision**: the idempotency key is derived deterministically from the
  withdrawal request id, carries a unique constraint in Apivo (C-5), and is
  passed unchanged to both Blnk and the payout rail. Retryable rail failures
  reuse it; terminal failures release the reservation back to confirmed
  balance.
- **Rationale**: derivation from the request means a retry cannot
  accidentally mint a new key, which is how duplicate payouts actually
  happen.
- **Alternatives considered**: random key per attempt (defeats the purpose),
  rail-side deduplication only (rail-specific, unverifiable in tests),
  distributed lock (liveness problems without solving the duplicate).

### D9 — Decision: reserve on request, not on approval

- **Decision**: requesting a withdrawal immediately moves the amount from
  `confirmed` to `reserved` (US4 scenario 2), before any human review.
- **Rationale**: the double-spend window is between request and approval,
  not after it. Reserving at request closes it with the ledger rather than
  with a lock.
- **Alternatives considered**: reserve on approval (leaves the window open),
  optimistic check at payout time (fails under concurrency, which is exactly
  when it matters).

---

## 6. Platform mechanics

### D10 — Decision: transactional outbox, in-process dispatch, at-least-once with idempotent consumers

- **Decision**: `platform/events` gains an outbox table per schema written
  in the same transaction as the state change, an in-process dispatcher that
  appends to `domain_event` and delivers to subscribers, at-least-once with
  a delivery idempotency key. No broker in the alpha.
- **Rationale**: `domain_event` already exists and is already append-only
  and backed up. A broker would add a stateful dependency and a dual-write
  problem to replace a table that works.
- **Alternatives considered**: NATS/Kafka (documented upgrade path inside
  the same package), direct in-process function calls between products
  (violates FR-081 and re-couples what the boundary exists to separate),
  Postgres `LISTEN/NOTIFY` alone (no durability, no replay).

### D11 — Decision: in-process scheduler with a Postgres advisory lock

- **Decision**: pollers and reconciliation runs are scheduled in-process;
  each job takes a Postgres advisory lock so two instances cannot double-run
  it. Cursors advance only after a window is fully persisted (FR-031).
- **Rationale**: no new infrastructure, and correct under the horizontal
  scaling the deployment already permits (HPA manifests exist).
- **Alternatives considered**: external cron (a second place to look when a
  job does not run), a job queue library (machinery beyond the need at this
  volume), Kubernetes CronJob (couples scheduling to one deployment target,
  which the constitution forbids leaking into application code).

### D12 — Decision: cashback surfaces are non-indexable and stay behind the existing gate

- **Decision**: wallet, click-out and operator routes are
  `noindex`/`nofollow` and excluded from any sitemap; the click-out endpoint
  is not crawlable and rate-limited per member and per IP. This reuses the
  single indexing gate the news alpha already ships (its D6), rather than
  adding a second one.
- **Rationale**: crawler traffic through a click-out endpoint would
  manufacture clicks and pollute attribution. The founder's block-all
  posture for the alpha covers this by default; the decision records that it
  must not be relaxed for cashback without a founder decision.
- **Alternatives considered**: allowing the merchant catalogue to be indexed
  for SEO value (a real growth argument — deferred to a founder decision,
  not taken by the plan).

---

## 7. Founder-blocked questions and the defaults the plan proceeds under

None of these is resolved here. Each entry states what the plan does while
it is open and what changes when it is answered.

| Q | Plan proceeds under | Changes if answered differently |
|---|---|---|
| **Q1** networks | One adapter built against recorded fixtures; no live credentials | Which adapter package ships first; go-live date. No domain change (ADR-0003). |
| ~~**Q2** PSD2 / e-money posture~~ | **DECIDED 2026-08-24** — rebate-claim posture (FR-041), now in the constitution under "Cashback alpha" | A move to stored value is a new founder decision taken with legal advice, and would change the substrate question entirely. A solicitor's confirmation before public launch remains worthwhile; it gates nothing. |
| **Q3** clawback after payout | Absorb as a business loss; record it; never chase the member | A negative-balance posture adds a state, member-facing copy and a collections policy. |
| **Q4** revenue share and rounding | Configuration with no committed value; rounding to the member's favour, remainder to the house account | Configuration values only, unless the rounding direction changes (one policy constant). |
| **Q5** payout rails and threshold | Manual/offline rail plus a stub, both enforcing C-4 and C-5; threshold is configuration | A new rail is one implementation behind the payout port. |
| **Q6** KYC and sanctions | Verified destination ownership only; no identity verification | Adds a verification flow and constrains payout size and rail choice. |
| **Q7** tax treatment | Rebate framing in all copy; no tax statements issued | Adds member reporting and possibly per-jurisdiction copy. |
| **Q8** click-log retention | No automated deletion, consistent with the 2026-08-14 news decision | Adds a retention job; the immutability triggers make deletion a deliberate, audited operation. |
| **Q9** repository/brand naming | Single repository, unrenamed, restructured internally | A GitHub rename is cosmetic by construction (ADR-0001). |

---

## 8. Constitution amendment (v1.0.0 → v1.1.0) — RATIFIED 2026-08-24

**Ratified by the founder on 2026-08-24.** The amendment is applied in
`.specify/memory/constitution.md`; what follows is the record of what
changed. **MINOR** bump: principles added and constraints expanded; none
removed or redefined.

**Sync Impact Report (as applied)**

- Version change: 1.0.0 → 1.1.0
- Added: Core Principle IX (Money is double entry, evidence-backed and
  exactly once) carrying invariants C-1 to C-7; a "Products" subsection
  under Architecture Constraints defining product domains, schema isolation
  and event-only integration; a "Rebrandability" line under Architecture
  Constraints.
- Modified: Architecture Constraints — *"a single binary, modular
  monolith. No microservices."* becomes *"a modular monolith per product
  domain, composed into one binary for the alpha; no microservices, and any
  future extraction must be a deployment change, not a redesign — see
  ADR-0001."*; a sentence permitting a self-hosted open-source ledger as a
  sidecar, with the C-1 trade named explicitly.
- Modified: Alpha Scope — cashback moves from "out of scope" to a named
  second product with its own scope block; the remaining ecosystem
  mini-apps stay out of scope.
- Removed: none.
- Follow-up TODOs: founder answers to Q1 and Q3–Q9 as they land (Q2 is
  decided and recorded in the constitution).

The blocking gate is cleared. ADR-0001 to ADR-0005 move from Proposed to
**Accepted**.

---

## 9. Sources

- [Open Loyalty — cashback product](https://www.openloyalty.io/product/cashback)
- [Open Loyalty licence text (community mirror)](https://github.com/kwarambatendai/openloyalty/blob/master/LICENSE)
- [Blnk — open-source ledger](https://github.com/blnkfinance/blnk)
- [Blnk configuration reference](https://docs.blnkfinance.com/advanced/configuration)
- [Formance Ledger](https://github.com/formancehq/ledger)
- [TigerBeetle](https://github.com/tigerbeetle/tigerbeetle)
- [Awin — publisher transaction API](https://help.awin.com/apidocs/proof-of-purchase-publisher-transaction-api)
- [Awin — publisher transactions list endpoint](https://developer.awin.com/apidocs/returns-a-list-of-transactions-for-a-given-publisher)
- [Awin — automation with APIs](https://www.awin.com/us/how-to-use-awin/automation-with-apis)
- [Awin — understanding transaction status](https://success.awin.com/s/article/Understanding-Transaction-Status-and-Payment-Process)
- [Best affiliate tracking software 2026 (category survey)](https://remoby.com/blog/best-affiliate-tracking-software-2026/)
- [EBA guidelines on the limited network exclusion under PSD2](https://www.eba.europa.eu/legacy/regulation-and-policy/regulatory-activities/payment-services-and-electronic-money-1)
