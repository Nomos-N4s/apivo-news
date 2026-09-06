# HTTP API Contract: Cashback Claims

Mounted under the same cashback route tree as 002, behind `CASHBACK_ENABLED`
and the same authentication. Money is always `{ minor, currency }`; no decimal
crosses this boundary (C-6). Errors are RFC 9457 problem documents, with
figures as extension members and never spelled into `detail`.

## Member endpoints

### POST /claims

Body:

```json
{
  "merchant_id": "…",
  "purchased_at": "2026-08-21",
  "amount": { "minor": 8430, "currency": "EUR" },
  "order_reference": "AG-88412"
}
```

- `201`: `{ "reference": "CL-2026-09-03-0117", "state": "submitted", "due_at": "2026-09-10T00:00:00Z" }`
- The reference and the due date are in the response because the member is
  promised both before anyone has looked at the claim (FR-102). A client that
  has to re-read the claim to learn its reference has a window in which the
  member has submitted something they cannot name.
- `403` when participation is not active — there is nothing to credit.
- `422` `purchase_date_in_future`.
- `409` `duplicate_claim` with the existing `reference` as an extension member,
  when the same member has an open claim for the same merchant, date and
  amount. Recorded and linked rather than refused outright only where the first
  is already decided; an open duplicate is a mistake, and telling the member
  their own reference is more use than a second one.

### POST /claims/{reference}/evidence

`multipart/form-data`, one file.

- `201`: `{ "evidence_id": "…", "content_hash": "…", "media_type": "…", "byte_size": 0 }`
- The media type in the response is the **sniffed** type, not the declared one.
- `413` when the file exceeds the configured size.
- `415` when the sniffed type is not in the accepted set.
- `409` when the claim is already decided: evidence after the fact attaches to
  a superseding review, not to a closed record.

### GET /claims · GET /claims/{reference}

The member's own claims. The detail carries the assertion, the evidence list
(hashes and types, never the bytes), every decision in order with its outcome,
reason, decided-at and named decider, and the entry or goodwill reference it
produced.

**A declined decision is in this payload exactly as a paid one is** (FR-107).
There is no field a client can use to hide it.

### GET /claims/{reference}/evidence/{evidence_id}

The bytes, for the member who uploaded them. `403` for anyone else who is not
an operator.

## Operator endpoints (operator role required)

### GET /ops/claims

Query: `state`, `limit`, `cursor`. Default order is oldest submitted first —
the queue the mockup draws.

Item: `{ reference, member_display, merchant_name, purchased_at, amount,
expected_cashback, age_days, classification, state, due_at }`.

`expected_cashback` is what the dated rate table says the claim would be worth
if it resolved in the member's favour. It is a computed figure for triage, not
a promise, and the response labels it as such.

### GET /ops/claims/{reference}

The operator detail. Two halves, because the screen has two halves:

```json
{
  "claim": { "…": "the member's assertion, plus evidence metadata" },
  "records": {
    "classification": "attributable | adjustable | unevidenced",
    "click": { "…": "the tracking-log row, or null" },
    "network_transaction": { "…": "the feed row, or null" },
    "unattributed_transaction_id": "… or null",
    "entry": { "…": "an existing entry for this purchase, or null" },
    "rate_at_purchase": { "…": "the dated rate row that applied, or null" }
  },
  "remedies": ["attribute", "decline"],
  "correspondence": []
}
```

`remedies` is **served, not derived** (plan §"The shape of the decision"). The
client renders the buttons this array names and no others; the handlers refuse
anything outside it. A screen that worked out for itself which remedies applied
could offer one the handler would reject, and the member's answer would depend
on which of the two was right.

`records.click` and `records.network_transaction` are null where there is none,
and the classification says `unevidenced`. The UI states the absence; it does
not render an empty row that reads like a pending lookup.

### POST /ops/claims/{reference}/decide

Body: `{ "outcome", "reason", "goodwill_amount"? }`.

- `reason` is mandatory and non-blank for every outcome, including the ones
  that pay (FR-103). The member reads it either way.
- `outcome` must be in the `remedies` array the read returned; otherwise `409`
  `remedy_not_available` naming what is available.
- `goodwill_amount` is accepted only with `outcome: "goodwill"`, and only up to
  the classification's `expected_cashback` unless the deployment configures a
  higher ceiling.
- `201` with the decision, including `decided_by` — taken from the operator's
  own token, never from the body. An operator cannot decide under another
  name.
- `409` `already_decided` with the existing decision, unless `supersedes` is
  explicitly set. Superseding is deliberate (FR-108).
- `503` when the outcome is `goodwill` and no goodwill house account is
  configured. The route stays mounted and the other outcomes keep working: a
  deployment that cannot pay goodwill can still decline and attribute.

### POST /ops/claims/{reference}/message

Records correspondence and moves the claim to `awaiting_member`. Body
`{ "body" }`, non-blank. Never resolves a claim.

## Contract test obligations

One contract test per endpoint, asserting status codes, the problem-document
shape and the money shape. In addition:

- A test asserting `POST /ops/claims/{reference}/decide` **ignores** a
  `decided_by` in the body and uses the token's identity.
- A test asserting an outcome outside `remedies` is refused, for each
  classification.
- A test asserting the decline path's response carries the reason, and the
  member read carries it too.
- A test asserting no endpoint here can produce a `cashback.entry` without a
  network transaction (C-2/C-10), by attempting it directly against the store.
