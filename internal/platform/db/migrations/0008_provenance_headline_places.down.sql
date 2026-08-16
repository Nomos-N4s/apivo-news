-- Reverses 0008_provenance_headline_places: back to the 0002 shape of the
-- view. CREATE OR REPLACE cannot drop columns, so the view is dropped and
-- recreated; a view holds no data, so nothing is lost. The underlying
-- evidence - articles, items, snapshots, places, costs - is untouched.

drop view article_provenance;

create view article_provenance as
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
    a.withdrawal_reason
from article a
join account approver on approver.id = a.approved_by
left join translation t on t.id = a.translation_id
join source_item si on si.id = coalesce(t.source_item_id, a.source_item_id)
join source s on s.id = si.source_id;

comment on view article_provenance is
    'I-5: for any article - source, licence snapshot at retrieval, model, prompt version, named approver and any withdrawal - in a single query.';
