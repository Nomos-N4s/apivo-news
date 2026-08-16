-- 0008_provenance_headline_places: the audit contract's missing columns.
--
-- The provenance endpoint (T028, US5) serves the audit screen from the
-- article_provenance view, and the contract's payload names three things
-- the 0002 shape of the view does not carry:
--
--   headline        what the article is called - the translation's
--                   headline, or the retrieved item's original title for
--                   an untranslated article (the same resolution the
--                   reader's front page uses).
--   original_title  the item title exactly as the feed provided it, part
--                   of the retrieval evidence beside the source URL.
--   places          where the article published to, as place slugs - the
--                   audit answers "visible on /el/munich", not a list of
--                   internal ids.
--   cost_microusd   what the translation cost (FR-006): lineage without
--                   its price is half the record.
--
-- CREATE OR REPLACE VIEW appends columns, so every existing column keeps
-- its name, position and provenance rules - in particular the legal basis
-- still reads from the immutable retrieval-time snapshots on source_item,
-- never from the mutable source row (I-4). The source columns remain
-- identity only.

create or replace view article_provenance as
select
    a.id              as article_id,
    a.published_at,
    a.attribution_block,
    a.approved_at,
    approver.id       as approver_id,
    approver.display_name as approver_name,
    approver.email    as approver_email,
    t.id              as translation_id,
    t.model,
    t.prompt_version,
    t.target_locale,
    t.generated_at,
    si.id             as source_item_id,
    si.source_url,
    si.original_author,
    si.published_at   as source_published_at,
    si.retrieved_at,
    si.content_hash,
    si.licence_snapshot,
    -- Legal basis as it applied AT RETRIEVAL: read from the immutable
    -- item snapshots, never from the mutable source row.
    si.usage_rule_snapshot as usage_rule,
    si.permission_evidence_snapshot as permission_evidence,
    s.id              as source_id,
    s.name            as source_name,
    s.url             as source_feed_url,
    s.jurisdiction,
    a.withdrawn_at,
    a.withdrawn_by,
    a.withdrawal_reason,
    si.original_title,
    -- The article's headline, resolved the way the reader sees it: a
    -- translated article renders its translation's headline, an
    -- untranslated one the feed's original title. Approval refuses an
    -- untitled untranslated origin, so for every article this can name
    -- the coalesce is non-null.
    -- The final '' keeps the audit readable for a row nothing in the schema
    -- forbids: an untranslated article over an untitled item. The Go approval
    -- flow refuses to create one, but the database accepts it, and an audit
    -- endpoint that answers 500 for exactly the anomalous record an auditor
    -- needs to inspect would be worse than showing the absence.
    coalesce(t.headline, si.original_title, '') as headline,
    -- Where the article published to. Slugs, not ids: the audit's answer
    -- is "visible on /el/munich". Sorted so the same article always
    -- reports the same list, and empty (never null) for the pre-0006
    -- articles no environment actually has.
    coalesce(
        (
            select array_agg(p.slug order by p.slug)
              from article_place ap
              join place p on p.id = ap.place_id
             where ap.article_id = a.id
               and p.slug is not null
        ),
        '{}'::text[]
    ) as places,
    t.cost_microusd
from article a
join account approver on approver.id = a.approved_by
left join translation t on t.id = a.translation_id
join source_item si on si.id = coalesce(t.source_item_id, a.source_item_id)
join source s on s.id = si.source_id;

comment on view article_provenance is
    'I-5: for any article - source, licence snapshot at retrieval, model, prompt version, cost, named approver, places and any withdrawal - in a single query.';

-- The audit endpoint lists an article's domain events by payload->>'article_id'.
-- Without this the read is a sequential scan of the whole append-only stream,
-- degrading with total volume across all articles rather than with the one
-- article's chain - the wrong scaling for a five-minute promise.
create index domain_event_article_id_idx
    on domain_event ((payload->>'article_id'))
    where payload ? 'article_id';
