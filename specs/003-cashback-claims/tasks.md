---

description: "Task list for Cashback Claims"
---

# Tasks: Cashback Claims

**Input**: Design documents from `/specs/003-cashback-claims/`

**Prerequisites**: [plan.md](plan.md), [spec.md](spec.md),
[data-model.md](data-model.md), [contracts/](contracts/), constitution v1.2.0
**plus the pending amendment** — see [spec.md](spec.md) §Governance Impact.

**Tests**: Included and mandatory. C-8, C-9 and C-10 each carry a test
asserting the DATABASE rejects the illegal state, by SQLSTATE, against a real
Postgres. Contract tests per endpoint. Go ≥90%, TypeScript ≥80%.

## Numbering

Task IDs continue from 002's ceiling (T147) rather than restarting at T001.
The repository's issue titles carry the ID (`CB-T082: …`) and the
tasks-to-issues deduplication matches on `\bT\d{3}\b` across the whole issue
history, so a second feature restarting at T001 would silently collide with
002's foundational tasks and skip its own. Continuing the sequence keeps every
ID globally unique in the tracker, which is where they are actually resolved.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel — different files, no incomplete dependency
- **[Story]**: which user story (US1–US4)

---

## Phase 1: Governance (Blocking — nothing else starts)

- [ ] T148 Draft the constitution amendment adding claims to the "Cashback alpha" scope block and C-8/C-9/C-10 to Principle IX, with a Sync Impact Report and a MINOR version bump, in `.specify/memory/constitution.md`
- [ ] T149 Record the founder's ratification decision and update [spec.md](spec.md) §Governance Impact from "blocked" to "resolved"

**Checkpoint**: the amendment is ratified, or this feature stops here.

---

## Phase 2: Schema and invariants (Blocking)

- [ ] T150 Write migration `internal/platform/db/migrations/0033_cashback_claims.{up,down}.sql` for `claim`, `claim_evidence`, `claim_correspondence`, their enums, indexes and immutability triggers per [data-model.md](data-model.md) §1–2, §4
- [ ] T151 Write migration `0034_cashback_claim_decisions.{up,down}.sql` for `claim_decision`, its outcome/classification enums, the C-10 check constraint, the `supersedes_id` uniqueness and the `raise_immutable()` trigger per §3
- [ ] T152 Write migration `0035_claim_provenance_view.{up,down}.sql` creating `cashback.claim_provenance` per §5
- [ ] T153 Add up/down round-trip tests for 0033–0035 to `internal/platform/db/migrations_test.go`
- [ ] T154 [P] Test that a blank or null `decided_by` is rejected by the database (**C-8**) in `internal/cashback/claims/invariants_test.go`
- [ ] T155 [P] Test that UPDATE, DELETE and TRUNCATE on `claim_decision` are rejected (**C-9**), same file
- [ ] T156 [P] Test that every illegal outcome/row combination is rejected by the C-10 check, and that the claim write path cannot create a `cashback.entry` without a network transaction (**C-10**), same file
- [ ] T157 Extend `sqlc.yaml` and generate stores into `internal/cashback/claims/store/`

**Checkpoint**: the illegal states are unrepresentable before the first row exists.

---

## Phase 3: Classification (Blocking for both paths)

- [ ] T158 [US2] Implement `classify.go` returning `attributable | adjustable | unevidenced` with the click, network transaction, existing entry and dated rate row it found, in `internal/cashback/claims/classify.go`
- [ ] T159 [P] [US2] Table-driven tests over all three cases and their boundaries — matching amount with no click, click with no transaction, transaction already attributed, rate table changed between purchase and claim — in `classify_test.go`
- [ ] T160 [US2] Implement the `remedies` derivation from a classification, in one place, used by both the read and the decide handler, in `remedies.go` + tests

---

## Phase 4: Member path

- [ ] T161 Define the `EvidenceStore` port (`Put`, `Get`, `Delete`) in `internal/cashback/claims/evidence.go` per [plan.md](plan.md) §Ports
- [ ] T162 [P] Implement the filesystem store in `internal/cashback/claims/evidence/fs/`
- [ ] T163 [P] Implement the S3-compatible store in `internal/cashback/claims/evidence/s3/`
- [ ] T164 Write the shared `EvidenceStore` conformance suite (round-trip, absent key, oversize refusal, concurrent put of the same hash) and run it against both implementations, in `evidence/conformance_test.go`
- [ ] T165 [US1] Implement `POST /claims` with reference generation, due-date computation and duplicate detection in `internal/cashback/claims/handlers.go`
- [ ] T166 [US1] Implement `POST /claims/{reference}/evidence` with content sniffing, hashing and size limits, same file
- [ ] T167 [US1] Implement `GET /claims`, `GET /claims/{reference}` and the evidence read, same file
- [ ] T168 [P] [US1] Contract tests for every member endpoint per [contracts/http-api.md](contracts/http-api.md), in `handlers_test.go`
- [ ] T169 [US1] Emit `cashback.claim.submitted` through the outbox in the same transaction as the insert (FR-109)

---

## Phase 5: Operator path

- [ ] T170 [US2] Implement `GET /ops/claims` — oldest first, with `expected_cashback` and classification per row — in `internal/cashback/claims/ops.go`
- [ ] T171 [US2] Implement `GET /ops/claims/{reference}` returning the claim, the records half and the served `remedies` array
- [ ] T172 [US3] Implement `POST /ops/claims/{reference}/decide`: reason mandatory, `decided_by` from the token, remedy checked against the served array, classification and records snapshot written onto the decision
- [ ] T173 [US3] Implement superseding decisions (`supersedes`), refusing a fork
- [ ] T174 [US4] Implement the goodwill outcome as a transfer from the configured house account, with `503` where none is configured, in `goodwill.go`
- [ ] T175 [US2] Implement `POST /ops/claims/{reference}/message` and the `awaiting_member` transition
- [ ] T176 [P] [US2] Contract tests for every operator endpoint, including the `decided_by`-in-body test and the remedy-refusal test per classification
- [ ] T177 [US3] Emit `cashback.claim.decided` on every decision, superseding ones included

---

## Phase 6: Frontend

- [ ] T178 [P] [US1] Typed claims client with fixture fallback and a `source` marker in `web/src/lib/cashback/claims.ts` + tests
- [ ] T179 [P] [US1] Claim copy — form labels, the three "what happens next" steps, every outcome and state — in the cashback translation catalogues, keyed by primary language subtag, no brand literal (FR-111)
- [ ] T180 [US1] Build the member claim form in `web/src/pages/[lang]/[place]/cashback/claim.astro`, phone-first and working at desktop width, per mockup 4e
- [ ] T181 [US3] Build the member claim record in `web/src/pages/[lang]/[place]/cashback/claims/[reference].astro`, showing every decision with its reason — declined as plainly as paid
- [ ] T182 [US2] Build the operator queue in `web/src/pages/ops/claims.astro`: list oldest-first, detail with the assertion left and our records right, remedies rendered from the served array, per mockup 4f
- [ ] T183 [P] Frontend test: a declined claim renders its reason and cannot be collapsed away, in `claims.test.ts`
- [ ] T184 [P] Frontend test: the operator screen renders exactly the remedies the API served and no others
- [ ] T185 Confirm the claim routes carry `noindex, nofollow` and add them to the middleware test (FR-112)

---

## Phase 7: Accounting and polish

- [ ] T186 [US4] Add goodwill as its own line in `GET /ops/exports/ledger`, separable by month, in `internal/cashback/ops/exports.go`
- [ ] T187 [US2] Compute and serve the median answer time against the five-day promise (SC-106)
- [ ] T188 [P] Document the claim routes in `api/openapi.json` and extend the route-coverage test
- [ ] T189 [P] Add the claims runbook section — a stuck queue, a goodwill spike, an evidence store outage — to `docs/RUNBOOK.md`
- [ ] T190 [P] Document `EVIDENCE_STORE_DRIVER`, its credentials and the goodwill house account in `.env.example` and `docs/ENVIRONMENTS.md`
- [ ] T191 Bring Go coverage for `internal/cashback/claims/...` to ≥90% and the new TypeScript to ≥80%
- [ ] T192 Wire SC-101 to SC-106 as automated checks in `internal/cashback/scenarios/`

---

## Dependencies

- Phase 1 blocks everything.
- Phase 2 blocks Phases 3–7: no handler is written against a schema whose
  invariants are not yet enforced.
- T158–T160 block T170–T174: the operator path serves what classification
  computes.
- T161 blocks T162, T163 and T166.
- T178 blocks T180, T181, T182.
