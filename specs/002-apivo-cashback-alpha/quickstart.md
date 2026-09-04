# Quickstart: Apivo Cashback Alpha

**Feature**: [spec.md](spec.md) | **Date**: 2026-08-24

Repository setup (hooks, Postgres, coverage gates) lives in the
[README](../../README.md). This file is the **validation guide** for
cashback: how to run it, and the scenarios that prove it works end to end.
It contains no implementation code — that belongs in `tasks.md` and the
implementation phase.

Design references: [data-model.md](data-model.md),
[contracts/http-api.md](contracts/http-api.md),
[contracts/ports.md](contracts/ports.md),
[contracts/events.md](contracts/events.md),
[ADR-0002](../../docs/adr/0002-cashback-money-substrate.md).

---

## Prerequisites

| What | Why | If unavailable |
|---|---|---|
| Postgres | schema + invariant tests | `make db-up` (compose) |
| Blnk + Redis | the ledger (ADR-0002) | run with `LEDGER_DRIVER=memory` — everything except the Blnk conformance suite still runs |
| Network credentials | live adapter | **not required** — `NETWORKS=fixture` (ADR-0003). An account id still is: `NETWORK_FIXTURE_ACCOUNT_ID` names the `cashback.network_account` row the cursors live on |
| Docker | compose stack | while Docker Desktop is unavailable on the founder's machine, run unit and pure suites locally and treat **CI as the verification of record**; say plainly in every PR which checks ran where |

## Run the stack locally

```bash
make db-up && make cashback-up
```

`cashback-up` starts Blnk and Redis beside Postgres and runs migrations
0010–0017. Then:

```bash
CASHBACK_ENABLED=true LEDGER_DRIVER=blnk \
  NETWORKS=fixture NETWORK_FIXTURE_ACCOUNT_ID=fixture-publisher go run ./cmd/apivo
```

Frontend as usual (`cd web && npm ci && npm run dev`). Cashback routes are
hidden entirely when `CASHBACK_ENABLED` is false — the flag controls route
mounting, not just navigation.

### Without Docker

**The server does not start without Docker, and this guide used to imply it
did.** `LEDGER_DRIVER=memory` removes the ledger and Redis from the loop —
it does **not** remove Postgres. `internal/platform/config/config.go:182`
requires `DATABASE_URL`, and without Docker there is no local Postgres to
point it at. What runs Docker-free is the **suite**, not the server:

```bash
go build ./... && go vet ./... && go test ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run
```

Every `DATABASE_URL`-keyed test skips and **names the key it is waiting
for**; none fails. That is the expected outcome, not a broken checkout, and
spike S3 asserts it in CI so it cannot quietly stop being true.

To run the server itself you need a Postgres — Docker locally, or a
`DATABASE_URL` pointing at a reachable database. With one, `LEDGER_DRIVER=memory
NETWORKS=fixture NETWORK_FIXTURE_ACCOUNT_ID=fixture-publisher` then exercises
catalogue, click-out, the entry state
machine, wallet and payout orchestration without Blnk or Redis.

## Seed a member and a catalogue

```bash
make cashback-seed
```

Seeds one fixture network, two merchants (one Munich-scoped, one
Germany-wide), three rate bands including one with exclusions and one
without Greek copy, and opts the given account in.

Provisioning an account is unchanged from the news quickstart: the
`account.id` must equal the Supabase Auth user id.

An operator additionally needs `account.role = 'operator'`. **That value
only exists once migration `0019_operator_role` has run** — before it,
`account.role` is constrained to `reader|editor` and the update is rejected
by the database, which is the correct behaviour, not a bug:

```bash
make db-up && make migrate
```

```sql
update account set role = 'operator' where id = '<supabase-auth-user-id>';
```

A payout approved by a `reader` or an `editor` is rejected by
`payout_insert_guard`, not merely by the HTTP layer — see
[data-model.md](data-model.md) §2.10.

---

## Validation scenarios

Each scenario states what to run and what must be true afterwards. These are
the acceptance gates; every one has an automated equivalent in the suite.

### V1 — Earn, confirm, and see it (US1, US3 · SC-001, SC-002, SC-006)

```bash
make cashback-scenario NAME=earn-confirm
```

Drives: click-out → fixture reports pending → poller ingests → wallet shows
Pending → fixture reports approved → reconciliation marks received → wallet
shows Confirmed.

**Expected**

- A `click` row exists with an unguessable `click_ref`, committed before the
  redirect was returned.
- The entry's amount derives from the **click-time** rate snapshot, even
  though the scenario changes the published rate mid-run (FR-013).
- Wallet Pending, then Confirmed, each equal to an independently computed
  ledger sum, to the minor unit.
- Zero orphan credits: the SC-002 query returns no rows.

### V2 — Evidence is immutable (US2 · C-3)

```bash
make cashback-scenario NAME=evidence-immutable
```

**Expected**

- Re-polling the same window creates no duplicate row.
- A changed status creates a **new** row with `supersedes_id` set; both
  remain readable in order.
- `UPDATE` and `DELETE` against `cashback.network_transaction` raise, with
  the SQLSTATE asserted.

### V3 — Reversal never edits history (US1 scenario 4 · SC-010)

```bash
make cashback-scenario NAME=reversal
```

**Expected**: a reversing entry and a reversing pair of postings; the
original entry byte-identical to before; the wallet showing both.

### V4 — Withdrawal is exactly once (US4 · C-4, C-5, SC-004)

```bash
make cashback-scenario NAME=withdrawal-exactly-once
```

Drives concurrent submissions, an approval replayed with the same
`Idempotency-Key`, a rail timeout followed by a retry, and a terminal rail
failure.

**Expected**

- Exactly one `payout` row across every path.
- `payout.approved_by` non-null and equal to the approving operator.
- Terminal failure returns the reserved amount to confirmed balance.
- A withdrawal against another member's destination is refused (403).
- Attempting to insert a payout without an approver raises SQLSTATE 23502.

### V5 — Unattributed and held (US1 scenario 5, US7 · FR-034, FR-060)

```bash
make cashback-scenario NAME=unattributed-and-held
```

**Expected**: a transaction with an unknown `click_ref` lands in the
unattributed queue and credits nobody; a transaction matching a hold rule
creates a `held` entry naming the rule; release and reject both require a
non-blank reason and append to `domain_event`.

### V6 — Reconciliation catches a short payment (US6)

```bash
make cashback-scenario NAME=reconciliation
```

**Expected**: an omitted approved transaction and a shorted one are both
listed with their deltas; neither silently changes a member's confirmed
balance; resolving records who and why.

### V7 — Ledger balances (C-1 · SC-003)

```bash
make cashback-verify-ledger
```

Runs the zero-sum check over every currency and the wallet-vs-ledger
comparison for every seeded member. **Expected**: all zero, all equal. This
check is also wired to run continuously in the deployed environment.

### V8 — Rebrand (FR-072 · SC-007)

```bash
make cashback-brand-check
```

Renders every member-facing surface under the fixture brand (different name,
palette, currency and default language) and runs the brand-literal lint.

**Expected**: no code change was needed; the lint finds no product name,
colour, domain or email literal outside the brand configuration and its
assets.

### V9 — Boundaries hold (FR-080..082 · SC-008)

```bash
make arch-test && make migration-lint
```

**Expected**: no `cashback` package imports a news package (or vice versa);
no foreign key crosses a product schema; no network-specific type escapes
its adapter package.

### V10 — Add a second network without touching the domain (SC-008)

Add a second fixture adapter under a new package name and register it in
`cmd/`. **Expected**: no file outside that package and the wiring line
changes; the adapter conformance suite passes unmodified.

---

## Spike validation (must pass before dependent work — ADR-0002)

| Spike | Command | Pass condition |
|---|---|---|
| **S1** Blnk on Supabase Postgres | `make spike-ledger-schema` | Blnk migrates into the `blnk` schema with a restricted role and never touches `public` |
| **S2** Cross-store consistency | `make spike-outbox-crash` | Killing the process between the Apivo commit and the ledger call leaves a recoverable state; replay produces exactly one transfer |
| **S3** CI without local Docker | CI run | The full cashback job passes in CI with Blnk and Redis as service containers |

A failing S1 or S2 triggers the fallbacks recorded in ADR-0002 — it does not
get worked around quietly.

## Coverage and gates

Unchanged from the repository standard: Go ≥ 90% statement coverage,
TypeScript ≥ 80%, `go vet` clean, strict `golangci-lint`, every exported
symbol documented, integration tests against a real Postgres in CI. Cashback
adds one gate: **the invariant suite for C-1 to C-7 must pass, and a green
coverage number without it means nothing.**
