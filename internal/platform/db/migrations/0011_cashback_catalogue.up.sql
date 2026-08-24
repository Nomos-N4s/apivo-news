-- 0011_cashback_catalogue: networks, merchants and the published rate bands.
--
-- The catalogue is what a member browses and what a click is issued
-- against. Two rules from the constitution shape every table here:
--
--   VII  Language and place are independent axes. Merchant copy is keyed by
--        BCP-47 primary language subtag; merchant reach is a many-to-many
--        relation to place. There is no combined locale anywhere.
--   IX   C-6, integer money. A rate is basis points (an integer) or a fixed
--        amount in minor units (a bigint) with an explicit ISO-4217
--        currency. No numeric, no float, no decimal - not here, not later.
--
-- A third rule comes from the business rather than the constitution, and
-- it decides the shape of three of these tables: THE SAME RETAILER IS
-- REACHABLE THROUGH SEVERAL NETWORKS AT ONCE. One retailer can be live on
-- two or more networks with different rates, different cookie windows and
-- different reliability, so:
--
--   * cashback.merchant holds the retailer as a business fact - who they
--     are, where they trade, whether we publish them;
--   * cashback.merchant_network holds one ROUTE to that retailer through
--     one network, and carries everything that is per-network, including
--     the FR-012 provenance of where the catalogue entry came from;
--   * cashback.offer hangs off the ROUTE, not off the retailer, because a
--     rate band is always a rate on one network's route.
--
-- Which route is used when several exist is arbitration, and arbitration
-- lives in the schema rather than in application code: a partial unique
-- index allows at most one preferred route per merchant.
--
-- ADR-0003 governs the network tables: credentials never enter the
-- database, and the poller's durable cursors live on the publisher account.

------------------------------------------------------------------------------
-- Networks and publisher accounts (ADR-0003).
------------------------------------------------------------------------------

create table cashback.network (
    -- A stable, human-typed identifier (awin, tradedoubler): it appears in
    -- configuration and in operator conversation, so it is the key rather
    -- than a surrogate nobody can say out loud.
    id text primary key
        constraint network_id_format check (id ~ '^[a-z][a-z0-9_]*$'),
    display_name text not null
        constraint network_display_name_not_blank check (btrim(display_name) <> ''),
    click_ref_param text not null
        constraint network_click_ref_param_not_blank check (btrim(click_ref_param) <> ''),
    max_query_window_days integer not null
        constraint network_max_query_window_positive check (max_query_window_days > 0),
    rate_limit_per_second integer not null
        constraint network_rate_limit_positive check (rate_limit_per_second > 0),
    -- Born inactive. A network starts publishing only when someone turns it
    -- on, so a half-configured row cannot begin issuing clicks by default.
    active boolean not null default false
);

comment on table cashback.network is
    'An affiliate network the cashback domain publishes through. Credentials are deliberately absent (ADR-0003): they are read from configuration, never stored here.';
comment on column cashback.network.click_ref_param is
    'The query parameter this network reads the click reference from (FR-021). The redirect is built from it, so a wrong value silently loses attribution for every click.';
comment on column cashback.network.max_query_window_days is
    'The network''s documented maximum transaction query window. The poller never asks for a wider one (FR-031, ADR-0003).';

create table cashback.network_account (
    id uuid primary key default gen_random_uuid(),
    network_id text not null references cashback.network (id),
    external_publisher_id text not null
        constraint network_account_publisher_id_not_blank check (btrim(external_publisher_id) <> ''),
    credential_ref text not null
        constraint network_account_credential_ref_not_blank check (btrim(credential_ref) <> ''),
    cursor_at timestamptz,
    trailing_cursor_at timestamptz,
    active boolean not null default false,
    constraint network_account_unique_per_network unique (network_id, external_publisher_id),
    -- The trailing cursor walks a window that has ALREADY BEEN READ, so it
    -- cannot exist before the main cursor does, and it cannot run ahead of
    -- it. Both halves matter: ahead means a restart advanced the wrong
    -- cursor, which is how a window gets skipped, and a trailing cursor
    -- with no main cursor is a re-read of history that was never read.
    constraint network_account_trailing_cursor_not_ahead
        check (
            trailing_cursor_at is null
            or (cursor_at is not null and trailing_cursor_at <= cursor_at)
        )
);

comment on table cashback.network_account is
    'A publisher account at a network. Two durable cursors: cursor_at advances only after a window is fully persisted (FR-031), trailing_cursor_at re-reads the ~100-day validation window to catch status changes (ADR-0003).';
comment on column cashback.network_account.credential_ref is
    'A KEY INTO CONFIGURATION naming where this account''s credential lives - never the credential itself (ADR-0003). Network credentials never enter the database or the repository.';
comment on column cashback.network_account.cursor_at is
    'How far forward transactions have been fully persisted. Advanced only after the whole window is written, so a restart re-fetches at most one window and never skips one (FR-031).';
comment on column cashback.network_account.trailing_cursor_at is
    'How far the slower trailing re-read has walked. Validation can take up to 90 days, so status changes are found by re-reading, not by waiting (ADR-0003).';

create index network_account_network_id_idx on cashback.network_account (network_id);

------------------------------------------------------------------------------
-- Merchants: the retailer, and separately every route to them.
------------------------------------------------------------------------------

create table cashback.merchant (
    id uuid primary key default gen_random_uuid(),
    slug text not null
        constraint merchant_slug_format check (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    -- Format check only, exactly as place.country does it: membership in
    -- the real ISO 3166-1 list is reference data, not a constraint. Null
    -- for a merchant that is not bound to one country.
    country char(2)
        constraint merchant_country_alpha2_format check (country is null or country ~ '^[A-Z]{2}$'),
    source_language_code text not null references public.language (code),
    status text not null default 'active'
        constraint merchant_status_known check (status in ('active', 'paused', 'left_network')),
    constraint merchant_slug_unique unique (slug)
);

comment on table cashback.merchant is
    'A retailer, as a business fact: who they are, where they trade, whether we publish them. Deliberately carries NOTHING per-network - the same retailer is reachable through several networks at once, and each of those routes is a merchant_network row.';
comment on column cashback.merchant.source_language_code is
    'The language the retailer''s copy is supplied in. When a member''s language has no merchant_copy row, this is the copy that is shown AND LABELLED as a fallback (US5 scenario 2) - never a blank, never a machine-invented name.';
comment on column cashback.merchant.status is
    'Whether this retailer is published at all. Independent of any single route: a merchant leaving one network is a merchant_network status, not this one.';

create index merchant_status_idx on cashback.merchant (status);

create table cashback.merchant_network (
    id uuid primary key default gen_random_uuid(),
    merchant_id uuid not null references cashback.merchant (id),
    network_id text not null references cashback.network (id),
    -- ADR-0004 names four records where a tenant boundary would fall, and
    -- "merchant availability" is one of them. Availability is this table:
    -- the retailer is a fact about the world, but which brand publishes
    -- them, through which network, is brand-scoped. The column is created
    -- with the table rather than added later, so it exists at every point
    -- in the migration history where a route does.
    --
    -- No default, deliberately. A brand literal in a migration is what the
    -- constitution's rebrandability rule forbids, and a row whose brand
    -- nobody stated is a row nobody can scope later.
    brand_id text not null
        constraint merchant_network_brand_id_not_blank check (btrim(brand_id) <> ''),
    external_merchant_id text not null
        constraint merchant_network_external_id_not_blank
            check (btrim(external_merchant_id) <> ''),
    retrieved_at timestamptz not null,
    raw_payload jsonb not null,
    status text not null default 'active'
        constraint merchant_network_status_known
            check (status in ('active', 'paused', 'left_network')),
    -- Which route to use when several exist. Arbitration is a rule, and a
    -- rule this schema can carry is a rule application code cannot get
    -- wrong: see merchant_network_one_preferred below.
    preferred boolean not null default false,
    -- One network's own id belongs to exactly one route.
    constraint merchant_network_unique_per_network unique (network_id, external_merchant_id),
    -- And one retailer has at most one route per network: two rows would
    -- be two cursors over the same commission stream.
    constraint merchant_network_one_route_per_network unique (merchant_id, network_id)
);

comment on table cashback.merchant_network is
    'One route to a retailer through one network. The same retailer is commonly live on several networks at once with different rates and different reliability, so everything per-network lives here - the network''s own id for them, when the catalogue entry was retrieved, and the payload it came from (FR-012).';
comment on column cashback.merchant_network.external_merchant_id is
    'The network''s own identifier for this retailer. Unique within the network: it is what an imported catalogue row and a reported transaction are matched back on.';
comment on column cashback.merchant_network.retrieved_at is
    'When this route''s catalogue entry was imported (FR-012). A property of the import, never of the retailer.';
comment on column cashback.merchant_network.raw_payload is
    'The network''s catalogue payload verbatim (FR-012), kept beside the normalised columns so a mapping bug is diagnosable without re-fetching.';
comment on column cashback.merchant_network.preferred is
    'Whether this is the route the catalogue publishes for this retailer. At most one route per merchant may be preferred, enforced by a partial unique index rather than by whichever code path happens to run first.';

comment on column cashback.merchant_network.brand_id is
    'Which brand publishes this route (ADR-0004). One value today; the column exists so adding a tenant is scoping rather than a schema archaeology exercise.';

create index merchant_network_merchant_id_idx on cashback.merchant_network (merchant_id);
create index merchant_network_network_id_idx on cashback.merchant_network (network_id);

-- The arbitration rule, made structural: at most one preferred route per
-- merchant. Rows that are not preferred do not collide, so a retailer may
-- have as many routes as networks carry them.
create unique index merchant_network_one_preferred
    on cashback.merchant_network (merchant_id)
    where preferred;

create table cashback.merchant_copy (
    merchant_id uuid not null references cashback.merchant (id),
    language_code text not null references public.language (code),
    name text not null
        constraint merchant_copy_name_not_blank check (btrim(name) <> ''),
    summary text
        constraint merchant_copy_summary_not_blank check (summary is null or btrim(summary) <> ''),
    terms text
        constraint merchant_copy_terms_not_blank check (terms is null or btrim(terms) <> ''),
    constraint merchant_copy_one_per_language primary key (merchant_id, language_code)
);

comment on table cashback.merchant_copy is
    'Per-language merchant copy, keyed by BCP-47 primary language subtag (constitution VII). A missing language is a missing row, resolved by labelled fallback to the merchant''s source language - never by inventing text. Copy describes the RETAILER, so it does not vary by route.';

create index merchant_copy_language_code_idx on cashback.merchant_copy (language_code);

create table cashback.merchant_place (
    merchant_id uuid not null references cashback.merchant (id),
    place_id uuid not null references public.place (id),
    constraint merchant_place_pkey primary key (merchant_id, place_id)
);

comment on table cashback.merchant_place is
    'Which places a merchant is available to. Many-to-many, and entirely independent of merchant_copy: a Greek speaker in Munich sees Munich merchants in Greek (constitution VII).';

create index merchant_place_place_id_idx on cashback.merchant_place (place_id);

------------------------------------------------------------------------------
-- Offers: the published rate bands, per route (C-6).
------------------------------------------------------------------------------

create table cashback.offer (
    id uuid primary key default gen_random_uuid(),
    -- A rate band belongs to a ROUTE, not to a retailer: the same retailer
    -- pays different rates on different networks, and the network a click
    -- is issued through follows from the band rather than being chosen
    -- beside it. One column, and a whole class of mismatch disappears.
    merchant_network_id uuid not null references cashback.merchant_network (id),
    rate_kind text not null
        constraint offer_rate_kind_known check (rate_kind in ('percent', 'fixed')),
    -- Basis points, not a percentage with a decimal point: 4% is 400. C-6
    -- is not only about money columns, it is about never introducing a
    -- fractional type that Go and JavaScript will disagree about.
    rate_bps integer
        constraint offer_rate_bps_range check (rate_bps is null or rate_bps between 0 and 10000),
    rate_fixed_minor bigint
        constraint offer_rate_fixed_positive check (rate_fixed_minor is null or rate_fixed_minor > 0),
    currency char(3)
        constraint offer_currency_iso4217_format check (currency is null or currency ~ '^[A-Z]{3}$'),
    member_share_bps integer not null
        constraint offer_member_share_bps_range check (member_share_bps between 0 and 10000),
    conditions text,
    exclusions text,
    valid_from timestamptz not null default now(),
    valid_to timestamptz,
    deeplink_template text not null
        constraint offer_deeplink_template_not_blank check (btrim(deeplink_template) <> ''),
    -- Exactly the fields for the chosen rate kind, and nothing else. A
    -- percent band with a stray currency, or a fixed band with no currency,
    -- is a rate nobody can compute a credit from (C-6).
    constraint offer_rate_kind_fields check (
        case rate_kind
            when 'percent' then rate_bps is not null
                and rate_fixed_minor is null
                and currency is null
            when 'fixed' then rate_fixed_minor is not null
                and currency is not null
                and rate_bps is null
            else false
        end
    ),
    constraint offer_validity_window check (valid_to is null or valid_to > valid_from)
);

comment on table cashback.offer is
    'A published rate band on one network''s route to a retailer. Every band records its conditions and its exclusions (FR-011), and the network it is sourced from follows from the route (merchant_network) rather than being repeated here where the two could disagree. All of it is snapshotted onto the click, because the click-time rate governs the credit (FR-013).';
comment on column cashback.offer.merchant_network_id is
    'The route this rate applies to. A retailer live on two networks has a band per network, and a click issued against a band is issued through that band''s network.';
comment on column cashback.offer.rate_bps is
    'The network''s commission rate in basis points when rate_kind is percent (4% = 400). Integer by construction: no fractional type ever enters the money path (C-6).';
comment on column cashback.offer.rate_fixed_minor is
    'The network''s commission as a fixed amount in minor units when rate_kind is fixed, always with an explicit currency (C-6).';
comment on column cashback.offer.member_share_bps is
    'The share of the commission the member receives, in basis points. Configuration, not a committed value - founder question Q4 is open and the schema carries no default for it.';
comment on column cashback.offer.deeplink_template is
    'The template the click-out redirect is built from. The click reference is substituted into the network''s own click-reference parameter (FR-021).';

create index offer_merchant_network_idx on cashback.offer (merchant_network_id);

-- Catalogue reads ask for the bands in force at a moment: valid_from has
-- passed and valid_to has not. An index predicate cannot say "now" - now()
-- is not IMMUTABLE - so the moment is a query parameter and the index has
-- to cover the whole validity window rather than one end of it.
--
-- An earlier version of this index was partial on `valid_to is null`, which
-- indexed only open-ended bands. A band with a future valid_to is just as
-- current, and the query that looked for it would have fallen out of the
-- index and onto a scan - so the index covered a narrower set than the
-- comment above it claimed. coalesce(valid_to, 'infinity') is immutable and
-- makes "still open" and "closes later" the same shape, so one index serves
-- the query `where merchant_network_id = $1 and valid_from <= $2
-- and coalesce(valid_to, 'infinity') > $2`.
create index offer_validity_window_idx
    on cashback.offer (merchant_network_id, coalesce(valid_to, 'infinity'::timestamptz), valid_from desc);
