-- Spike S1 (ADR-0002) — the restricted role the Blnk ledger connects as.
--
-- This file is the production recipe as much as it is the spike's setup: it
-- is what an operator runs, once, against the Supabase (EU) database before
-- pointing Blnk at it. Everything it grants is deliberate, and everything it
-- does NOT grant is the point.
--
-- Run it as the database owner, exactly like this:
--
--   psql "$DATABASE_URL" \
--       -v ON_ERROR_STOP=1 \
--       -v blnk_password="$(openssl rand -base64 32)" \
--       -f bootstrap.sql
--
-- Two things about that invocation are load-bearing.
--
-- ON_ERROR_STOP=1 is REQUIRED. Several statements below are guards that work
-- by raising, and without it psql reports the error and carries on, leaving a
-- half-configured role behind and exiting 0.
--
-- blnk_password is REQUIRED and has no default. An earlier revision of this
-- file created the production-named role `blnk_app` with the password
-- `blnk_app` hardcoded here — a credential in a public repository, on the
-- role that owns the ledger, in the file an operator is most likely to run
-- verbatim against a real database. It refuses now instead.
--
-- The file is otherwise self-contained: it takes the database name from
-- current_database() rather than from a psql variable, so the invocation
-- above is the whole invocation.

-- Guard: refuse without a password. RAISE rather than \quit, because \quit
-- exits psql with status 0 and a caller would read that as success.
\if :{?blnk_password}
\else
DO $$
BEGIN
    RAISE EXCEPTION USING
        MESSAGE = 'bootstrap.sql requires a password for the blnk_app role',
        HINT    = 'psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -v blnk_password="$(openssl rand -base64 32)" -f bootstrap.sql';
END
$$;
\endif

-- The role. Created when absent and re-passworded when present, so the file
-- is re-runnable: a recipe that only works on an empty database is a recipe
-- nobody can re-apply after rotating a credential.
--
-- The interpolation is at top level and not inside a dollar-quoted block on
-- purpose: psql does not substitute variables inside quoted strings, so
-- :'blnk_password' has to sit where the parser can see it. psql quotes it as
-- a literal, so a password containing quotes is safe.
SELECT NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'blnk_app') AS blnk_role_absent \gset

\if :blnk_role_absent
CREATE ROLE blnk_app LOGIN PASSWORD :'blnk_password';
\else
ALTER ROLE blnk_app LOGIN PASSWORD :'blnk_password';
\endif

-- The schema. Created here, by the database owner, rather than left to
-- Blnk's first migration: a role that may create schemas may create them
-- anywhere, and the whole point of this role is that it may not.
--
-- The schema itself stays owned by the database owner and is only granted to
-- blnk_app. That is deliberate: the ledger may fill its schema and may not
-- drop it, and the objects it creates inside are still its own.
CREATE SCHEMA IF NOT EXISTS blnk;

-- What the role MAY do: connect, and own its own schema completely. Blnk
-- creates tables, indexes, sequences and its own migration bookkeeping in
-- there, so it needs CREATE, not merely USAGE.
--
-- GRANT takes a database NAME, not an expression, so the two database-level
-- statements go through format() with current_database(). That is what keeps
-- this file runnable exactly as documented above.
DO $$
BEGIN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO blnk_app', current_database());
END
$$;

GRANT USAGE, CREATE ON SCHEMA blnk TO blnk_app;

-- What the role MAY NOT do, stated explicitly rather than left to a default
-- that a future Postgres or a managed provider might change.
--
-- USAGE on `public` is granted to PUBLIC by default and is what would let
-- this role read Apivo's tables if any grant ever reached it; CREATE on
-- `public` has not been granted to PUBLIC since Postgres 15, and revoking it
-- again here costs nothing and survives a database restored from an older
-- dump.
REVOKE ALL ON SCHEMA public FROM blnk_app;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM blnk_app;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM blnk_app;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA public FROM blnk_app;

-- No CREATE on the database by default: the role cannot make itself a second
-- home.
--
-- Spike S1 found that Blnk's first migration issues
-- `CREATE SCHEMA IF NOT EXISTS blnk`, and PostgreSQL checks the
-- database-level privilege BEFORE it takes the IF NOT EXISTS shortcut — so
-- pre-creating the schema above does not avoid the grant, and the migration
-- fails with "permission denied for database" without it. The grant is
-- therefore added by run.sh, deliberately and visibly, rather than baked in
-- here where a reader would never learn it was needed. It widens the role to
-- "may create new schemas"; it does NOT widen it to `public`, which the
-- revokes above and checks 6 and 7 of the spike prove.
DO $$
BEGIN
    EXECUTE format('REVOKE CREATE ON DATABASE %I FROM blnk_app', current_database());
END
$$;

-- Belt and braces on top of Blnk's own `SET search_path TO blnk`: even a
-- session that skipped it resolves unqualified names inside blnk, never in
-- public.
ALTER ROLE blnk_app SET search_path = blnk;

-- A table in `public` for the spike to prove the role cannot read. It is
-- created before the first snapshot and dropped after the last, so it is
-- present in both and does not register as a difference.
CREATE TABLE IF NOT EXISTS public.spike_s1_probe (id integer PRIMARY KEY);
