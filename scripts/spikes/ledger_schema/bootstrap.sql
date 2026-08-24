-- Spike S1 (ADR-0002) — the restricted role the Blnk ledger connects as.
--
-- This file is the production recipe as much as it is the spike's setup: it
-- is what an operator runs, once, against the Supabase (EU) database before
-- pointing Blnk at it. Everything it grants is deliberate, and everything it
-- does NOT grant is the point.
--
-- Run it as the database owner:
--
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f bootstrap.sql
--
-- The credentials below are for the local compose stack and for CI, both of
-- which live on a loopback address behind no network anyone else can reach.
-- A deployment creates the same role with a real secret, out of band, and
-- never from a file in this repository.

-- The role. Created only when absent, so the file is re-runnable: a spike
-- that cannot be re-run is a spike nobody runs twice.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'blnk_app') THEN
        CREATE ROLE blnk_app LOGIN PASSWORD 'blnk_app';
    END IF;
END
$$;

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
GRANT CONNECT ON DATABASE :"dbname" TO blnk_app;
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

-- No CREATE on the database: the role cannot make itself a second home.
REVOKE CREATE ON DATABASE :"dbname" FROM blnk_app;

-- Belt and braces on top of Blnk's own `SET search_path TO blnk`: even a
-- session that skipped it resolves unqualified names inside blnk, never in
-- public.
ALTER ROLE blnk_app SET search_path = blnk;

-- A table in `public` for the spike to prove the role cannot read. It is
-- created before the first snapshot and dropped after the last, so it is
-- present in both and does not register as a difference.
CREATE TABLE IF NOT EXISTS public.spike_s1_probe (id integer PRIMARY KEY);
