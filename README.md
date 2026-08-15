# Apivo News — epiloYES

A multilingual local newspaper for Greek communities abroad. A Greek speaker
in Munich reads Munich news in Greek: **language and place are independent
axes**, and the entire product follows from that.

Alpha scope: Greek and German; Munich as reader locale; Greek national and
Munich local RSS/Atom feeds as sources; translated headline and extract
linking back to the source; human approval on every item; full provenance.

## The invariants

Content licensing carries real legal exposure, so these are enforced **by the
database**, not by application code. Each one has a test that attempts the
illegal write and requires Postgres to reject it
([invariants_test.go](internal/platform/db/invariants_test.go)):

- **I-1** — An article cannot exist without a named human approver
  (`article.approved_by` is `NOT NULL`).
- **I-2** — Provenance is captured at retrieval, in the same transaction as
  the content. A `source_item` without provenance is unrepresentable.
- **I-3** — `source_item` is immutable; it is legal evidence of what was
  retrieved and under what terms. Enforced by trigger.
- **I-4** — Licence terms are snapshotted at retrieval
  (`source_item.licence_snapshot`).
- **I-5** — Every published sentence is traceable to source, licence, model,
  prompt version and approver — one query against `article_provenance`.

New sources always default to `extract_and_link`; `full_text` requires
recorded written permission and is rejected without it.

## Architecture

| Area | Decision |
|---|---|
| Frontend | Astro (`web/`), TypeScript strict, `@astrojs/node` adapter |
| Backend | Go — a single binary, modular monolith (`cmd/apivo`) |
| Database | Supabase (Postgres), EU region; plain Postgres 17 locally |
| Auth | Supabase Auth: Astro uses the JS SDK, Go validates the JWT |
| Types | Generated from the schema — `sqlc` (Go), `supabase gen types` (TS). Never hand-written on both sides |
| Deployment | Container-first: Cloudflare Containers today, Kubernetes-ready |

Module boundaries (enforced by [arch_test.go](internal/arch/arch_test.go)):

```
cmd/apivo/main.go     composition root
internal/
  ingestion/     feed polling, normalisation, provenance capture
  translation/   LLM adapter behind an interface, lineage recording
  editorial/     review queue, approval, publish
  content/       articles, sources, locale resolution
  identity/      JWT validation, entitlement checks
  platform/      db, config, logging, http — shared primitives only
```

`platform/` may be imported by anyone. No other `internal/` package imports a
sibling's internals — modules communicate through interfaces defined by the
consumer, wired in `cmd`.

## The HTTP API

[api/openapi.json](api/openapi.json) is the machine-readable description of
the HTTP surface: OpenAPI 3.1, written from the handlers rather than from
the contract prose, so a client generator and the server agree. The binary
embeds it and serves it at `GET /api/v1/openapi.json` — a consumer can ask
the server it is actually talking to what it serves.

Two gates keep it honest: CI validates the document against the
specification with a pinned open-source validator (`make openapi-lint` runs
the same check locally and needs only Node), and a test in the composition
root compares the served document with the routes the modules register, in
both directions — an undocumented route and a documented endpoint nobody
serves both fail the build.

## Development

Prerequisites: Go ≥ 1.26, Node ≥ 22, Docker.

```sh
# one-time: route git hooks (commit hygiene) through .githooks
git config core.hooksPath .githooks

# full local stack: Postgres → Go API (migrates on start) → Astro frontend.
# The web app is the only published HTTP surface — http://localhost:4321;
# Postgres is also published on loopback (5432) for local tooling and tests.
# The API has no host port and is reached by service name on the compose
# network. All host bindings are loopback-only, so nothing listens on the LAN.
docker compose up --build

# or Postgres alone — enough for the tests and for iterating outside containers
docker compose up -d --wait postgres

# run everything, including the schema invariant tests
DATABASE_URL="postgres://apivo:apivo@localhost:5432/apivo?sslmode=disable" \
  go test -shuffle=on ./...

# frontend
cd web && npm ci && npm run check && npm run build
```

`make setup db-up test cover lint openapi-lint sqlc web-check` wraps the same steps (see
the [Makefile](Makefile)). On Windows, `-race` needs a C toolchain; run
`make test RACE=` or plain `go test` locally and let CI run the race
detector.

Configuration comes from the environment — see
[.env.example](.env.example). The binary migrates the database on start;
migrations live in
[internal/platform/db/migrations](internal/platform/db/migrations) and are
the single source of truth for the schema. After changing them, run
`make sqlc` and commit the regenerated `internal/content/store` (CI fails on
drift).

TypeScript types follow the same rule: `make ts-types` applies the
migrations to a scratch database and regenerates
[web/src/lib/database.types.ts](web/src/lib/database.types.ts) with the
Supabase CLI. Run it alongside `make sqlc` after every migration change and
commit the result (CI fails on drift here too).

## Cloudflare deployment

[wrangler.jsonc](wrangler.jsonc) declares both images — the Go API
(`Dockerfile`) and the Astro frontend (`web/Dockerfile`) — as Cloudflare
Containers pinned to `jurisdiction: "eu"` for GDPR residency. The
containers are the artefact; the only Cloudflare-specific glue is the
Worker shim in [deploy/cloudflare/worker.js](deploy/cloudflare/worker.js),
which routes all public traffic to the web container. The Go API has no
public route. Secrets (`DATABASE_URL`, later provider keys) are set with
`npx wrangler secret put <NAME>` and are never committed. CI validates the
configuration on every PR with
`wrangler deploy --dry-run --containers-rollout none` — a full parse and
Worker bundle needing no Cloudflare credentials; images build at deploy
time (`npx wrangler deploy`).

## Quality gates (CI-enforced)

- Go: `golangci-lint` (strict config), `go vet`, tests with `-race`,
  **minimum 90 % statement coverage** — the build fails below it.
  Machine-generated code (`internal/content/store`) is excluded from the
  metric and exercised by integration tests instead.
- Integration tests run against a real Postgres service container — the
  invariant tests are never skipped in CI.
- TypeScript: `strict` (Astro `strictest` preset), `astro check`, minimum
  80 % coverage once frontend logic lands.
- Every exported Go symbol is documented.

## Commit rules

- Conventional Commits, referencing the issue: `feat(ingestion): capture
  provenance at retrieval (#12)`.
- Every commit signed and verified.
- One PR per issue; nothing lands directly on `main`.
