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
    country text not null
        constraint place_country_is_iso3166_alpha2 check (country ~ '^[A-Z]{2}$'),
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
    licence_terms text not null,
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
    original_author text,
    published_at timestamptz,
    retrieved_at timestamptz not null default now(),
    content_hash text not null
        constraint source_item_content_hash_is_sha256_hex check (content_hash ~ '^[0-9a-f]{64}$'),
    raw_body text not null,
    licence_snapshot text not null
        constraint source_item_licence_snapshot_not_blank check (btrim(licence_snapshot) <> ''),
    constraint source_item_unique_per_source unique (source_id, content_hash)
);

comment on table source_item is
    'IMMUTABLE (I-3). Exactly what was retrieved, when, and under which licence terms (I-2, I-4). Legal evidence; never updated, never deleted.';
comment on column source_item.licence_snapshot is
    'The source''s licence terms as they applied at retrieval time (I-4). The legal defence rests on this value, not on source.licence_terms today.';
comment on column source_item.content_hash is
    'SHA-256 hex digest of raw_body; deduplicates retrievals and fingerprints the evidence.';

create index source_item_source_id_idx on source_item (source_id);
create index source_item_retrieved_at_idx on source_item (retrieved_at);

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
    body text not null
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

create index article_translation_id_idx on article (translation_id);
create index article_source_item_id_idx on article (source_item_id);
create index article_approved_by_idx on article (approved_by);
create index article_published_at_idx on article (published_at desc)
    where published_at is not null;

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
    s.id              as source_id,
    s.name            as source_name,
    s.url             as source_feed_url,
    s.jurisdiction,
    s.usage_rule
from article a
join account approver on approver.id = a.approved_by
left join translation t on t.id = a.translation_id
join source_item si on si.id = coalesce(t.source_item_id, a.source_item_id)
join source s on s.id = si.source_id;

comment on view article_provenance is
    'I-5: for any article - source, licence snapshot at retrieval, model, prompt version and named approver, in a single query.';
