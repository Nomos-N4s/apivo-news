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
  there is no anonymous cashback surface (FR-023). Operator endpoints
  additionally require the operator role, enforced again by the database on
  write.
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
- 200 items:
  `{ merchant_id, slug, name, name_language, name_is_fallback, summary, rates: [{ kind, display_rate, currency?, conditions, exclusions }] }`.
  `name_is_fallback` is how US5 scenario 2 renders "shown in German" rather
  than silently pretending.
- 400 on unknown `lang` or `place`. Empty result is an empty list, never a
  500.

### GET /merchants/{slug}

- 200: merchant detail with **every** published rate band, its conditions
  and its exclusions (US5 scenario 3), plus `typical_confirmation_days`.
- 404 unknown or inactive.

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
| `GET /ops/held` | entries in `held` with the rule that held them |
| `POST /ops/held/{id}/release` · `/reject` | body `{ reason }`, required and non-blank (US7 scenario 3) |
| `GET /ops/withdrawals?state=awaiting_approval` | approval queue |
| `POST /ops/withdrawals/{id}/approve` | **records `approved_by` from the token subject** (C-4); submits the payout with the request-derived idempotency key (C-5, D8) |
| `POST /ops/withdrawals/{id}/reject` | body `{ reason }`; releases the reservation back to confirmed |
| `POST /ops/reconciliation/runs` | import a network statement; stores it verbatim and immutably |
| `GET /ops/reconciliation/runs/{id}/differences` | every difference with its amount delta (US6 scenario 1) |
| `POST /ops/reconciliation/differences/{id}/resolve` | body `{ resolution, reason }`; any member-facing effect is a new posting, never an edit |
| `GET /ops/provenance/payouts/{id}` | the C-7 chain in one response |
| `GET /ops/exports/ledger` · `/exports/reconciliation` | accounting exports (FR-062) |

Every operator action appends to `domain_event` with the acting account and
the reason (FR-061). An action with a blank reason is rejected with 400 —
the audit record is part of the action, not an afterthought.

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
