# Feature Specification: Evidence a Member Can Check

**Feature Branch**: `xcoder/005-provable-evidence`
**Spec Directory**: `specs/005-provable-evidence/`
**Created**: 2026-09-06
**Status**: Draft — awaiting founder answers to Q14–Q16
**Input**: Founder, 2026-09-06: *"hmm, it makes me wonder, why not a 3 entry
ledger? a la bitcoin / crypto?"*

---

> **On the number.** This is **005**. 004 is the multi-network feature
> (`specs/004-multi-network-linkwise/`), which is in flight; 003 is the
> claims feature. Taking the next free number rather than reusing one costs
> nothing now and is impossible to correct after a merge.

> **On the order.** This is queued **behind the local end-to-end demo**
> ([#533](https://github.com/Nomos-N4s/apivo-news/pull/533)), at the
> founder's instruction of 2026-09-06. The demo makes the product visible;
> this makes its evidence checkable. Nothing here starts before that lands,
> and Q14–Q16 below gate it further.

---

## Why this exists

The founder asked why the ledger is not "three entry, a la bitcoin". The
question is better than it looks, and answering it exactly is what this spec
is for.

**Triple-entry accounting** (Ian Grigg, 2005) is not a third column in a
journal. It is the idea that a transaction between two parties produces a
**digitally signed receipt that both parties hold**, and that this receipt —
not either party's own books — is the authoritative record. Each party still
keeps double entry; the receipt is the third entry, and it is the one neither
side can quietly change.

Bitcoin is one implementation of the same idea with a particular answer to
one sub-problem: who witnesses the receipt. It answers "a public network of
mutually distrusting parties". That answer costs a consensus protocol, a
token, and settlement latency, and it buys a property this product does need:
**the operator cannot rewrite history without it being noticeable.**

So the useful question is not "should this be a blockchain" — it should not —
but: *how much of the receipt property does this product already have, and
what is missing?*

## What exists today

Verified against the schema, not remembered.

| Fact | Where |
|---|---|
| Every network report is fingerprinted with SHA-256 over the reported facts — click reference, both statuses, both amounts, currency, transaction time — unit-separated so two different field splits cannot collide | `cashback.network_transaction.content_digest`, computed by `cashback.network_transaction_guard()`, migration 0012 |
| The digest is computed **by the database**. A caller-supplied value is discarded, so the fingerprint cannot disagree with what it fingerprints | same trigger |
| An unchanged re-report is a no-op, enforced by the schema rather than remembered by the poller | `unique (network_id, external_id, content_digest)` |
| A status or amount change is a **new row superseding the old**, never an edit; one root per transaction, no forks, and a superseding row must be about the same transaction at the same network | `supersedes_id`, `network_transaction_one_root`, the guard |
| UPDATE, DELETE and TRUNCATE are rejected outright on the click, the network transaction, the entry transitions, the ledger links, the reconciliation runs and the event deliveries | `public.raise_immutable()` triggers, migrations 0012, 0013, 0015, 0021 |
| DELETE and TRUNCATE are rejected on the earning, the payout and the unattributed-transaction row. Their UPDATEs go through a guard that decides which transitions are legal, so their *history* lives in the append-only tables above rather than in the row itself | `entry_guard()`, `payout_guard()`, migrations 0013, 0014, 0024 |
| The rate that governs a credit is the one snapshotted at click time; a later rate change never reaches back | `cashback.click.rate_snapshot`, `member_share_bps_snapshot` |
| Balances are never a settable number: they are the sum of immutable postings, checked continuously to sum to zero per currency | C-1, ADR-0002, the zero-sum check |

That is a great deal more than most cashback products have, and it is worth
saying plainly: **the evidence is already immutable and already
fingerprinted.** What follows is not a rescue.

## What is missing

Five gaps, each of which is a thing a member or an auditor cannot currently
do.

**G1 — the digests fingerprint rows, not the sequence.** Each
`content_digest` covers one row's contents. Nothing links row *N* to row
*N−1*. Delete a run of rows and insert a consistent alternative history, and
every individual digest still verifies, because each was only ever a
statement about itself.

**G2 — the triggers bind the application, not everyone.** `raise_immutable()`
stops the application's role from editing evidence, which is what it is for
and it does it well. It does not bind a superuser, a `alter table … disable
trigger`, or a restore from a doctored dump. Principle VIII's preference for
database-enforced invariants is right, and this is the edge of what a
database can enforce **about its own operator**.

**G3 — nothing leaves the database.** Evidence held solely by the party it
would incriminate is evidence that party can rewrite. This, not hashing, is
the actual property Bitcoin has and we do not: not "there are hashes" but
"the hashes were witnessed, at a known time, by parties who do not work for
the operator".

**G4 — a member holds nothing they can check.** C-7 guarantees that *one
query* answers the whole chain — payout, approver, postings, entries, network
evidence, click, rate at click time — in under five minutes. That answers the
operator's question ("show me the chain"). It does not answer the member's
("prove you did not change it after I clicked"). Today the only available
answer is "trust our database", which is exactly the answer a receipt exists
to replace.

**G5 — the click and the postings carry no digest at all.** Only
`network_transaction` has one. The member's half of the evidence — the rate
and share snapshotted at click time, which is what decides what they are owed
— is protected by immutability triggers alone. A chain built over network
reports and not over clicks would leave the disputed half outside it, and the
dispute members actually raise is *"the rate was different when I clicked"*.

## User Scenarios & Testing

### User Story 1 — The evidence chain breaks when history is altered (Priority: P1)

An auditor, or a monitoring job, can tell that the evidence for a period is
intact — or that it is not — without trusting the application that wrote it.
Each evidence row carries the digest of the row before it in its chain, so a
deleted, inserted or reordered row breaks the chain from that point on and
the break names where it happened.

**Why this priority**: it is the one gap that can be closed entirely inside
our own schema, with no external dependency, no recurring cost and no founder
decision. It also has to exist before either of the other stories can:
checkpoints publish a chain tip, and a receipt proves membership in a chain.

**Independent Test**: insert evidence, verify the chain end to end, then
delete a row with the triggers temporarily disabled (as a superuser can) and
verify the check now fails and names the first broken link.

**Acceptance Scenarios**:

1. **Given** a chain of network reports for one publisher account, **When**
   the verifier walks it, **Then** every link reproduces from the row's own
   contents and the previous link, and the walk reports the tip.
2. **Given** the same chain with one row deleted out of the middle by a
   superuser, **When** the verifier walks it, **Then** it fails, names the
   first row whose predecessor is missing, and does not report a tip.
3. **Given** the same chain with one row's contents altered by a superuser,
   **When** the verifier walks it, **Then** it fails at that row, because the
   recomputed content digest no longer matches the link built from it.
4. **Given** two clicks by different members arriving concurrently, **When**
   both are chained, **Then** both succeed and the chain has a single
   well-defined order — a chain is a sequence, and the schema, not the
   application, decides it.

---

### User Story 2 — The chain tip is witnessed outside the operator (Priority: P2)

At a fixed cadence, the tip of each chain — and a Merkle root over the rows
added in that period — is published somewhere the operator cannot silently
rewrite, signed, with an independently attested time. Anyone can later show
that a given state of the evidence existed at that time.

**Why this priority**: it is what converts "we can detect tampering" into "we
cannot tamper undetected", which is the whole of G3. It is second because it
carries a recurring cost, an external dependency and a public commitment —
all founder decisions (Q14).

**Independent Test**: publish a checkpoint, then verify from a machine with
no access to our database that the published root matches a chain state and
carries a time attestation.

**Acceptance Scenarios**:

1. **Given** a period's evidence, **When** the checkpoint job runs, **Then**
   it publishes a signed root and records locally what it published, and the
   two agree.
2. **Given** a published checkpoint, **When** the operator alters evidence
   from before it, **Then** the recomputed root no longer matches the
   published one, and the mismatch is detectable by anyone holding the
   published value.
3. **Given** the publishing destination is unreachable, **When** the job
   runs, **Then** it fails loudly and retries; it never advances its local
   record of what was published, because a checkpoint believed-published and
   not published is worse than none.
4. **Given** a published checkpoint, **When** anybody reads it, **Then** it
   reveals no member, no merchant, no amount and no transaction — a root and
   a time, and nothing that can be tested against a guess.

---

### User Story 3 — A member holds a receipt they can verify (Priority: P3)

For any earning, a member can download a receipt containing what was clicked,
the rate and share that applied at click time, what the network reported, the
digests of both, and a Merkle inclusion proof to a published checkpoint. A
short, published verification script confirms the receipt without any access
to our systems.

**Why this priority**: this is the triple-entry receipt itself, and the answer
to the founder's question. It is third because it needs both of the above,
and because whether it is member-facing in the alpha is a founder decision
(Q15).

**Independent Test**: a member downloads a receipt, an outside party runs the
published verifier against the published checkpoint alone, and it confirms —
then fails when a single figure in the receipt is edited.

**Acceptance Scenarios**:

1. **Given** a confirmed earning, **When** the member requests its receipt,
   **Then** they get the click's snapshot, the report's facts, both digests,
   the inclusion proof and the checkpoint it proves against.
2. **Given** a receipt and only the published checkpoint, **When** the
   verifier runs, **Then** it confirms without contacting us.
3. **Given** a receipt with any figure altered, **When** the verifier runs,
   **Then** it refuses and names the field that does not reproduce.
4. **Given** an earning whose evidence predates the first checkpoint,
   **When** the member requests a receipt, **Then** they are told plainly
   that it cannot yet be proved and from which date receipts are provable —
   never given a receipt that quietly proves nothing.

---

### Edge Cases

- **A chain with a hole from before this feature.** Existing rows have no
  links. Chains start at a named migration boundary, and everything before it
  is honestly outside the chain rather than retro-linked — retro-linking
  would be the operator asserting a history rather than proving one.
- **Concurrent inserts into one chain.** A chain is a sequence, so two
  concurrent inserts must be ordered by the database. This is a serialisation
  point and therefore a contention point; the scope of a chain (per publisher
  account? per network? one global?) is a throughput decision with a research
  question attached.
- **A network re-reporting an old transaction.** Supersession already handles
  it: a new row, chained at the point it arrived, superseding the old one.
  The chain is ordered by arrival, not by transaction date, and must say so.
- **A checkpoint published for a chain that was already broken.** Publishing
  must verify before it publishes, or it certifies a lie.
- **A member requesting a receipt for a reversed earning.** The reversal is
  part of the evidence and belongs in the receipt; a receipt that showed only
  the credit would be a receipt for something that is not true any more.
- **Retention.** Q8 (click-log retention) is undecided. A deletion required
  by retention or by a member's erasure request breaks a chain by
  construction. The chain must be able to record a *tombstone* — the row's
  digest survives, its contents do not — or the two policies contradict each
  other. This is the sharpest interaction in the feature and it is called out
  again in Assumptions.

## Requirements

### Functional Requirements

- **FR-201**: Every evidence row MUST carry a link to the previous row in its
  chain, and the link MUST be computed by the database from the row's own
  contents and its predecessor's link. A caller-supplied link MUST be
  discarded, exactly as `content_digest` already is.
- **FR-202**: `cashback.click` MUST carry a content digest over the facts
  that decide what is owed — offer, member, rate snapshot, member share
  snapshot, click reference and click time — so the member's half of the
  evidence is inside the chain (G5).
- **FR-203**: The chain MUST be verifiable end to end by a check that
  recomputes every content digest from the row and every link from its
  predecessor, and that reports the first break rather than a boolean.
- **FR-204**: The verification MUST run continuously in a deployed
  environment, and a break MUST be treated as an incident, exactly as the
  zero-sum check's failure is.
- **FR-205**: Chains MUST begin at a named boundary. Rows written before it
  MUST be reported as unchained rather than retro-linked.
- **FR-206**: A checkpoint MUST publish a Merkle root and a chain tip, and
  MUST NOT publish anything from which a member, a merchant, an amount or a
  transaction can be recovered or confirmed against a guess.
- **FR-207**: The checkpoint job MUST verify the chain before publishing, and
  MUST refuse to publish over a broken one.
- **FR-208**: The checkpoint job MUST NOT record a checkpoint as published
  unless the destination confirmed it.
- **FR-209**: A receipt MUST contain everything needed to verify it apart
  from the published checkpoint, and MUST be verifiable with no access to our
  systems.
- **FR-210**: A receipt for evidence that cannot yet be proved MUST say so
  and MUST NOT be issued as if it could.
- **FR-211**: The chain MUST support a tombstone, so that a row removed under
  a retention or erasure obligation leaves its digest and loses its contents,
  and the chain still verifies. [NEEDS CLARIFICATION: gated on Q8]
- **FR-212**: None of this MUST change how money moves. Balances stay the sum
  of double-entry postings (C-1) and credits stay evidence-backed (C-2); this
  feature adds proof about evidence, never a second source of truth about
  money.

### Key Entities

- **Evidence chain**: an ordered sequence of evidence rows within a scope,
  where each row carries its predecessor's link. Its scope is a research
  question (contention vs. number of chains to verify and checkpoint).
- **Checkpoint**: a published, signed, time-attested statement that a chain
  had a given tip and that a period's rows had a given Merkle root.
- **Receipt**: what a member holds — the click snapshot, the network report,
  both digests, an inclusion proof, and the checkpoint it proves against.
  This is the third entry, in Grigg's sense.

## Success Criteria

- **SC-201**: Altering, deleting or reordering any evidence row written after
  the chain boundary is detected by the continuous check, and the check names
  the first affected row.
- **SC-202**: The continuous verification of a full production-sized chain
  completes inside its scheduled window with margin, and its cost is
  reported.
- **SC-203**: Chaining adds no more than a stated, measured latency to a
  click-out. The click-out is on the member's critical path; a proof system
  that makes clicking slower has taken from the member to give to the
  auditor.
- **SC-204**: An outside party, given only a receipt and the published
  checkpoint, reaches the same verdict as we do — on a good receipt and on a
  tampered one.
- **SC-205**: No published artefact permits confirming a guessed member,
  merchant, amount or transaction.

## Assumptions

- The double-entry ledger is unchanged and stays the only source of truth for
  balances (C-1, ADR-0002). This feature is about evidence, not balances.
- No token, no consensus protocol, no distributed ledger, and no on-chain
  money. Where a public chain appears at all it is as a timestamping
  destination for a hash, considered in `research.md` beside cheaper options.
- Existing evidence rows are not rewritten. Chains start at a boundary.
- The receipt format is ours and published; it is not a standard we adopt.
- Verification is a job, on the schedule the zero-sum check already
  establishes as the pattern for a continuously-verified invariant.

## Governance Impact

### What this feature does NOT decide

Nothing here resolves Q1–Q13. It does not change the revenue share (Q4), the
payout rails (Q5) or the regulatory posture on balances (Q2, decided). It
must not: a proof about evidence never changes what a member is owed.

### Three new founder questions

Recorded, not answered. Per the constitution's Governance section, this spec
records them and the safe default that applies until they are answered.

> **Q14 — Where checkpoints are published, at what cadence, and whether it is
> a public commitment.** This carries a recurring cost, an external
> dependency, and a promise that is easy to make and hard to withdraw: a
> product that has published checkpoints for a year and stops has said
> something. `research.md` compares the destinations without choosing one.
> **Default until answered**: publish nowhere; build the chain (US1) and the
> verification, which need no destination, and hold US2.

> **Q15 — Whether receipts are member-facing in the alpha.** A receipt is a
> marketing asset and a support-load change as much as a technical one, and
> it invites a question the support process must be ready for ("my receipt
> does not verify"). **Default until answered**: an internal capability, with
> no member-facing surface.

> **Q16 — What a tombstone may remove.** FR-211 sits directly on Q8
> (click-log retention) and on any erasure obligation. Whether a member's
> right to have data removed leaves a verifiable digest behind, and whether
> that digest is itself personal data, is a decision with legal advice
> attached. **Default until answered**: no deletion, which is the existing Q8
> default, and therefore no tombstone; the contradiction is documented rather
> than resolved in code.

### An amendment this would eventually need

If US1 ships, C-3 ("Immutable network evidence") understates what the schema
then guarantees: not only that evidence rejects edits, but that the sequence
of it is verifiable. That is a **MINOR** amendment with a Sync Impact Report,
and it is the founder's act — this spec prepares it and does not perform it.

## Deliberately not in this feature

- **A blockchain.** No consensus, no token, no smart contract, no
  distributed ledger. The property wanted is external witness of a hash;
  everything else a chain brings is cost.
- **Signing every row.** Per-row signatures with the operator's own key prove
  the operator wrote it, which nobody disputes. The chain plus an external
  checkpoint is what proves the operator did not *rewrite* it.
- **Retro-linking existing rows.** See FR-205.
- **Any change to how a credit is computed.** C-2 stands as written.

## Sequencing

1. **Blocked on**: the local end-to-end demo
   ([#533](https://github.com/Nomos-N4s/apivo-news/pull/533)) — founder's
   instruction, 2026-09-06.
2. **US1** needs no founder answer and no external dependency. It can be
   planned and built as soon as the demo lands.
3. **US2** is gated on Q14. **US3** is gated on Q14 and Q15.
4. `plan.md` and `tasks.md` are deliberately absent until Q14–Q16 are
   answered for the stories they gate. US1's plan may be written first,
   scoped to US1 alone.
