-- 0001_init: foundational schema.
--
-- This migration encodes the licensing invariants at the database layer, on
-- purpose. Application code cannot be trusted to uphold legal guarantees:
--
--   I-1  An article cannot exist without a named human approver
--        -> article.approved_by NOT NULL REFERENCES account.
--   I-2  Provenance is captured at retrieval, in the same transaction as the
--        content -> provenance columns live on source_item itself and are
--        NOT NULL; a source_item without provenance is unrepresentable.
--   I-3  source_item is immutable -> BEFORE UPDATE/DELETE/TRUNCATE triggers
--        raise an exception. It is legal evidence of what was retrieved.
--   I-4  Licence terms are snapshotted at retrieval
--        -> source_item.licence_snapshot NOT NULL.
--   I-5  Every published sentence is traceable to source, licence, model,
--        prompt version and approver -> article_provenance view, one query.
--
-- Language and place are independent axes: a Greek speaker in Munich wants
-- Munich news in Greek. There is deliberately no combined locale tag.

-- digest() backs the database-computed content fingerprint. Present on
-- Supabase and in the standard postgres images alike.
create extension if not exists pgcrypto;

------------------------------------------------------------------------------
-- Immutability guard, shared by source_item and domain_event.
------------------------------------------------------------------------------

create function raise_immutable() returns trigger
language plpgsql
as $$
begin
    raise exception 'table % is immutable: % is not allowed (retrieved content is legal evidence)',
        tg_table_name, tg_op;
end;
$$;

comment on function raise_immutable() is
    'Trigger guard enforcing I-3: rows in immutable tables can never be updated, deleted or truncated.';

------------------------------------------------------------------------------
-- Locale model: language and place are independent axes.
------------------------------------------------------------------------------

create table language (
    code text primary key
        constraint language_code_is_bcp47_subtag check (code ~ '^[a-z]{2,3}$')
);

comment on table language is
    'BCP-47 primary language subtags only (el, de, en). Never combined with place into a single locale tag.';

insert into language (code) values ('el'), ('de'), ('en');

create table place (
    id uuid primary key default gen_random_uuid(),
    parent_id uuid references place (id),
    name text not null
        constraint place_name_not_blank check (btrim(name) <> ''),
    -- Format check only (two uppercase ASCII letters). Membership in the
    -- actual ISO 3166-1 list is a reference-data concern, deliberately not
    -- encoded as a constraint here.
    country text not null
        constraint place_country_alpha2_format check (country ~ '^[A-Z]{2}$'),
    jurisdiction_override text,
    constraint place_not_own_parent check (parent_id is null or parent_id <> id)
);

comment on table place is
    'Self-referencing hierarchy of places (e.g. Munich -> Bavaria -> Germany). jurisdiction_override applies when a place''s legal jurisdiction differs from its country default.';

create index place_parent_id_idx on place (parent_id);

------------------------------------------------------------------------------
-- Sources and retrieved items (the provenance core).
------------------------------------------------------------------------------

create table source (
    id uuid primary key default gen_random_uuid(),
    name text not null
        constraint source_name_not_blank check (btrim(name) <> ''),
    url text not null unique,
    language_code text not null references language (code),
    jurisdiction text not null,
    licence_terms text not null
        constraint source_licence_terms_not_blank check (btrim(licence_terms) <> ''),
    usage_rule text not null default 'extract_and_link'
        constraint source_usage_rule_known check (usage_rule in ('extract_and_link', 'full_text')),
    permission_evidence text,
    created_at timestamptz not null default now(),
    -- Full-text usage only ever with recorded written permission; neither
    -- null nor blank evidence is permission.
    constraint source_full_text_requires_evidence
        check (
            usage_rule <> 'full_text'
            or (permission_evidence is not null and btrim(permission_evidence) <> '')
        )
);

comment on table source is
    'A licensed RSS/Atom feed. usage_rule defaults to extract_and_link and is only upgraded after explicit founder review with recorded written permission.';
comment on column source.licence_terms is
    'The licence terms currently on record for this source. Snapshotted onto every source_item at retrieval (I-4).';

create table source_item (
    id uuid primary key default gen_random_uuid(),
    source_id uuid not null references source (id),
    source_url text not null
        constraint source_item_source_url_not_blank check (btrim(source_url) <> ''),
    original_title text,
    original_author text,
    published_at timestamptz,
    retrieved_at timestamptz not null default now(),
    -- Computed by the database from the stored body (UTF-8 database
    -- encoding): a fingerprint that cannot disagree with the evidence it
    -- fingerprints.
    content_hash text not null generated always as (encode(digest(raw_body, 'sha256'), 'hex')) stored,
    raw_body text not null,
    licence_snapshot text not null
        constraint source_item_licence_snapshot_not_blank check (btrim(licence_snapshot) <> ''),
    usage_rule_snapshot text not null
        constraint source_item_usage_rule_snapshot_known
            check (usage_rule_snapshot in ('extract_and_link', 'full_text')),
    permission_evidence_snapshot text,
    constraint source_item_unique_per_source unique (source_id, content_hash)
);

comment on table source_item is
    'IMMUTABLE (I-3). Exactly what was retrieved, when, and under which licence terms (I-2, I-4). Legal evidence; never updated, never deleted.';
comment on column source_item.licence_snapshot is
    'The source''s licence terms as they applied at retrieval time (I-4). Written by trigger from the source row in the same transaction - callers cannot record false terms. The legal defence rests on this value, not on source.licence_terms today.';
comment on column source_item.usage_rule_snapshot is
    'The usage rule in force at retrieval, written by trigger. Later source edits never rewrite the legal basis of what was already retrieved.';
comment on column source_item.permission_evidence_snapshot is
    'The permission evidence on record at retrieval, written by trigger; preserved even if the source row changes later.';
comment on column source_item.content_hash is
    'SHA-256 hex digest of raw_body, computed by the database (generated column); deduplicates retrievals and fingerprints the evidence.';
comment on column source_item.original_title is
    'The item title exactly as the feed provided it (null when the feed omitted one). Part of the retrieval evidence.';

create index source_item_source_id_idx on source_item (source_id);
create index source_item_retrieved_at_idx on source_item (retrieved_at);

-- I-4 authority: the snapshot columns are written from the source row in
-- the same transaction as the content. Caller-supplied values are ignored.
create function source_item_snapshot_terms() returns trigger
language plpgsql
as $$
begin
    select s.licence_terms, s.usage_rule, s.permission_evidence
      into new.licence_snapshot, new.usage_rule_snapshot, new.permission_evidence_snapshot
      from source s
     where s.id = new.source_id;
    return new;
end;
$$;

comment on function source_item_snapshot_terms() is
    'Copies licence terms, usage rule and permission evidence from the source row at insert (I-2, I-4). The database, not the caller, is the authority on what applied at retrieval.';

create trigger source_item_snapshot
    before insert on source_item
    for each row execute function source_item_snapshot_terms();

create trigger source_item_immutable
    before update or delete on source_item
    for each row execute function raise_immutable();
create trigger source_item_no_truncate
    before truncate on source_item
    for each statement execute function raise_immutable();

------------------------------------------------------------------------------
-- Translation lineage.
------------------------------------------------------------------------------

create table translation (
    id uuid primary key default gen_random_uuid(),
    source_item_id uuid not null references source_item (id),
    target_locale text not null references language (code),
    model text not null
        constraint translation_model_not_blank check (btrim(model) <> ''),
    prompt_version text not null
        constraint translation_prompt_version_not_blank check (btrim(prompt_version) <> ''),
    generated_at timestamptz not null default now(),
    -- Extract-and-link output: a translated headline and a short extract,
    -- never a full-text translation.
    headline text not null
        constraint translation_headline_not_blank check (btrim(headline) <> ''),
    extract text not null
        constraint translation_extract_not_blank check (btrim(extract) <> '')
);

comment on table translation is
    'IMMUTABLE machine translation lineage: which model, which prompt version, when, from which retrieved item (I-5). Corrections and re-translations create a new row; rewriting an old one would silently falsify the provenance of every article built on it.';

create index translation_source_item_id_idx on translation (source_item_id);

create trigger translation_immutable
    before update or delete on translation
    for each row execute function raise_immutable();
create trigger translation_no_truncate
    before truncate on translation
    for each statement execute function raise_immutable();

------------------------------------------------------------------------------
-- Identity: account, per-purpose consent, single entitlement gate.
-- Built in the first migration deliberately - retrofitting identity would
-- force a user migration later.
------------------------------------------------------------------------------

create table account (
    id uuid primary key default gen_random_uuid(),
    email text not null,
    display_name text not null
        constraint account_display_name_not_blank check (btrim(display_name) <> ''),
    created_at timestamptz not null default now()
);

comment on table account is
    'A person. Approvers referenced by article.approved_by are rows here: a named human, not a role or a service (I-1). id will align with Supabase Auth user ids once auth is wired.';

create unique index account_email_unique on account (lower(email));

create table consent (
    id uuid primary key default gen_random_uuid(),
    account_id uuid not null references account (id),
    purpose text not null
        constraint consent_purpose_not_blank check (btrim(purpose) <> ''),
    granted_at timestamptz not null default now(),
    revoked_at timestamptz,
    constraint consent_revoked_after_granted
        check (revoked_at is null or revoked_at >= granted_at)
);

comment on table consent is
    'Per-purpose consent rows, never a boolean column. Revocation closes a row; a new grant opens a new row, preserving the full consent history.';

create index consent_account_id_idx on consent (account_id);
create unique index consent_one_active_per_purpose
    on consent (account_id, purpose) where revoked_at is null;

-- Consent history is evidence of lawful processing: rows are never
-- deleted or rewritten. The only permitted change is the one-way
-- revocation transition; a re-grant is a new row.
create function consent_guard() returns trigger
language plpgsql
as $$
begin
    if tg_op = 'DELETE' then
        raise exception 'consent history is append-only: rows are never deleted, revocation closes them';
    end if;
    if new.id is distinct from old.id
        or new.account_id is distinct from old.account_id
        or new.purpose is distinct from old.purpose
        or new.granted_at is distinct from old.granted_at then
        raise exception 'consent identity and grant are frozen: a new grant is a new row';
    end if;
    if old.revoked_at is not null and new.revoked_at is distinct from old.revoked_at then
        raise exception 'consent revocation is one-way and final';
    end if;
    return new;
end;
$$;

comment on function consent_guard() is
    'Protects consent history: no deletes, no rewriting of identity or grant, revocation only ever moves from null to a timestamp.';

create trigger consent_guard
    before update or delete on consent
    for each row execute function consent_guard();
create trigger consent_no_truncate
    before truncate on consent
    for each statement execute function raise_immutable();

create function is_entitled(p_account_id uuid, p_action text) returns boolean
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

------------------------------------------------------------------------------
-- Articles: nothing exists without a named human approver.
------------------------------------------------------------------------------

create table article (
    id uuid primary key default gen_random_uuid(),
    translation_id uuid references translation (id),
    source_item_id uuid references source_item (id),
    approved_by uuid not null references account (id),
    approved_at timestamptz not null default now(),
    published_at timestamptz,
    attribution_block text not null
        constraint article_attribution_not_blank check (btrim(attribution_block) <> ''),
    -- An article is born from exactly one origin: a translation of a
    -- retrieved item, or the retrieved item itself (untranslated).
    constraint article_exactly_one_origin
        check (num_nonnulls(translation_id, source_item_id) = 1),
    constraint article_published_after_approval
        check (published_at is null or published_at >= approved_at)
);

comment on table article is
    'Only approved content. A row here IS the approval (I-1): approved_by is NOT NULL, so a draft or unapproved article is unrepresentable. The review queue operates on translations/source_items; approval creates the article.';
comment on column article.approved_by is
    'The named human who approved this article (I-1). Enforced by the database, not application code.';
comment on column article.attribution_block is
    'The attribution rendered with the article, pointing back to the original publisher.';

-- One article per origin, enforced by the database: a concurrent
-- double-approve cannot create two articles from the same translation or
-- retrieved item. Corrections flow through a new translation (or a new
-- retrieval), never a second article on the same origin.
create unique index article_one_per_translation
    on article (translation_id) where translation_id is not null;
create unique index article_one_per_source_item
    on article (source_item_id) where source_item_id is not null;
create index article_approved_by_idx on article (approved_by);
create index article_published_at_idx on article (published_at desc)
    where published_at is not null;

-- An article's identity, origin, approval and attribution are frozen at
-- approval; the provenance the view reports for a published article can
-- never be silently reassigned (I-1, I-5). The only permitted transition
-- is publication itself: published_at may go from null to a value, once.
create function article_guard() returns trigger
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

create trigger article_guard
    before update on article
    for each row execute function article_guard();
create trigger article_no_delete
    before delete on article
    for each row execute function raise_immutable();
create trigger article_no_truncate
    before truncate on article
    for each statement execute function raise_immutable();

create table article_place (
    article_id uuid not null references article (id),
    place_id uuid not null references place (id),
    primary key (article_id, place_id)
);

comment on table article_place is
    'Which places an article concerns. Many-to-many: language and place are independent axes.';

create index article_place_place_id_idx on article_place (place_id);

create table reader_place (
    account_id uuid not null references account (id),
    place_id uuid not null references place (id),
    primary key (account_id, place_id)
);

comment on table reader_place is
    'Which places a reader follows. Many-to-many: diaspora readers follow two places at once.';

create index reader_place_place_id_idx on reader_place (place_id);

------------------------------------------------------------------------------
-- Domain events: append-only audit stream.
------------------------------------------------------------------------------

create table domain_event (
    id uuid primary key default gen_random_uuid(),
    type text not null
        constraint domain_event_type_not_blank check (btrim(type) <> ''),
    payload jsonb not null,
    occurred_at timestamptz not null default now()
);

comment on table domain_event is
    'APPEND ONLY. What happened, when. Updates and deletes are blocked by trigger.';

create index domain_event_occurred_at_idx on domain_event (occurred_at);
create index domain_event_type_idx on domain_event (type);

create trigger domain_event_immutable
    before update or delete on domain_event
    for each row execute function raise_immutable();
create trigger domain_event_no_truncate
    before truncate on domain_event
    for each statement execute function raise_immutable();

------------------------------------------------------------------------------
-- I-5: full provenance of any article in one query.
------------------------------------------------------------------------------

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
