# Tasks: epiloYES Alpha

**Input**: Design documents from `/specs/001-epiloyes-alpha/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/http-api.md

**Organization**: grouped by plan phase; each phase maps onto its GitHub
milestone (M1–M5). Every task becomes one GitHub issue and one PR.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: parallelisable — different files, no dependency on unfinished tasks
- **[US#]**: the user story from spec.md the task serves

## Phase A — M1 Foundations

- [ ] T001 [P] Multi-stage Dockerfile for the Go binary in `Dockerfile`: distroless nonroot final stage, `apivo healthcheck` subcommand wired as HEALTHCHECK
- [ ] T002 [P] Multi-stage Dockerfile for the Astro node-adapter server in `web/Dockerfile`, non-root, healthcheck
- [ ] T003 Compose services `api` and `web` joining postgres in `docker-compose.yml`; api on the internal network only (contract topology)
- [ ] T004 [P] [US1] Crawler gate middleware in `web/src/middleware.ts`: 403 for known crawler/AI/archive user agents, `robots.txt` disallow-all, `X-Robots-Tag` on every response (FR-013, D6)
- [ ] T005 [P] `wrangler.toml` for Cloudflare Containers, `jurisdiction: eu`, both images
- [ ] T006 [P] Kubernetes manifests under `deploy/k8s/` (Deployment, Service, Ingress, HPA, ConfigMap, Secret stubs) + kubeconform job in CI
- [ ] T007 [P] `supabase gen types` wiring: generated `web/src/lib/database.types.ts` + CI drift job mirroring sqlc-drift
- [ ] T008 Vitest with 80% coverage threshold in `web/`, activated with the middleware tests from T004

## Phase B — M2 Ingestion & Provenance

- [ ] T009 [P] [US4] Identity module `internal/identity`: JWKS-validated Supabase JWTs (jwx), subject→account mapping, editor-role check (D4)
- [ ] T010 Migration 0002 in `internal/platform/db/migrations/`: `account.role` + editor-approver trigger, withdrawal columns + article-guard v2 + one-per-origin index rebuild (`where withdrawn_at is null`), `translation.cost_microusd` (NOT NULL, no default) + `translation_spend` ledger, `place.slug` + Munich/Greece seeds, `source.active` — each rule with its DB-rejection test in `invariants_test.go`
- [ ] T011 [P] [US2] Feed normalisation in `internal/ingestion`: gofeed adapter (D1) + deterministic extract-derivation rule (D9), table-driven tests
- [ ] T012 [US2] Provenance write path in `internal/ingestion`: single-transaction `source_item` insert onto the snapshot/hash constraints, dedupe handling, `item.retrieved` domain events (I-2, I-4)
- [ ] T013 [US2] Poll loop in `internal/ingestion`: per-source ticker with jitter, conditional GET, `source.active` respected (D2)
- [ ] T014 [US2] `POST /api/v1/editorial/sources` in `internal/editorial` + platform routing: contract tests for 201/400/401/403/409, usage rule not accepted as input

## Phase C — M3 Translation & Editorial

- [ ] T015 [US3] `translation.Translator` interface + OpenAI-compatible adapter in `internal/translation/providers/openaicompat`: configurable base URL + model + key, covers Groq-class hosts, self-hosted vLLM and OpenAI alike (founder direction 2026-08-14)
- [ ] T016 [US3] Pilot evaluation harness: run the adapter across candidate models on ~50 real feed items (el→de, de→el), produce a quality/cost report for the founder's model pick
- [ ] T017 [US3] Cost controls in `internal/translation`: per-article ceiling, monthly cap against `translation_spend` in-transaction, pipeline halt + `pipeline.halted` event (FR-006; caps $0.02 / $25)
- [ ] T018 [US3] Translation write path: prompt versioning, immutable `translation` rows with cost recorded, `item.translated` events (I-5)
- [ ] T019 [US4] `GET /api/v1/editorial/queue` incl. withdrawn-origin correction candidates, contract tests
- [ ] T020 [US4] `POST /api/v1/editorial/approvals` + `POST .../articles/{id}/publication`: DB-enforced approver, one-per-origin 409 semantics, domain events, contract tests (I-1)
- [ ] T021 [US4] `POST /api/v1/editorial/articles/{id}/withdrawal`: record-preserving withdrawal + audited who/why (FR-016)
- [ ] T022 [US4] `/editor` pages in `web/src/pages/editor/`: queue, approve, withdraw, via Supabase Auth JS SDK and the user's JWT (D7)

## Phase D — M4 Reader

- [ ] T023 [P] [US1] Reader queries in `internal/content` via sqlc: front-page feed by language+places, article detail, published-and-not-withdrawn semantics
- [ ] T024 [US1] `GET /api/v1/front` + `GET /api/v1/articles/{id}` with column-backed headline/extract (translated and untranslated paths), contract tests
- [ ] T025 [US1] Locale-scoped front page `web/src/pages/[lang]/[place]/index.astro` + attribution component; only el and de mounted (FR-015)
- [ ] T026 [US1] Article page with rendered attribution and source link (SC-008)
- [ ] T027 [US1] CDN cache headers + Lighthouse SC-007 measurement setup (p75 LCP < 2 s, throttled 4G)

## Phase E — M5 Hardening

- [ ] T028 [US5] `GET /api/v1/editorial/articles/{id}/provenance` from `article_provenance` + the timed I-5 audit drill on randomly picked items (SC-002)
- [ ] T029 [P] [US6] Consent capability integration tests: grant/revoke/re-grant history, guard rejections, `is_entitled` behaviour
- [ ] T030 [P] Security review of auth paths (JWT validation, editor gating, secret handling)
- [ ] T031 [US6] Registration UI + consent capture + `reader_place` follows — first on the cut list; schema capability ships regardless
- [ ] T032 Load sanity pass + SC-007 measured and recorded in the PR
- [ ] T033 Alpha definition-of-done drill (§10): a Munich reader journey walked end to end, all four provenance facts recovered by query in under five minutes

## Dependencies

- T003 ← T001, T002 · T008 ← T004 · T012 ← T010, T011 · T013 ← T012
- T014 ← T009, T010 · T017/T018 ← T010, T015 · T019–T021 ← T009, T010
- T022 ← T019–T021 · T024 ← T023 · T025/T026 ← T024 · T033 ← everything
- T016 report gates the production model choice (founder pick; tracked in the blocked-on-founders issue)
