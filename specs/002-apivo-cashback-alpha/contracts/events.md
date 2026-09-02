# Event Contract: Cross-Product Integration

**Feature**: [../spec.md](../spec.md) | **Date**: 2026-08-24

Products communicate only through events (ADR-0001, FR-081). This document
is the contract between them: what is published, what may be consumed, and
what must never be assumed.

## Transport

Transactional outbox → `platform/events` dispatcher → append-only
`public.domain_event` → in-process subscribers. At-least-once delivery with
a delivery idempotency key. No broker in the alpha (research D10).

The outbox row is written **in the same transaction** as the state change
it describes. There is no code path that publishes an event without its
state change, or commits a state change without its event.

## Envelope

```json
{
  "event_id": "uuid",
  "type": "cashback.entry.confirmed",
  "version": 1,
  "occurred_at": "2026-08-24T10:00:00Z",
  "producer": "cashback",
  "subject": "uuid",
  "idempotency_key": "string",
  "payload": { }
}
```

- `type` is `<producer>.<entity>.<past-tense-verb>`, always past tense: an
  event is a fact that happened, never a request.
- `version` is bumped on any breaking payload change; both versions are
  published for one release cycle.
- Consumers **must** ignore unknown fields and unknown types.

### Where each envelope field lives

The existing `public.domain_event` table is `(id, type, payload,
occurred_at)` — it predates this contract and carries none of the routing
fields. **Migration `0018_domain_event_envelope` adds them as real
columns**, rather than burying them in `payload`, so the dispatcher can
index and deduplicate on them:

| Envelope field | Storage | Notes |
|---|---|---|
| `event_id` | existing `domain_event.id` | renamed only in the JSON projection; the column keeps its name |
| `type` | existing `domain_event.type` | unchanged |
| `occurred_at` | existing `domain_event.occurred_at` | unchanged |
| `payload` | existing `domain_event.payload` | carries **only** the event's own data — never envelope fields |
| `version` | **new** `version int not null default 1` | |
| `producer` | **new** `producer text not null default 'news'` | the default is what every pre-existing row was |
| `subject` | **new** `subject uuid` | nullable; ordering guarantees are per `(type, subject)` |
| `idempotency_key` | **new** `idempotency_key text` | nullable, with a partial unique index `where idempotency_key is not null` — this is what makes redelivery a no-op |

The migration is additive: `ADD COLUMN` with a constant default does not
rewrite the table and does not fire the row-level immutability triggers, so
`domain_event` stays append-only and existing rows stay untouched. The two
existing writers (`internal/ingestion/store.go` and
`internal/editorial/queries/approval.sql`) keep working unchanged — they
insert `(type, payload)` and take `producer='news'`, `version=1` by default.
Backfilling them with explicit values is **not** in scope; the defaults are
correct for every row they wrote.

Envelope fields never appear inside `payload`. A producer that writes one
there is a defect, and the dispatcher test asserts it.

## Published by `identity` (consumed by both products)

| Type | Payload | Notes |
|---|---|---|
| `identity.account.created` | `{ account_id, created_at }` | |
| `identity.account.deleted` | `{ account_id, deleted_at }` | cashback does **not** delete financial rows in response; it closes participation and flags any in-flight withdrawal for operator attention (spec edge case) |
| `identity.account.role_changed` | `{ account_id, roles }` | |

## Published by `cashback`

| Type | Payload |
|---|---|
| `cashback.participation.started` / `.ended` | `{ account_id, terms_version, at }` |
| `cashback.click.created` | `{ click_id, account_id, offer_id, at }` |
| `cashback.transaction.ingested` | `{ network_transaction_id, network_id, status, at }` |
| `cashback.transaction.unattributed` | `{ network_transaction_id, at }` |
| `cashback.unattributed.dismissed` | `{ unattributed_id, network_transaction_id, resolved_by, reason, at }` |
| `cashback.entry.created` | `{ entry_id, account_id, state, amount, at }` |
| `cashback.entry.state_changed` | `{ entry_id, from, to, ledger_transfer_ref, at }` |
| `cashback.entry.reversed` | `{ entry_id, reversal_entry_id, reason, at }` |
| `cashback.withdrawal.requested` / `.approved` / `.rejected` | `{ request_id, account_id, amount, actor?, reason?, at }` |
| `cashback.payout.settled` / `.failed` | `{ payout_id, request_id, rail_reference?, classification?, at }` |
| `cashback.reconciliation.statement_imported` | `{ run_id, network_account_id, network_id, period_start, period_end, lines, statement_digest, imported_by, at }` |
| `cashback.reconciliation.difference_found` | `{ run_id, difference_id, kind, delta, at }` |

Amounts in payloads are `{ "minor": int, "currency": "EUR" }` — the same
shape as the HTTP contract, for the same reason (C-6).

`cashback.unattributed.dismissed` carries the acting account and the reason
in its payload rather than leaving them to a lookup. That is what makes it
the audit record FR-061 asks for: a consumer that had to come back and ask
the cashback schema why would be making exactly the synchronous call-back
consumer rule 2 forbids. Its subject is the queue row, and its idempotency
key is the row — a row is resolved at most once, so a second append under
that key means the resolution is already in the stream.

`cashback.reconciliation.statement_imported` is published once per run, when
an operator's statement becomes an immutable run (US6). Its subject is the
run and its idempotency key is the run: the same statement imported again
for the same account and period is the same run, so it is announced once.
It carries who imported it (FR-061) and how many lines the statement names;
the statement itself stays in the run.

## Published by `news` (existing)

Cashback consumes **none** of them today. It may not start consuming one
without a plan update — silent cross-product coupling through an event is
the failure mode this contract exists to prevent.

## Consumer rules

1. **Idempotent by construction.** Every handler is safe to run twice on the
   same `event_id`. Handlers record processed ids; a redelivery is a no-op.
2. **No synchronous call-back.** A handler may not call into the producing
   domain. If it needs data the event did not carry, the event is wrong and
   the event gets fixed.
3. **No ordering assumption across types.** Ordering holds per `subject`
   within a type, and nowhere else.
4. **Failure is visible.** A handler that fails after its retry budget moves
   the delivery to a dead-letter table that an operator queue surfaces. A
   silently dropped event is a defect.
5. **No event carries a secret**, a raw bank detail or a full network
   payload. Events carry identifiers; the data stays in its owning schema.

## Test obligations

- Outbox atomicity: a forced failure between the state change and the
  outbox write leaves neither.
- Redelivery: replaying the entire event log leaves identical state
  (verified by comparing ledger sums and entry states).
- Boundary: an architecture test asserts no `cashback` package imports a
  news package, and no handler in one product calls the other's API.
