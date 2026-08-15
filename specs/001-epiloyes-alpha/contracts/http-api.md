# HTTP API Contract: epiloYES Alpha

**Feature**: [../spec.md](../spec.md) | **Date**: 2026-08-14

The Go binary serves this API. The Astro frontend is its first consumer
(server-side); the editorial pages are the second. Every endpoint gets a
contract test asserting status codes, shape, and auth behaviour before its
implementation lands (constitution: tests are part of the definition of
each endpoint, and integration tests run against real Postgres).

Topology

The Go API is **not publicly routable**: in every deployment (compose,
Cloudflare, Kubernetes) it listens on an internal network and the Astro
server is the only public HTTP surface, sitting behind the single crawler
gate (research D6). As defence in depth the API additionally stamps
`X-Robots-Tag: noindex, nofollow` on every response (implemented in the
platform HTTP server), so a misconfigured topology still exposes nothing
indexable.

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
  `{ id, headline, extract, lang, places: [slug], attribution, source_url, published_at }`.
  Column backing: for a translated origin, `headline` = `translation.headline`
  and `extract` = `translation.extract`; for an untranslated (same-language)
  origin, `headline` = `source_item.original_title` and `extract` is derived
  deterministically from `source_item.raw_body` by the D9 extract rule.
  Approval of an untranslated item whose feed provided no title is rejected
  (400) — there would be nothing to render as a headline.
- 400: unknown `lang` or `place`. Never 500 on empty results — empty list.

### GET /api/v1/articles/{id}

- 200: the article page payload: same shape as front items plus
  `approved_at`. Withdrawn or unpublished → 404 (existence of unpublished
  work is not public).
- 404: unknown id.

## Editorial endpoints (JWT + editor role)

### GET /api/v1/editorial/queue

Review queue: retrieved items and their translations that have no
non-withdrawn article yet — origins whose only articles are withdrawn
reappear here as correction candidates, flagged with their withdrawal
history.

- Query: `lang` (optional filter), `limit`, `cursor`.
- 200: `{ items: [{ source_item_id, translation_id|null, source_name,
  headline_original|null, headline_translated|null, extract_translated|null,
  retrieved_at, licence_snapshot, correction_candidate,
  withdrawals: [{ article_id, withdrawn_at, withdrawn_by, reason }] }],
  next_cursor: string|null }`. Column backing:
  `headline_original` = `source_item.original_title`,
  `headline_translated`/`extract_translated` = `translation.headline`/`.extract`.
  `correction_candidate` is true exactly when `withdrawals` is non-empty —
  the origin is back in the queue because its only articles were withdrawn.
  `withdrawals` is newest first, and `[]` (never null) for a fresh origin.
- 400: unparseable `limit` (outside 1–100), malformed `lang`, a `cursor`
  this endpoint did not issue, or an unrecognised query parameter.
- 401 without token; 403 for non-editors.

### POST /api/v1/editorial/approvals

Approval creates the article — the database enforces the named-editor
rule; this endpoint merely carries the intent.

- Body: `{ "translation_id": uuid }` XOR `{ "source_item_id": uuid }`,
  plus `{ "attribution": string, "publish": bool }`.
- 201: `{ article_id, approved_by, approved_at, published_at|null }`.
- 400: both or neither origin supplied; an origin id that is not a uuid or
  names nothing; blank attribution; untranslated origin whose feed provided
  no title.
- 403: token subject is not an editor (mirrors the DB trigger).
- 409: origin already has a **non-withdrawn** article. An origin whose
  only articles are withdrawn may be approved again — that is the
  documented correction flow — and the partial unique indexes (0002
  shape) enforce exactly this at the database.
- Side effect: `article.approved` (+ `article.published` when publish)
  domain events in the same transaction.

### POST /api/v1/editorial/articles/{id}/publication

Publishes an approved-but-unpublished article (the `publish: false`
path); the database permits `published_at` to be set exactly once.

- 200: `{ article_id, published_at }`.
- 400: the `{id}` path segment is not a uuid — a malformed id is a client
  mistake worth naming, distinct from an article that does not exist.
- 404: unknown article; 409: already published.
- 403: the actor no longer holds the editor role at the moment of the
  write. No trigger can enforce this one — nothing on the article records
  who released it — so the publishing transaction takes a locking read of
  the actor's account row and the UPDATE itself carries the editor
  predicate. A demotion and a publication therefore serialize.
- Side effect: `article.published` domain event, in the same transaction.

Lifecycle note: an approved article is either published (this endpoint or
`publish: true` at approval) or remains permanently unpublished — a
frozen record of an approval that was never released. Withdrawal applies
only to published articles; there is deliberately no way to erase an
approval (I-1, I-5).

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

- 200: `{ article_id, source: { name, feed_url, jurisdiction },
  source_item: { source_url, original_title|null, retrieved_at, content_hash,
  licence_snapshot, usage_rule_snapshot, permission_evidence_snapshot|null,
  original_author|null }, translation: { model, prompt_version, target_locale,
  generated_at }|null, approval: { approver_name, approver_email, approved_at },
  published_at|null, withdrawal: { withdrawn_at, withdrawn_by, reason }|null }`.
  The `source` object is identity only; the legal basis (usage rule,
  licence, permission evidence) always comes from the retrieval-time
  snapshots on `source_item`, matching the `article_provenance` view.
- Works for withdrawn articles — audit sees full history.

## Operational endpoints (existing)

- `GET /healthz` — liveness. `GET /readyz` — readiness (DB ping).

## Non-goals of this contract

No search, no comments, no user-generated content, no image handling, no
full-text bodies in any public payload (extract-and-link only). Reader
registration endpoints — and the consent grant/revoke endpoints that go
with them — ship only if the registration UI survives the cut line; the
schema capability exists and is integration-tested regardless.
