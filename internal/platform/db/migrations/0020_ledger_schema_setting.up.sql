-- 0020_ledger_schema_setting: the C-1 check stops hardcoding where the
-- ledger lives, so it can be proved anywhere - including where a real
-- ledger is already running.
--
-- 0016 wrote the schema name into the check twice: a pg_namespace lookup
-- for nspname = 'blnk', and to_regclass('blnk.balances'). That made the
-- one assertion the money system cannot do without - C-1, the invariant
-- ADR-0002 moved OUT of our schema and promised to verify continuously -
-- the only assertion in the repository that cannot be exercised against a
-- database where the thing it checks is present. A test that wants to show
-- the check can see an imbalance has to build a stand-in ledger, and the
-- only name it may build it under is the name the real one already holds.
-- Spike S1 creates `blnk` before the suite runs, so the test could not
-- create it and the whole cashback job went red.
--
-- The fix is not to move the test out of the way. It is to let the check be
-- told where to look:
--
--   cashback.ledger_schema   a Postgres setting, `blnk` when nobody sets
--                            one. Production never sets it, so nothing
--                            about the deployed check changes.
--
-- and a test then does
--
--   select set_config('cashback.ledger_schema', 'ledger_standin_1a2b', true)
--
-- inside its own rolled-back transaction, builds a ledger there, and
-- collides with nothing - not with the real ledger beside it, and not with
-- another test running at the same time.
--
-- ---------------------------------------------------------------------------
-- The setting cannot become a way to switch C-1 off
-- ---------------------------------------------------------------------------
--
-- A knob that selects which schema to check is also a knob that can point
-- the check at a schema that is not there, and 0016's "no ledger here means
-- vacuously true" would then quietly swallow a typo: the view would be
-- empty, the assertion would pass, and nobody would be summing anything.
-- That is precisely the failure the second half of 0016 exists to prevent,
-- re-introduced through the front door.
--
-- So the two cases are separated by whether anyone NAMED a schema:
--
--   nobody set it, `blnk` is absent    the ledger is not co-located.
--                                      Null, and the check is vacuously
--                                      true - honest, and unchanged from
--                                      0016.
--   someone set it, that schema is     a claim about where the ledger is
--   absent                             that is not true. RAISES.
--   the schema is there but its        RAISES, exactly as 0016.
--   balances are not readable
--
-- Which leaves the check strictly harder to blind than it was: before this
-- migration the only way to reach "vacuously true" was for the ledger to be
-- genuinely absent, and that is still the only way.

------------------------------------------------------------------------------
-- Where the ledger lives.
------------------------------------------------------------------------------

create function cashback.ledger_schema_override() returns text
language sql
stable
as $$
    -- Null when nobody named a schema. current_setting's missing_ok form is
    -- what makes an unset custom setting a null rather than an error, and
    -- the nullif collapses a setting that was set to nothing at all into
    -- the same "nobody named one" - a blank is not a schema name.
    select nullif(btrim(coalesce(current_setting('cashback.ledger_schema', true), '')), '');
$$;

comment on function cashback.ledger_schema_override() is
    'The ledger schema someone explicitly named through the cashback.ledger_schema setting, or null when nobody did. The distinction is load-bearing: a NAMED schema that is absent is a false claim about where the ledger is and raises, where an absent default merely means the ledger is not co-located here (C-1, ADR-0002).';

create function cashback.ledger_schema() returns text
language sql
stable
as $$
    select coalesce(cashback.ledger_schema_override(), 'blnk');
$$;

comment on function cashback.ledger_schema() is
    'The schema the ledger''s own tables live in: whatever cashback.ledger_schema is set to, and blnk otherwise - the name Blnk gives itself and the name scripts/spikes/ledger_schema/bootstrap.sql creates. Deployments do not set it; tests do, so the C-1 check can be proved against a ledger they built rather than only against the one they must not touch.';

------------------------------------------------------------------------------
-- C-1's reader, no longer pinned to one schema name.
------------------------------------------------------------------------------

create or replace function cashback.ledger_balance_relation() returns text
language plpgsql
stable
as $$
declare
    named_schema text := cashback.ledger_schema_override();
    resolved_schema text := cashback.ledger_schema();
    balance_relation text;
begin
    -- Two questions, and the difference between them is the whole point.
    -- "Is the ledger installed here at all" and "can I read its postings"
    -- have different right answers, and an earlier version of this
    -- function conflated them: it returned null for both, so a ledger that
    -- was present but unreadable produced an empty view and a C-1 check
    -- that passed while seeing nothing. That is the failure this whole
    -- invariant exists to prevent, committed by the check itself.
    if not exists (select 1 from pg_namespace where nspname = resolved_schema) then
        if named_schema is not null then
            -- Somebody said the ledger is in this schema and it is not.
            -- Reporting "no ledger here, nothing to check" would turn a
            -- misconfiguration into a silent pass.
            raise exception 'cashback.ledger_schema names the schema %, which does not exist: the C-1 zero-sum check was pointed at a ledger that is not there, and a check that cannot see the postings it exists to sum must fail rather than report zero rows', named_schema;
        end if;
        -- No ledger schema and nobody claimed otherwise: not co-located.
        -- Vacuously true is honest here, because there are no postings in
        -- this database to disagree about.
        return null;
    end if;

    balance_relation := to_regclass(format('%I.balances', resolved_schema))::text;
    if balance_relation is null then
        raise exception 'the ledger schema % is present but its balances relation is not readable: the C-1 zero-sum check cannot see the postings it exists to sum, and a check that cannot see them must fail rather than report zero rows', resolved_schema;
    end if;
    return balance_relation;
end;
$$;

comment on function cashback.ledger_balance_relation() is
    'The ledger''s balance relation, in whatever schema cashback.ledger_schema() resolves to. Null only when the ledger is not co-located in this database and nobody claimed it was; RAISES when a schema was named and is absent, and when the schema is present but its balances cannot be read - because a zero-sum check that cannot see the postings must fail loudly rather than pass on nothing (C-1, ADR-0002).';
