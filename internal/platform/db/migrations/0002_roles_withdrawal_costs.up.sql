-- 0002_roles_withdrawal_costs: roles, withdrawal, cost lineage, place slugs.
--
-- Tightens and extends the invariant core of 0001. Nothing here loosens a
-- trigger or a constraint; new columns on immutable tables are written at
-- insert time only.
--
--   I-1 tightened  "named human approver" becomes "named EDITOR"
--                  -> account.role + BEFORE INSERT trigger on article.
--   I-5 extended   withdrawal ends publication while preserving the whole
--                  record -> all-or-none withdrawal columns, article guard
--                  v2 allowing exactly one more one-way transition, the
--                  provenance view carrying the withdrawal record.
--   FR-006         every translation records its cost explicitly; a
--                  monthly ledger backs the spend cap.
--   FR-009         places gain stable slugs + the alpha reference places.

------------------------------------------------------------------------------
-- Editor role: approval authority is a named editor (I-1 tightened).
------------------------------------------------------------------------------

alter table account
    add column role text not null default 'reader'
        constraint account_role_known check (role in ('reader', 'editor'));

comment on column account.role is
    'What this person may do: readers read, editors approve. Approval authority is checked by the database (article_require_editor_approver, is_entitled), never by application code alone.';

-- Only the role rule lives in this trigger: a missing approver stays a
-- NOT NULL violation (23502) and a nonexistent one a foreign-key
-- violation (23503), so every failure keeps its natural SQLSTATE.
create function article_require_editor_approver() returns trigger
language plpgsql
as $$
begin
    if exists (
        select 1
          from account a
         where a.id = new.approved_by
           and a.role <> 'editor'
    ) then
        raise exception 'article.approved_by must hold the editor role (I-1): approval authority belongs to named editors';
    end if;
    return new;
end;
$$;

comment on function article_require_editor_approver() is
    'I-1 tightened: the named human approver must hold the editor role at approval time. approved_by itself stays frozen afterwards (article_guard).';

create trigger article_require_editor
    before insert on article
    for each row execute function article_require_editor_approver();

------------------------------------------------------------------------------
-- Withdrawal: publication can end, the record cannot (I-5, FR-016).
------------------------------------------------------------------------------

alter table article
    add column withdrawn_at timestamptz,
    add column withdrawn_by uuid references account (id),
    add column withdrawal_reason text
        constraint article_withdrawal_reason_not_blank
            check (withdrawal_reason is null or btrim(withdrawal_reason) <> ''),
    -- A withdrawal is who, when and why, together: a partial withdrawal
    -- is unrepresentable.
    add constraint article_withdrawal_all_or_none
        check (num_nonnulls(withdrawn_at, withdrawn_by, withdrawal_reason) in (0, 3)),
    -- Only a published article can be withdrawn, and never before its
    -- publication. The explicit published_at arm matters: without it a
    -- NULL published_at would make the comparison unknown and the check
    -- would wave an unpublished "withdrawal" through.
    add constraint article_withdrawal_follows_publication
        check (
            withdrawn_at is null
            or (published_at is not null and withdrawn_at >= published_at)
        );

comment on column article.withdrawn_at is
    'When publication ended (FR-016). Withdrawal keeps the row, the approval and the retrieved evidence; published-and-visible means published_at IS NOT NULL AND withdrawn_at IS NULL.';
comment on column article.withdrawn_by is
    'The named human who withdrew this article. Set together with withdrawn_at and withdrawal_reason, once, and frozen from then on.';
comment on column article.withdrawal_reason is
    'Why the article was withdrawn. Part of the audit record; never blank when set.';

-- Guard v2: everything 0001 froze stays frozen. The single new legal
-- transition is withdrawal - the three withdrawal columns moving from
-- null to set, together, once - and it is one-way and final.
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
    if old.withdrawn_at is not null
        and (new.withdrawn_at is distinct from old.withdrawn_at
            or new.withdrawn_by is distinct from old.withdrawn_by
            or new.withdrawal_reason is distinct from old.withdrawal_reason) then
        raise exception 'article withdrawal is one-way and final (I-5): who withdrew it, when and why is frozen once recorded';
    end if;
    return new;
end;
$$;

comment on function article_guard() is
    'Freezes every provenance-bearing article column after approval; the only legal transitions are the one-way publish and the one-way, all-at-once withdrawal.';

-- One ACTIVE article per origin: a withdrawn article frees its origin for
-- a corrected re-approval, while two active articles from the same origin
-- stay impossible - a concurrent double-approve still loses.
drop index article_one_per_translation;
create unique index article_one_per_translation
    on article (translation_id)
    where translation_id is not null and withdrawn_at is null;

drop index article_one_per_source_item;
create unique index article_one_per_source_item
    on article (source_item_id)
    where source_item_id is not null and withdrawn_at is null;

-- The provenance view carries the withdrawal record: audit sees the full
-- history in one query; reader-facing queries exclude withdrawn rows.
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
    a.withdrawal_reason
from article a
join account approver on approver.id = a.approved_by
left join translation t on t.id = a.translation_id
join source_item si on si.id = coalesce(t.source_item_id, a.source_item_id)
join source s on s.id = si.source_id;

comment on view article_provenance is
    'I-5: for any article - source, licence snapshot at retrieval, model, prompt version, named approver and any withdrawal - in a single query.';

------------------------------------------------------------------------------
-- Translation cost lineage (FR-006).
------------------------------------------------------------------------------

-- The final shape has NO default: a translation whose cost was not
-- recorded is a rejected insert, never a silent zero. The column is added
-- with a temporary default only so the ALTER stays valid against a table
-- that already holds rows - no real environment should have any yet (the
-- translation module does not exist), but a migration must not gamble on
-- that - and the default is dropped immediately, in the same transaction.
alter table translation
    add column cost_microusd bigint not null default 0
        constraint translation_cost_not_negative check (cost_microusd >= 0);

alter table translation
    alter column cost_microusd drop default;

comment on column translation.cost_microusd is
    'Provider-reported cost of this translation in micro-USD, recorded explicitly at insert (FR-006). No default: an omitted cost is an error. An explicit 0 is legal only when the provider genuinely charged nothing (e.g. included quota).';

create table translation_spend (
    -- One row per calendar month: the key is the first day of the month,
    -- so two rows can never describe the same month.
    month date primary key
        constraint translation_spend_month_is_first_day
            check (month = date_trunc('month', month)::date),
    spent_microusd bigint not null default 0
        constraint translation_spend_not_negative check (spent_microusd >= 0)
);

comment on table translation_spend is
    'Monthly translation spend ledger, updated in the same transaction as each translation insert. Caps (per-article ceiling, monthly cap) are configuration; the translation module refuses work once the ledger reaches the cap and emits a pipeline.halted domain event.';

------------------------------------------------------------------------------
-- Reader-facing place addressing (FR-009) + alpha reference places.
------------------------------------------------------------------------------

alter table place
    add column slug text
        constraint place_slug_not_blank check (slug is null or btrim(slug) <> ''),
    add constraint place_slug_unique unique (slug);

comment on column place.slug is
    'Stable, human-readable address for locale-scoped reader pages (e.g. /el/munich). Unique when present; places that are not reader destinations may have none.';

-- Alpha reference places, seeded here like language in 0001. Names are
-- kept in English; the reader-locale exonyms: Germany (de Deutschland,
-- el Γερμανία), Bavaria (de Bayern, el Βαυαρία), Munich (de München,
-- el Μόναχο), Greece (de Griechenland, el Ελλάδα).
insert into place (name, country, slug) values ('Germany', 'DE', 'germany');
insert into place (parent_id, name, country, slug)
select id, 'Bavaria', 'DE', 'bavaria' from place where slug = 'germany';
insert into place (parent_id, name, country, slug)
select id, 'Munich', 'DE', 'munich' from place where slug = 'bavaria';
insert into place (name, country, slug) values ('Greece', 'GR', 'greece');

------------------------------------------------------------------------------
-- Source operations (US2): pause a feed without deleting anything.
------------------------------------------------------------------------------

alter table source
    add column active boolean not null default true;

comment on column source.active is
    'Whether the crawler currently polls this feed. Pausing flips this to false; the source row, its licence terms and every retrieved item stay untouched.';

------------------------------------------------------------------------------
-- Entitlement gate v2: editorial actions require the editor role.
------------------------------------------------------------------------------

create or replace function is_entitled(p_account_id uuid, p_action text) returns boolean
language sql
stable
as $$
    -- The single entitlement gate. Every access decision routes through
    -- here so future rules (tiers, embargoes, consent-gated features)
    -- change one function, not scattered call sites.
    --
    -- Rule: editor-class actions - anything in the editorial.* namespace
    -- - are entitled only to accounts holding the editor role, mirroring
    -- the approval trigger. Every other action keeps the alpha rule: any
    -- existing account is entitled.
    select case
        when p_action is null then false
        when p_action like 'editorial.%' then
            exists (select 1 from account where id = p_account_id and role = 'editor')
        else
            exists (select 1 from account where id = p_account_id)
    end;
$$;

comment on function is_entitled(uuid, text) is
    'Single entitlement gate: may this account perform this action? All entitlement logic lives here and only here; editorial.* actions require the editor role.';
