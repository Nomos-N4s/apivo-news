# Quickstart: epiloYES Alpha

**Feature**: [spec.md](spec.md) | **Date**: 2026-08-14

Developer setup lives in the repository [README](../../README.md) (hooks,
Postgres via compose, test commands, coverage gates). This file adds the
alpha-specific flows once the feature lands.

## Run the stack locally

```sh
git config core.hooksPath .githooks     # once
docker compose up -d --wait postgres
DATABASE_URL="postgres://apivo:apivo@localhost:5432/apivo?sslmode=disable" go run ./cmd/apivo
cd web && npm ci && npm run dev         # frontend on :4321, API on :8080
```

## Exercise the alpha end to end (once implemented)

1. **Add a source** (editor JWT): `POST /api/v1/editorial/sources` with a
   licensed Greek or Munich feed. Verify the created source shows
   `usage_rule: extract_and_link` — you cannot set anything else.
2. **Let ingestion poll** (or trigger a poll in dev): retrieved items
   appear with full provenance; re-polling creates no duplicates.
3. **Translate**: items gain headline+extract translations carrying
   model, prompt version and cost.
4. **Approve**: `POST /api/v1/editorial/approvals` as a named editor;
   verify an approval without an editor account is impossible (the
   database refuses).
5. **Read**: open `/el/munich` — the front page shows the item in Greek
   with attribution and a working link to the original.
6. **Audit**: `GET /api/v1/editorial/articles/{id}/provenance` — source,
   licence snapshot, model, prompt version, approver, in one call.
7. **Withdraw**: `POST .../withdrawal` with a reason; the item leaves the
   site, every record remains, the audit shows who and why.

## Verifying the invariants by hand

```sh
DATABASE_URL=... go test -run 'TestDatabaseRejectsIllegalWrites|TestSourceItemIsImmutable|TestTranslationIsImmutable|TestImmutableTablesRejectTruncate|TestArticleProvenanceView' ./internal/platform/db/ -v
```

Every invariant has a test that attempts the illegal write and requires
Postgres to reject it. If one of these ever fails, stop shipping.
