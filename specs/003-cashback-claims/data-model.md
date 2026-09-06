# Data Model: Cashback Claims

All tables live in the `cashback` schema. No foreign key crosses a product
schema boundary: the member is `account_id uuid NOT NULL`, referenced the same
way every other cashback table references it, with no FK to `public.account`.

All money is `amount_minor bigint NOT NULL` plus `currency char(3) NOT NULL`
(C-6). No decimal type appears anywhere below.

## 1. `cashback.claim`

The member's assertion.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PK | |
| `brand_id` | `uuid NOT NULL` | tenant boundary, per Rebrandability |
| `account_id` | `uuid NOT NULL` | the claiming member |
| `reference` | `text NOT NULL UNIQUE` | member-visible, generated at insert (FR-102) |
| `merchant_id` | `uuid NOT NULL` | `REFERENCES cashback.merchant` |
| `purchased_at` | `date NOT NULL` | the member's own statement of the date |
| `amount_minor` | `bigint NOT NULL CHECK (amount_minor > 0)` | the basket |
| `currency` | `char(3) NOT NULL` | |
| `order_reference` | `text` | nullable — not every shop gives one |
| `state` | `cashback.claim_state NOT NULL` | see below |
| `due_at` | `timestamptz NOT NULL` | the five-working-day promise (FR-102) |
| `submitted_at` | `timestamptz NOT NULL DEFAULT now()` | |
| `duplicate_of_id` | `uuid` | `REFERENCES cashback.claim` — the same purchase, claimed twice |

`cashback.claim_state`: `submitted`, `in_review`, `awaiting_member`, `decided`,
`closed`.

Indexes: `(state, submitted_at)` for the oldest-first queue;
`(account_id, submitted_at DESC)` for the member's own list;
`(merchant_id, purchased_at)` for the classification lookup.

**The claim itself is mutable** — its state moves, and a duplicate link is
discovered later. What must not move is the decision, which is why the decision
is a separate table rather than columns here.

## 2. `cashback.claim_evidence`

A file the member attached. Immutable: rejects UPDATE, DELETE and TRUNCATE by
trigger, for the same reason `source_item` does — it is evidence of what was
submitted, and the submission is the fact.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PK | |
| `claim_id` | `uuid NOT NULL REFERENCES cashback.claim` | |
| `storage_key` | `text NOT NULL` | opaque to the database; the `EvidenceStore` owns its meaning |
| `content_hash` | `text NOT NULL` | sha-256 of the bytes as received |
| `media_type` | `text NOT NULL` | as sniffed, never as declared by the client |
| `byte_size` | `bigint NOT NULL CHECK (byte_size > 0)` | |
| `uploaded_at` | `timestamptz NOT NULL DEFAULT now()` | |

Unique on `(claim_id, content_hash)`: the same file attached twice is one
piece of evidence.

## 3. `cashback.claim_decision`

Append-only. **C-8** and **C-9** live here.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PK | |
| `claim_id` | `uuid NOT NULL REFERENCES cashback.claim` | |
| `outcome` | `cashback.claim_outcome NOT NULL` | |
| `reason` | `text NOT NULL CHECK (btrim(reason) <> '')` | the member reads this |
| `decided_by` | `text NOT NULL CHECK (btrim(decided_by) <> '')` | **C-8** — the named human |
| `decided_at` | `timestamptz NOT NULL DEFAULT now()` | |
| `supersedes_id` | `uuid UNIQUE REFERENCES cashback.claim_decision` | FR-108 |
| `classification` | `cashback.claim_classification NOT NULL` | as computed at decision time |
| `records_snapshot` | `jsonb NOT NULL` | the three record rows as they read at decision time |
| `entry_id` | `uuid REFERENCES cashback.entry` | where the outcome attributed or adjusted |
| `goodwill_transfer_ref` | `text` | where the outcome paid goodwill |

`cashback.claim_outcome`: `attributed`, `adjusted`, `goodwill`, `declined`,
`escalated_to_network`.

`cashback.claim_classification`: `attributable`, `adjustable`, `unevidenced`.

Trigger `raise_immutable()` on UPDATE, DELETE, TRUNCATE — **C-9**.

`supersedes_id` is `UNIQUE`, so a decision can be superseded once. A chain is
allowed; a fork is not, because two live successors to one decision is a
question with no answer for the member reading their record.

### The C-10 constraint

A check constraint ties the outcome to the row it may cite:

```sql
CHECK (
  (outcome = 'attributed'  AND entry_id IS NOT NULL AND goodwill_transfer_ref IS NULL)
  OR (outcome = 'adjusted' AND entry_id IS NOT NULL AND goodwill_transfer_ref IS NULL)
  OR (outcome = 'goodwill' AND entry_id IS NULL     AND goodwill_transfer_ref IS NOT NULL)
  OR (outcome IN ('declined','escalated_to_network')
      AND entry_id IS NULL AND goodwill_transfer_ref IS NULL)
)
```

The half of C-10 this does not carry — that an attributed entry cites a real
network transaction — is already carried by `entry_evidence_guard` on
`cashback.entry` (C-2, migration 0013). The claim path creates entries through
the same write path as the unattributed queue, so it inherits that guard rather
than restating it. A claim cannot reach around it, and the invariant test
asserts the reach-around fails.

## 4. `cashback.claim_correspondence`

Recorded messages, append-only, the mockup's "Ask the reader".

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PK | |
| `claim_id` | `uuid NOT NULL REFERENCES cashback.claim` | |
| `direction` | `cashback.correspondence_direction NOT NULL` | `to_member` \| `from_member` |
| `body` | `text NOT NULL CHECK (btrim(body) <> '')` | |
| `author` | `text NOT NULL` | the named operator, or the member's account id |
| `sent_at` | `timestamptz NOT NULL DEFAULT now()` | |

Immutable by trigger. Correspondence is not a decision and never resolves a
claim; it only ever moves the state to `awaiting_member` and back.

## 5. View: `cashback.claim_provenance`

One query answering SC-102 — the analogue of `article_provenance` and
`cashback.provenance`:

claim, member, merchant, amount, evidence hashes, every decision in order with
its reason and named decider, the classification and records snapshot each was
taken on, and the entry or goodwill transfer it produced.

Any schema change that breaks this view fails the invariant suite.

## 6. Events

Emitted through the transactional outbox into `public.domain_event` (FR-109):

| Event | When |
|---|---|
| `cashback.claim.submitted` | on insert |
| `cashback.claim.decided` | on each decision, including a superseding one |
| `cashback.claim.correspondence_sent` | on each message |

Producer `cashback`, subject the claim reference, idempotency key derived from
the row id and the event kind, per `002/contracts/events.md`.
