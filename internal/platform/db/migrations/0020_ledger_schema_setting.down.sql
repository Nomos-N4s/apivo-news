-- Reverses 0020_ledger_schema_setting, restoring 0016's body verbatim: the
-- C-1 check goes back to reading the schema named `blnk` and nothing else,
-- and the cashback.ledger_schema setting stops meaning anything.
--
-- The reader is restored FIRST so it no longer calls the two functions
-- dropped below. Their bodies are strings to PostgreSQL, so nothing here is
-- refused for a dependency it cannot see - which is exactly why the order
-- has to be written down rather than relied on to be enforced.
--
-- Verbatim includes 0016's comments. They record why this function raises
-- rather than returning null when the ledger is present but unreadable -
-- a bug this very function once had - and a rollback that dropped the
-- reasoning would leave the next reader free to reintroduce it. The round
-- trip compares function bodies, so "verbatim" is now a claim the suite
-- checks rather than one this header makes.

create or replace function cashback.ledger_balance_relation() returns text
language plpgsql
stable
as $$
declare
    balance_relation text;
begin
    -- Two questions, and the difference between them is the whole point.
    -- "Is the ledger installed here at all" and "can I read its postings"
    -- have different right answers, and an earlier version of this
    -- function conflated them: it returned null for both, so a ledger that
    -- was present but unreadable produced an empty view and a C-1 check
    -- that passed while seeing nothing. The comment two paragraphs up
    -- promised the opposite. That is the failure this whole invariant
    -- exists to prevent, committed by the check itself.
    if not exists (select 1 from pg_namespace where nspname = 'blnk') then
        -- No ledger schema: not co-located. Vacuously true is honest here,
        -- because there are no postings in this database to disagree about.
        return null;
    end if;

    balance_relation := to_regclass('blnk.balances')::text;
    if balance_relation is null then
        raise exception 'the ledger schema is present but blnk.balances is not readable: the C-1 zero-sum check cannot see the postings it exists to sum, and a check that cannot see them must fail rather than report zero rows';
    end if;
    return balance_relation;
end;
$$;

comment on function cashback.ledger_balance_relation() is
    'The ledger''s balance relation. Null only when the ledger is not co-located in this database at all; RAISES when the ledger schema is present but its balances cannot be read, because a zero-sum check that cannot see the postings must fail loudly rather than pass on nothing (C-1, ADR-0002).';

drop function cashback.ledger_schema();
drop function cashback.ledger_schema_override();
