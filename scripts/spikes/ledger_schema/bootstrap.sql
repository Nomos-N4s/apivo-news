-- Spike S1 (ADR-0002) — the restricted role the Blnk ledger RUNS as.
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
-- role that reaches the ledger, in the file an operator is most likely to run
-- verbatim against a real database. It refuses now instead.
--
-- The file is otherwise self-contained: it takes the database name from
-- current_database() rather than from a psql variable, so the invocation
-- above is the whole invocation.
--
-- ---------------------------------------------------------------------------
-- THE POSTURE (founder decision, 2026-08-24)
-- ---------------------------------------------------------------------------
--
-- Two roles, split by what they do rather than by convenience:
--
--   the database owner   runs `blnk migrate up`. Migrations are DDL, they
--                        happen on a deploy, and they are reviewed.
--   blnk_app             runs the ledger SERVER, day in and day out, with
--                        USAGE on one schema and DML on its tables. No DDL,
--                        no CREATE on the database, no reach into `public`.
--
-- Spike S1 established why the split is needed. Blnk's first migration issues
-- `CREATE SCHEMA IF NOT EXISTS blnk`, and PostgreSQL checks the
-- database-level CREATE privilege BEFORE it takes the IF NOT EXISTS
-- shortcut — so pre-creating the schema does not avoid the grant, and a
-- single role doing both jobs would need CREATE on the database permanently,
-- for the sake of one statement on one deploy. The founder took the split
-- instead, so the role that is exposed every second of every day is the
-- narrow one.
--
-- What blnk_app therefore cannot do, and what that buys:
--
--   CREATE SCHEMA          it cannot give itself a second home
--   DDL inside blnk        a compromised ledger process cannot drop or
--                          reshape the tables that hold members' balances
--   anything in public     Apivo's tables are legal evidence, and the ledger
--                          has no business seeing them
--
-- Grants on the ledger's tables come from ALTER DEFAULT PRIVILEGES below,
-- applied BEFORE the migration runs, so every table the migration creates is
-- readable and writable by blnk_app the moment it exists. Spike S1's check 8
-- verifies that actually happened rather than assuming it.

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

-- The schema, created and OWNED by the database owner. blnk_app is a guest
-- in it: it may use what is there, and may not change its shape.
CREATE SCHEMA IF NOT EXISTS blnk;

-- What blnk_app MAY do: connect, and use its one schema.
--
-- GRANT takes a database NAME, not an expression, so the database-level
-- statements go through format() with current_database(). That is what keeps
-- this file runnable exactly as documented above.
DO $$
BEGIN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO blnk_app', current_database());
END
$$;

GRANT USAGE ON SCHEMA blnk TO blnk_app;

-- DML on every table the migration is ABOUT to create.
--
-- ALTER DEFAULT PRIVILEGES applies to objects created LATER, by the named
-- role, which is why this runs before `blnk migrate up` and names the owner
-- that will run it. Get this wrong and the ledger starts, connects, and
-- fails on its first query with "permission denied" — which is why S1's
-- check 8 proves the grant landed rather than trusting it.
DO $$
BEGIN
    EXECUTE format(
        'ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA blnk '
        'GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO blnk_app', current_user);
    EXECUTE format(
        'ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA blnk '
        'GRANT USAGE, SELECT ON SEQUENCES TO blnk_app', current_user);
END
$$;

-- And on anything already there, so re-running this after a migration
-- repairs grants rather than only affecting the next one. No-ops on a fresh
-- database.
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA blnk TO blnk_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA blnk TO blnk_app;

-- What blnk_app MAY NOT do, stated explicitly rather than left to a default
-- that a future Postgres or a managed provider might change.
--
-- No CREATE on the schema: the runtime role does no DDL. Migrations are the
-- owner's job.
REVOKE CREATE ON SCHEMA blnk FROM blnk_app;

-- No CREATE on the database: it cannot make itself a second home. This is
-- the grant the founder's posture exists to avoid needing at all.
DO $$
BEGIN
    EXECUTE format('REVOKE CREATE ON DATABASE %I FROM blnk_app', current_database());
END
$$;

-- Nothing in `public`. USAGE there is granted to PUBLIC by default and is
-- what would let this role read Apivo's tables if any grant ever reached it;
-- CREATE on `public` has not been granted to PUBLIC since Postgres 15, and
-- revoking it again here costs nothing and survives a database restored from
-- an older dump.
REVOKE ALL ON SCHEMA public FROM blnk_app;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM blnk_app;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM blnk_app;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA public FROM blnk_app;

-- Belt and braces on top of Blnk's own `SET search_path TO blnk`: even a
-- session that skipped it resolves unqualified names inside blnk, never in
-- public.
ALTER ROLE blnk_app SET search_path = blnk;

-- A table in `public` for the spike to prove the role cannot read. It is
-- created before the first snapshot and dropped after the last, so it is
-- present in both and does not register as a difference.
CREATE TABLE IF NOT EXISTS public.spike_s1_probe (id integer PRIMARY KEY);
