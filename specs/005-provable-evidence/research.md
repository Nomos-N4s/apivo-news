# Research: Evidence a Member Can Check

**Feature**: `specs/005-provable-evidence/`
**Created**: 2026-09-06
**Status**: open questions, not decisions

This file exists to make Q14 answerable without the founder having to do the
comparison, and to record the things that must be **measured** rather than
reasoned about. Nothing here chooses. Where a choice is implied, it is marked
as a recommendation and it is still the founder's or the plan's to take.

---

## R1 — What is one chain?

A chain is a sequence, so every insert into one is a serialisation point. The
scope of a chain therefore trades contention against the number of chains to
verify, checkpoint and prove against.

| Scope | Serialisation | Chains to verify | Notes |
|---|---|---|---|
| One global chain | every click and every network report contends on one tip | 1 | simplest to checkpoint and to prove against; the click path is on the member's critical path, so this is the option most likely to fail SC-203 |
| Per network publisher account | contention only among that account's writes | one per connected account — small, and it is the unit `network_account` already is | the poller writes reports per account in a job that is already serial; clicks are not per account, so clicks need their own scope |
| Per member | almost no contention | as many as there are members | cheap to write, expensive to verify and to checkpoint; a member's own chain is also exactly what an operator could rewrite wholesale, since nobody else has any of it |
| One chain per table | two chains: clicks, reports | 2 | clicks still contend globally, which is the SC-203 risk again |

**What must be measured, not argued**: the click-out latency added by taking
the tip under contention, at the concurrency the product actually sees. The
click-out is already the one member-facing write on the critical path. A
proof system that makes clicking slower has taken from the member to give to
the auditor, which is why SC-203 exists.

**Recommendation to test first**: per `network_account` for reports (it
matches how the poller already works), and a small fixed number of parallel
chains for clicks, each row assigned to one by a hash of its click reference.
Parallel chains keep contention bounded and verification cheap, at the cost
of a checkpoint covering *n* tips rather than one. Whether *n* parallel chains
weaken anything is the sub-question: they do not, so long as each row's chain
assignment is itself derived from the row's own immutable contents, so a row
cannot be moved between chains to hide it.

## R2 — How the link is computed, in the database

`content_digest` is already computed by a `BEFORE INSERT` trigger and a
caller-supplied value is discarded (migration 0012). FR-201 says the link is
computed the same way. The open part is how the trigger obtains the
predecessor's link without two concurrent inserts both reading the same tip.

Three mechanisms, all standard:

1. **A tip row per chain, taken `FOR UPDATE`.** The trigger locks the chain's
   tip row, reads it, writes the new link, updates the tip. Correct, simple,
   and the lock is held for the rest of the transaction — which for a
   click-out is short. The tip table is mutable by construction, which needs
   saying out loud: it is a cursor, not evidence, and losing it must be
   recoverable by walking the chain.
2. **A transactional advisory lock per chain.** `pg_advisory_xact_lock` on
   the chain id, then read the current tip by query. No extra table; the tip
   is derived. Slightly more expensive to read the tip (an index lookup on
   the chain's last sequence number) and it interacts with the advisory locks
   the scheduler already uses — the locker's capacity arithmetic in
   `cmd/apivo/main.go` is a live constraint, not a footnote.
3. **A per-chain sequence number with a unique constraint**, and a retry on
   conflict. Optimistic; the retry is in the application, which is where this
   repository has been reluctant to put invariants (Principle VIII).

**Open**: whether the chain-tip read can be folded into the existing insert
without adding a round trip. The measurement in R1 answers this at the same
time.

## R3 — Merkle mechanics

The published artefact is a root, and a receipt carries an inclusion proof to
it. Three details that are easy to get wrong and expensive to change after
the first checkpoint is published:

- **Domain separation between leaves and internal nodes.** Hashing a leaf and
  an internal node the same way permits second-preimage attacks, where an
  internal node is presented as a leaf. RFC 6962 (Certificate Transparency)
  prefixes leaves with `0x00` and internal nodes with `0x01` for exactly this
  reason. Adopt the same discipline; the cost is one byte.
- **What a leaf is.** Recommendation: the row's chain link, not its content
  digest — the link already commits to the content digest *and* to
  everything before it, so a proof about a leaf is a proof about a position
  in a history rather than about an isolated fact.
- **Odd-node handling.** Promoting an odd node unchanged (rather than
  duplicating it) avoids the duplicate-leaf ambiguity Bitcoin's tree has
  (CVE-2012-2459). Fix the rule once, publish it with the verifier, and never
  change it.

**Open**: whether the tree is per period (simple, and a receipt proves
against exactly one checkpoint) or an append-only log with consistency proofs
between checkpoints (stronger — it proves the log was never forked, not just
that a row was in it). The second is what Certificate Transparency does and
it is meaningfully better; it is also meaningfully more to build and to
verify. Sized against Q15: if receipts are internal only, per-period trees
are enough to start.

## R4 — Where a checkpoint goes (this is Q14)

The property wanted is narrow: a hash, witnessed at a known time, by
something that cannot quietly rewrite what it witnessed, and checkable by a
third party who has no account with us and none with the witness.

| Destination | What it witnesses | Third-party checkable | Recurring cost | Dependency risk |
|---|---|---|---|---|
| **RFC 3161 timestamp authority** | a hash existed at a time, signed by the TSA | yes, with the TSA's certificate — a standard format, many implementations | typically per-request or a subscription; some free TSAs exist with no service guarantee | trusts one authority; if it disappears, old tokens remain verifiable only while its certificate chain does |
| **OpenTimestamps → Bitcoin** | a hash existed before a block, witnessed by a public chain nobody operates | yes, from a Bitcoin node or a block explorer | free to submit; aggregation means one on-chain transaction serves many | depends on the public calendar servers for aggregation, though a proof once complete is self-contained |
| **Sigstore Rekor (transparency log)** | an append-only public log entry, with inclusion and consistency proofs | yes, and the log's own consistency is checkable | free, public instance; a private instance is infrastructure to run | a public entry is public forever, which makes FR-206 load-bearing |
| **A signed tag in a public git repository** | that we said it, signed, at a time we recorded | partly — git's timestamps are ours, so this witnesses authorship, not time | free; already have signing set up | weakest: it proves we published a value, not that we could not have republished a different one before anybody looked |
| **A public feed we host** | nothing an adversary could not rewrite | no | free | not a witness at all; listed to be ruled out |

**The honest ranking on the property that matters** — can the operator
rewrite it without anyone noticing — is: OpenTimestamps ≈ Rekor > RFC 3161 >
signed git tag > our own feed. The honest ranking on operational simplicity
is roughly the reverse.

**Recommendation to put to the founder**: OpenTimestamps, because it is free,
aggregated, needs no account, produces a self-contained proof, and its
witness is the one nobody can lean on. Its costs are latency (a proof
completes when a block confirms, so a checkpoint is provable hours after it
is made, not immediately) and the calendar-server dependency at submission
time. Both are acceptable for a daily checkpoint; neither would be for a
per-transaction one.

**Not recommended**: anything that publishes per-row digests. See R5.

## R5 — Why only roots may be published

A digest of low-entropy inputs is a lookup table, not a secret. The inputs
here are guessable: an amount in minor units, a currency, a status from a
four-value vocabulary, a timestamp to the microsecond, a merchant. An
adversary who suspects a member bought a specific thing at a specific shop
for a specific price can enumerate the remaining unknowns and confirm the
guess against a published digest.

So: publish Merkle roots, and nothing else. A root over many leaves is not
enumerable, because the leaves' order and count are not known to the guesser
and each leaf is itself a chain link committing to everything before it.

This is why FR-206 is written as "reveals nothing that can be tested against
a guess" rather than "contains no personal data". A digest contains no
personal data in the sense of a field; it can still confirm one.

**Open**: the inclusion proof in a member's receipt necessarily reveals the
sibling digests on its path. Those siblings are other members' rows. This is
the same exposure Certificate Transparency has and accepts, and it is
acceptable for the same reason — a sibling digest confirms nothing without a
guess of its preimage, and the preimage is not derivable from the path. It
still needs stating in the privacy review rather than discovered in it.

## R6 — What the click digest must cover (FR-202)

`network_transaction.content_digest` covers the reported facts and
deliberately not `raw_payload`, because networks put their own response
timestamps and pagination metadata in a payload and every re-poll would look
like a change. The click digest needs the same care in the other direction:
it must cover everything that decides what is owed, and nothing that varies
without meaning.

Candidate fields, from `cashback.click`: the click reference, the account,
the offer, `rate_snapshot`, `member_share_bps_snapshot`, and `clicked_at`.

**Deliberately excluded, and each needs confirming**: `context_digest` (a
privacy-minimised abuse signal, not a term of the deal), and anything an
abuse rule may later add. If a field does not decide what a member is owed,
putting it in the digest makes an unrelated change look like tampering.

## R7 — What must be measured

Not researched — measured, before US1's plan is written.

1. Click-out p50 and p99 with and without chaining, at realistic concurrency,
   for each candidate scope in R1. This decides R1 and it is the SC-203 gate.
2. Full-chain verification wall time at a projected year-one row count, and
   its effect on the connection pool the scheduler's capacity arithmetic
   already constrains.
3. Checkpoint size and proof size at the same row count, which decides
   whether a receipt is a small JSON document or an attachment.

## Prior art worth reading before the plan

- Grigg, *Triple Entry Accounting* (2005) — the receipt, not the third
  column. The whole of US3 is this paper.
- RFC 6962, *Certificate Transparency* — leaf/node domain separation,
  inclusion proofs and consistency proofs. The closest existing system to
  what US2 and US3 describe, and one that has been attacked in public for a
  decade.
- CVE-2012-2459 — the duplicate-leaf ambiguity in Bitcoin's Merkle tree, and
  why the odd-node rule in R3 is not a detail.
- RFC 3161 — the timestamp token format, if Q14 goes that way.
