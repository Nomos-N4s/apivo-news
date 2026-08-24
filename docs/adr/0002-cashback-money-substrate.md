# ADR-0002: Cashback money substrate — adopt the Blnk open-source ledger

- **Status**: Accepted (2026-08-24)
- **Date**: 2026-08-24
- **Deciders**: founder
- **Related**: [0001](0001-super-app-architecture.md), [0003](0003-affiliate-network-integration.md)

## Context

The founder's directive is explicit: the cashback system must be built on an
**open source, completely free to use** offering, and the choice must be
recorded as an architecture decision.

A survey of the field (August 2026) produced one finding that shapes
everything below:

> **There is no production-grade, unrestricted open-source, end-to-end
> cashback platform.** The category does not exist. What exists is three
> adjacent categories, each solving a different part of the problem.

**1. Loyalty engines.** The reference implementation is
[Open Loyalty](https://www.openloyalty.io/product/cashback), which markets
cashback explicitly. Its Community Edition licence is *"Based on MIT
License"* but is not MIT: the community grant applies only to
*"non-commercial purposes, or if its intended or actual commercial use
involves a maximum of 200 Customers"*; beyond that, *"is used for
commercial purposes and … involves over 200 Customers"* requires a
negotiated Enterprise fee, and *"any breach … will result in an immediate
termination of any and all rights granted"*
([licence text](https://github.com/kwarambatendai/openloyalty/blob/master/LICENSE)).
A cashback product whose success condition is more than 200 members cannot
be built on a licence that terminates at 200 members. It also brings a
PHP/Symfony + Elasticsearch stack the founder does not run.

**2. Affiliate tracking software.** Refferq, Peerclick, Raider, eLitius,
Weferral, Post Affiliate Pro. These are **the wrong side of the market**:
they let a merchant *run* an affiliate programme and pay affiliates. Apivo
is not a merchant running a programme — Apivo is the publisher, an affiliate
*of* Awin and Tradedoubler, sharing its own commission with members.
Nothing in this category models a member wallet, a commission share, or
reconciliation against a network statement. Independent surveys of the
category agree that *"truly free, fully-featured, well-maintained
open-source affiliate tracking software is rare"*
([Remoby, 2026](https://remoby.com/blog/best-affiliate-tracking-software-2026/)).

**3. Open-source financial ledgers.** This is the category that is
genuinely production-grade and genuinely unrestricted — and, not
coincidentally, it covers the part of cashback that is hardest to get right
and most expensive to get wrong: **balances, double entry, idempotent money
movement and reconciliation**.

The GitHub projects literally named "cashback" (`setxpro/cashback-customer-wallet`,
`vippermaeglin/cashflix`, `IvanAlekseevichPopov/cashback`) are
demonstration-grade personal projects, unmaintained, with no licence
guarantees worth building a money product on.

So the honest framing of the decision is not *"which cashback platform do we
adopt"* — none qualifies — but **"which open-source component takes the
money-correctness burden off us, and what do we build around it?"**

## Decision

**Adopt [Blnk](https://github.com/blnkfinance/blnk) (Apache-2.0) as the
double-entry ledger and money substrate. Build the cashback domain —
catalogue, click-out, attribution, entry state machine, reconciliation,
payout orchestration — inside the Apivo Go monolith on top of it.**

Blnk owns: ledger accounts, balances, transfers, postings, multi-currency,
idempotency keys, balance monitors, reconciliation primitives.

Apivo owns: everything that is cashback-specific and everything that is
legal evidence.

Deployment shape:

- Blnk runs as a sidecar service beside the Go binary on the existing
  Hetzner host (`deploy/hetzner/compose`) and as a service container in CI.
- Blnk is pointed at **the same Postgres instance** as Apivo, in its own
  `blnk` schema with its own role — subject to the Phase-0 spike below.
  This keeps one database, one backup and one point-in-time recovery, and
  it makes the cross-schema invariant check below a plain SQL query rather
  than a distributed reconciliation.
- Redis is added as a dependency (Blnk requires it for queueing and
  caching). It holds no source of truth; losing it loses throughput, not
  money.

### Invariant ownership after this decision

| Invariant | Enforced where | How |
|---|---|---|
| C-1 double entry, zero-sum | **Blnk** | Blnk's transfer model; verified continuously by an Apivo check that sums postings per currency across the `blnk` schema and alerts on non-zero |
| C-2 attribution | Apivo Postgres | `NOT NULL` FK from cashback entry to network transaction record |
| C-3 immutable evidence | Apivo Postgres | UPDATE/DELETE/TRUNCATE triggers, exactly as `source_item` |
| C-4 named approver on payout | Apivo Postgres | `NOT NULL approved_by` — the row **is** the approval |
| C-5 exactly-once payout | Apivo Postgres **and** Blnk | unique idempotency key in Apivo; the same key passed to Blnk and to the payout rail |
| C-6 integer minor units | Apivo Postgres | `BIGINT` amount + `CHAR(3)` currency, `CHECK` constraints; no floating point column exists |
| C-7 traceability | Apivo Postgres | `cashback_provenance` view joining payout → approver → entries → evidence → click |

**The honest cost**: C-1 is the one invariant that leaves Apivo's own
schema, which is a real softening of constitution principle VIII
(database-enforced invariants over application discipline). It is mitigated,
not eliminated, by (a) co-locating Blnk's schema in the same database so the
check is a real SQL query over real rows, (b) running that check
continuously and failing loudly, and (c) never letting a member-facing
number be computed anywhere but from ledger postings. This trade is stated
plainly so the founder accepts it explicitly rather than discovering it.

### Mandatory Phase-0 spikes before any dependent work

- **S1**: Blnk runs against a Supabase Postgres in a dedicated schema with a
  restricted role, and its migrations do not touch the `public` schema. *If
  this fails*, fall back to a Blnk-owned Postgres on the Hetzner host and
  accept a second database plus a periodic reconciliation job in place of
  the cross-schema query.
- **S2**: An Apivo transaction and a Blnk transfer can be made
  reliably consistent through the outbox with a shared idempotency key —
  demonstrated including a kill-between-the-two crash test.
- **S3**: Blnk's Go SDK is usable from the monolith with a pinned version,
  and the whole stack starts in CI without Docker Desktop on the founder's
  machine (CI is the verification of record).

## Consequences

**Positive**

- The founder's directive is met with a component that is genuinely
  Apache-2.0, with no member cap, no commercial gate and no seat licence.
- Same language (Go), same database (Postgres) as the rest of the stack —
  no second ecosystem to learn or operate.
- The hardest correctness problem in the product (money) is carried by
  software whose only job is that problem, including idempotency and
  reconciliation primitives we would otherwise write ourselves.
- Cashback-specific logic stays in Apivo, so no vendor decides our domain
  model, and swapping the ledger later touches one package
  (`internal/cashback/wallet`) behind an interface.

**Negative / accepted costs**

- Two new runtime dependencies (Blnk, Redis) for a solo operator.
- Blnk is a modest project (~460–500 GitHub stars, actively maintained
  through mid-2026) rather than a decade-old standard. Adoption risk is real
  and is mitigated by the ledger-port interface below.
- C-1 moves out of Apivo's schema, as stated above.
- Local development on the founder's machine is constrained while Docker
  Desktop stays broken; CI remains the verification of record, and a
  `ledger=stub` mode must let non-ledger work proceed locally.

**Required mitigation (non-negotiable)**

All ledger access goes through **one consumer-defined interface** in
`internal/cashback/wallet` — `Ledger` with `EnsureAccount`, `Post`,
`Balance`, `History` — with three implementations: Blnk, in-memory stub, and
a Postgres implementation kept as the exit route. Nothing outside that
package imports a Blnk type. Swapping the ledger must be a five-engineer-day
change, on the same rule the constitution already applies to the translation
adapter.

## Alternatives considered

| Alternative | Licence / stack | Why rejected |
|---|---|---|
| **Open Loyalty CE** | "Based on MIT", **commercial use capped at 200 customers**; PHP/Symfony + Elasticsearch | The licence terminates exactly where the product succeeds. Stack mismatch compounds it. This is the closest thing to an off-the-shelf cashback engine and it is still not usable. |
| **Formance Ledger** | MIT, Go + Postgres | Strong runner-up, same language and database. Rejected because production use is documented as supported only through its Kubernetes operator deployment mode, which imposes a platform the alpha does not run, and Numscript adds a DSL to learn for flows that are three postings long. Revisit if Blnk stalls. |
| **TigerBeetle** | Apache-2.0, Zig | Excellent and genuinely safer at scale, single binary with no dependencies. Rejected for the alpha: no strings, no queries, no joins — every lookup is by 128-bit id, so a full Postgres projection must be maintained anyway, which is most of the work Blnk removes. **Named as the documented escape hatch** if volume ever justifies it. |
| **Apache Fineract** | Apache-2.0, Java | Core-banking scope; savings accounts and a full banking domain we do not want, on a JVM the founder does not run. Wrong size by an order of magnitude. |
| **Affiliate tracking software (Refferq, Peerclick, Raider, …)** | Mixed | Wrong side of the market: they run a merchant's affiliate programme. No wallet, no commission share, no network reconciliation. |
| **GitHub "cashback" projects** | Mixed / unclear | Demonstration-grade, unmaintained, no licence assurance. Not a basis for handling members' money. |
| **Build the ledger in Postgres ourselves** | n/a | Three tables, a zero-sum trigger and a uniqueness constraint is genuinely achievable and would keep every invariant inside our own schema — but it directly contradicts the founder's instruction to adopt an open-source offering, and it puts money correctness back on us. **Kept as the implemented exit route behind the `Ledger` interface**, so the decision is reversible in days rather than months. |
| **Hosted ledger (Fragment, Increase, Moov, …)** | Commercial SaaS | Not free, not open source. Excluded by the directive. |

## Revisit triggers

- Blnk goes unmaintained for two consecutive quarters, or relicenses.
- Spike S1 or S2 fails and the fallback costs more than the Postgres exit
  route.
- Transaction volume approaches the point where a single-node ledger is a
  bottleneck (TigerBeetle path).
- A compliance opinion (founder question Q2) requires a licensed e-money
  provider, which changes the substrate question entirely.
