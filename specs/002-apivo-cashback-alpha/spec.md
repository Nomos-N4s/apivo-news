# Feature Specification: Apivo Cashback Alpha — Member Cashback for the Apivo Super App

**Feature Branch**: `002-apivo-cashback-alpha`

**Created**: 2026-08-24

**Status**: Approved — constitution amendment ratified 2026-08-24 (see Governance Impact)

**Input**: User description: "An open source (completely free to use) cashback
system for Apivo the SuperApp. We have the news portal; we now need to expand
the MVP to have cashbacks in place. There are open source offerings out there,
we must use one of them, so a proper architecture decision record must be made.
It must be ready to be rebranded. We have the stack we have for apivo-news; I'm
open to a different stack for apivo-cashback. Start thinking how we make this
super app easier to maintain: microservices? multiple modular monoliths?
events? multi-repo? monorepo?"

## Governance Impact (resolved)

This feature was blocked by constitution v1.0.0, which listed cashback under
*"Out of scope — do not build, do not scaffold"* and constrained the backend
to *"a single binary, modular monolith. No microservices."*

**The founder ratified the amendment on 2026-08-24.** Constitution
**v1.1.0** now carries:

- **Principle IX** — Money Is Double Entry, Evidence-Backed and Exactly Once,
  holding invariants C-1 to C-7 below, with the C-1 exception named
  explicitly under Principle VIII.
- **Architecture Constraints** — "a modular monolith per product domain,
  composed into one binary"; a **Products** subsection (schema per product,
  no cross-product FK, no cross-product import, events only); a
  **Rebrandability** subsection; and permission for a self-hosted
  open-source ledger to run as a sidecar.
- **Product Scope & Delivery Rules** — cashback is a named second product
  with its own scope block and its own cut order.

The blocking gate is cleared. Implementation may proceed.

## Product Frame

Apivo is a super app for Greek communities abroad. epiloYES (news) is the
first surface; **cashback is the second**. The two share one member
identity, one place/language model, one brand system and one operational
spine — but they are separate bounded products with separate legal
exposure.

The cashback business model is **affiliate publishing with revenue share**:

1. A member browses merchant offers inside Apivo.
2. The member clicks out; Apivo issues a tracked redirect through an
   affiliate network deeplink carrying a per-click reference.
3. The member buys at the merchant.
4. The network reports the transaction (pending, then approved or declined)
   and later pays Apivo the commission.
5. Apivo credits the member an agreed share of that commission and pays it
   out once the member's confirmed balance passes a threshold.

Apivo never touches the merchant's checkout, never holds the member's card
and never sells goods. **Apivo's product is a claim on a future rebate**,
not stored value — see FR-041 and Open Question Q2.

## Clarifications

### Session 2026-08-24

- Q: Ratify the constitution amendment (v1.0.0 → v1.1.0) that brings
  cashback into scope and reworks the single-binary constraint? →
  A: **Ratified.** Constitution v1.1.0 is in effect; ADR-0001 to ADR-0005
  move to Accepted.
- Q: Regulatory posture on member balances (Q2 — PSD2 / e-money)? →
  A: **Rebate-claim posture for the alpha/MVP.** No member-to-member
  transfers, no spending inside Apivo, payouts only to a destination the
  member owns and has verified. Recorded in the constitution under
  "Cashback alpha". Moving to stored value is a new founder decision taken
  with legal advice.

Eight founder-blocked questions remain (Q1, Q3–Q9), recorded below under
**Open Questions (founder-only)**. Consistent with Governance, this spec
records a safe default for each and resolves none of them.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A Munich member earns cashback on a purchase (Priority: P1)

A signed-in member browses cashback offers in Greek, opens a merchant they
recognise, sees the rate ("4% cashback, typically confirmed in 30 days"),
clicks through, and buys at the merchant's own site. Within the merchant's
reporting window the purchase appears in the member's Apivo wallet as
**Pending**, showing the merchant, the date, the purchase amount and the
cashback amount. When the network confirms it, the entry becomes
**Confirmed** and the confirmed balance rises.

**Why this priority**: This is the entire cashback thesis end to end. Every
other story exists to make this one lawful, correct and payable. Without
click to transaction to credit, there is no product.

**Independent Test**: With a sandbox or replayed network feed, click out
from a seeded offer, replay a matching pending transaction, assert a Pending
wallet entry linked to that click; replay the approval, assert Confirmed and
a correct confirmed balance.

**Acceptance Scenarios**:

1. **Given** a signed-in member and an active offer, **When** they click
   through, **Then** a click record is created with a unique click
   reference, and the member is redirected to the network deeplink carrying
   that reference — or, if the deeplink cannot be built, the member is told
   plainly and no click record is created.
2. **Given** a click record exists, **When** a network transaction arrives
   quoting that click reference, **Then** a cashback entry is created in
   Pending state for the member, with the member's share computed from the
   commission and the rate that applied **at click time**.
3. **Given** a Pending entry, **When** the network reports the transaction
   approved, **Then** the entry becomes Confirmed and the member's confirmed
   balance increases by exactly the entry amount.
4. **Given** a Pending or Confirmed entry, **When** the network reports the
   transaction declined or reversed, **Then** a reversing entry is recorded,
   the balance is reduced, and the original entry is never edited or
   deleted.
5. **Given** a transaction arrives quoting an unknown or absent click
   reference, **Then** it is recorded as **Unattributed** and surfaced to
   the operator; no member is credited by guesswork.

---

### User Story 2 - Network transactions ingested as immutable evidence (Priority: P1)

The operator connects an affiliate network publisher account. The system
polls the network's publisher transaction API on a schedule and stores every
transaction exactly as reported, together with when it was retrieved, from
which network account, with which query window, and the full raw payload.
Retrieved network records can never be altered or deleted; a later status
change is a **new** record superseding the old one.

**Why this priority**: Equal-first with US1. Cashback owes real money to
real people out of money a third party says it will pay. The reconciliation
defence rests on evidence captured at retrieval — exactly as licence
provenance does for news (constitution III and IV).

**Independent Test**: Point the ingester at a recorded network response,
verify each transaction is stored with complete retrieval evidence in one
transaction, verify re-polling the same window creates no duplicates, and
verify any UPDATE or DELETE on a stored record is rejected by the database.

**Acceptance Scenarios**:

1. **Given** a configured network account, **When** the poller runs,
   **Then** each reported transaction is stored with network id, publisher
   account, retrieval timestamp, query window, raw payload and a content
   fingerprint — all present, or nothing is stored at all.
2. **Given** a stored network transaction record, **When** any change or
   removal is attempted, **Then** the database refuses.
3. **Given** the same transaction is reported again unchanged, **When**
   polling repeats, **Then** no duplicate record is created.
4. **Given** the same transaction is reported with a changed status or
   amount, **Then** a new record is stored superseding the previous one, and
   both remain readable in order.
5. **Given** the network API errors or rate-limits, **When** the poller
   runs, **Then** it backs off and retries without losing or double-counting
   a window, and the failure is visible to the operator.

---

### User Story 3 - The member sees an honest wallet (Priority: P2)

The member opens their wallet and sees three numbers that never disagree
with the entries below them: **Pending**, **Confirmed (available)** and
**Paid out**. Each entry names the merchant, the purchase date, the purchase
amount, the cashback amount, its state and the expected confirmation window.
Nothing in the wallet is presented as spendable before it is confirmed.

**Why this priority**: Trust is the product in cashback; a wallet that
overstates availability creates both complaints and liability. It follows
US1 and US2 because it renders their output.

**Independent Test**: Seed entries across every state, load the wallet, and
assert each displayed total equals the sum of its entries computed
independently from the ledger.

**Acceptance Scenarios**:

1. **Given** entries in mixed states, **When** the wallet loads, **Then**
   each total equals the independent ledger sum for that state, to the minor
   unit.
2. **Given** an entry was reversed, **When** the member views history,
   **Then** they see both the original credit and the reversal with a plain
   reason, and neither is hidden.
3. **Given** the member has no activity, **When** the wallet loads, **Then**
   it says so plainly with no filler and no error.

---

### User Story 4 - Withdrawal with a named approver (Priority: P2)

Once confirmed balance passes the payout threshold, the member requests a
withdrawal to their own verified destination. The request is reviewed, and
on approval the payout is executed once — never twice — and the confirmed
balance is reduced by the paid amount in the same transaction that records
the payout.

**Why this priority**: This is where money leaves the business. It is
second-priority only because nothing can be withdrawn until US1 and US2
produce confirmed balances.

**Independent Test**: Drive a withdrawal end to end against a stub payout
rail, including a duplicate submission and a rail failure, and assert
exactly-once semantics and balance integrity in both.

**Acceptance Scenarios**:

1. **Given** confirmed balance below the threshold, **When** withdrawal is
   requested, **Then** it is refused with the shortfall stated.
2. **Given** sufficient confirmed balance, **When** withdrawal is requested,
   **Then** the amount is moved to a **Reserved** state so it cannot be
   requested twice, and a request awaiting approval is created.
3. **Given** a withdrawal request, **When** it is approved, **Then** the
   approval records a named human approver and the payout is submitted with
   an idempotency key derived from the request.
4. **Given** a payout already submitted, **When** the same request is
   retried for any reason, **Then** the rail is not charged twice and the
   ledger records exactly one payout.
5. **Given** the payout rail rejects or fails permanently, **Then** the
   reserved amount returns to confirmed balance and the member is told.
6. **Given** a withdrawal destination, **When** it is not owned by the
   requesting member, **Then** the request is refused.

---

### User Story 5 - Localised merchant catalogue (Priority: P3)

A member in Munich reading in Greek browses merchants relevant to Munich and
to Germany, with names, categories and terms shown in Greek where available
and in the source language otherwise. Language and place stay independent
axes, exactly as in news.

**Why this priority**: The catalogue drives clicks, but a small seeded
catalogue is enough to prove US1; breadth can follow.

**Independent Test**: Seed merchants across two countries and two languages,
load the catalogue as an `el` reader scoped to Munich, and verify scoping,
fallback and rate rendering.

**Acceptance Scenarios**:

1. **Given** merchants scoped to different places, **When** the catalogue
   loads for Munich, **Then** only merchants available to that place are
   listed.
2. **Given** a merchant with no Greek copy, **When** the catalogue loads in
   Greek, **Then** the source-language copy is shown and labelled, never a
   blank and never a machine-invented name.
3. **Given** a merchant whose rate varies by category, **When** the merchant
   page loads, **Then** every published rate band is shown with its
   conditions and its exclusions.

---

### User Story 6 - Operator reconciles what the network actually paid (Priority: P3)

At the end of a network's payment cycle the operator imports the network's
statement and the system reconciles reported commissions against amounts
actually received, flagging every difference. Member cashback is only
promoted to payable on commissions the business has actually been paid.

**Why this priority**: Without it, Apivo pays members out of commissions
that never arrive. It is P3 only because manual reconciliation can carry the
first weeks at alpha volume.

**Independent Test**: Import a statement that omits one approved transaction
and shorts another, and assert both are flagged and neither silently changes
a member's confirmed balance.

**Acceptance Scenarios**:

1. **Given** an imported network statement, **When** reconciliation runs,
   **Then** every reported-but-unpaid and paid-but-unreported item is listed
   with its amount difference.
2. **Given** a mismatch, **When** it is resolved, **Then** the resolution
   records who resolved it and why, and any member-facing effect is a new
   ledger posting, never an edit.

---

### User Story 7 - Abuse and self-dealing are contained (Priority: P3)

Clicks and credits that look like abuse — a member cycling purchases through
many accounts, click floods from one device, transactions on excluded
categories — are held for review rather than auto-credited, and holds are
visible and explainable to the member.

**Why this priority**: Cashback attracts fraud from day one, but at alpha
volume a review queue with hard rules is sufficient; scoring can come later.

**Independent Test**: Replay abusive click and transaction patterns and
assert each lands in the review queue with its triggering rule named, and
that a normal pattern passes untouched.

**Acceptance Scenarios**:

1. **Given** click volume from one member or device above the configured
   rule, **When** the next click is made, **Then** it is recorded and
   rate-limited, and the member sees a plain message.
2. **Given** a transaction matching a hold rule, **When** it is ingested,
   **Then** it is credited in **Held** state, not Pending, and appears in
   the review queue naming the rule.
3. **Given** a held entry, **When** an operator releases or rejects it,
   **Then** the decision records a named human and a reason.

---

### Edge Cases

- A member deletes their account while entries are pending or a withdrawal
  is in flight.
- The network reverses a transaction **after** the member has been paid out
  (clawback posture — Q3).
- A merchant leaves the network mid-cycle; existing pending entries must
  still resolve.
- Currency differs between the network's commission and the member's payout
  currency.
- Rates change between click time and transaction time (the click-time rate
  governs — FR-013).
- The same transaction is reported by two networks (duplicate attribution).
- Rounding: a 4% share of an odd commission in minor units.
- The network's API changes shape or a required field is absent.
- Clock skew between Apivo and the network's reported timestamps.

## Requirements *(mandatory)*

### Cashback Invariants (NON-NEGOTIABLE, database-enforced)

Mirroring the constitution's I-1 to I-5 discipline: every one of these is
enforced by the database and carries a test asserting the **database**
rejects the illegal state, by SQLSTATE, against a real Postgres.

- **C-1 (Double entry)**: A member balance is never stored as a settable
  number. It exists only as the sum of immutable ledger postings, and every
  posting belongs to a transfer whose postings sum to zero per currency.
- **C-2 (Attribution)**: A cashback credit cannot exist without a reference
  to exactly one network transaction record and, through it, at most one
  click record. Credits with no evidence are unrepresentable.
- **C-3 (Immutable network evidence)**: Network transaction records reject
  UPDATE, DELETE and TRUNCATE. Status changes arrive as new superseding
  records.
- **C-4 (No payout without a named approver)**: A payout row cannot exist
  without a non-null named human approver, enforced by `NOT NULL` — the row
  **is** the approval, exactly as `article.approved_by` is for news.
- **C-5 (Exactly-once money movement)**: Every outbound payout carries a
  unique idempotency key with a database uniqueness constraint; a second
  attempt cannot create a second payout.
- **C-6 (Integer money)**: All monetary amounts are integer minor units with
  an explicit ISO-4217 currency code. Floating point in a money column, or a
  posting without a currency, is rejected by the schema.
- **C-7 (Traceability)**: For any member payout, one query returns the full
  chain — payout to approver to ledger postings to cashback entries to
  network transaction evidence to click to offer and rate at click time — in
  under five minutes.

### Functional Requirements

**Identity and membership**

- **FR-001**: A cashback member is the same Apivo account as a news reader;
  no second account, no second login.
- **FR-002**: Cashback participation is an explicit opt-in recorded with
  timestamp and the terms version accepted.
- **FR-003**: A member can leave cashback and export their cashback history;
  leaving ends participation without deleting financial records required for
  accounting (mirrors the news "unpublish keeps the record" decision).

**Catalogue**

- **FR-010**: Merchants, offers and rate bands are stored with place scope
  and per-language copy; language and place remain independent axes
  (constitution VII).
- **FR-011**: Every rate band records its conditions, exclusions and the
  network it is sourced from.
- **FR-012**: Catalogue data imported from a network records where it came
  from and when it was retrieved.
- **FR-013**: The rate a member sees at click time is snapshotted onto the
  click record and governs the credit, even if the published rate later
  changes.

**Click-out**

- **FR-020**: Every click-out creates a click record with a unique,
  unguessable click reference before the redirect is issued.
- **FR-021**: The redirect passes the click reference to the network in the
  network's own click-reference parameter.
- **FR-022**: Click records store member, offer, rate snapshot, timestamp
  and a privacy-minimised device or context fingerprint sufficient for abuse
  rules — never more than needed.
- **FR-023**: Click-out for a signed-out visitor prompts sign-in first; an
  anonymous click can never later be credited to an account.

**Transaction ingestion**

- **FR-030**: Each supported network has an adapter behind one interface;
  adding a network must not change the domain, and any adapter must be
  replaceable in under five engineer-days (mirrors the translation-adapter
  constraint).
- **FR-031**: Polling respects each network's documented rate limits and
  query-window limits, and never loses or double-counts a window across
  restarts.
- **FR-032**: The raw network payload is stored verbatim alongside the
  normalised record.
- **FR-033**: Transaction state is normalised to a single domain state
  machine: `pending` to `confirmed` or `declined`, plus `reversed` from
  either.
- **FR-034**: A transaction with no matching click is recorded as
  unattributed and never auto-credited.

**Cashback and ledger**

- **FR-040**: A member's share is computed from the commission actually
  reported, the click-time rate snapshot and a documented rounding policy;
  rounding differences accrue to a house account and are never lost.
- **FR-041**: Member balances are a **claim on a future rebate**: they
  cannot be transferred between members, cannot be spent inside Apivo, and
  can only be paid to a destination the member owns. (Default posture for
  Q2; a different posture is a founder decision taken with legal advice.)
- **FR-042**: Cashback entry states are `held`, `pending`, `confirmed`,
  `reserved`, `paid` and `reversed`, and every transition is a ledger
  posting.
- **FR-043**: Confirmed balance only counts entries whose underlying
  commission is both approved by the network and reconciled as received.

**Withdrawal and payout**

- **FR-050**: Withdrawal requires confirmed balance at or above the
  configured threshold.
- **FR-051**: A payout destination must be verified as belonging to the
  member before it can receive money.
- **FR-052**: Payout rails sit behind one interface; the alpha ships at
  least one rail plus a manual/offline rail that still enforces C-4 and C-5.
- **FR-053**: Payout failures are classified explicitly as terminal or
  retryable; retryable failures reuse the same idempotency key.

**Operator surface**

- **FR-060**: Operators have queues for unattributed transactions, held
  entries, reconciliation differences and withdrawal approvals.
- **FR-061**: Every operator action records a named human and a reason and
  is appended to the audit stream.
- **FR-062**: Ledger and reconciliation reports are exportable for
  accounting.

**Rebranding (explicit founder requirement)**

- **FR-070**: No product name, legal entity, domain, colour, logo, support
  address or currency default is hardcoded in application code; all of it
  resolves from one brand configuration.
- **FR-071**: All member-facing text lives in translation catalogues keyed
  by BCP-47 primary language subtag; no English string literals in
  templates.
- **FR-072**: A second brand can be stood up from configuration and assets
  alone — no code fork, no schema change — proved by a test that renders the
  primary surfaces under a fixture brand.
- **FR-073**: Emails, legal documents and payout descriptors are templated
  from the same brand configuration.

**Cross-product (super app)**

- **FR-080**: News and cashback share identity, place and language, brand
  and platform primitives, and nothing else; neither reads the other's
  tables.
- **FR-081**: Cross-product communication is asynchronous, through the
  append-only domain event stream, with no synchronous call from one product
  domain into the other.
- **FR-082**: An architecture test fails the build when a domain imports a
  sibling domain's internals or references its schema.

### Key Entities

- **Member participation** — an Apivo account's opt-in to cashback, terms
  version, status.
- **Network account** — a publisher account at an affiliate network, with
  credentials held outside the database and a polling cursor.
- **Merchant** — a retailer available through one or more networks, with
  place scope and per-language copy.
- **Offer / rate band** — a published cashback rate with conditions,
  exclusions, validity window and source network.
- **Click** — a member's tracked click-out, its unguessable reference, and
  the rate snapshot that governs any resulting credit.
- **Network transaction record** — immutable evidence of what a network
  reported, with retrieval metadata and raw payload.
- **Cashback entry** — the member-facing unit of earning, in one state, tied
  to exactly one network transaction record.
- **Ledger account / transfer / posting** — the double-entry substrate that
  is the only source of truth for any balance.
- **Withdrawal request** — a member's request, its reservation, its named
  approver and its outcome.
- **Payout** — one outbound money movement with its idempotency key and rail
  reference.
- **Reconciliation run / difference** — a network statement import and every
  discrepancy it revealed.
- **Audit event** — append-only record of every operator and system
  decision.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A purchase made through Apivo appears in the member's wallet
  as Pending within one polling cycle of the network reporting it, and no
  later than 24 hours after the network reports it.
- **SC-002**: 100% of member credits trace to a stored network transaction
  record and, where attributed, to a click record — verified by a query that
  returns zero orphan credits.
- **SC-003**: The sum of all ledger postings per currency is exactly zero at
  all times, verified continuously by an automated check.
- **SC-004**: Zero duplicate payouts across a test suite that includes
  concurrent submissions, retries and rail timeouts.
- **SC-005**: The full provenance chain for any payout (C-7) is returned by
  one query in under five minutes of operator effort.
- **SC-006**: Wallet totals reconcile with independently computed ledger
  sums to the minor unit, for every member, in an automated check.
- **SC-007**: A second brand renders every member-facing surface with no
  code change, proved in CI.
- **SC-008**: Adding a second affiliate network requires changes only inside
  its adapter package, proved by an architecture test.
- **SC-009**: Wallet and catalogue pages render in under 2 seconds at alpha
  scale.
- **SC-010**: Every reversal or clawback leaves an auditable pair of
  postings; no balance is ever corrected by editing history.

## Open Questions (founder-only)

Per constitution Governance these are decided by the founder alone. Safe
defaults are recorded; the plan must not silently resolve them.

- **Q1 — Which affiliate networks first?** Publisher accounts require
  application and approval, and coverage differs sharply between Greece and
  Germany. *Default until answered*: build against one adapter with recorded
  fixtures and no live credentials.
- ~~**Q2 — Regulatory posture on member balances (PSD2 / e-money).**~~
  **DECIDED 2026-08-24**: the rebate-claim posture of FR-041 — no
  member-to-member transfers, no spending inside Apivo, payouts only to the
  member's own verified destination. In the constitution under "Cashback
  alpha". A move to stored value is a new founder decision taken with legal
  advice; a solicitor's confirmation of this posture is still worth
  obtaining before public launch, but no longer gates anything.
- **Q3 — Clawback posture** when a transaction reverses after payout: absorb
  as a business loss, or carry a negative member balance against future
  earnings? *Default*: absorb, record the loss, never chase the member.
- **Q4 — Revenue share and rounding**: what percentage of commission goes to
  the member, and does rounding favour the member or the house? *Default*:
  configuration with no value committed; rounding to the member's favour at
  the minor unit, remainder to a house account.
- **Q5 — Payout rails and threshold**: SEPA credit transfer, PayPal payouts,
  vouchers? Minimum threshold? *Default*: manual/offline rail plus a stub,
  threshold as configuration.
- **Q6 — KYC and sanctions posture** for payouts. *Default*: verified
  destination ownership only; no identity verification in the alpha, which
  constrains payout size and rail choice.
- **Q7 — Tax treatment and member reporting** in Germany and Greece
  (cashback as a purchase rebate versus taxable income). *Default*: rebate
  framing in all copy; no tax statements issued.
- **Q8 — Click-log retention.** Ties directly to the still-deferred news
  retention question. *Default*: no automated deletion, consistent with the
  2026-08-14 decision.
- **Q9 — Repository and brand naming.** Does the monorepo become `apivo`
  with epiloYES as a brand inside it, or does `apivo-news` stay and gain a
  second product? *Default*: single repository, unrenamed, restructured
  internally — see [research.md](research.md) ADR-0002.

## Assumptions

- Apivo can obtain at least one affiliate network publisher account; without
  one there is no commission source and the product cannot exist. Building
  against recorded fixtures de-risks the wait but does not remove it.
- Alpha volumes are small: hundreds of members, tens of merchants, low
  thousands of clicks per month. Nothing in this spec requires horizontal
  scale.
- Members are the existing Apivo/epiloYES identity population; no separate
  onboarding funnel is in scope.
- The existing Supabase Postgres (EU) is acceptable for financial records at
  alpha volume.

## Out of Scope (alpha)

Price comparison, fuel saver, loyalty points and tiers, reviews, TV and
radio listings, referral bonuses, in-app spending of balances,
member-to-member transfers, browser extension, mobile apps, card-linked
offers, in-store cashback, multi-currency wallets, automated fraud scoring,
and the ebest.gr user migration.
