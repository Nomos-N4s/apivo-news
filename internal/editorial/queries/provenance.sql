-- Provenance query (T028, US5, FR-010).
--
-- The five-minute audit: for one article, the full chain - source
-- identity, the licence terms as they applied AT RETRIEVAL (the immutable
-- source_item snapshots, never the mutable source row: I-4), translation
-- lineage with its recorded cost, the named approver, and any withdrawal.
-- The chain is the article_provenance view, read whole, which is exactly
-- I-5's "one query" claim.

-- name: GetArticleProvenance :one
-- One statement, one round trip. The provenance chain itself is the single
-- view read I-5 promises. The article's domain events ride along as a jsonb
-- aggregate in a correlated subquery rather than as a second query in a
-- transaction: folding them into the view instead would multiply or
-- aggregate every provenance row for every consumer of the view, taxing a
-- claim that is about the chain - so the view stays the chain, and the
-- events are this endpoint's addition, still within the same statement and
-- the same snapshot.
--
-- Events match on payload->>'article_id': every article event (approved,
-- published, withdrawn) carries it, written in the same transaction as the
-- fact it records. Ordered oldest first - the audit reads it as a
-- timeline - with the id as a stable tie-break for events sharing a
-- timestamp.
select
    v.article_id,
    v.headline,
    v.places::text[] as places,
    v.source_name,
    v.source_feed_url,
    v.jurisdiction,
    v.source_url,
    v.original_title,
    v.retrieved_at,
    v.content_hash,
    v.licence_snapshot,
    v.usage_rule as usage_rule_snapshot,
    v.permission_evidence as permission_evidence_snapshot,
    v.original_author,
    v.translation_id,
    v.model,
    v.prompt_version,
    v.target_locale,
    v.generated_at,
    v.cost_microusd,
    v.approver_name,
    v.approver_email,
    v.approved_at,
    v.published_at,
    v.withdrawn_at,
    v.withdrawn_by,
    v.withdrawal_reason,
    coalesce(
        (
            select jsonb_agg(
                jsonb_build_object(
                    'type', e.type,
                    'occurred_at', e.occurred_at,
                    'payload', e.payload
                )
                -- The tie-break is semantic, not random: approve-and-publish
                -- writes both events in one transaction, so occurred_at -
                -- the transaction timestamp - is identical for the pair, and
                -- breaking the tie on a random uuid would show publication
                -- before approval on half of all audited articles. Lifecycle
                -- order is the truth the timeline exists to state.
                order by e.occurred_at,
                    case e.type
                        when 'article.approved' then 0
                        when 'article.published' then 1
                        when 'article.withdrawn' then 2
                        else 3
                    end,
                    e.id
            )
            from domain_event e
            where e.payload->>'article_id' = v.article_id::text
        ),
        '[]'::jsonb
    )::text as events
from article_provenance v
where v.article_id = sqlc.arg(article_id)::uuid;
