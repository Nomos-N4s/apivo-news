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

## Provision an editor (once per person)

Signing in is not the same as being an editor. Supabase Auth says who
someone is; the `account` row is what makes them a person this system can
name as an approver, and nothing creates it automatically. Until it
exists, `identity.Authenticate` finds no row for the token's subject and
answers 401 — a valid sign-in that every editorial call rejects.

`account.id` **must equal the Supabase Auth user id**. That id is the
`sub` claim of every token the project issues, and `Authenticate` looks
the account up by it directly; any other value is an account nobody can
authenticate as.

1. Configure both halves of auth: `JWKS_URL` on the api (it verifies the
   tokens) and `PUBLIC_SUPABASE_URL` + `PUBLIC_SUPABASE_ANON_KEY` on the
   web (it issues them). Left empty, the editorial routes are unmounted
   and the screens say they are a preview.
2. Create the person in Supabase Studio → Authentication → Users, and
   copy their user id.
3. In Studio's SQL editor, run this once per editor:

   ```sql
   insert into account (id, email, display_name, role)
   values ('<the Supabase user id>', 'eleni@example.org', 'Eleni Papadaki', 'editor');
   ```

   `display_name` is the name `article_provenance` reports as the approver
   of everything this person ever approves (I-1), so it is a real human
   name, not a handle or a team. `role` must be `editor`: the trigger from
   migration 0002 refuses an article whose approver is a reader.
4. Optionally, on the same user in Studio, set the metadata the editorial
   chrome reads — user metadata `{"display_name": "Eleni Papadaki"}` and
   app metadata `{"role": "editor"}`. This is display only: the web
   container never reads the `account` table, so without it the screens
   fall back to the email address and name the role as reader. Nothing is
   permitted by it — approval authority is checked by the database.

There is deliberately no `apivo` subcommand for this. Creating approvers
is a rare, founder-only act with legal weight, and a CLI that mints them
is a second path to the authority the database is the sole gate for.

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
