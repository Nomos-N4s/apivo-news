-- Reverses 0002_roles_withdrawal_costs, restoring the exact 0001 state:
-- original entitlement gate, original article guard, original one-per-origin
-- indexes, original provenance view. Destroys the withdrawal record, the
-- role assignments and the cost lineage: never run against an environment
-- holding real data.

-- Entitlement gate back to its 0001 body.
create or replace function is_entitled(p_account_id uuid, p_action text) returns boolean
language sql
stable
as $$
    -- The single entitlement gate. Every access decision routes through
    -- here so future rules (tiers, embargoes, consent-gated features) change
    -- one function, not scattered call sites. Alpha rule: any existing
    -- account is entitled to any action.
    select exists (select 1 from account where id = p_account_id)
       and p_action is not null;
$$;

comment on function is_entitled(uuid, text) is
    'Single entitlement gate: may this account perform this action? All entitlement logic lives here and only here.';

alter table source drop column active;

-- The seeded reference places go with the slug column that addresses
-- them. Parents and children fall in one statement: the parent_id foreign
-- key is checked at statement end, so ordering inside the DELETE does not
-- matter.
delete from place where slug in ('munich', 'bavaria', 'germany', 'greece');
alter table place drop column slug; -- drops place_slug_unique and place_slug_not_blank with it

drop table translation_spend;

alter table translation drop column cost_microusd; -- drops translation_cost_not_negative with it

-- The provenance view must lose the withdrawal columns before they can be
-- dropped, and CREATE OR REPLACE cannot remove columns: drop and recreate
-- the 0001 view verbatim.
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
    s.jurisdiction
from article a
join account approver on approver.id = a.approved_by
left join translation t on t.id = a.translation_id
join source_item si on si.id = coalesce(t.source_item_id, a.source_item_id)
join source s on s.id = si.source_id;

comment on view article_provenance is
    'I-5: for any article - source, licence snapshot at retrieval, model, prompt version and named approver, in a single query.';

-- One-per-origin indexes back to their 0001 shape (covering withdrawn
-- rows again).
drop index article_one_per_translation;
create unique index article_one_per_translation
    on article (translation_id) where translation_id is not null;

drop index article_one_per_source_item;
create unique index article_one_per_source_item
    on article (source_item_id) where source_item_id is not null;

-- Article guard back to its 0001 body.
create or replace function article_guard() returns trigger
language plpgsql
as $$
begin
    if new.id is distinct from old.id
        or new.translation_id is distinct from old.translation_id
        or new.source_item_id is distinct from old.source_item_id
        or new.approved_by is distinct from old.approved_by
        or new.approved_at is distinct from old.approved_at
        or new.attribution_block is distinct from old.attribution_block then
        raise exception 'article identity, origin, approval and attribution are frozen (I-1, I-5): corrections publish a new article';
    end if;
    if old.published_at is not null and new.published_at is distinct from old.published_at then
        raise exception 'article publication time is frozen once set (I-5): withdrawal is a separate, audited transition';
    end if;
    return new;
end;
$$;

comment on function article_guard() is
    'Freezes every provenance-bearing article column after approval; only the one-way publish transition is allowed.';

-- Dropping the withdrawal columns drops every withdrawal CHECK with them.
alter table article
    drop column withdrawn_at,
    drop column withdrawn_by,
    drop column withdrawal_reason;

drop trigger article_require_editor on article;
drop function article_require_editor_approver();

alter table account drop column role;
