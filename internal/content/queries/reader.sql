-- Reader-facing queries (T023). Published-and-visible semantics throughout:
-- published_at IS NOT NULL AND withdrawn_at IS NULL (FR-016) - withdrawal
-- ends publication, so withdrawn articles never reach readers.
--
-- Column backing per the HTTP contract: a translated origin renders
-- translation.headline/.extract; an untranslated (same-language) origin
-- renders source_item.original_title with an extract derived at read time
-- from raw_body (research D9). The query layer therefore returns both the
-- translation columns and the raw source fields; the endpoint picks per
-- origin shape.
--
-- Language resolution: a translated article matches on the translation's
-- target locale, an untranslated one on its source's language - which is
-- exactly coalesce(t.target_locale, s.language_code).

-- name: ListFrontPage :many
-- Front-page feed: newest first, keyset-paginated on (published_at, id)
-- descending. The cursor is the last row of the previous page; a null
-- cursor_published_at means the first page. Offset pagination is
-- deliberately not used - the feed moves while readers page through it.
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
    si.raw_body,
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
        or (a.published_at, a.id)
            < (sqlc.narg(cursor_published_at)::timestamptz, sqlc.narg(cursor_id)::uuid)
  )
order by a.published_at desc, a.id desc
limit sqlc.arg(row_limit);

-- name: GetPublishedArticle :one
-- Article detail. Same visibility rule as the feed: an unpublished or
-- withdrawn article is absent (the endpoint maps no-rows to 404 - the
-- existence of unpublished work is not public).
select
    a.id,
    a.published_at,
    a.approved_at,
    a.attribution_block,
    t.headline  as translation_headline,
    t.extract   as translation_extract,
    t.target_locale,
    si.original_title,
    si.raw_body,
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
