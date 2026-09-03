# Feature Specification: Many Affiliate Networks — Linkwise Beside Awin

**Feature Branch**: `xcoder/003-multi-network`
**Spec Directory**: `specs/003-multi-network-linkwise/`
**Created**: 2026-09-03
**Status**: Draft — awaiting founder answers to Q10–Q13
**Input**: Founder, 2026-09-03: *"we have a new mvp feature request, we must
work also with Linkwise and not Awin only!"*

---

## Governance Impact (resolved and unresolved)

### Q1 is amended by this feature

Constitution Governance makes *"which affiliate networks to join"* (Q1) a
founder-only question. It was answered once:

> **DECIDED 2026-08-31**: **Awin**, and only Awin for the alpha. […]
> Coverage in Greece against Germany is a merchant-programme question to
> answer per programme, not a second network. **A second network is a new
> founder decision and a new adapter.**
> — `specs/002-apivo-cashback-alpha/spec.md`, Q1

The founder's message of 2026-09-03 is that new decision. This spec records
it; it does not take it.

> **Q1, AMENDED 2026-09-03**: **Awin and Linkwise.** Linkwise is the
> Greece/Cyprus network; Awin covers Germany. Together they match the
> product thesis — a Greek speaker in Munich — on both sides of it. Each is
> a separate adapter package and a separate publisher account, and neither
> may be reachable without its own credential.

Two follow-ups this creates, both listed as tasks rather than assumed:

1. The constitution's Governance section lists Q1 among the *open* questions
   and records only Q2 as decided. It is behind by one decision already
   (Q1/2026-08-31) and now two. A **PATCH amendment** with a Sync Impact
   Report brings it up to date. Amending the constitution is the founder's
   act; the task prepares it, it does not perform it.
2. `002/spec.md`'s Q1 entry says a second network is a new decision. It is
   correct and stays; a pointer to this spec is added so the two do not read
   as contradicting each other.

### What this feature does **not** decide

Nothing here resolves Q3–Q9. In particular it does not touch the revenue
share (Q4) or the payout rails (Q5), and it must not: a second network
changes where commission comes from, never what a member is owed from it.

---

## Product Frame

The constitution already requires this feature's *shape*, twice:

> Every external dependency sits behind a consumer-defined interface,
> swappable in under five engineer-days, **proved by a second working
> implementation in the repository**. This applies to […] **affiliate
> network adapters** […].
> — Constitution, Architecture Constraints

> **SC-008**: Adding a second affiliate network requires changes only inside
> its adapter package, proved by an architecture test.
> — `002/spec.md`

and `002/quickstart.md`'s own validation scenario **V10** is literally *"Add
a second network without touching the domain (SC-008)"*.

So the founder's request is not a change of direction. It is the alpha's own
definition of done, brought forward and made real with a network that has
commercial value rather than a second fixture.

### The frame it changes

**The product does not integrate Awin today.** `*awin.Client` does not
implement `networks.Network` — it is missing `FetchTransactions` and
`Limits`, proved at the compiler — so the binary ships only the fixture
adapter and `NETWORK_DRIVER=awin` fails to start. See
[research.md](research.md) §1.

This is not rot: `002/research.md` §9.2 records that Awin publishes no
schema for its transactions response and refuses to map money fields from
guesswork. T139/T140/T142/T143/T144 remain open for that reason.

Therefore the feature is **"make the first live adapter reachable, prove the
seam with a second, and hold both to the same evidentiary standard"** — not
"add a second network to a working first one".

---

## Clarifications

### Session 2026-09-03 (founder input)

- Linkwise is to be integrated alongside Awin, not instead of it.
- No statement was made about a Linkwise publisher account, credentials, or
  API documentation access. Recorded as **Q11**, and it gates the same way
  Awin's did.

### Deferred to the founder

Q10–Q13 below. The plan must not silently resolve them; the recorded safe
defaults apply until answered.

---

## User Scenarios & Testing *(mandatory)*

### User Story 1 — A deployment polls two networks at once (Priority: P1)

An operator connects a publisher account at each of two networks. The
deployment ingests transactions from both, imports both catalogues, and
serves click-outs for retailers on either — with no code change beyond the
adapters and one wiring table.

**Why this priority**: it is the feature. Everything else is a consequence.

**Independent Test**: configure two networks (fixture plus a second fixture
under another id), start the binary, and assert both accounts' sweeps and
both catalogue imports are registered under distinct job names and distinct
fleet-wide locks, and that the capacity check counts them all.

**Acceptance Scenarios**:

1. **Given** two networks configured and connected, **When** the binary
   starts, **Then** it registers two forward sweeps, two trailing sweeps and
   two catalogue imports, each under its own advisory lock, and refuses to
   start if the pool cannot hold them.
2. **Given** one of the two networks is unconfigured or its account row is
   inactive, **When** the binary starts, **Then** the other network polls
   normally and the incomplete one is reported **by name** at ERROR, and the
   deployment still serves.
3. **Given** two networks, **When** one network's API is failing, **Then**
   the other network's sweeps are unaffected — separate locks, separate
   limiters, separate cursors.

---

### User Story 2 — A member's purchase is credited to the network that earned it (Priority: P1)

A member clicks a retailer reachable through network A. Network B reports a
transaction whose reference happens to name that click. The member is
credited **once**, from network A's report, at network A's click-time rate.

**Why this priority**: this is money safety, and it is the one thing that
gets *worse* the moment a second network is live. Today it is unreachable
because only one network exists; it becomes reachable on the day this
feature ships.

**Independent Test**: against the real schema, issue a click through network
A, store a report from network B citing that reference, run the crediting
job, and assert no entry is opened and the report is queued as unattributed.

**Acceptance Scenarios**:

1. **Given** a click issued through network A, **When** network B reports a
   transaction echoing that click's reference, **Then** no entry is opened,
   the report is queued as unattributed with a reason naming the mismatch,
   and no money moves.
2. **Given** a click issued through network A, **When** network A reports
   it, **Then** exactly one entry is opened, priced from that click's
   snapshot and billed to that route's brand.
3. **Given** a click already credited, **When** a second transaction on
   another network cites the same click, **Then** the second is refused —
   by the **database**, not only by the reader.

---

### User Story 3 — A retailer on both networks publishes the better route (Priority: P2)

A retailer is reachable through both networks. The catalogue publishes one
of them, deliberately and visibly, and an operator can change which.

**Why this priority**: without it the first network to import a retailer
owns its member-facing rate for ever, and a retailer that leaves the
preferred network vanishes from the catalogue while still live on the other.
Both are silent.

**Independent Test**: import the same retailer from two networks, assert
exactly one route is published, that an operator can move it, and that when
the published route goes `left_network` the surviving route takes over
rather than the merchant page going empty.

**Acceptance Scenarios**:

1. **Given** a retailer imported by network A and then by network B,
   **When** a member views the merchant, **Then** exactly one route's bands
   are shown and which one is a recorded decision, not an accident of import
   order.
2. **Given** the published route goes `left_network`, **When** the catalogue
   is read, **Then** a surviving active route on another network is
   published instead, and the change is announced.
3. **Given** an operator with a reason, **When** they move the published
   route, **Then** who and why are recorded (FR-061), and the change is
   visible to members on the next read.

---

### User Story 4 — An operator sees per-network health (Priority: P2)

An operator can see, per network: whether it is connected and active, how
far each cursor has walked, when it last polled, how many transactions it
reported, and what its statements reconciled to.

**Why this priority**: with one network, "cashback is broken" is
unambiguous. With two it is not, and every operator queue currently answers
across networks without saying which.

**Independent Test**: with two networks connected, call the operator
surface and assert every queue row names its network and can be filtered by
it.

---

### User Story 5 — A third network costs one package and one table row (Priority: P3)

Adding a third network changes only its own adapter package plus one entry
in the driver table.

**Why this priority**: it is SC-008 itself, and the proof that the second
network was integrated rather than special-cased.

**Independent Test**: `make arch-test` — `network_isolation_test.go` already
refuses fewer than two adapter packages; with three it must still pass
unmodified.

---

### Edge Cases

- Two networks report the **same purchase** (a retailer running both, a
  member's browser carrying both cookies). Only one click exists, so only
  one report can match it; the other must be unattributed, never a second
  credit.
- A network reports a **sale and commission in different currencies**.
  `Reported.Validate` refuses it at the port. Whether that is Linkwise's
  behaviour is Q13.
- A network's **subid is shorter than 22 characters** or restricts the
  character set. Apivo's minted reference would not survive the round trip
  and attribution silently fails. This is the hardest external constraint on
  the port (`IssuedClickRef`, ≥22 URL-safe characters).
- A network's **deeplink needs a fact the port does not carry**.
  `DeeplinkTarget` holds exactly four; a fifth is a genuine port change.
- One network is **rate-limited to a crawl** while the other is idle. Each
  adapter must hold its own limiter; a shared one throttles the fast network
  or overruns the slow one.
- Two networks with **different catalogue languages** —
  `NETWORK_SOURCE_LANGUAGE` is one global scalar today.
- A retailer is reachable **only through a route that cannot carry a click
  reference** — Linkwise describes SubID support as per-programme
  (research.md §5.1). Nothing fails: the member clicks, buys, and the
  network pays the publisher. Only the member is never credited, and every
  diagnostic looks healthy. The retailer must not be published, and must be
  visible to an operator as reachable-but-unattributable rather than absent.

---

## Requirements *(mandatory)*

### Invariants inherited unchanged

C-1 to C-7 are unchanged and non-negotiable. Two acquire a second meaning
here and are called out because a second network is exactly what could break
them quietly:

- **C-2 (Attribution)** — a credit rests on one network transaction and,
  through it, at most one click. With two networks this must additionally
  mean **the click's own network**, or one purchase can back two credits.
- **C-6 (Integer money)** — one currency per report, per the evidence row.

### Functional Requirements

**Configuration and wiring**

- **FR-090**: A deployment MUST be able to configure more than one affiliate
  network at once, each with its own driver, publisher account, credential
  and source language.
- **FR-091**: A network whose configuration is incomplete MUST NOT prevent
  the deployment from starting or stop another network polling; it MUST be
  reported **by name**, at ERROR, saying which key is missing.
- **FR-092**: The binary MUST have exactly one registry of the adapters it
  ships — a driver's public facts and its constructor together — so a driver
  that can be seeded can also be served. (Today two switches disagree.)
- **FR-093**: An adapter MUST receive its credential from configuration
  resolved through `network_account.credential_ref`, never from the database
  and never from the repository (ADR-0003).

**Scheduling**

- **FR-094**: Every scheduled job MUST be named per network, so that two
  networks register distinct jobs under distinct fleet-wide locks.
- **FR-095**: The startup capacity check MUST count the jobs that were
  actually registered, per connected network.

**Attribution (money safety)**

- **FR-096**: A reported transaction MUST match a click only if the click
  was issued through the **reporting network**. A reference matching a click
  on another network MUST be treated as unattributed.
- **FR-097**: One click MUST back at most one credit. Enforced by the
  **database**, per Principle VIII, with a test asserting the database
  rejects the second by SQLSTATE.
- **FR-098**: The unattributed queue MUST distinguish "no click has this
  reference" from "the click with this reference belongs to another
  network", because they are different operator actions.

**Catalogue arbitration**

- **FR-099**: When a retailer is reachable through more than one network,
  exactly one route MUST be published, and which one MUST be a recorded
  decision.
- **FR-100**: When the published route becomes `left_network` or its network
  becomes inactive, a surviving active route MUST take over rather than the
  retailer publishing nothing.
- **FR-101**: An operator MUST be able to move the published route, with a
  named human and a non-blank reason (FR-061).

**Operator surface**

- **FR-102**: Every operator queue row that concerns a network MUST name it,
  and the queues MUST be filterable by network.
- **FR-103**: An operator MUST be able to list the connected networks with
  each one's active flag, cursors, last poll and documented limits.

**Adapters**

- **FR-104**: Each real adapter MUST pass the shared conformance suite, and
  the suite MUST be run against every adapter the binary ships — not only
  the fixture.
- **FR-105**: No money field of any adapter MAY be mapped from documentation
  alone. Each MUST be mapped from a **recorded, redacted response** from a
  real publisher account, stored under the adapter's own `testdata/`, to the
  standard `002/research.md` §9.2 set for Awin.
- **FR-106**: An adapter MUST hold its own rate limiter and its own retry
  budget; neither may be shared between networks.
- **FR-107**: A route that cannot carry a click reference MUST NOT be the
  published route for its retailer, MUST still be recorded and visible to an
  operator, and MUST NOT be assumed attributable by default — an adapter
  that cannot tell declares the whole network unattributable rather than
  guessing per route.

### Key Entities

No new entity. Three existing ones gain meaning:

- **`cashback.network`** — a row per network, already keyed and
  format-checked. A second network is data, not DDL.
- **`cashback.click`** — must become able to answer *which network issued
  me* without a three-table join, so attribution can be constrained.
- **`cashback.merchant_network.preferred`** — becomes an arbitration
  decision somebody makes and can revisit, rather than a value written once
  at first import.

---

## Success Criteria *(mandatory)*

- **SC-020**: A deployment configured with two networks polls both, imports
  both catalogues and serves click-outs for both, with per-network job names
  and locks — proved by an integration test against the real scheduler.
- **SC-021**: A report from one network citing a click issued through
  another opens **no** entry and moves **no** money — proved against a real
  Postgres, and the second credit is refused by the database by SQLSTATE.
- **SC-022**: A retailer on two networks publishes exactly one route; when
  that route dies the other takes over — proved by a schema test.
- **SC-023**: Every adapter the binary ships passes the shared conformance
  suite in CI, with at least two real adapters in the table.
- **SC-024**: Adding a third network changes only its adapter package and
  one driver-table entry — `network_isolation_test.go` passing unmodified is
  the proof (SC-008, now with three packages).
- **SC-025**: No money field in any adapter is mapped from a source other
  than a recorded response committed under that adapter's `testdata/`.
- **SC-026**: A retailer whose only routes cannot carry a click reference
  publishes nothing and appears in an operator listing saying why — proved
  by a schema test asserting the database refuses to prefer such a route.

---

## Open Questions (founder-only)

Per constitution Governance. Safe defaults recorded; the plan must not
resolve them.

- **Q10 — Route arbitration when a retailer is on both networks.** Publish
  the higher member rate, the higher commission, the network with better
  validation history, or an explicit operator choice? *Default*: explicit
  operator choice, with first-import as the initial value and a demotion
  rule when the published route dies. Automatic rate arbitration is **not**
  built by default: it makes the catalogue change under members without
  anybody deciding.
- **Q11 — Linkwise publisher account and API access.** Is an account
  approved? Are credentials held? Is API documentation accessible, and is
  there a sandbox? *Default*: assume none; build against recorded fixtures
  and hold the adapter behind the same "no field mapped without a recording"
  gate Awin is behind. **This gates FR-105 for Linkwise.**
- **Q12 — Does Awin stay?** The request says "not Awin only", which reads as
  both. *Default*: both, with Awin's unfinished transactions work
  (T139–T144) in scope for this feature, since a second network cannot prove
  a seam the first has never used.
- **Q13 — Currency posture.** If a network reports sale and commission in
  different currencies, or in a currency the member's wallet does not hold —
  refuse the report, or carry a second currency column? *Default*: refuse at
  the port, as `Reported.Validate` does today, and treat a mixed-currency
  network as a founder decision before its adapter is written. Note the
  constitution puts **multi-currency wallets out of scope**.

---

## Assumptions

- Both networks speak HTTP and JSON. The port's retry and rate-limiting
  vocabulary assumes it; a SOAP or file-drop network would need new port
  vocabulary and is not assumed here.
- Alpha volumes are unchanged — hundreds of members, tens of merchants.
  Nothing here requires horizontal scale, and two networks doubles a small
  number.
- The existing `cashback.network` row per network, written by
  `connect-network`, remains the only way a network enters the system.
- No member-facing copy changes. A member sees a retailer and a rate; which
  network carries it is not a member-facing fact.

---

## Out of Scope

- **Automatic rate arbitration** between networks (see Q10's default).
- **Multi-currency wallets** — out of scope by the constitution.
- **A third network.** SC-024 asks that the third be cheap, not that it be
  built.
- **Deduplicating one purchase reported by two networks.** One click backs
  one credit (FR-097); two networks reporting the same purchase resolves to
  one match and one unattributed row, which is the correct outcome and needs
  no cross-network reconciliation.
- **Per-network member-facing presentation** — no badge, no filter, no
  "powered by" line.
- **Frontend work of any kind.** The operator surfaces in US4 are HTTP
  endpoints; their pages belong to the frontend workstream.
