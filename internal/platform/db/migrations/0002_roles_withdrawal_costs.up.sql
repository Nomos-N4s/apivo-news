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
--                  v2 allowing exactly one more one-way transition, an
--                  audit event written by trigger in the same transaction,
--                  the provenance view carrying the withdrawal record.
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
    'What this person may do: readers read, editors approve. Approval authority is checked by the database (article_insert_guard, is_entitled), never by application code alone.';

-- Legacy-approver promotion: any account that already approved an article
-- under 0001 semantics was the named human approver of record, so it holds
-- exactly the authority the editor role now names. Promote those accounts
-- before the tightened invariant starts relying on the role, so history
-- stays consistent with it. Pre-release every environment is empty and
-- this is a no-op.
update account
   set role = 'editor'
 where id in (select approved_by from article);

-- The BEFORE INSERT trigger enforcing the role (and the born-active rule)
-- is created below, once the withdrawal columns it also guards exist.

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

-- The single BEFORE INSERT guard, two rules:
--
--   1. I-1 tightened: the approver must hold the editor role. Only the
--      role rule lives here - a missing approver stays a NOT NULL
--      violation (23502) and a nonexistent one a foreign-key violation
--      (23503), so every failure keeps its natural SQLSTATE.
--   2. An article is born active: withdrawal is a lifecycle transition
--      (article_guard), never an initial state. Without this rule an
--      INSERT could arrive pre-withdrawn - skipping the guarded
--      transition and, because the one-per-origin indexes cover active
--      rows only, slipping a second article past them.
create function article_insert_guard() returns trigger
language plpgsql
as $$
declare
    approver_role text;
begin
    -- The role is read WITH A ROW LOCK (FOR SHARE), not with a plain
    -- snapshot read. Foreign-key locks cover only key columns, so a
    -- concurrent demotion of this very account could otherwise commit
    -- unseen and this trigger would approve against a stale role. FOR
    -- SHARE conflicts with the demotion UPDATE's row lock, so the two
    -- transactions serialize in either order, and under READ COMMITTED a
    -- locking read sees the latest committed version of the row:
    -- whichever side commits second sees the other's write and raises
    -- (here, or in account_role_guard). A reader can never be recorded
    -- as the approver.
    select a.role
      into approver_role
      from account a
     where a.id = new.approved_by
       for share;
    -- No row means null or nonexistent approver; fall through so those
    -- keep their natural SQLSTATEs (see rule 1 above).
    if approver_role is not null and approver_role <> 'editor' then
        raise exception 'article.approved_by must hold the editor role (I-1): approval authority belongs to named editors';
    end if;
    if num_nonnulls(new.withdrawn_at, new.withdrawn_by, new.withdrawal_reason) <> 0 then
        raise exception 'an article cannot be created already withdrawn (I-5): withdrawal is a one-way transition after publication';
    end if;
    return new;
end;
$$;

comment on function article_insert_guard() is
    'BEFORE INSERT guard: the approver must hold the editor role at approval time (I-1 tightened), and no withdrawal column may be pre-set - articles are born active.';

create trigger article_insert_guard
    before insert on article
    for each row execute function article_insert_guard();

-- Guard v2: everything 0001 froze stays frozen. The single new legal
-- transition is withdrawal - the three withdrawal columns moving from
-- null to set, together, once - and it is one-way and final.
create or replace function article_guard() returns trigger
language plpgsql
as $$
declare
    withdrawer_role text;
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
    -- Withdrawal is an editorial decision, symmetric with approval: on
    -- the transition the withdrawer must hold the editor role. The role
    -- is read WITH A ROW LOCK (FOR SHARE) for the same reason as in
    -- article_insert_guard: it serializes against a concurrent demotion
    -- of the withdrawer, so whichever transaction commits second sees
    -- the other's write and raises. Only the role rule lives here - a
    -- null withdrawn_by locks nothing and falls through to the
    -- all-or-none CHECK (23514), a nonexistent account to the foreign
    -- key (23503).
    if old.withdrawn_at is null and new.withdrawn_at is not null then
        select a.role
          into withdrawer_role
          from account a
         where a.id = new.withdrawn_by
           for share;
        if withdrawer_role is not null and withdrawer_role <> 'editor' then
            raise exception 'article.withdrawn_by must hold the editor role: withdrawal is an editorial decision';
        end if;
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
    'Freezes every provenance-bearing article column after approval; the only legal transitions are the one-way publish and the one-way, all-at-once withdrawal by an editor.';

-- The audit record is not left to application discipline (Principle
-- VIII): the same transaction that withdraws an article writes the
-- domain event, by trigger. The stream itself is append-only (0001), so
-- the record of who withdrew what, and why, can never be quietly erased.
create function article_record_withdrawal_event() returns trigger
language plpgsql
as $$
begin
    insert into domain_event (type, payload)
    values ('article.withdrawn', jsonb_build_object(
        'article_id', new.id,
        'withdrawn_by', new.withdrawn_by,
        'reason', new.withdrawal_reason,
        'withdrawn_at', new.withdrawn_at
    ));
    return null;
end;
$$;

comment on function article_record_withdrawal_event() is
    'Writes the article.withdrawn domain event - which article, who, why - in the same transaction as the withdrawal itself (FR-016).';

create trigger article_withdrawal_event
    after update on article
    for each row
    when (old.withdrawn_at is null and new.withdrawn_at is not null)
    execute function article_record_withdrawal_event();

-- I-1 stays provable over history: while an editor's approvals or
-- withdrawals are on record, the role that authorised them cannot be
-- taken away - otherwise the provenance chain would point at an account
-- whose editorship is no longer demonstrable. Concurrency is covered by
-- the FOR SHARE reads in the article triggers: an in-flight approval or
-- withdrawal holds a share lock on the account row, so this demotion
-- UPDATE waits for it and then re-checks against the committed article.
-- Offboarding semantics (for example a terminal former_editor role) are
-- a founder-level decision for a later migration.
create function account_role_guard() returns trigger
language plpgsql
as $$
begin
    if old.role = 'editor'
        and new.role is distinct from old.role
        and exists (
            select 1
              from article ar
             where ar.approved_by = old.id
                or ar.withdrawn_by = old.id
        ) then
        raise exception 'account role is frozen while editorial decisions reference it (I-1): recorded approvals and withdrawals require a demonstrable editor';
    end if;
    return new;
end;
$$;

comment on function account_role_guard() is
    'Blocks demoting an editor whose approvals or withdrawals are on record, keeping every recorded editorial decision attributable to a verifiable editor (I-1).';

create trigger account_role_guard
    before update on account
    for each row execute function account_role_guard();

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

-- The column has NO default: a translation whose cost was not recorded
-- is a rejected insert, never a silent zero. That honesty extends to
-- history: if translation rows already exist their real costs are
-- unknown, and this migration refuses to invent them - a populated
-- deployment must backfill provider-reported costs explicitly before
-- applying 0002. Pre-release the table is empty in every environment
-- and this guard never fires; it exists so the migration cannot gamble
-- on that.
do $$
begin
    if exists (select 1 from translation) then
        raise exception 'translation rows exist with no recorded cost: backfill cost_microusd from provider records before applying 0002 - an unknown cost must never silently become 0';
    end if;
end;
$$;

alter table translation
    add column cost_microusd bigint not null
        constraint translation_cost_not_negative check (cost_microusd >= 0);

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
--
-- Seeding fails fast on conflict rather than guessing: whether a
-- pre-existing row of the same name IS the reference place - and with
-- which parent chain - is not decidable by a migration, so an operator
-- must reconcile reference places manually before upgrading. Pre-release
-- every database is empty and this never fires. (The slug arm is
-- belt-and-braces: the column was created above, so no row can carry a
-- seed slug yet; it protects against future reordering.)
do $$
begin
    if exists (
        select 1
          from place
         where slug in ('germany', 'bavaria', 'munich', 'greece')
            or (name, country) in (('Germany', 'DE'), ('Bavaria', 'DE'), ('Munich', 'DE'), ('Greece', 'GR'))
    ) then
        raise exception 'reference places already exist: reconcile Germany/Bavaria/Munich (DE) and Greece (GR) with the 0002 seed hierarchy manually before applying this migration - it will not guess which existing row is the reference place';
    end if;
end;
$$;

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
