# HTTP API Contract: Apivo Cashback Alpha

**Feature**: [../spec.md](../spec.md) | **Date**: 2026-08-24

Served by the same Go binary as the news API (ADR-0001), mounted under
`/api/v1/cashback` and behind the `cashback` feature flag. Every endpoint
gets a contract test asserting status codes, shape and auth behaviour
before its implementation lands.

## Topology and conventions

- The Go API stays **not publicly routable**; Astro is the only public HTTP
  surface, behind the single crawler gate. Cashback adds no second gate
  (research D12).
- Base path `/api/v1/cashback`. JSON, UTF-8. Errors are RFC 9457
  problem+json.
- Auth: `Authorization: Bearer <Supabase JWT>` on **every** endpoint below —
  there is no anonymous cashback surface (FR-023).
- **The operator role.** Migration 0002 constrained `account.role` to
  `check (role in ('reader', 'editor'))`, so the authority these `/ops/*`
  endpoints need was unrepresentable. **Migration `0019_operator_role`**
  (landed) extends that constraint to `('reader', 'editor', 'operator')`
  and adds the database enforcement:
  - a `BEFORE INSERT` trigger on `cashback.payout` requiring
    `approved_by` to hold the `operator` role — the direct analogue of
    `article_insert_guard` for editors, and it reads the role
    **`FOR SHARE`**, exactly as 0002 does, so a concurrent demotion cannot
    slip past it (the PR #48 lesson);
  - an extension of `account_role_guard` so an operator who has approved a
    payout cannot be silently demoted out of that authority.
  The HTTP role check mirrors the database rule; it never replaces it. It
  is `identity.RequireOperator`, deliberately a second function beside
  `RequireEditor` rather than one check parameterised by a role: an editor
  is not an operator with fewer permissions, and a helper taking the
  required role as an argument makes "operator or editor" as easy to write
  as either one alone.
- Pagination: `limit` (default 20, max 100) + opaque `cursor`; lists return
  `{ "items": [...], "next_cursor": string|null }`.
- **Money is always** `{ "minor": <integer>, "currency": "EUR" }`. No
  endpoint ever emits a decimal string or a float (C-6). Formatting for
  display happens in the frontend from these two fields.
- Every mutating endpoint accepts `Idempotency-Key`; replaying a key returns
  the original result rather than acting twice.

## Member endpoints

### GET /participation

- 200: `{ status, opted_in_at, terms_version, default_currency }`.
- 404 when the account has never opted in — the frontend renders the
  opt-in, not an error.

### POST /participation

Opt in (FR-002).

- Body: `{ terms_version }`.
- 201: the participation resource. 409 if already active.
- 400 when `terms_version` is not the current one — a stale consent is never
  recorded.

### DELETE /participation

Leave cashback (FR-003). 200 with `{ status: "left", left_at }`. Financial
records are untouched; pending entries continue to resolve.

### GET /catalogue

- Query: `lang` (required), `place` (required, repeatable), `category`,
  `q`, `limit`, `cursor`. Language and place stay separate parameters
  (constitution VII).
- **`category` has nothing behind it.** No table in the schema records one,
  and inventing a taxonomy would settle four product questions (who owns
  the set, whether it hangs off the retailer or the band, how it localises,
  whether it filters or browses) in a member-facing URL. Tracked as issue
  #414; unbuilt until it is answered.
- 200 items:
  `{ merchant_id, slug, name, name_language, name_is_fallback, summary, rates: [...] }`,
  the rates in the same shape `GET /merchants/{slug}` publishes them.
  `name_is_fallback` is how US5 scenario 2 renders "shown in German" rather
  than silently pretending.
- 400 on unknown `lang` or `place`. Empty result is an empty list, never a
  500.

### GET /merchants/{slug}

- Query: `lang`, optional. A merchant with no copy in it falls back to the
  retailer's source language and says so in `name_is_fallback`; an absent
  or unrecognised `lang` does the same rather than failing, because a page
  a member has navigated to should render.
- 200: merchant detail with **every** published rate band, its conditions
  and its exclusions (US5 scenario 3), plus `typical_confirmation_days`.
- **Every rate is what the member earns**, never the network's commission.
  A band records the commission and, separately, the share of it the member
  receives; the two are composed before the response is built, rounding the
  way the credit itself rounds (the member's favour, Q4). Publishing the
  commission would promise roughly twice what arrives, and would publish
  the margin.
- Each rate is `{ offer_id, kind, bps | amount, conditions, exclusions,
  valid_to }` — basis points for a `percent` band, `{ minor, currency }`
  for a `fixed` one. Not a single `display_rate` field: a fixed rate is
  money, and money on this API is always `{ minor, currency }` (C-6).
- Bands come from the **preferred route only** — the one route the
  catalogue publishes, which is the route a click is issued through.
  Quoting another network's band would quote a rate no click can earn.
- An empty `rates` list is 200, not 404: a retailer whose bands have lapsed
  pays nothing today and is still a shop that exists.
- **`typical_confirmation_days` is always null today.** Nothing in the
  schema records it — not on the retailer, not on the route, not on the
  network. It is emitted as null rather than omitted, and never as a
  plausible constant: a member reads it as "you will be paid in about six
  weeks".
- 404 unknown or inactive — one answer for both, because which retailers
  we have stopped publishing is not a page request's business. A retailer
  we publish with no copy in any language is 500, not 404: a broken row and
  a missing one are different facts.

### POST /clickouts

Create the tracked click and get the redirect target (FR-020, FR-021).

- Body: `{ offer_id }`.
- 201: `{ click_ref, redirect_url, expires_at }`. The click row and its rate
  snapshot are committed **before** the response is written; if the deeplink
  cannot be built, nothing is committed and the response is 502 with a
  problem document the frontend renders plainly (US1 scenario 1).
- 401 when unauthenticated — an anonymous click is never created and can
  never be back-attributed (FR-023).
- 409 when the offer is expired or its merchant is inactive.
- 429 when the member or context exceeds the click rule (US7 scenario 1),
  with `Retry-After`.

### GET /wallet

- 200:
  ```json
  {
    "pending":   { "minor": 0, "currency": "EUR" },
    "confirmed": { "minor": 0, "currency": "EUR" },
    "reserved":  { "minor": 0, "currency": "EUR" },
    "paid_out":  { "minor": 0, "currency": "EUR" },
    "payout_threshold": { "minor": 0, "currency": "EUR" }
  }
  ```
- Each total is computed from ledger postings, never from a stored balance
  (D7). A contract test recomputes them independently and asserts equality
  to the minor unit (SC-006).

### GET /wallet/entries

- Query: `state`, `limit`, `cursor`.
- 200 items:
  `{ entry_id, merchant_name, transacted_at, sale_amount, cashback_amount, state, expected_confirmation_at, reversal_of_id?, reason? }`.
- Reversals appear as their own items alongside the original; neither is
  hidden (US3 scenario 2).

### GET /payout-destinations · POST /payout-destinations

- `POST` body: `{ kind, details }`. 201 with `verified_at: null`.
- **Verification is a separate flow**; an unverified destination is
  rejected by `POST /withdrawals` with 409, never silently used (FR-051).

### POST /withdrawals

- Body: `{ destination_id, amount: { minor, currency } }`.
- 201: `{ request_id, state: "awaiting_approval", reserved_amount }`. The
  reservation happens in the same transaction (D9), so a concurrent second
  request sees reduced confirmed balance.
- 409 `insufficient_confirmed_balance`, with `shortfall` in the problem
  document (US4 scenario 1).
- 409 `destination_not_verified`.
- 403 when the destination does not belong to the caller (US4 scenario 6).

**`reserved_amount` is not an echo of `amount`, and this is why it is its own
field.** Cashback is reserved in WHOLE ENTRIES, oldest first: an entry cites
the network report that evidences it (C-2), so there is no half of one to
take. A member with entries of 10.00 and 20.00 who asks for 15.00 has 30.00
reserved, and 30.00 is what a payout will pay. The alternatives were both
worse - refusing an amount no prefix of entries adds up to, or reserving
less than was asked for - and neither can be explained to a member.

Migration 0016 is what forces the reservation to be ONE ledger transfer
rather than one per entry: it answers C-7 by joining every reserving
transition against the request's single `reserved_transfer_ref`.

**The two 409 balance refusals share one code.** Below the threshold
(FR-050) and beyond the confirmed balance are different walls, but a client
acts on both the same way - make up the shortfall - so both are
`insufficient_confirmed_balance` and the `detail` says which. Both carry the
figures as extension members (RFC 9457 §3.2): `shortfall` always, plus
`threshold`/`confirmed` or `confirmed`/`requested`. No amount is spelled
into `detail`, because minor units rendered as prose ("2500 EUR") read as a
price to a member; the client formats the figures for their locale.

**503 when the deployment cannot pay out yet** - no threshold, or no house
account earnings are credited from. The route is still mounted: a 404 would
tell a client the API is not there, and refusing to start would take the
wallet and the operator queue down over a member-facing feature.

### GET /withdrawals · GET /withdrawals/{id}

State, decision reason where rejected, and payout reference where settled.

### GET /export

Member's own cashback history as JSON or CSV (FR-003).

## Operator endpoints (operator role required)

| Endpoint | Purpose |
|---|---|
| `GET /ops/unattributed` | transactions with no matching click (FR-034, FR-060) |
| `POST /ops/unattributed/{id}/attribute` | attach to an account with a reason; creates an entry with `click_id = null` |
| `POST /ops/unattributed/{id}/dismiss` | close with a reason |

Two notes on the unattributed queue, both consequences of rules stated
elsewhere rather than new decisions.

**`/attribute` is not lawful for every row.** `entry_evidence_guard`
(migration 0013) requires an entry to cite the click the network named, so
a null `click_id` is legal only where the network named no reference at
all. A report whose reference matched no click can therefore only be
dismissed, and the listing says which kind each row is in an `attributable`
field so an operator interface never offers an action the database will
refuse.

**A row stops being work without anybody closing it.** "Still open" is
derived, not stored: nobody has resolved it, the report it names is still
what the network last said about the transaction, and nothing has been
credited against it. A stale page acting on a row that has since moved gets
409 with which of the three it is; an id that names no row gets 404,
because a queue row is never deleted and so an unknown id was never issued.
| `GET /ops/held` | entries in `held` with the rule that held them and what it said; see below |
| `POST /ops/held/{id}/release` · `/reject` | body `{ reason }`, required and non-blank (US7 scenario 3); see below |
| `GET /ops/withdrawals?state=awaiting_approval` | approval queue |
| `POST /ops/withdrawals/{id}/approve` | **records `approved_by` from the token subject** (C-4); submits the payout with the request-derived idempotency key (C-5, D8) |
| `POST /ops/withdrawals/{id}/reject` | body `{ reason }`; releases the reservation back to confirmed |
| `POST /ops/reconciliation/runs` | import a network statement and derive its differences; see below |
| `GET /ops/reconciliation/runs/{id}/differences` | every difference with its amount delta (US6 scenario 1); see below |
| `POST /ops/reconciliation/differences/{id}/resolve` | body `{ resolution, reason }`; see below |
| `GET /ops/provenance/payouts/{id}` | the C-7 chain in one response |
| `GET /ops/exports/ledger` · `/exports/reconciliation` | accounting exports (FR-062); see below |

Every operator action appends to `domain_event` with the acting account and
the reason (FR-061). An action with a blank reason is rejected with 400 —
the audit record is part of the action, not an afterthought.

### GET /ops/held

Every credit a hold rule kept out of a member's pending balance and nobody
has yet decided (US7), oldest first, paged like every operator queue. Each
row carries the credit (`id`, `account_id`, `brand_id`,
`network_transaction_id`, `click_id`, `amount`, `held_since`), the rule
holding it and what the rule said (`hold_rule`, `hold_reason`), and what the
network reported (`network_id`, `external_id`, `report_status`, `sale`,
`commission`, `transacted_at`) — so an operator decides from one screen.

"Still to decide" is derived, not stored: the row is `held`, and nothing
undoes it. A rejected credit's own row keeps the state it was rejected in,
because a rejection is a reversing entry beside it, never an edit.

### POST /ops/held/{id}/release · /reject

Body `{ reason }`, required and non-blank; the acting account is the token
subject (C-4). Both record who and why on the transition they write and
publish their event in the same transaction (FR-061).

**Release** moves the credit to `pending` under the ordinary lifecycle. It
confirms nothing: an operator can assert that the credit is ordinary, not
that the network paid, so it still waits on the network's word and a
reconciled statement (FR-043). The response is the recorded release —
`state`, the `hold_rule` it was held under, `released_by`, `reason`,
`ledger_transfer_ref`, `released_at`.

**Reject** undoes the credit the way a network reversal does: a reversing
entry beside it, born `reversed`, citing the credit's own report with the
operator as its cause, carrying the member's share back to the network
receivable (SC-010). The credit's own row is never edited, and a credit is
rejected once. The response names both — `id` and `reversal_entry_id` —
with `rejected_by`, `reason` and `rejected_at`.

400 for a blank reason; 404 for an id that was never issued; 409 for a credit
that is not held (the detail names its state), was already rejected, or moved
between the read and the decision; 503 where the deployment has not named
`HOUSE_ACCOUNT_NETWORK_RECEIVABLE` — the queue itself stays readable.

### POST /ops/reconciliation/runs

Body `{ network_account_id, period: { start, end }, statement }`. The
statement is the network's document and is stored verbatim and immutably
(C-3); the only shape this API reads from it is

```json
{"lines": [{"transaction_id": "<the network's own id>", "paid": {"minor": 250, "currency": "EUR"}}]}
```

`lines` is required — a statement that paid nothing says so with an empty
list — and every line names one transaction once and says what was paid in
minor units and a currency; a negative amount is a deduction. Everything
else in the document is kept and ignored. The document is read in full
before anything is written: a statement whose lines cannot be read is
refused with 400 naming the line, because the row it would land in cannot
be corrected afterwards.

**The same statement for the same account and period is one run.** A
second import answers 200 with the first run rather than 201 with a second
one, so a retry of an upload that timed out is safe. Detection runs on every
import, recording only what an earlier pass did not.

**Detection derives three kinds of difference**, laying the lines beside the
*current* report of every transaction the statement names plus every
current confirmed report for the period: `reported_not_paid` (a confirmed
report in the period no line paid), `amount_mismatch` (a line paying a
report a different figure than it is owed — the commission while the
network intends to pay, nothing once it declined or reversed), and
`paid_not_reported` (a line naming no report, or one in another currency).
A pending report is not yet expected on a statement; if the statement pays
it anyway, the amount must still match.

Importing moves no money and changes no entry. An open difference keeps its
transaction from confirming (FR-043); resolving it lifts that.

### GET /ops/reconciliation/runs/{id}/differences

One page of the run's differences, oldest first, paged with `limit` and
`cursor` like every operator queue. Each item carries `kind`, the report
named (`network_transaction_id`, null for money matching no report), the
network's own `transaction_id` either way, `expected` and `actual` as
`{ minor, currency }` where the kind has them, `delta` (paid less owed:
negative for a shorted or unpaid report, positive for an overpayment or
money nothing expected), `superseded` (the network has restated the report
since the difference was filed), and `resolution` — null while open,
otherwise `{ resolution, resolved_by, reason, resolved_at }`.

### POST /ops/reconciliation/differences/{id}/resolve

Body `{ resolution, reason }`. The reason is required and non-blank; the
acting account is the token subject. A row is decided once: a second
decision answers 409 naming the first.

**Two verdicts, and neither moves money.** `explained` — another fact
accounts for the disagreement and nothing is owed either way (a later
statement paid it, the network has since restated the report and the
reversal followed, two lines were one payment). `absorbed` — the delta is
the business's to bear or to keep, and the member's figure stands as
reported. Either lifts the difference from the confirmation gate.

**"The network owes us and we are chasing it" is deliberately not a
verdict.** An open difference *is* the chase, and it keeps the gate shut
until the money arrives or the network restates the report; resolving it
early would confirm a member's balance out of money never received, which
is the one thing US6 exists to prevent. Any member-facing effect of a
disagreement is a new posting resting on the network's own restated report
(C-3), never on an operator's reading of it.

### GET /ops/exports/ledger · GET /ops/exports/reconciliation

The two accounting journals (FR-062), whole for a window: `from` and `to`
are required RFC 3339 timestamps, the window is half-open `[from, to)` so
two adjacent exports share no row and miss none, and `format` is `json`
(default) or `csv`. Both are sent as attachments named for the journal and
the window's dates. An export is always the complete journal for its
window: it takes no filter, and a window holding more than 50,000 rows is
refused with 413 rather than truncated, because a truncated journal is one
an accountant sums.

**The ledger journal** is every movement of a member's money as the
earnings module recorded it, in the order it occurred: `transition_id`,
`entry_id`, `account_id`, `brand_id`, `network_transaction_id`,
`from_state` (null for the opening transition), `to_state`, `amount`,
`ledger_transfer_ref` (the ledger's own reference, C-7), `reason`,
`actor_id` (null where a poll caused the move) and `occurred_at`. It is
read against the ledger provider's statement.

**The reconciliation journal** is every difference detection found, in the
order it was found, with the statement it came from: `run_id`,
`network_account_id`, `network_id`, `external_publisher_id`,
`statement_period`, then `kind`, the report named, the network's own
`transaction_id`, `expected`, `actual`, `delta`, `detected_at`, and the
`resolution` if an operator made one. It is read against the network's
statements.

The JSON document carries `exported_at`, the window and `row_count`, so a
truncated file is one a reader can detect. The CSV is the JSON flattened,
every amount as two columns - minor units and currency - because C-6 holds
in a spreadsheet exactly as it holds on the wire; text that this process
did not write (reasons, network ids, brand and publisher ids) is guarded
against being read as a formula.

## Contract test obligations

1. Every endpoint: unauthenticated → 401; wrong role → 403.
2. `POST /clickouts`: no click row exists after a deeplink failure.
3. `POST /withdrawals`: two concurrent requests for the full balance
   produce one success and one 409 (SC-004 companion).
4. `POST /ops/withdrawals/{id}/approve`: replay with the same
   `Idempotency-Key`, and concurrent approval, both yield exactly one
   `payout` row.
5. `GET /wallet`: totals equal independently computed ledger sums (SC-006).
6. Every money field in every response is `{minor, currency}` — asserted by
   a schema test over the OpenAPI document, so a decimal can never appear.
7. The OpenAPI document in `api/openapi.json` covers every route; a route
   without a documented operation fails the existing route-coverage test.
