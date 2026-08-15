-- Reader-facing queries (T023). Published-and-visible semantics throughout:
-- published_at IS NOT NULL AND withdrawn_at IS NULL (FR-016) - withdrawal
-- ends publication, so withdrawn articles never reach readers.
--
-- Column backing per the HTTP contract: a translated origin renders
-- translation.headline/.extract; an untranslated (same-language) origin
-- renders source_item.original_title with an extract derived at read time
-- from raw_body (research D9). The query layer returns whichever of the two
-- a row actually needs - raw_body only for untranslated rows - and the
-- endpoint picks per origin shape.
--
-- Language resolution: a translated article matches on the translation's
-- target locale, an untranslated one on its source's language - which is
-- exactly coalesce(t.target_locale, s.language_code).

-- name: ListFrontPage :many
-- Front-page feed: newest first, keyset-paginated on (published_at, id)
-- descending. Offset pagination is deliberately not used - the feed moves
-- while readers page through it.
--
-- The cursor is the (published_at, id) of the previous page's last row, and
-- it is atomic: BOTH halves must be present for the keyset comparison to
-- apply. A half-supplied cursor is treated as no cursor - the first page -
-- because the alternative is worse: SQL row comparison against a NULL
-- yields UNKNOWN, which would filter out every row and hand the reader a
-- silently empty feed. Callers that build cursors from encodeCursor always
-- supply both halves; the endpoint rejects malformed cursors with 400
-- before reaching here, so this arm is the last line of defence.
--
-- raw_body is fetched only for untranslated rows, through a left join on the
-- article's own source_item: it is the full retrieval evidence blob, and a
-- translated row renders from the translation columns, so reading it for
-- every feed row would be pure overfetch on the hottest read path.
--
-- Place scoping honours the many-to-many model via EXISTS, so an article
-- tagged to several requested places still appears exactly once.
select
    a.id,
    a.published_at,
    a.attribution_block,
    t.headline  as translation_headline,
    t.extract   as translation_extract,
    t.target_locale,
    si.original_title,
    untranslated.raw_body,
    si.source_url,
    s.language_code as source_language,
    (
        select coalesce(array_agg(p.slug order by p.slug), '{}'::text[])
          from article_place ap
          join place p on p.id = ap.place_id
         where ap.article_id = a.id and p.slug is not null
    )::text[] as place_slugs
from article a
left join translation t on t.id = a.translation_id
join source_item si on si.id = coalesce(t.source_item_id, a.source_item_id)
join source s on s.id = si.source_id
-- Untranslated origins only: article_exactly_one_origin means
-- a.source_item_id is set exactly when there is no translation, so this
-- join matches only those rows and the evidence blob is never read - nor
-- carried across the wire - for a translated one.
left join source_item untranslated on untranslated.id = a.source_item_id
where a.published_at is not null
  and a.withdrawn_at is null
  and coalesce(t.target_locale, s.language_code) = sqlc.arg(lang)::text
  and exists (
        select 1
          from article_place ap
          join place p on p.id = ap.place_id
         where ap.article_id = a.id
           and p.slug = any (sqlc.arg(places)::text[])
  )
  and (
        sqlc.narg(cursor_published_at)::timestamptz is null
        or sqlc.narg(cursor_id)::uuid is null
        or (a.published_at, a.id)
            < (sqlc.narg(cursor_published_at)::timestamptz, sqlc.narg(cursor_id)::uuid)
  )
order by a.published_at desc, a.id desc
limit sqlc.arg(row_limit);

-- name: GetPublishedArticle :one
-- Article detail. Same visibility rule as the feed: an unpublished or
-- withdrawn article is absent (the endpoint maps no-rows to 404 - the
-- existence of unpublished work is not public). raw_body is conditional
-- exactly as in the feed, so both row shapes carry the same fields and the
-- endpoint renders them through one code path.
select
    a.id,
    a.published_at,
    a.approved_at,
    a.attribution_block,
    t.headline  as translation_headline,
    t.extract   as translation_extract,
    t.target_locale,
    si.original_title,
    untranslated.raw_body,
    si.source_url,
    s.language_code as source_language,
    (
        select coalesce(array_agg(p.slug order by p.slug), '{}'::text[])
          from article_place ap
          join place p on p.id = ap.place_id
         where ap.article_id = a.id and p.slug is not null
    )::text[] as place_slugs
from article a
left join translation t on t.id = a.translation_id
join source_item si on si.id = coalesce(t.source_item_id, a.source_item_id)
join source s on s.id = si.source_id
-- Untranslated origins only: article_exactly_one_origin means
-- a.source_item_id is set exactly when there is no translation, so this
-- join matches only those rows and the evidence blob is never read - nor
-- carried across the wire - for a translated one.
left join source_item untranslated on untranslated.id = a.source_item_id
where a.id = $1
  and a.published_at is not null
  and a.withdrawn_at is null;

-- name: ListPlacesBySlugs :many
-- Resolves reader-supplied place slugs to the places that actually exist.
-- The endpoint compares what came back against what was asked and rejects
-- requests naming unknown places (contract: 400).
select slug::text as slug
from place
where slug = any (sqlc.arg(slugs)::text[]);
