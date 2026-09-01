---

description: "Task list for the Apivo Cashback Alpha"
---

# Tasks: Apivo Cashback Alpha

**Input**: Design documents from `/specs/002-apivo-cashback-alpha/`

**Prerequisites**: [plan.md](plan.md), [spec.md](spec.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/), [quickstart.md](quickstart.md), constitution v1.1.0

**Tests**: **Included and mandatory.** The constitution requires every invariant to carry a test asserting the *database* rejects the illegal state, plus contract tests per endpoint, port conformance suites, 90% Go / 80% TS coverage, and integration tests against a real Postgres. Test tasks here are not optional extras.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies on incomplete tasks)
- **[Story]**: Which user story this task belongs to (US1–US7)
- Every task names its file path

## Path Conventions

Go modular monolith + Astro frontend, per [plan.md](plan.md) Project Structure:

- Backend domain: `internal/cashback/<subdomain>/`
- Platform kernel: `internal/platform/<package>/`
- Migrations: `internal/platform/db/migrations/`
- Composition: `cmd/apivo/`
- Frontend: `web/src/`
- Deployment: `deploy/`, `docker-compose.yml`

---

## Phase 1: Setup & Spikes (Blocking)

**Purpose**: Stand up the new infrastructure and resolve the three ADR-0002 spikes. A failing spike changes the ledger decision — this phase exists so that happens in week one, not month three.

- [ ] T001 Add `CASHBACK_ENABLED`, `LEDGER_DRIVER`, `NETWORK_DRIVER`, `BLNK_URL`, `REDIS_URL` and network credential keys to `internal/platform/config/config.go` with validation and log redaction
- [ ] T002 [P] Add `blnk` and `redis` services to `docker-compose.yml` with healthchecks and pinned image tags
- [ ] T003 [P] Add `blnk` and `redis` services to `deploy/hetzner/compose/` and wire secrets through `deploy/hetzner/env/`
- [ ] T004 [P] Add `blnk` and `redis` Deployment/Service/ConfigMap manifests to `deploy/k8s/` and extend the kubeconform CI check
- [ ] T005 Add a `cashback` job to `.github/workflows/` running Blnk, Redis and Postgres as service containers
- [ ] T006 **Spike S1** — verify Blnk migrates into a dedicated `blnk` schema with a restricted role and never touches `public`, in `scripts/spikes/ledger_schema/`
- [ ] T007 **Spike S2** — verify Apivo commit and Blnk transfer stay consistent through the outbox with a shared idempotency key, including a kill-between-the-two crash test, in `scripts/spikes/outbox_crash/`
- [ ] T008 **Spike S3** — verify the full cashback CI job is green without local Docker and that `LEDGER_DRIVER=memory` runs the stack, in `scripts/spikes/no_docker/`
- [ ] T009 [P] Add `cashback-up`, `cashback-seed`, `cashback-scenario`, `cashback-verify-ledger`, `cashback-brand-check`, `migration-lint` targets to `Makefile`
- [ ] T010 [P] Document the new environment variables in `.env.example` and `docs/ENVIRONMENTS.md`
- [ ] T011 Pin the Blnk Go SDK version in `go.mod` and record the pin rationale in `docs/RELEASING.md`
- [ ] T012 Record spike outcomes in `specs/002-apivo-cashback-alpha/research.md` §S-results; if S1 or S2 failed, open `docs/adr/0006-*.md` adopting the recorded ADR-0002 fallback rather than working around it

**Checkpoint**: Infrastructure runs, spikes resolved, ledger decision confirmed or formally revised.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The platform kernel, the schema, the invariants and the ledger port. **No user story work can begin until this phase is complete** — every story writes ledger postings, and the invariants must exist before the first row does.

### Platform kernel

- [ ] T013 [P] Implement `Amount{Minor int64; Currency}` with arithmetic, explicit rounding modes and no float path in `internal/platform/money/money.go` + `money_test.go`
- [ ] T014 [P] Implement the `Brand` type and loader in `internal/platform/brand/brand.go` + `brand_test.go`, with a fixture brand in `internal/platform/brand/testdata/fixture/`
- [ ] T015 [P] Implement the TypeScript brand loader and CSS custom properties in `web/src/lib/brand/index.ts` + `brand.test.ts`
- [ ] T016 Implement the brand-literal CI lint (greps for brand proper nouns, colours, domains, emails outside the brand config) in `scripts/lint-brand-literals.sh` and wire it into `.github/workflows/`
- [ ] T017 [P] Implement the transactional outbox writer and dispatcher over `public.domain_event` in `internal/platform/events/outbox.go`, `dispatcher.go` + tests
- [ ] T018 Implement the subscriber registry, at-least-once delivery with a delivery idempotency key, and the dead-letter table in `internal/platform/events/subscriber.go` + `deadletter.go`, per [contracts/events.md](contracts/events.md)
- [ ] T019 [P] Implement the in-process job scheduler with a Postgres advisory lock in `internal/platform/scheduler/scheduler.go` + `scheduler_test.go`

### Schema

- [ ] T020 Write migration `internal/platform/db/migrations/0010_cashback_schema.{up,down}.sql` creating the `cashback` schema, its role and grants
- [ ] T021 Write migration `0011_cashback_catalogue.{up,down}.sql` for `network`, `network_account`, `merchant`, `merchant_copy`, `merchant_place`, `offer` per [data-model.md](data-model.md) §2.2
- [ ] T022 Write migration `0012_cashback_clicks_evidence.{up,down}.sql` for `click` and `network_transaction`, their `raise_immutable()` triggers, and the dedup/current-row unique indexes per §2.3–2.4
- [ ] T023 Write migration `0013_cashback_earnings.{up,down}.sql` for `entry`, `entry_transition`, `unattributed_transaction`, `ledger_link` per §2.5–2.6
- [ ] T024 Write migration `0014_cashback_payout.{up,down}.sql` for `payout_destination`, `withdrawal_request`, `payout` per §2.7
- [ ] T025 Write migration `0015_cashback_reconciliation.{up,down}.sql` for `reconciliation_run`, `reconciliation_difference` per §2.8
- [ ] T026 Write migration `0016_cashback_provenance_view.{up,down}.sql` creating `cashback.provenance` and `cashback.ledger_zero_sum`
- [ ] T027 Write migration `0017_participation_brand.{up,down}.sql` for `participation` and the `brand_id` columns on the tenant-boundary tables
- [ ] T028 Add up/down round-trip tests for migrations 0010–0017 in `internal/platform/db/migrations_test.go`
- [ ] T029 Extend `sqlc.yaml` for the `cashback` schema and generate stores into `internal/cashback/*/store/`
- [ ] T030 Extend the TypeScript type generation and drift check for the `cashback` schema in `web/src/lib/database.types.ts` and the CI drift job

### Invariants (C-1 to C-7)

- [ ] T031 [P] Test that a non-zero per-currency sum in `cashback.ledger_zero_sum` fails the check (C-1) in `internal/cashback/wallet/invariants_test.go`
- [x] T032 [P] Test that inserting an `entry` without `network_transaction_id` is rejected by SQLSTATE (C-2) in `internal/cashback/earnings/invariants_test.go` — landed in `internal/platform/db/cashback_earnings_test.go` instead, beside the other schema invariant tests and the seeding helpers they share: `TestCashbackEarningsRejectIllegalWrites` covers a credit with no evidence at all (23502), one citing evidence that does not exist (23503) and one resting on somebody else's click
- [x] T033 [P] Test that UPDATE/DELETE/TRUNCATE on `network_transaction`, `click` and `reconciliation_run` raise (C-3) in `internal/cashback/networks/invariants_test.go` — landed in `internal/platform/db/cashback_evidence_test.go` and `cashback_reconciliation_test.go` instead, beside the other schema invariant tests and the seeding helpers they share
- [x] T034 [P] Test that inserting a `payout` without `approved_by` is rejected by SQLSTATE 23502 (C-4) in `internal/cashback/payout/invariants_test.go` — landed in `internal/platform/db/cashback_payout_test.go` instead, same reason: `TestCashbackPayoutRejectsIllegalWrites/payout with no approver` asserts 23502, and a second case asserts that a uuid naming nobody is refused by the foreign key, because C-4 says a NAMED human
- [x] T035 [P] Test that a duplicate `idempotency_key` and a duplicate `request_id` are rejected under concurrency (C-5) in `internal/cashback/payout/idempotency_test.go` — landed in `internal/platform/db/cashback_payout_test.go` instead: `TestConcurrentDoubleSubmitProducesOnePayout` runs two real transactions rather than a sequential pair, and `TestPayoutIdempotencyKeyIsDerivedFromTheRequest` asserts the key is the database's to compute
- [x] T036 [P] Test that no `numeric`/`float`/`double` column exists in the `cashback` schema and every money column has its currency (C-6) in `internal/cashback/schema_money_test.go` — landed in `internal/platform/db/cashback_catalogue_test.go` instead, as `TestNoFractionalMoneyTypeExistsInTheCashbackSchema`: the invariant is that no fractional type EXISTS anywhere in the schema, which is a question about the whole schema asked of the catalog rather than a per-column assertion
- [x] T037 [P] Test that `cashback.provenance` returns the full payout→click chain in one query (C-7) in `internal/cashback/ops/provenance_test.go` — landed in `internal/platform/db/cashback_provenance_test.go` instead: `TestCashbackProvenanceAnswersTheWholeChain` reads payout to click in one query, and `TestCashbackProvenanceCoversAnOperatorAttributedEntry` covers the row whose click is null because a human attributed it (FR-034)

### Boundaries

- [ ] T038 Extend the architecture test to forbid cross-product-domain imports at any depth in `internal/arch/arch_test.go`
- [ ] T039 Implement the migration lint rejecting foreign keys that cross a product schema boundary in `scripts/lint-migrations.sh` + CI wiring
- [ ] T040 Mount the cashback route tree behind `CASHBACK_ENABLED` with panic isolation at the HTTP boundary in `cmd/apivo/main.go` + `cashback_wiring_test.go`

### Ledger port

- [ ] T041 Define the `Ledger` port (`EnsureAccount`, `Post`, `Balance`, `History`) and the `Transfer`/`Posting` types in `internal/cashback/wallet/ledger.go` per [contracts/ports.md](contracts/ports.md) §1
- [ ] T042 [P] Implement the in-memory ledger in `internal/cashback/wallet/memory/memory.go`
- [ ] T043 [P] Implement the Blnk-backed ledger in `internal/cashback/wallet/blnk/blnk.go`
- [ ] T044 [P] Implement the Postgres ledger (the documented exit route) in `internal/cashback/wallet/postgres/postgres.go`
- [ ] T045 Write the shared ledger conformance suite (idempotent replay, zero-sum rejection, mixed-currency rejection, concurrent same-key post, balance-after-reversal, crash injection) in `internal/cashback/wallet/conformance_test.go` and run it against all three implementations
- [ ] T046 Register the continuous zero-sum check as a scheduled job in `internal/cashback/wallet/zerosum.go` + wiring in `cmd/apivo/main.go`
- [ ] T047 Implement house accounts (rounding remainder, clawback loss) resolved from configuration in `internal/cashback/wallet/house.go`
- [ ] T048 Implement the minimal offer read used by click-out in `internal/cashback/catalogue/offer.go` + `store/` queries

### Deltas to shared `public` tables

> **Numbered out of sequence deliberately.** These were added after review of
> PR #283 found that the event envelope and the operator role are not
> representable in the current schema. The backlog was already issued, and
> renumbering 134 tasks would invalidate 134 issues. **Execution order is
> what matters: both land before T017–T018 and before any `/ops/*`
> endpoint.**

- [ ] T135 Write migration `internal/platform/db/migrations/0018_domain_event_envelope.{up,down}.sql` adding `version`, `producer`, `subject` and `idempotency_key` to `public.domain_event` with a partial unique index on `idempotency_key`, per [data-model.md](data-model.md) §2.10 — **blocks T017, T018**
- [x] T136 Write migration `internal/platform/db/migrations/0019_operator_role.{up,down}.sql` extending `account_role_known` to include `operator`, adding `payout_insert_guard` (approver must hold the operator role, read `FOR SHARE`) and extending `account_role_guard`, per [data-model.md](data-model.md) §2.10 — **blocks every `/ops/*` task and T092**

**Checkpoint**: Foundation ready. The invariants exist before any row does, and the ledger is swappable and proven.

---

## Phase 3: User Story 2 - Network transactions ingested as immutable evidence (Priority: P1)

**Goal**: Every commission a network reports is stored exactly as reported, immutably, with complete retrieval evidence, and never double-counted.

**Why before US1**: both are P1, but a credit cannot exist without evidence. This phase produces the evidence US1 turns into money.

**Independent Test**: Point the ingester at recorded fixtures; verify complete retrieval evidence in one transaction, no duplicates on re-poll, a superseding row on status change, and that the database rejects any mutation.

- [x] T049 [US2] Define the `Network` port (`ID`, `BuildDeeplink`, `FetchTransactions`, `FetchCatalogue`, `Limits`) and `Reported`/`ReportedMerchant` types in `internal/cashback/networks/network.go` per [contracts/ports.md](contracts/ports.md) §2
- [x] T050 [P] [US2] Implement the `fixture` adapter with a scripted click→pending→approved→reversed lifecycle in `internal/cashback/networks/fixture/fixture.go` + `testdata/`
- [x] T051 [P] [US2] Write the adapter conformance suite (status-mapping totality, raw-payload presence, window clamping, rate-limit adherence, deeplink round-trip, resumable iteration) in `internal/cashback/networks/conformance_test.go`
- [x] T052 [US2] Implement evidence persistence writing the normalised record and verbatim raw payload in one transaction in `internal/cashback/networks/evidence.go` + `store/`
- [x] T053 [US2] Implement the database-computed content digest and the dedup path in `internal/cashback/networks/digest.go`
- [x] T054 [US2] Implement superseding-record logic for changed status or amount in `internal/cashback/networks/supersede.go`
- [x] T055 [US2] Implement the poller with durable primary and trailing cursors, advancing only after a window is fully persisted, in `internal/cashback/networks/poller.go`
- [x] T056 [US2] Implement the per-adapter rate limiter with exponential backoff and jitter in `internal/cashback/networks/ratelimiter.go` + `backoff.go` + `retryable.go`
- [x] T057 [US2] Register the poll and trailing re-read jobs with the scheduler in `cmd/apivo/main.go`
- [x] T058 [US2] Record transactions with no matching click as unattributed in `internal/cashback/networks/unattributed.go`
- [ ] T059 [US2] Implement `GET /ops/unattributed`, `POST /ops/unattributed/{id}/attribute` and `/dismiss` in `internal/cashback/ops/handlers.go` per [contracts/http-api.md](contracts/http-api.md) — the listing and `/dismiss` are served, behind the operator role and mounted with the cashback flag. **`/attribute` waits on T068 and T069**: it writes a `cashback.entry`, and an entry needs a member's share and a ledger posting, which those tasks own — one written without them is a credit nobody can reconcile. It is also lawful only for the rows this queue records today; where the network named a reference that matched no click (T067's half), `entry_evidence_guard` refuses a credit citing no click, which is what the listing's `attributable` field reports
- [x] T060 [P] [US2] Integration test: re-polling an unchanged window creates no duplicate; a changed status creates a superseding row and both stay readable, in `internal/cashback/networks/ingest_integration_test.go`
- [x] T061 [P] [US2] Integration test: rate-limit backoff, and no window lost or double-counted across a restart, in `internal/cashback/networks/poller_integration_test.go`
- [x] T062 [P] [US2] Publish `cashback.transaction.ingested` and `.unattributed` events through the outbox in `internal/cashback/networks/events.go`

**Checkpoint**: Evidence is captured, immutable, deduplicated and resumable. Nothing has been credited yet.

---

## Phase 4: User Story 1 - A member earns cashback on a purchase (Priority: P1) 🎯 MVP

**Goal**: Click-out to credit, end to end: a tracked click, an attributed transaction, an entry in the right state, and a balance that moved.

**Independent Test**: Click out from a seeded offer, replay a matching pending transaction, assert a Pending entry linked to that click with the click-time rate; replay the approval, assert Confirmed and a correct confirmed balance.

**Depends on**: Phase 3 (the fixture adapter and evidence store).

- [x] T063 [US1] Implement click-reference generation (≥128 bits of entropy) and the click store in `internal/cashback/clickout/click.go` + `store/`
- [x] T064 [US1] Implement `POST /clickouts` committing the click and its rate snapshot before the redirect is returned, in `internal/cashback/clickout/handlers.go`
- [x] T065 [US1] Implement the deeplink-failure path so nothing is committed and the member gets a plain problem document, in `internal/cashback/clickout/handlers.go` — landed with T064: they are one handler, and "nothing is committed" is a property of the ORDER T064 chose (the redirect is built before the click is recorded), not a rollback that could be added afterwards
- [x] T066 [US1] Implement per-member and per-context click rate limiting returning 429 with `Retry-After` in `internal/cashback/clickout/ratelimit.go` — the per-context half is off unless `CLICK_CONTEXT_HEADER` names a header the deployment's edge is trusted to set, because otherwise the digest is the proxy and the limit would bracket every member behind it
- [x] T067 [US1] Implement the attribution matcher resolving a reported `click_ref` to a click in `internal/cashback/earnings/attribution.go`, and **queue a reference that matches no click as unattributed** — T058 records only the reports the network attached no reference to, because this module owns `cashback.click`, so FR-034's other half arrives here
- [x] T068 [US1] Implement member-share computation from the click-time snapshot with the rounding remainder posted to the house account, in `internal/cashback/earnings/share.go`
- [x] T069 [US1] Implement the entry state machine writing a ledger transfer and an `entry_transition` for every transition, in `internal/cashback/earnings/statemachine.go`
- [x] T070 [US1] Implement the confirmation path gated on network approval **and** reconciled receipt (FR-043) in `internal/cashback/earnings/confirm.go`
- [x] T071 [US1] Implement the reversal path inserting a reversing entry and posting pair, never editing history, in `internal/cashback/earnings/reversal.go`
- [x] T072 [P] [US1] Contract tests for `POST /clickouts` (401 unauthenticated, 409 expired offer, 429 rate limited, no click row after deeplink failure) in `internal/cashback/clickout/handlers_test.go` — all four cases are covered by the handler tests that landed with T064/T065 and T066; what is left is whether they want a file of their own
- [x] T073 [P] [US1] Integration test: full earn journey with a mid-run published-rate change proving the click-time rate governs, in `internal/cashback/earnings/journey_integration_test.go` — the journey needed a step no task named: nothing opened the entry an attributed report earns, so `internal/cashback/earnings/open.go` landed with it
- [x] T074 [P] [US1] Integration test: a transaction with an unknown click reference credits nobody **and lands in the unattributed queue as work an operator may only dismiss**, in `internal/cashback/earnings/unattributed_integration_test.go` — writing it surfaced that only one of FR-034's two feeders announced anything, so `cashback.transaction.unattributed` is now published from the matcher as well as from the poller
- [x] T075 [P] [US1] Test the SC-002 orphan-credit query returns zero rows, in `internal/cashback/earnings/orphan_test.go`
- [x] T076 [P] [US1] Publish `cashback.click.created`, `cashback.entry.created` and `.state_changed` events in `internal/cashback/earnings/events.go` — the two entry events are there; `cashback.click.created` is in `internal/cashback/clickout/events.go` instead, because an event must be appended in the transaction that made its fact true and the click's insert is in clickout, which earnings cannot reach without an import cycle

**Checkpoint**: 🎯 **MVP.** A member can earn cashback and the money is provably correct. Nothing is visible to them yet.

---

## Phase 5: User Story 3 - The member sees an honest wallet (Priority: P2)

**Goal**: Three totals that never disagree with the entries below them.

**Independent Test**: Seed entries across every state, load the wallet, assert each displayed total equals an independently computed ledger sum to the minor unit.

- [x] T077 [US3] Implement wallet balance projection computed from postings, never from a stored balance, in `internal/cashback/wallet/projection.go` — the four stage balances the ledger can answer. `paid_out`, which `GET /wallet` also returns, is not a balance: no account holds money that has left the business, so it is read from the settled payouts and lands with T078
- [x] T078 [US3] Implement `GET /wallet` returning `{minor, currency}` totals plus the payout threshold, in `internal/cashback/wallet/handlers.go` — the threshold is configuration (Q5), as `PAYOUT_THRESHOLD_MINOR` and `PAYOUT_THRESHOLD_CURRENCY`; it belongs on the brand once brand loading reaches the API (ADR-0004). The composition root gained the ledger with it: `LEDGER_DRIVER` selected an implementation nothing had constructed
- [x] T079 [US3] Implement `GET /wallet/entries` with state filter and cursor pagination in `internal/cashback/wallet/handlers.go` — `expected_confirmation_at` is in the response and always null: nothing in the schema records a confirmation window, and inventing one would be a date a member plans around. It needs a nullable window on `cashback.merchant_network` filled by the catalogue import, which is a migration of its own. The endpoint also takes `lang`, because a merchant name has to be looked up in some language and the fallback has to be labelled (US5 scenario 2)
- [x] T080 [US3] Implement `GET /participation`, `POST /participation`, `DELETE /participation` in `internal/cashback/wallet/participation.go`
- [x] T081 [US3] Implement `GET /export` for the member's own history as JSON and CSV in `internal/cashback/wallet/export.go`
- [ ] T082 [P] [US3] Build the wallet page with island-rendered totals in `web/src/pages/[lang]/[place]/cashback/wallet.astro`
- [ ] T083 [P] [US3] Add wallet, entry-state and empty-state copy to the translation catalogues in `web/src/i18n/`
- [x] T084 [P] [US3] Contract test: wallet totals equal independently computed ledger sums for every member (SC-006) in `internal/cashback/wallet/totals_test.go`
- [ ] T085 [P] [US3] Frontend test: a reversed entry shows both the credit and the reversal with a reason, in `web/src/pages/[lang]/[place]/cashback/wallet.test.ts`

**Checkpoint**: The member can see, and what they see is true.

---

## Phase 6: User Story 4 - Withdrawal with a named approver (Priority: P2)

**Goal**: Money leaves the business exactly once, only with a named human's approval, only to a destination the member owns.

**Independent Test**: Drive a withdrawal end to end against the stub rail including duplicate submission, rail timeout and terminal failure; assert exactly-once semantics and balance integrity in every path.

- [x] T086 [US4] Implement the payout destination store with the ownership check in `internal/cashback/payout/destination.go` + `store/`
- [x] T087 [US4] Implement destination verification and the rule that an unverified destination cannot receive money, in `internal/cashback/payout/verification.go`
- [x] T088 [US4] Define the `PayoutRail` port with terminal/retryable failure classification in `internal/cashback/payout/rail.go` per [contracts/ports.md](contracts/ports.md) §3
- [x] T089 [P] [US4] Implement the `manual` rail (operator executes, records the reference, still enforces C-4 and C-5) in `internal/cashback/payout/manual/manual.go`
- [x] T090 [P] [US4] Implement the `stub` rail with timeout, duplicate-submission and permanent-failure modes in `internal/cashback/payout/stub/stub.go`
- [x] T091 [US4] Implement `POST /withdrawals` reserving the amount in the same transaction as the request, in `internal/cashback/payout/handlers.go`
- [x] T092 [US4] Implement `POST /ops/withdrawals/{id}/approve` recording `approved_by` and submitting with the request-derived idempotency key, in `internal/cashback/ops/withdrawals.go`
- [x] T093 [US4] Implement rejection releasing the reservation back to confirmed balance, in `internal/cashback/payout/reject.go`
- [x] T094 [US4] Implement failure classification and same-key retry in `internal/cashback/payout/retry.go`
- [x] T095 [US4] Implement `GET /payout-destinations`, `POST /payout-destinations`, `GET /withdrawals`, `GET /withdrawals/{id}` in `internal/cashback/payout/handlers.go`
- [ ] T096 [P] [US4] Build the withdrawal request and status pages in `web/src/pages/[lang]/[place]/cashback/withdraw.astro`
- [ ] T097 [P] [US4] Build the operator approval queue in `web/src/pages/ops/withdrawals.astro`
- [x] T098 [P] [US4] Exactly-once suite: concurrent submissions, replayed `Idempotency-Key`, rail timeout then retry, terminal failure (SC-004) in `internal/cashback/payout/exactly_once_test.go`
- [x] T099 [P] [US4] Contract test: a withdrawal against another member's destination is refused with 403, in `internal/cashback/payout/handlers_test.go`
- [x] T100 [P] [US4] Publish `cashback.withdrawal.*` and `cashback.payout.*` events in `internal/cashback/payout/events.go`

**Checkpoint**: The full money loop closes. Cashback is earnable, visible and payable.

---

## Phase 7: User Story 5 - Localised merchant catalogue (Priority: P3)

**Goal**: A Munich member reading in Greek browses merchants relevant to them, with honest language fallback.

**Independent Test**: Seed merchants across two countries and two languages; load the catalogue as an `el` reader scoped to Munich and verify scoping, labelled fallback and rate rendering.

- [x] T101 [US5] Implement per-language copy resolution with explicit fallback labelling in `internal/cashback/catalogue/copy.go`
- [x] T102 [US5] Implement place-scoped merchant queries over `merchant_place` in `internal/cashback/catalogue/store/queries/`
- [ ] T103 [US5] Implement `GET /catalogue` with separate `lang` and `place` parameters in `internal/cashback/catalogue/handlers.go`
- [x] T104 [US5] Implement `GET /merchants/{slug}` returning every published rate band with conditions and exclusions, in `internal/cashback/catalogue/handlers.go`
- [x] T105 [US5] Implement catalogue import from `Network.FetchCatalogue` recording source and retrieval time, in `internal/cashback/catalogue/import.go` — the importer landed earlier and nothing was calling it, which is why this stayed open: a deployment kept whatever retailers were in the database when it was last seeded and never learned about a new one, a departed one or a renamed one. It now runs every six hours in one transaction, gated on `NETWORK_SOURCE_LANGUAGE` and `BRAND_DIR`; with either unset the api serves normally and says at ERROR that no import is scheduled
- [ ] T106 [P] [US5] Build the catalogue listing page in `web/src/pages/[lang]/[place]/cashback/index.astro`
- [ ] T107 [P] [US5] Build the merchant detail page in `web/src/pages/[lang]/[place]/cashback/[slug].astro`
- [x] T108 [P] [US5] Test language fallback is labelled and never blank or invented, in `internal/cashback/catalogue/copy_test.go`
- [ ] T109 [P] [US5] Test that adding a second network adapter changes no file outside its package (SC-008) in `internal/arch/network_isolation_test.go`

**Checkpoint**: The catalogue drives real clicks.

---

## Phase 8: User Story 6 - Operator reconciles what the network actually paid (Priority: P3)

**Goal**: Members are only paid out of commissions the business has actually received.

**Independent Test**: Import a statement that omits one approved transaction and shorts another; assert both are flagged and neither silently changes a confirmed balance.

- [ ] T110 [US6] Implement immutable statement import in `internal/cashback/ops/reconciliation.go` + `store/`
- [ ] T111 [US6] Implement difference detection for reported-not-paid, paid-not-reported and amount mismatch, in `internal/cashback/ops/differences.go`
- [ ] T112 [US6] Implement difference resolution recording who and why, with any member-facing effect as a new posting, in `internal/cashback/ops/resolve.go`
- [ ] T113 [US6] Implement `POST /ops/reconciliation/runs`, `GET /ops/reconciliation/runs/{id}/differences`, `POST /ops/reconciliation/differences/{id}/resolve` in `internal/cashback/ops/handlers.go`
- [ ] T114 [US6] Implement `GET /ops/exports/ledger` and `/exports/reconciliation` for accounting in `internal/cashback/ops/exports.go`
- [ ] T115 [P] [US6] Build the reconciliation queue pages in `web/src/pages/ops/reconciliation.astro`
- [ ] T116 [P] [US6] Integration test: an omitted and a shorted transaction are both flagged with deltas and change no balance, in `internal/cashback/ops/reconciliation_integration_test.go`

**Checkpoint**: The business knows what it was actually paid.

---

## Phase 9: User Story 7 - Abuse and self-dealing are contained (Priority: P3)

**Goal**: Suspicious clicks and credits are held for review, not auto-credited, and holds are explainable.

**Independent Test**: Replay abusive click and transaction patterns; assert each lands in the review queue naming its rule, and a normal pattern passes untouched.

- [ ] T117 [US7] Implement the privacy-minimised context digest used by abuse rules in `internal/cashback/clickout/context.go`
- [ ] T118 [US7] Implement configurable hold rules evaluated at ingestion in `internal/cashback/earnings/holdrules.go`
- [ ] T119 [US7] Implement `GET /ops/held`, `POST /ops/held/{id}/release`, `/reject` requiring a non-blank reason, in `internal/cashback/ops/held.go`
- [ ] T120 [P] [US7] Build the held-entries queue in `web/src/pages/ops/held.astro`
- [ ] T121 [P] [US7] Test that a held entry is never credited until released and that release/reject record a named human and reason, in `internal/cashback/earnings/holdrules_test.go`
- [ ] T122 [P] [US7] Test that a blank reason on any operator action is rejected with 400, in `internal/cashback/ops/handlers_test.go`

**Checkpoint**: Fraud has a queue, not a payout.

---

## Phase 10: Polish & Cross-Cutting Concerns

- [ ] T123 Stamp `noindex`/`nofollow` on all cashback routes and exclude them from sitemaps in `web/src/middleware.ts`
- [ ] T124 [P] Document every cashback route in `api/openapi.json` and make the route-coverage test cover them, in `cmd/apivo/openapi_routes_test.go`
- [ ] T125 [P] Add the OpenAPI schema test asserting every money field is `{minor, currency}` and no decimal appears, in `cmd/apivo/openapi_money_test.go`
- [ ] T126 Handle `identity.account.deleted` by closing participation and flagging in-flight withdrawals, never deleting financial rows, in `internal/cashback/wallet/account_lifecycle.go`
- [ ] T127 [P] Implement the fixture-brand render test across every member-facing surface (SC-007) in `web/src/lib/brand/rebrand.test.ts`
- [ ] T128 [P] Drive the brand-literal lint to zero exceptions across `internal/`, `web/` and `internal/platform/db/migrations/`
- [ ] T129 [P] Wire quickstart scenarios V1–V10 as automated tests in `internal/cashback/scenarios/`
- [ ] T130 [P] Add the seed command backing `make cashback-seed` in `cmd/apivo/seed_cashback.go`
- [ ] T131 Bring Go statement coverage for `internal/cashback/...` and `internal/platform/{money,brand,events,scheduler}` to ≥90% and TypeScript to ≥80%
- [ ] T132 [P] Add the cashback runbook (poller stalls, zero-sum alarm, stuck payout, network outage) to `docs/RUNBOOK.md`
- [ ] T133 [P] Document the cashback deployment topology and the Blnk/Redis operational contract in `docs/ENVIRONMENTS.md`
- [ ] T134 Run the full quickstart validation and record which checks ran locally versus in CI, in the PR description

---

## Dependencies & Execution Order

### Phase dependencies

- **Phase 1 (Setup & Spikes)**: no dependencies — starts immediately. **T006–T008 gate everything**: a failed spike revises ADR-0002 before code depends on it.
- **Phase 2 (Foundational)**: depends on Phase 1. **Blocks every user story.**
- **Phase 3 (US2)**: depends on Phase 2.
- **Phase 4 (US1)**: depends on Phase 3 — attribution needs ingested evidence. **This is the MVP boundary.**
- **Phases 5–9 (US3, US4, US5, US6, US7)**: all depend on Phase 4 and are otherwise independent of each other.
- **Phase 10 (Polish)**: depends on the stories being delivered.

### Cross-story dependencies (stated honestly)

| Story | Needs | Why |
|---|---|---|
| US1 | US2 | A credit cannot exist without evidence (C-2) |
| US1 | T048 | Click-out needs a readable offer |
| US3 | US1 | The wallet renders entries US1 creates |
| US4 | US3 | Withdrawal draws on confirmed balance |
| US6 | US2 | Reconciliation compares against ingested evidence |
| US7 | US1, US2 | Hold rules act on clicks and ingested transactions |
| US5 | — | Independent; only enriches the catalogue US1 already reads minimally |

### Within each phase

- Migrations before the stores generated from them.
- **T135 before T017/T018** (the dispatcher needs the envelope columns) and
  **T136 before T059, T092, T113, T119 and every other `/ops/*` task** (the
  operator role must exist before authority can be enforced).
- Invariant tests land in the **same PR** as the constraint they assert — never later.
- Port definitions before adapters; the conformance suite before the second adapter.
- Services before endpoints; endpoints before pages.

---

## Parallel Opportunities

**Phase 1** — T002, T003, T004 (three deployment targets), then T009, T010 alongside the spikes.

**Phase 2** — the biggest fan-out in the plan:

```text
Track A (platform):   T013 money │ T014 brand │ T015 web brand │ T017 events │ T019 scheduler
Track B (schema):     T020 → T021 → T022 → T023 → T024 → T025 → T026 → T027 (sequential — one schema)
Track C (invariants): T031 … T037 (after their migration lands; independent of each other)
Track D (ledger):     T042 memory │ T043 blnk │ T044 postgres (after T041)
Track E (boundaries): T038 arch test │ T039 migration lint
```

**Phase 3** — T050 (fixture adapter) and T051 (conformance suite) in parallel after T049; T060, T061, T062 in parallel at the end.

**Phase 4** — T072–T076 all parallel once the implementation tasks land.

**Phases 5–9** — once Phase 4 is done, five worktree agents can take US3, US4, US5, US6 and US7 concurrently. Each owns disjoint packages and pages.

**Phase 10** — T124, T125, T127, T128, T129, T130, T132, T133 are all independent.

---

## Implementation Strategy

### MVP first

1. Phase 1 — infrastructure and spikes. **Stop if a spike fails**; revise ADR-0002 first.
2. Phase 2 — foundation. The invariants exist before the first row.
3. Phase 3 — evidence ingestion on fixtures.
4. Phase 4 — 🎯 **MVP**: click-out to confirmed credit, provably correct.
5. **Validate**: run quickstart V1, V2, V3 and V7. The ledger balances or nothing ships.

### Incremental delivery after MVP

Phase 5 (wallet, so a member can see it) → Phase 6 (withdrawal, so the loop closes) → Phases 7–9 in any order → Phase 10.

The first demonstrable product is Phases 1–5: a member earns and sees cashback. The first *complete* product is Phases 1–6.

### Parallel agent strategy

Per the repository's working style — worktree-isolated agents, one small PR per issue, `gh stack` for dependent chains:

- Phase 1: one agent on spikes (T006–T008), one on deployment (T002–T005).
- Phase 2: four agents on tracks A, B, C+D, E above.
- Phase 3–4: sequential; they are one causal chain and splitting them costs more than it saves.
- Phases 5–9: five agents, one per story.

### Founder-blocked items inside this backlog

- **Q1 (networks)** — T050 ships the `fixture` adapter; the first real adapter is a later self-contained PR that changes nothing outside its package.
- **Q4 (share and rounding)** — T068 reads both from configuration; no value is committed.
- **Q5 (rails and threshold)** — T089/T090 ship `manual` and `stub`; a real rail is one implementation behind the port.
- **Q3, Q6, Q7, Q8, Q9** — defaults recorded in [research.md](research.md) §7; none blocks a task here.

---

## Notes

- Every task lands with its tests; the constitution's coverage gates are not a final phase.
- Commit atomically — one logical change per commit, each independently revertable.
- Any task that touches a legally load-bearing rule lands its database-rejection test in the same PR.
- Stop at any checkpoint to validate the story independently.

---

## Phase 11: Convergence

Appended 2026-08-31, after founder question **Q1 was answered: Awin**
(spec.md, Clarifications 2026-08-31). ADR-0003 built the port and deferred
only the choice, so nothing here changes the architecture — it fills the one
gap the answer opens, plus one the assessment found beside it.

Existing tasks are untouched and unrenumbered. Phases 6–10 already carry
tasks for withdrawals, catalogue, reconciliation, abuse and polish; they are
unbuilt but not un-tasked, so nothing is repeated here. Frontend work is
excluded by instruction, not because it is done.

**The Awin adapter (Q1, ADR-0003, SC-008).** Nothing outside the adapter
package may learn Awin's vocabulary — the architecture test is what proves
SC-008 rather than asserting it.

- [x] T137 Implement the Awin HTTP transport in `internal/cashback/networks/awin/client.go`: authenticated requests, the per-account rate limit read from `network.rate_limit_per_minute`, and retries over the existing backoff helpers. Credentials come from the environment and are never written to the repository or the database (ADR-0003) per spec Q1
- [x] T138 Implement `BuildDeeplink` in `internal/cashback/networks/awin/deeplink.go`, placing the issued click reference in the parameter the route names rather than a literal, per FR-021
- [ ] T139 Implement `FetchTransactions` in `internal/cashback/networks/awin/transactions.go`: Awin's transaction API mapped to `Reported`, carrying the verbatim raw payload so a wrong normalisation is re-derived from stored evidence and never re-fetched (ADR-0003, C-3), with windows capped at Awin's documented 31 days per FR-031 (missing)
- [ ] T140 Implement the Awin status mapping in `internal/cashback/networks/awin/status.go` — their vocabulary onto `pending → confirmed | declined` with `reversed` reachable from either — unit-tested against recorded fixtures per ADR-0003 (missing)
- [x] T141 Implement `FetchCatalogue` in `internal/cashback/networks/awin/catalogue.go`, mapping Awin's programme feed to `ReportedMerchant` per FR-012
- [ ] T142 Record real Awin responses as redacted fixtures under `internal/cashback/networks/awin/testdata/`, so every test above and all of CI run with no live credentials, per ADR-0003 (missing)
- [ ] T143 Wire the adapter: add the driver constant to `internal/platform/config/cashback.go`, the case to `networkAdapter()` in `cmd/apivo/networks.go`, and document `NETWORK_DRIVER=awin` with `NETWORK_ACCOUNT_ID` and `NETWORK_API_KEY` in `deploy/k8s/README.md` per spec Q1 (missing)
- [ ] T144 Run the shared conformance suite in `internal/cashback/networks/conformance_test.go` against the Awin adapter, so the port holds it to the same contract the fixture adapter passes — this is the SC-008 proof (missing)

**Connecting a publisher account.** The schema has carried `network` and
`network_account` since 0011 and `connectNetwork` requires both, but nothing
in the tree ever writes them: no endpoint, no admin command, no seed. A
deployment configured for Awin would log "no publisher account is connected"
and ingest nothing, with hand-written SQL against production as the only
remedy. T130 is the local `make cashback-seed` and is not this.

- [x] T145 Provide a supported, idempotent way to connect a network publisher account in a deployed environment — the `cashback.network` row with its documented `max_query_window_days` and `rate_limit_per_minute`, and the `network_account` row the cursors hang off — per plan Phase E and FR-030/FR-031

**Nothing settles a payout.** Found while building T100. `cashback.payout`
has a `settled` state, a `settled_at` column and
`payout_settled_iff_settlement_time` tying them together;
`RecordPayoutOutcome` can write it; `Rail.Status` exists to be asked. Nothing
calls any of it, so no payout ever leaves `submitted` and the only outcome
reachable is `failed`. Phase 6's checkpoint says the money loop closes — it
does not: money leaves and nothing observes it arriving. `paid_out` on
`GET /wallet` is read from settled payouts (T077, landed with T078), so it
reports zero for every member forever, and `cashback.withdrawal`'s `paid`
state is unreachable. That is a wrong number on a member-facing screen rather
than a missing feature.

- [x] T146 Implement the settlement sweep: for each payout in `submitted`, ask its rail `Status` and record the answer — `settled` moves the payout and its request to `paid` and announces `cashback.payout.settled`; `failed` takes the release path `Retries.giveUp` already implements; anything else leaves it. In `internal/cashback/payout/settle.go`, scheduled from `cmd/apivo`, per FR-053 and contracts/events.md

- [x] T147 Implement `POST /ops/withdrawals/{id}/settle`, the operator recording that a payout they carried out by hand has landed — the manual rail's only settlement path, since its `Status` always answers `submitted` by design — in `internal/cashback/ops/withdrawals.go` and `internal/cashback/payout/settle.go`, per FR-053 and FR-061
