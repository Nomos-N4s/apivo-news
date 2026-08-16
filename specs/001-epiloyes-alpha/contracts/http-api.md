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
  retrieved_at, licence_snapshot, source_url, original_author|null,
  original_published_at|null, content_hash, extract_original, source_lang,
  target_lang|null, model|null, prompt_version|null, cost_microusd|null,
  correction_candidate,
  withdrawals: [{ article_id, withdrawn_at, withdrawn_by, reason }] }],
  next_cursor: string|null }`. Column backing:
  `headline_original` = `source_item.original_title`,
  `headline_translated`/`extract_translated` = `translation.headline`/`.extract`.
  The evidence block (#87) carries what the permanent approval rests on,
  before the click: `source_url`, `original_author` and
  `original_published_at` from the immutable `source_item`;
  `content_hash`, the database-computed fingerprint; `extract_original`,
  derived in Go from `raw_body` by the shared D9 reducer and bounded to
  300 runes — the raw body crosses the database hop, never the wire;
  `source_lang` = `source.language_code`; `target_lang`, `model`,
  `prompt_version`, `cost_microusd` from the translation (FR-005,
  FR-006), null together for an untranslated origin.
  `original_published_at` is null when the feed declared no publication
  date — never substituted, because the attribution the client composes
  from it is frozen permanently at approval.
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
  plus `{ "attribution": string, "publish": bool,
  "places": [slug, …] }`. `places` is required with at least one place
  slug: the front page is scoped by place (FR-009), so an article tagged
  to no place can never appear on any of them — the article and its
  `article_place` rows commit together, and the database refuses an
  article with none at commit.
- 201: `{ article_id, approved_by, approved_at, published_at|null }`.
- 400: both or neither origin supplied; an origin id that is not a uuid or
  names nothing; blank attribution; untranslated origin whose feed provided
  no title; no places, a blank or repeated place slug, or a slug that
  names no place (`unknown place "x"`, the front page's vocabulary).
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

- Body: `{ "reason": string }` (required, non-blank). `withdrawn_by` is
  the authenticated editor, never a body field.
- 200: `{ article_id, withdrawn_at, withdrawn_by, reason }`. `reason` is
  the value the database froze into `article.withdrawal_reason` — the
  record, not an echo of the request — because the confirmation screen
  renders this response as the record of what happened.
- 400: blank reason, or an `{id}` path segment that is not a uuid.
- 404: unknown or never-published article; 409: already withdrawn.
- 403: the database refuses a withdrawer without the editor role,
  symmetrically with approval.
- Side effect: `article.withdrawn` domain event (who, why) in the same
  transaction — written by the 0002 trigger, so the endpoint emits none
  of its own.

### POST /api/v1/editorial/sources

- Body: `{ name, url, language, jurisdiction, licence_terms }`. The
  usage rule is NOT accepted here: new sources are always
  `extract_and_link` (upgrades are a separate founder-gated flow, out of
  alpha scope).
- 201: the source. 409: duplicate feed URL.

### GET /api/v1/editorial/sources

The registered feeds and their poll state — the licensing invariant made
visible, and `source.active`'s one read path.

- Query: `active` (optional, exactly `true` or `false`), `limit`,
  `cursor`. Keyset on `(created_at desc, id desc)`; unknown and repeated
  query parameters are 400, like the queue's.
- 200: `{ items: [{ id, name, url, language, jurisdiction, licence_terms,
  usage_rule, permission_evidence|null, active, last_polled_at|null,
  created_at }], next_cursor: string|null, cycle: { retrieved,
  duplicates_skipped, failures: [name, …] } }`. `url` is the same column
  the registration wrote, under one name across both source endpoints.
  The licensing fields are the **current** source row, and the contract
  says so: the legal basis of anything already retrieved is the
  retrieval-time snapshot on `source_item` (I-4), which the provenance
  endpoint serves and this list deliberately does not.
  `permission_evidence` is returned, behind the editor gate — the screen
  exists to make the licensing basis visible, and it is what separates a
  lawful `full_text` source from an impossible one.
  `cycle` sums each **active** source's last-poll counters (0007's
  `last_poll_retrieved`/`last_poll_duplicates`) and lists by name the
  active feeds whose `last_poll_error` is set — readings of the poll
  state, never invented figures. `failures` is sorted by name, `[]`
  never null.
- 400: unparseable `limit`, an `active` that is not exactly true/false,
  a cursor this endpoint did not issue, or an unknown or repeated
  parameter.
- 401 without token; 403 for non-editors.

### GET /api/v1/editorial/articles/{id}/provenance

The five-minute audit, served from `article_provenance` (I-5).

- 200: `{ article_id, headline, places: [slug, …],
  source: { name, feed_url, jurisdiction },
  source_item: { source_url, original_title|null, retrieved_at, content_hash,
  licence_snapshot, usage_rule_snapshot, permission_evidence_snapshot|null,
  original_author|null }, translation: { model, prompt_version, target_locale,
  generated_at, cost_microusd }|null, approval: { approver_name,
  approver_email, approved_at }, published_at|null,
  withdrawal: { withdrawn_at, withdrawn_by, reason }|null,
  events: [{ type, occurred_at, detail }] }`.
  The `source` object is identity only; the legal basis (usage rule,
  licence, permission evidence) always comes from the retrieval-time
  snapshots on `source_item`, matching the `article_provenance` view.
  Column backing: `headline` = `translation.headline`, else
  `source_item.original_title` (the resolution the reader sees); `places`
  is the article's place slugs, sorted, `[]` never null;
  `cost_microusd` = `translation.cost_microusd` (FR-006). `events` is the
  article's rows of the append-only `domain_event` stream (FR-012),
  oldest first, `detail` carrying each recorded payload verbatim (minus
  the `article_id` the response is scoped to); they arrive in the same
  statement as the chain — the view read stays I-5's one query.
- 400: the `{id}` path segment is not a uuid; 404: unknown id.
- 401 without token; 403 for non-editors.
- Works for withdrawn articles — audit sees full history.

## Operational endpoints (existing)

- `GET /healthz` — liveness. `GET /readyz` — readiness (DB ping).

## Non-goals of this contract

No search, no comments, no user-generated content, no image handling, no
full-text bodies in any public payload (extract-and-link only). Reader
registration endpoints — and the consent grant/revoke endpoints that go
with them — ship only if the registration UI survives the cut line; the
schema capability exists and is integration-tested regardless.
