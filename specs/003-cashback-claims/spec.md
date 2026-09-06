# Feature Specification: Cashback Claims — A Missing Credit Is a Record, Not a Ticket

**Feature Branch**: `003-cashback-claims`

**Created**: 2026-09-03

**Status**: Draft — blocked on a constitution amendment (see Governance Impact)

**Input**: Design handoff from the Apivo design canvas, turn 4, screens **4e**
(member: "Report a missing cashback") and **4f** (operator: claims queue with
evidence and our records side by side). The founder's direction on the handoff:
build the cashback screens against the shipped backend, and take the claim flow
— which has no backend — through the full spec process rather than drawing it.

## Governance Impact (blocking)

Constitution **v1.2.0** does not name claims. It is not that claims are
forbidden; it is that the "Cashback alpha" scope block lists what is in scope
and a claim handling surface is not among the items. Principle: *"scope stays
inside the declared product scope"* is checked on every PR, so an
implementation would be refused at review with nothing to point at.

This feature therefore requires a **MINOR amendment** adding claims to the
cashback scope block and adding the three invariants below to Principle IX.
The amendment is drafted alongside this spec; **implementation does not start
until the founder ratifies it.**

### The three invariants this feature adds

- **C-8 (No claim decision without a named decider)**: a claim decision row
  cannot exist without a non-null named human decider. The row IS the
  decision, exactly as `article.approved_by` is the approval and
  `payout.approved_by` is the payout's.
- **C-9 (Claim decisions are append-only)**: a decision rejects UPDATE, DELETE
  and TRUNCATE. Reversing a decision is a new superseding decision citing the
  one it supersedes, never an edit. The member's record shows both.
- **C-10 (A claim never moves money by itself)**: a claim resolution that
  results in a credit does so through the existing evidence-backed paths — an
  attribution of an already-ingested network transaction, or a goodwill
  posting from a named house account, recorded as goodwill. A claim may not
  write a `cashback.entry` citing evidence that does not exist, because C-2
  makes that unrepresentable, and it may not write a balance at all, because
  C-1 makes that unrepresentable.

## Product Frame

The mockup's claim screen carries one sentence that decides the whole design:

> *A claim is a record, not a support ticket.*

A support ticket is a conversation that ends when somebody stops replying. A
claim is an assertion that money is owed, and the product's whole posture — for
news and for cashback alike — is that a claim on money carries a state, dated
evidence, and a named person behind the decision. The newsroom cannot publish a
sentence without a named approver; operations cannot refuse a member's money
without a named decider and a reason the member gets to read.

The mockup states the promise in the member's own words — "a named operator
answers within 5 days", and "the decision and its reason join your record —
either way". Both halves are requirements here, and the second is the harder
one: a refusal is recorded and shown as plainly as a payment.

## Why a claim cannot simply pay out

This is the constraint that shapes everything downstream, and it is worth
stating before the user stories rather than discovering it in them.

**C-2**: a cashback credit cannot exist without a reference to exactly one
network transaction record. So when a member says "I bought this and got
nothing", exactly one of three things is true, and they have different remedies:

1. **The network reported it and we failed to attribute it.** The transaction
   is sitting in `cashback.unattributed_transaction`. The remedy already
   exists: `POST /ops/unattributed/{id}/attribute`. The claim's job is to find
   it and point the operator at it. This is the common case and it costs the
   business nothing — the commission was already earned.

2. **The network reported it and attributed it, but at the wrong rate.** The
   entry exists and is wrong. The remedy is an adjustment posting citing the
   dated rate that should have applied — the same machinery a reversal uses,
   in the other direction.

3. **The network never reported it.** There is no evidence and there never
   was. No entry may be created: an entry that cites nothing is exactly the
   row C-2 exists to forbid. The remedies are to escalate to the network
   (which may later produce the transaction, at which point case 1 applies),
   or to pay the member **goodwill** out of a named house account — money the
   business spends, not commission it passes on. Goodwill is visible as
   goodwill in the member's record and in the accounting export. It is never
   dressed as cashback.

An operator who cannot tell these apart will reach for whichever button pays
the member, and the ledger will stop meaning anything within a quarter. So the
operator screen's job is not to collect a verdict — it is to **show which of
the three cases this is**, from our own records, before the verdict is offered.

## Clarifications

### Session 2026-09-03

- **Q: Is a claim a support channel?** A: No. It has a fixed shape, a fixed
  set of outcomes and a decision record. Free-text conversation with the
  member ("Ask the reader" in the mockup) attaches to the claim as recorded
  correspondence; it is not the claim.
- **Q: Can a member claim for a purchase where they never clicked out?** A:
  They can submit it, and it will be recorded and answered. It will almost
  always resolve as case 3, because without a click there is nothing for the
  network to attribute. The answer says so in those words rather than
  refusing the form.
- **Q: What stops a claim being a free money tap?** A: The decision is named,
  permanent and exported. Goodwill has its own house account, so the total
  paid as goodwill is a number somebody has to look at monthly rather than a
  rounding error spread across cashback.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A member reports a purchase that never tracked (Priority: P1)

A member in Munich orders €84.30 of groceries through a partner's tracked
link. Three weeks later the wallet still shows nothing for it. From the entry
list's footer — *"Missing a purchase? Report it"* — they open the claim form,
which already knows who they are and which partners they have clicked out to.
They give the partner, the purchase date, the amount and the order number, and
attach the order confirmation. On submit they get a reference immediately, and
the claim appears in their own record with the state "answered within 5 days"
and the date that promise falls due.

**Acceptance scenarios**

1. **Given** a signed-in member with participation active, **when** they submit
   a complete claim, **then** a claim row exists with a member-visible
   reference, a `submitted` state and a due date five working days out, and
   the response carries the reference.
2. **Given** the same member, **when** they open their claims list, **then**
   the claim is there with its state and its due date, before any operator has
   looked at it.
3. **Given** a claim for a purchase date more than the network's reporting
   window in the past, **when** it is submitted, **then** it is still recorded,
   and the acknowledgement says the window has closed and what that means for
   the likely answer.
4. **Given** a member who is not signed in, **when** they open the claim form,
   **then** they are asked to sign in; a claim without an account has nobody to
   credit and no record to join.

### User Story 2 - An operator sees which of the three cases it is (Priority: P1)

An operator opens the queue, oldest first. For the selected claim the screen
shows the member's assertion on the left — what they say they bought, for how
much, with the receipt as submitted — and **our records on the right**: whether
a click exists in the tracking log for that member and merchant near that time,
whether the network's feed carries a transaction matching the amount, and what
the rate table said on the day. The screen states which case the claim falls
into and offers the remedy that case allows, not a generic pay button.

**Acceptance scenarios**

1. **Given** a claim whose merchant and amount match an unattributed
   transaction, **when** the operator opens it, **then** the screen names that
   transaction, links to it, and the primary action attributes it — creating a
   normal cashback entry through the existing path.
2. **Given** a claim whose entry exists but was credited at 5% when the dated
   rate table says 7% applied that day, **when** the operator opens it,
   **then** the screen shows both rates and the difference, and the primary
   action is an adjustment for the difference, citing the rate row.
3. **Given** a claim with no click and no network transaction, **when** the
   operator opens it, **then** the screen says so plainly, no attribution
   action is offered, and the available outcomes are decline-with-reason or a
   goodwill payment that names the house account it comes from.
4. **Given** any outcome, **when** the operator submits it without a reason,
   **then** it is rejected. A decision a member cannot read is not a decision.

### User Story 3 - The decision joins the member's record (Priority: P1)

Whatever the answer, the member sees it: the outcome, the reason in words, the
date, and — where it paid — the entry or the goodwill posting it produced. A
declined claim is as visible as a paid one and stays visible.

**Acceptance scenarios**

1. **Given** a decided claim, **when** the member opens it, **then** they see
   the outcome, its reason and its date, and where money moved, the row it
   moved on.
2. **Given** a decision that was wrong and is superseded, **when** the member
   opens the claim, **then** both decisions are shown in order with the reason
   for the second; neither is hidden or edited.
3. **Given** a decided claim, **when** anything attempts to UPDATE or DELETE
   the decision row, **then** the database rejects it (C-9).

### User Story 4 - Goodwill is visible as goodwill (Priority: P2)

A goodwill payment is money the business gives away. It is posted from a named
house account, it appears in the member's record labelled as goodwill rather
than as cashback, and the accounting export carries the monthly total as its
own line.

**Acceptance scenarios**

1. **Given** a goodwill outcome, **when** the postings are read, **then** they
   debit the configured goodwill house account and credit the member, and the
   transfer's reference cites the claim.
2. **Given** a month with goodwill payments, **when** the accounting export
   runs, **then** goodwill is a separate total, never folded into cashback.
3. **Given** a deployment with no goodwill house account configured, **when**
   an operator opens a case-3 claim, **then** the goodwill action is absent and
   the screen says why, rather than failing at submit.

### Edge Cases

- A member submits the same purchase twice. The second submission is recorded
  and linked to the first; only one can pay.
- The network reports the transaction *after* a claim was declined as case 3.
  The transaction attributes normally and credits the member; the claim's
  declined decision is superseded by one that cites the arriving evidence.
- A member deletes their account with a claim open. Financial rows are never
  deleted (FR-082 in 002); the claim is closed with a recorded reason and the
  personal fields in it follow the same retention rule as the rest of the
  member's record.
- A receipt is uploaded that is not a receipt. It is evidence either way and
  is retained as submitted; the operator's reason says what was wrong with it.
- Two operators open the same claim. The second to decide is refused: a
  decision exists, and superseding it is a deliberate act with its own reason.

## Requirements *(mandatory)*

### Claim invariants (NON-NEGOTIABLE, database-enforced)

Each carries a test asserting the DATABASE rejects the illegal state, by
SQLSTATE, against a real Postgres.

- **C-8**: `claim_decision.decided_by` is `NOT NULL` and not blank.
- **C-9**: `claim_decision` rejects UPDATE, DELETE and TRUNCATE by trigger.
- **C-10**: no foreign key or write path lets a claim create a `cashback.entry`
  that does not cite a `network_transaction`; a goodwill transfer is a distinct
  transfer kind whose postings name the goodwill house account.

### Functional Requirements

- **FR-101**: A member with active participation MUST be able to record a
  claim naming the merchant, the purchase date, the amount in minor units with
  its currency, an optional order reference and an optional evidence file.
- **FR-102**: A submitted claim MUST receive a member-visible reference and a
  due date, both in the submit response, before any operator sees it.
- **FR-103**: A claim MUST be answerable only by an operator, and only with a
  non-blank reason (the same rule every operator action in 002 carries).
- **FR-104**: The operator surface MUST show, for each claim, the tracking-log
  row, the network-feed row and the dated rate row that bear on it, or state
  plainly that there is none.
- **FR-105**: The system MUST classify each claim as attributable, adjustable
  or unevidenced, and MUST offer only the remedies that classification allows.
- **FR-106**: A resolution that credits MUST do so through attribution or
  adjustment where evidence exists, and through a goodwill transfer from a
  named house account where it does not. No other credit path exists.
- **FR-107**: A decision MUST be append-only and MUST be visible to the member
  with its reason, whatever the outcome.
- **FR-108**: A superseding decision MUST cite the decision it supersedes; both
  remain visible.
- **FR-109**: Claim state and decisions MUST be emitted to `public.domain_event`
  through the transactional outbox, like every other cross-cutting fact.
- **FR-110**: Evidence files MUST be stored outside the database with a
  content hash recorded in it, MUST never be served to anyone but the member
  who uploaded them and an operator, and MUST be covered by the same retention
  answer as the rest of the member's cashback record.
- **FR-111**: All member-facing claim text MUST live in the translation
  catalogues keyed by BCP-47 primary language subtag, and MUST carry no brand
  literal.
- **FR-112**: Claim routes MUST be `noindex, nofollow` like every other
  cashback route.

### Key Entities

- **claim** — the member's assertion: member, merchant, purchase date, amount,
  order reference, state, reference, due date, submitted timestamp.
- **claim_evidence** — a file the member attached: content hash, media type,
  size, storage key. Immutable.
- **claim_decision** — append-only: claim, outcome, reason, decided_by,
  decided_at, superseded_decision_id, and the resolution row it produced
  (entry, adjustment or goodwill transfer reference) where it produced one.
- **claim_correspondence** — recorded messages between operator and member on
  a claim. Append-only. Not the claim.

## Success Criteria *(mandatory)*

- **SC-101**: A member can submit a claim and read its reference in under
  60 seconds from the wallet, on a phone.
- **SC-102**: For any decided claim, one query returns the assertion, the
  evidence, our records at decision time, the decision, its reason, its named
  decider, and the posting it produced — in under five minutes, matching C-7's
  bar.
- **SC-103**: 100% of decisions carry a non-blank reason and a named decider,
  asserted by the invariant suite, not by review.
- **SC-104**: No decision row in the database has ever been updated, asserted
  by the trigger test.
- **SC-105**: Goodwill totals are separable from cashback totals in the
  accounting export for any month, to the minor unit.
- **SC-106**: Median answer time is reported on the operator surface against
  the five-day promise, as the mockup draws it.

## Open Questions (founder-only)

Recorded, not resolved. Safe defaults apply until answered.

- **Q10 — Goodwill budget.** Is there a cap on goodwill per member, per month,
  or in total, and what happens at the cap? *Default: no cap, and the monthly
  total is reported so the absence of a cap is visible.*
- **Q11 — Evidence retention.** How long are receipts kept, and are they
  deleted on account deletion given they evidence a money decision? *Default:
  no automated deletion, matching the news retention default; the question is
  flagged because a receipt is more personal than a click log.*
- **Q12 — The five-day promise.** Is five working days a commitment the
  business is making publicly, or an internal target? The mockup states it to
  the member. *Default: shown as a target with the current median beside it.*
- **Q13 — Who may decide.** Is any operator allowed to decide any claim, or
  does a goodwill payment above some amount need a second named person, as the
  payout cycle close does? *Default: any operator, with every decision named
  and exported.*

## Assumptions

- Claims are for the member's own purchases only; there is no third-party or
  merchant-raised claim in this feature. The design conversation's "merchant's
  own claims view" is a later feature.
- The mockup's claim kinds — "Missing cashback" and "Wrong amount" — map onto
  the classification in FR-105 rather than being a member-chosen category. The
  member describes a purchase; the system works out which kind it is.
- Illustrative amounts, merchants and windows in the mockup carry no product
  meaning.

## Out of Scope (this feature)

Merchant-raised claims; an appeal flow for a declined entry (distinct from a
superseding decision an operator takes); automated claim resolution; automated
fraud scoring on claims, which the constitution puts out of scope for both
products; any card-linked or in-store claim, which has no evidence path
(#445).
