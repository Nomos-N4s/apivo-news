# Implementation Plan: epiloYES Alpha — Licensed, Translated Local News for the Greek Diaspora

**Branch**: `001-epiloyes-alpha` | **Date**: 2026-08-14 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/001-epiloyes-alpha/spec.md`

## Summary

Deliver the alpha end to end: licensed RSS/Atom feeds are polled with
provenance captured at retrieval; headline+extract translations carry full
model lineage under hard cost caps; a named editor approves (and can
withdraw) every item, enforced by the database; a Munich reader gets a
locale-scoped Greek front page with attribution and source links; and any
published item's full provenance is one query away. The invariant core
already exists in migration 0001 on the foundations branch — this plan
builds the pipeline and reader on top of it, plus migration 0002
([data-model.md](data-model.md)), the HTTP contract
([contracts/http-api.md](contracts/http-api.md)), and the deployment
artefacts the constitution requires (containers first; Cloudflare today,
Kubernetes-ready).

## Technical Context

**Language/Version**: Go 1.26 (backend); TypeScript 5.9 strict / Node 24 (frontend)

**Primary Dependencies**: pgx/v5, golang-migrate, sqlc 1.31.1, gofeed (D1),
lestrrat-go/jwx v3 (D4); Astro 7 + @astrojs/node, Supabase Auth JS SDK

**Storage**: Postgres — Supabase (EU region) in production, postgres:17 via
compose locally and as a CI service container

**Testing**: `go test` (table-driven; invariant + contract + integration
against real Postgres, 90% gate); vitest + astro check (80% gate once
frontend logic lands)

**Target Platform**: Linux containers — Cloudflare Containers
(`jurisdiction: eu`) now, plain Kubernetes manifests kept valid in parallel

**Project Type**: web application — Go modular monolith API + Astro SSR frontend

**Performance Goals**: front page renders < 2 s (SC-007) via SSR + CDN
caching with explicit cache headers; API p95 < 200 ms locally at alpha scale

**Constraints**: invariants I-1..I-5 stay database-enforced; no scraping;
extract-and-link only; translation halts at monthly cap (never overspends);
all crawling blocked at the single frontend gate with the API
non-publicly-routable and noindex-stamped (D6, pending founder sign-off of
the placement); nothing platform-specific in application code

**Scale/Scope**: alpha — a handful of feeds, ~100 items/day, hundreds of
readers; no horizontal-scale work

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Status | Evidence in this plan |
|---|---|---|
| I. Sole authorship & signed commits | PASS | Workflow unchanged: signed sole-author commits, one PR per issue, hygiene enforced by hook + CI |
| II. I-1 human approval | PASS | Approval only via `article` insert; 0002 tightens approver to editor role via DB trigger; endpoint mirrors, never replaces, the DB rule |
| III. I-2 provenance at retrieval | PASS | Ingestion writes `source_item` in one transaction; no deferred provenance path exists in the design |
| IV. I-3 immutable evidence | PASS | No plan item touches the triggers; withdrawal (FR-016) adds columns to `article`, never edits evidence |
| V. I-4 licence snapshot | PASS | Snapshot column written at retrieval from `source.licence_terms`; per-item, never blank |
| VI. I-5 traceability | PASS | Provenance endpoint reads the `article_provenance` view; 0002 extends the view with withdrawal history |
| VII. Language ⊥ place | PASS | API takes `lang` and `place` as separate parameters; routes carry both explicitly; no combined locale value anywhere |
| VIII. DB-enforced invariants | PASS | Every new rule that is legally load-bearing (editor role, withdrawal integrity) lands as constraint/trigger with a DB-rejection test |
| Architecture constraints | PASS | Monolith modules only; consumer-defined interfaces wired in `cmd`; types generated both sides (sqlc + supabase gen types, both drift-checked); containers first; platform bindings stay in `internal/platform` |
| Alpha scope | PASS | No out-of-scope feature appears in any phase; registration UI is last and first to cut |

**Post-Phase-1 re-check**: PASS — the data-model deltas and contract
introduce no violation; the one openly-named trade (D6: origin-level
robots enforcement rather than literal CDN edge, for portability) is a
deployment concern, not a constitutional one, and is flagged for founder
review.

## Project Structure

### Documentation (this feature)

```text
specs/001-epiloyes-alpha/
├── spec.md
├── plan.md              # this file
├── research.md          # decisions D1–D10 + priced provider shortlist
├── data-model.md        # 0001 recap + 0002 deltas + FR traceability
├── quickstart.md
├── checklists/requirements.md
└── contracts/http-api.md
```

### Source Code (repository root)

```text
cmd/apivo/                  # composition root: wiring, routes, pollers
internal/
├── ingestion/              # gofeed adapter, normalisation, provenance tx, poll loop
├── translation/            # Translator interface, providers/<name>/, cost ledger, prompt versions
├── editorial/              # queue queries, approval, withdrawal
├── content/                # reader queries (front, article), locale resolution, store (sqlc)
├── identity/               # JWT validation (jwx), account mapping, entitlement checks
└── platform/               # config, logging, db+migrations, http primitives
web/
├── src/pages/              # /{lang}/{place}/ front page, article pages, /editor
├── src/middleware.ts       # D6: robots.txt + X-Robots-Tag, the one indexing gate
└── src/lib/                # supabase client, generated database.types.ts
deploy/
└── k8s/                    # Deployment/Service/Ingress/HPA/ConfigMap/Secret stubs
Dockerfile                  # Go binary (distroless, non-root, healthcheck)
web/Dockerfile              # Astro node adapter (standalone)
wrangler.toml               # Cloudflare Containers, jurisdiction eu
```

**Structure Decision**: the existing modular-monolith layout absorbs the
feature; no new top-level components. Each module keeps its internals
private; `cmd/apivo` wires modules through interfaces the consumers
define. Deployment artefacts are additive files, not code changes.

## Phases

Phases map onto the backlog milestones (M1–M5). Every phase leaves CI
green and coverage gates satisfied; invariant-bearing work always lands
with its database-rejection test in the same PR.

### Phase A — Platform completion (M1 Foundations)

Deployment and delivery mechanics the constitution promises:
multi-stage Dockerfiles (distroless/scratch, non-root, healthcheck via the
binary's own health subcommand), compose gains the two app services,
`wrangler.toml` for Cloudflare Containers (EU jurisdiction), plain k8s
manifests validated by kubeconform in CI, the D6 indexing middleware, the
supabase-gen-types wiring with its drift check, and vitest with the 80%
threshold activated alongside the first real frontend logic.

### Phase B — Ingestion & provenance (M2)

Opens with the identity module (D4: jwx JWKS validation, sub→account
mapping, editor-role check) — it precedes the first authenticated
endpoint, and its absence would otherwise be discovered mid-phase. Then:
migration 0002 (editor role + trigger, withdrawal columns, cost ledger,
place slugs + seeds, source.active) with invariant tests extended
accordingly; the ingestion module: gofeed normalisation (D1), single-
transaction provenance writes onto 0001's constraints (title captured with
the content), dedupe, conditional GET, poll loop (D2); `POST
/editorial/sources` with contract tests incl. 401/403; domain events for
retrievals.

### Phase C — Translation & editorial (M3)

The `Translator` interface, the founder-chosen provider adapter (decision
at this plan's review), prompt versioning, per-article ceiling and monthly
cap against the ledger with pipeline-halt semantics and `pipeline.halted`
events; editorial queue/approval/withdrawal endpoints per the contract
with the DB enforcing approver identity; `/editor` pages in Astro behind
Supabase Auth (D7).

### Phase D — Reader (M4)

Reader API (`/front`, `/articles/{id}`) from the content module;
locale-scoped Astro pages `/{lang}/{place}/`, attribution component,
cache headers for CDN; place seed hierarchy live; SC-007 measured.

### Phase E — Hardening (M5)

The timed I-5 audit drill on randomly picked items (SC-002); provenance
endpoint; the SC-007 measurement (p75 LCP < 2 s, Lighthouse throttled 4G,
recorded in the PR); consent capability verified at the schema level by
DB integration tests — consent grant/revoke endpoints ship only together
with the registration UI, which is first on the cut list; load sanity
pass; security review of auth paths; founder-blocked tracking issue kept
current (retention schedule before public launch).

## Founder decision needed at this review

- **Translation provider** — pick from the priced shortlist in
  [research.md](research.md) (D5). The adapter interface, caps and ledger
  land regardless; only the first `providers/<name>` implementation waits
  on this.
- **Cap values**: per-article ceiling and monthly cap numbers (config
  values; defaults proposed in research.md alongside the shortlist).
- **Crawler gate placement (D6)**: your recorded decision says "at the
  edge"; the plan implements one enforcement point that ships inside the
  frontend artefact (portable to every deployment target), with the API
  non-publicly-routable and noindex-stamped, and Cloudflare-level rules
  as an additive fence. Approve this reading of "one place", or require a
  literal CDN-edge rule (which would not travel to Kubernetes).

## Complexity Tracking

No constitution violations to justify. The single named trade-off (D6
origin-level enforcement of the indexing block, chosen for portability
over literal CDN-edge placement) is documented in research.md and
surfaced for founder review rather than silently decided.
