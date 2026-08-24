-- 0010_cashback_schema: the cashback product's own schema, role and grants.
--
-- Constitution v1.1.0 makes the product boundary structural: "A product
-- domain owns its own Postgres schema" and "No foreign key crosses a
-- product schema boundary." This migration creates that boundary and, just
-- as importantly, expresses it in privileges, so it is enforced by Postgres
-- rather than by everyone remembering it.
--
-- Nothing in the news schema changes. The only tables cashback is ever
-- granted on in `public` are the shared reference data the constitution
-- names - account, place, language - plus the append-only domain_event
-- stream that is the sole channel between the two products.
--
-- The cashback tables themselves arrive in 0011-0017. This migration is the
-- container and the permission model, so the boundary exists before the
-- first cashback table does.

create schema cashback;

comment on schema cashback is
    'The cashback product domain. Owns every cashback table; reads only public.account, public.place and public.language, and appends to public.domain_event. No foreign key from here may reach a news table.';

------------------------------------------------------------------------------
-- The domain role.
------------------------------------------------------------------------------

-- A NOLOGIN group role: a permission set, not an account. The application
-- connects as its own user and inherits this role, so the boundary can be
-- granted, audited and revoked in one place without managing a second
-- credential.
--
-- Roles are cluster-wide rather than database-scoped, so this migration can
-- find the role already there - a preview environment beside this one, or a
-- scratch database the test suite migrates in parallel.
--
-- The creation is written as CREATE-and-catch rather than check-then-create
-- on purpose. Two databases in the same cluster can migrate at the same
-- time (the test suite does exactly that), and each holds only its own
-- migration advisory lock, so an absence check can be overtaken between the
-- check and the CREATE. Both SQLSTATEs below mean the same thing here -
-- somebody else created it first, which is the outcome this block wanted -
-- so both are swallowed and nothing else is.
do $$
begin
    create role cashback_domain nologin;
exception
    when duplicate_object or unique_violation then
        null;
end;
$$;

------------------------------------------------------------------------------
-- Grants inside the cashback schema.
------------------------------------------------------------------------------

-- PUBLIC gets nothing. A new schema grants nothing by default; saying so
-- explicitly keeps the intent readable next to the grants that follow.
revoke all on schema cashback from public;

grant usage on schema cashback to cashback_domain;

-- Default privileges cover the tables 0011-0017 create, so each of those
-- migrations adds tables without also having to remember the grant. They
-- attach to the role running this statement, which is the migration role -
-- the same role every later migration runs as.
--
-- UPDATE and DELETE are granted deliberately. The tables that must never
-- change (click, network_transaction, reconciliation_run) are protected by
-- immutability TRIGGERS, which no grant can bypass, rather than by withheld
-- privileges, which a later GRANT could quietly undo.
alter default privileges in schema cashback
    grant select, insert, update, delete on tables to cashback_domain;

------------------------------------------------------------------------------
-- Grants across the boundary, and the silence where the news tables are.
------------------------------------------------------------------------------

grant usage on schema public to cashback_domain;

-- Shared reference data, read-only. Cashback resolves members, places and
-- languages; it never writes them.
grant select on public.account to cashback_domain;
grant select on public.place to cashback_domain;
grant select on public.language to cashback_domain;

-- The event stream is the only channel between the two products
-- (constitution, "Products"): cashback appends to it and reads it back. The
-- table is append-only by trigger, so INSERT is the whole write story.
grant select, insert on public.domain_event to cashback_domain;

-- Deliberately NOT granted: source, source_item, translation, article,
-- article_place, reader_place, consent and translation_spend. A cashback
-- query that reaches for a news table fails with a permission error rather
-- than succeeding and creating a coupling nobody notices.
