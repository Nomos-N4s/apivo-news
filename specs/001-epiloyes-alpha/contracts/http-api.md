# HTTP API Contract: epiloYES Alpha

**Feature**: [../spec.md](../spec.md) | **Date**: 2026-08-14

The Go binary serves this API. The Astro frontend is its first consumer
(server-side); the editorial pages are the second. Every endpoint gets a
contract test asserting status codes, shape, and auth behaviour before its
implementation lands (constitution: tests are part of the definition of
each endpoint, and integration tests run against real Postgres).

Conventions

- Base path `/api/v1`. JSON bodies, UTF-8.
- Errors: RFC 9457 problem+json (`type`, `title`, `status`, `detail`).
- Auth: `Authorization: Bearer <Supabase JWT>` where marked. The Go side
  validates the token signature and maps the subject to an `account` row;
  editorial endpoints additionally require the editor role (enforced again
  by the database on write).
- Pagination: `limit` (default 20, max 100) + `cursor` (opaque). Lists
  return `{ "items": [...], "next_cursor": string|null }`.

## Public (reader) endpoints

### GET /api/v1/front

Locale-scoped front page feed.

- Query: `lang` (required, `el`|`de`), `place` (required, place slug;
  repeatable — diaspora readers follow two places), `limit`, `cursor`.
- 200: items of published, non-withdrawn articles, newest first:
  `{ id, headline, extract, lang, places: [slug], attribution, source_url, published_at }`
  where `headline`/`extract` come from the translation when the article
  originates from one, otherwise from the retrieved item (same-language).
- 400: unknown `lang` or `place`. Never 500 on empty results — empty list.

### GET /api/v1/articles/{id}

- 200: the article page payload: same shape as front items plus
  `approved_at`. Withdrawn or unpublished → 404 (existence of unpublished
  work is not public).
- 404: unknown id.

## Editorial endpoints (JWT + editor role)

### GET /api/v1/editorial/queue

Review queue: retrieved items and their translations that have no article
yet.

- Query: `lang` (optional filter), `limit`, `cursor`.
- 200: `{ items: [{ source_item_id, translation_id|null, source_name,
  headline_original, headline_translated|null, extract_translated|null,
  retrieved_at, licence_snapshot }] }`
- 401 without token; 403 for non-editors.

### POST /api/v1/editorial/approvals

Approval creates the article — the database enforces the named-editor
rule; this endpoint merely carries the intent.

- Body: `{ "translation_id": uuid }` XOR `{ "source_item_id": uuid }`,
  plus `{ "attribution": string, "publish": bool }`.
- 201: `{ article_id, approved_by, approved_at, published_at|null }`.
- 400: both or neither origin supplied; blank attribution.
- 403: token subject is not an editor (mirrors the DB trigger).
- 409: origin already has an article.
- Side effect: `article.approved` (+ `article.published` when publish)
  domain events in the same transaction.

### POST /api/v1/editorial/articles/{id}/withdrawal

Withdrawal ends publication and preserves every record (FR-016).

- Body: `{ "reason": string }` (required, non-blank).
- 200: `{ article_id, withdrawn_at, withdrawn_by }`.
- 404: unknown or never-published article; 409: already withdrawn.
- Side effect: `article.withdrawn` domain event (who, why) in the same
  transaction.

### POST /api/v1/editorial/sources

- Body: `{ name, url, language, jurisdiction, licence_terms }`. The
  usage rule is NOT accepted here: new sources are always
  `extract_and_link` (upgrades are a separate founder-gated flow, out of
  alpha scope).
- 201: the source. 409: duplicate feed URL.

### GET /api/v1/editorial/articles/{id}/provenance

The five-minute audit, served from `article_provenance` (I-5).

- 200: `{ article_id, source: { name, feed_url, jurisdiction, usage_rule },
  source_item: { source_url, retrieved_at, content_hash, licence_snapshot,
  original_author|null }, translation: { model, prompt_version, target_locale,
  generated_at }|null, approval: { approver_name, approver_email, approved_at },
  published_at|null, withdrawal: { withdrawn_at, withdrawn_by, reason }|null }`
- Works for withdrawn articles — audit sees full history.

## Operational endpoints (existing)

- `GET /healthz` — liveness. `GET /readyz` — readiness (DB ping).

## Non-goals of this contract

No search, no comments, no user-generated content, no image handling, no
full-text bodies in any public payload (extract-and-link only). Reader
registration endpoints ship only if the registration UI survives the cut
line; the schema capability exists regardless.
