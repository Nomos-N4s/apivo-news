# Data Model — Apivo Cashback Alpha

**Feature**: `002-apivo-cashback-alpha` | **Date**: 2026-08-24

Conventions follow `internal/platform/db/migrations/0001_init.up.sql`:
lowercase SQL, singular table names, every constraint named, `comment on`
for every table and every legally load-bearing column. Money is **always**
`bigint` minor units plus a `char(3)` ISO-4217 currency; no `numeric`, no
`float`, anywhere (C-6).

New migrations land as `0010_cashback_init` onward — the existing news
migrations 0001–0009 are untouched.

## 1. Schema layout

| Schema | Owns | Read by |
|---|---|---|
| `public` (existing) | news tables, `account`, `place`, `language`, `domain_event` | everyone (shared reference data) |
| `cashback` (new) | every table below | the cashback domain only |
| `blnk` (new, external) | Blnk's own tables — accounts, balances, transactions | Blnk; Apivo reads it **only** through the zero-sum verification view |

Rules enforced by a migration lint (ADR-0001):

- No foreign key crosses from `cashback` into a news table.
- `cashback` may reference `public.account`, `public.place` and
  `public.language` — the shared reference data — and nothing else.
- Nothing writes to `blnk` except Blnk itself.

## 2. Entities

### 2.1 Membership

**`cashback.participation`** — an account's opt-in.

| Column | Type | Notes |
|---|---|---|
| `account_id` | `uuid` PK → `public.account` | one participation per account (FR-001) |
| `brand_id` | `text not null` | ADR-0004 forward compatibility |
| `opted_in_at` | `timestamptz not null` | |
| `terms_version` | `text not null`, not blank | FR-002 |
| `status` | `text not null` `check in ('active','left')` | |
| `left_at` | `timestamptz` | null unless `status='left'` |
| `default_currency` | `char(3) not null` | |

Constraint `participation_left_has_timestamp`: `status = 'left'` iff
`left_at is not null`. Leaving never deletes financial rows (FR-003).

### 2.2 Catalogue

**`cashback.network`** — an affiliate network Apivo publishes through.

`id text pk` (`awin`, `tradedoubler`, …), `display_name`, `click_ref_param`,
`max_query_window_days int not null`, `rate_limit_per_second int not null`,
`active bool not null default false`. Credentials are **not** stored here
(ADR-0003).

**`cashback.network_account`** — a publisher account at a network.

`id uuid pk`, `network_id → network`, `external_publisher_id text not null`,
`credential_ref text not null` (a key into `platform/config`, never a
secret), `cursor_at timestamptz`, `trailing_cursor_at timestamptz`,
`active bool`. The two cursors implement FR-031 and the ~100-day trailing
re-read (ADR-0003).

**`cashback.merchant`** — a retailer.

`id uuid pk`, `brand_id`, `slug text not null unique`, `country char(2)`,
`status`, `retrieved_from network_id`, `retrieved_at timestamptz not null`,
`raw_payload jsonb not null` (FR-012).

**`cashback.merchant_copy`** — per-language copy, language ⊥ place
(constitution VII).

`(merchant_id, language_code) pk` → `public.language`, `name text not null`,
`summary`, `terms`. Absent language falls back to the merchant's source
language and is **labelled**, never machine-invented (US5 scenario 2).

**`cashback.merchant_place`** — many-to-many scope to `public.place`.

**`cashback.offer`** — a published rate band.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PK | |
| `merchant_id` | → `merchant` | |
| `network_id` | → `network` | FR-011 |
| `rate_kind` | `text check in ('percent','fixed')` | |
| `rate_bps` | `int` | basis points when `percent` |
| `rate_fixed_minor` | `bigint` | minor units when `fixed` |
| `currency` | `char(3)` | required when `fixed` |
| `member_share_bps` | `int not null` | Q4 — configuration, no committed value |
| `conditions`, `exclusions` | `text` | FR-011 |
| `valid_from`, `valid_to` | `timestamptz` | |
| `deeplink_template` | `text not null` | |

Constraint `offer_rate_kind_fields`: exactly the fields for the chosen
`rate_kind` are non-null; `rate_bps between 0 and 10000`.

### 2.3 Click-out

**`cashback.click`** — the tracked redirect, and the rate snapshot that
governs any credit.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PK | |
| `click_ref` | `text not null unique` | unguessable, ≥128 bits of entropy (FR-020) |
| `account_id` | → `public.account`, **not null** | anonymous clicks are unrepresentable (FR-023) |
| `offer_id` | → `offer`, not null | |
| `clicked_at` | `timestamptz not null` | |
| `rate_snapshot` | `jsonb not null` | the whole rate band as published at click time (FR-013) |
| `member_share_bps_snapshot` | `int not null` | |
| `context_digest` | `text` | privacy-minimised device/context digest for abuse rules (FR-022) |

`click` is **append-only** (`raise_immutable()` on UPDATE/DELETE/TRUNCATE) —
it is the attribution evidence the member's money rests on.

### 2.4 Network evidence (C-3)

**`cashback.network_transaction`** — immutable evidence of what a network
reported.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PK | |
| `network_id` | → `network`, not null | |
| `network_account_id` | → `network_account`, not null | |
| `external_id` | `text not null` | the network's transaction id |
| `click_ref` | `text` | null when the network reported none |
| `status_raw` | `text not null` | the network's own vocabulary, verbatim |
| `status` | `text not null check in ('pending','confirmed','declined','reversed')` | FR-033 |
| `sale_amount_minor` | `bigint not null` | |
| `commission_minor` | `bigint not null` | |
| `currency` | `char(3) not null` | |
| `transacted_at` | `timestamptz not null` | as reported |
| `retrieved_at` | `timestamptz not null` | US2 scenario 1 |
| `query_window_start`, `query_window_end` | `timestamptz not null` | |
| `raw_payload` | `jsonb not null` | verbatim (FR-032) |
| `content_digest` | `text not null` | database-computed, as `source_item` does |
| `supersedes_id` | `uuid` → self | set when a later report changes status or amount |

- Triggers reject UPDATE, DELETE, TRUNCATE via the existing
  `raise_immutable()` (C-3).
- Unique on `(network_id, external_id, content_digest)` — an unchanged
  re-report creates nothing (US2 scenario 3); a changed one creates a new
  superseding row (US2 scenario 4).
- Partial unique index on `(network_id, external_id)` where
  `supersedes_id is null` guarantees exactly one current row per
  transaction.

### 2.5 Earnings

**`cashback.entry`** — the member-facing unit of earning.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PK | |
| `account_id` | → `public.account`, not null | |
| `brand_id` | `text not null` | |
| `network_transaction_id` | → `network_transaction`, **not null** | **C-2** |
| `click_id` | → `click` | null only for operator-attributed entries |
| `state` | `text not null check in ('held','pending','confirmed','reserved','paid','reversed')` | FR-042 |
| `amount_minor` | `bigint not null check (amount_minor > 0)` | |
| `currency` | `char(3) not null` | |
| `hold_rule` | `text` | non-null iff `state='held'` (US7) |
| `reversal_of_id` | `uuid` → self | set on reversal entries |
| `created_at` | `timestamptz not null` | |

C-2 is the `not null` FK: a credit with no evidence cannot be inserted.
Entries are **not** mutated on reversal — a reversing entry is inserted
(SC-010).

**`cashback.entry_transition`** — append-only state history, one row per
transition, each carrying the `ledger_transfer_ref` that made it real
(D7). A transition without a transfer reference is rejected.

**`cashback.unattributed_transaction`** — a network transaction with no
matching click (FR-034), with `resolved_by`, `resolved_reason`,
`resolved_at`. Never auto-credited.

### 2.6 Ledger boundary

Balances live in **Blnk** (ADR-0002). Apivo stores only the join:

**`cashback.ledger_link`** — `entry_id`, `transition_id`,
`ledger_transfer_ref text not null unique`, `posted_at`. This is the seam
that makes C-7 a single query and makes the ledger swappable.

**`cashback.ledger_zero_sum` (view)** — sums postings per currency across
the `blnk` schema. An automated check asserts every row is zero (C-1,
SC-003) and alerts loudly otherwise. If spike S1 fails and Blnk gets its own
database, this view becomes a periodic reconciliation job with the same
assertion.

House accounts (rounding remainder per D6, clawback losses per Q3) are
ledger accounts in Blnk, named in configuration, never hardcoded.

### 2.7 Payout

**`cashback.payout_destination`** — `id`, `account_id` (not null),
`kind` (`sepa`, `manual`, `stub`), `details_ref` (a reference, never raw
bank details in this table), `verified_at timestamptz`,
`verified_method`. Constraint: a destination with `verified_at is null`
cannot be referenced by a withdrawal request (FR-051, US4 scenario 6).

**`cashback.withdrawal_request`** — `id`, `account_id`, `destination_id`,
`amount_minor`, `currency`, `state`
(`awaiting_approval | approved | rejected | paid | failed`),
`requested_at`, `reserved_transfer_ref not null` (D9 — the reservation
exists before review), `decided_by → public.account`, `decided_at`,
`decision_reason`.

**`cashback.payout`** — one outbound money movement.

| Column | Type | Notes |
|---|---|---|
| `id` | `uuid` PK | |
| `request_id` | → `withdrawal_request`, not null, **unique** | one payout per request |
| `approved_by` | → `public.account`, **not null** | **C-4** — the row *is* the approval |
| `idempotency_key` | `text not null unique` | **C-5**, derived from `request_id` (D8) |
| `amount_minor`, `currency` | | |
| `rail` | `text not null` | |
| `rail_reference` | `text` | |
| `state` | `text not null check in ('submitted','settled','failed')` | |
| `submitted_at`, `settled_at` | `timestamptz` | |

### 2.8 Reconciliation

**`cashback.reconciliation_run`** — `id`, `network_account_id`,
`statement_period_start/end`, `imported_at`, `imported_by`,
`raw_statement jsonb not null` (immutable, same treatment as network
evidence).

**`cashback.reconciliation_difference`** — `run_id`, `kind`
(`reported_not_paid | paid_not_reported | amount_mismatch`),
`network_transaction_id`, `expected_minor`, `actual_minor`, `currency`,
`resolved_by`, `resolved_reason`, `resolved_at` (US6).

`entry.state` may only reach `confirmed` when the underlying transaction is
both network-approved and reconciled as received (FR-043) — enforced by the
promotion query, and asserted by an integration test.

### 2.9 Audit

Operator actions append to the existing `public.domain_event` stream, which
is already immutable (constitution IV). No second audit table.

### 2.10 Deltas to existing shared tables

Two things cashback needs do not exist yet in `public`. Both are additive
migrations against shared tables, so they are called out separately from the
`cashback` schema and land **before** any code that depends on them.

**`0018_domain_event_envelope`** — `domain_event` is `(id, type, payload,
occurred_at)` and carries none of the routing fields the event contract
needs. Adds:

| Column | Type | Notes |
|---|---|---|
| `version` | `int not null default 1` | |
| `producer` | `text not null default 'news'` | correct for every pre-existing row |
| `subject` | `uuid` | nullable; per-subject ordering is the only ordering guaranteed |
| `idempotency_key` | `text` | nullable, partial unique index `where idempotency_key is not null` — makes redelivery a no-op |

`ADD COLUMN` with a constant default neither rewrites the table nor fires
the row-level immutability triggers, so append-only holds and the two
existing writers (`internal/ingestion/store.go`,
`internal/editorial/queries/approval.sql`) keep working untouched. Full
field mapping in [contracts/events.md](contracts/events.md).

**`0019_operator_role`** — migration 0002 constrains `account.role` to
`check (role in ('reader', 'editor'))`, so an operator is currently
unrepresentable and the `/ops/*` authority cannot be database-enforced.
Adds:

- the constraint extended to `('reader', 'editor', 'operator')`;
- `payout_insert_guard`, a `BEFORE INSERT` trigger on `cashback.payout`
  requiring `approved_by` to hold the `operator` role. It reads the role
  **`FOR SHARE`**, exactly as `article_insert_guard` does, so a concurrent
  demotion cannot slip past it;
- `account_role_guard` extended so an operator who has approved a payout
  cannot be silently demoted out of that authority.

This **strengthens C-4** the way migration 0002 tightened I-1: the `NOT
NULL` on `approved_by` makes an unapproved payout unrepresentable, and the
trigger makes an *unauthorised* approver unrepresentable too. Its test
asserts the database rejects a payout approved by a reader or an editor, by
SQLSTATE.

## 3. State machine

```text
                    ┌─────────────── hold rule matched ──────────────┐
                    ▼                                                │
network_transaction(pending) ──▶ entry(held) ──release──▶ entry(pending)
                    │                    └──reject──▶ (no credit, audited)
                    │                                        ▲
                    └──────────── no hold rule ──────────────┘

entry(pending) ──network approved AND reconciled──▶ entry(confirmed)
entry(pending) ──network declined─────────────────▶ entry(reversed)
entry(confirmed) ──withdrawal requested───────────▶ entry(reserved)
entry(reserved) ──payout settled──────────────────▶ entry(paid)
entry(reserved) ──payout failed / rejected────────▶ entry(confirmed)
entry(confirmed|paid) ──network reversal──────────▶ reversing entry inserted
```

Every arrow writes a ledger transfer and an `entry_transition` row. No arrow
edits an existing entry.

## 4. Invariant enforcement map

| Invariant | Mechanism | Test asserts |
|---|---|---|
| **C-1** double entry | Blnk transfers; `ledger_zero_sum` view checked continuously | non-zero sum trips the check and fails the suite |
| **C-2** attribution | `entry.network_transaction_id NOT NULL` FK | insert without evidence → SQLSTATE 23502/23503 |
| **C-3** immutable evidence | `raise_immutable()` on `network_transaction`, `click`, `reconciliation_run` | UPDATE/DELETE → raised exception |
| **C-4** named approver | `payout.approved_by NOT NULL` **plus** `payout_insert_guard` requiring the `operator` role (§2.10) | insert without approver → SQLSTATE 23502; insert approved by a reader or editor → raised exception |
| **C-5** exactly once | `payout.idempotency_key` UNIQUE, `payout.request_id` UNIQUE | concurrent double submit → one row, one SQLSTATE 23505 |
| **C-6** integer money | `bigint` + `char(3)`, `CHECK` on currency format | no `numeric`/`float` column exists — schema-wide assertion query |
| **C-7** traceability | `cashback.provenance` view | one query returns payout → approver → postings → entries → evidence → click → offer |

Each is a table-driven Go test asserting **the database** rejects the
illegal state, by SQLSTATE, against a real Postgres — the same discipline
as `invariants_test.go` for I-1..I-5.

## 5. Migration plan

| Migration | Contents |
|---|---|
| `0010_cashback_schema` | `create schema cashback`; roles and grants; migration lint fixture |
| `0011_cashback_catalogue` | `network`, `network_account`, `merchant`, `merchant_copy`, `merchant_place`, `offer` |
| `0012_cashback_clicks_evidence` | `click`, `network_transaction`, immutability triggers, unique indexes |
| `0013_cashback_earnings` | `entry`, `entry_transition`, `unattributed_transaction`, `ledger_link` |
| `0014_cashback_payout` | `payout_destination`, `withdrawal_request`, `payout` |
| `0015_cashback_reconciliation` | `reconciliation_run`, `reconciliation_difference` |
| `0016_cashback_provenance_view` | `cashback.provenance`, `cashback.ledger_zero_sum` |
| `0017_participation_brand` | `participation`; `brand_id` on the tenant-boundary tables (ADR-0004) |
| `0018_domain_event_envelope` | **Delta to a shared table** — see §2.10 |
| `0019_operator_role` | **Delta to a shared table** — see §2.10 |

Every migration has a tested `.down.sql`, as the repository already
requires. sqlc generates `internal/cashback/*/store` from the same schema —
types are never hand-written on both sides.

## 6. Requirement traceability

| Requirement | Where it lands |
|---|---|
| FR-001..003 | `participation` |
| FR-010..013 | `merchant`, `merchant_copy`, `merchant_place`, `offer`, `click.rate_snapshot` |
| FR-020..023 | `click` (+ `account_id NOT NULL`) |
| FR-030..034 | `network`, `network_account` cursors, `network_transaction`, `unattributed_transaction` |
| FR-040..043 | `entry`, `entry_transition`, `ledger_link`, house accounts, reconciliation gate |
| FR-050..053 | `payout_destination`, `withdrawal_request`, `payout` |
| FR-060..062 | operator queues over `unattributed_transaction`, held `entry`, `reconciliation_difference`, `withdrawal_request`; `domain_event` |
| FR-070..073 | `brand_id` columns; brand config outside the database |
| FR-080..082 | schema separation, migration lint, import test |
