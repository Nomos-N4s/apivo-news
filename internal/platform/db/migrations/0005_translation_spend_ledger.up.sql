-- 0005_translation_spend_ledger: the ledger becomes the authority on spend.
--
-- FR-006 gives translation a budget: a per-article ceiling and a monthly
-- cap. 0002 recorded the two facts a budget is checked against - every
-- translation's cost, and a monthly ledger keyed by month - but it left the
-- relation between them to application code. Three holes followed, and all
-- three are closed here, in the database:
--
--   1. Nothing said one translation per item and locale, so a re-run, a
--      retry after a partial failure, or two workers on the same item each
--      bought the same translation again - and each new row appeared in the
--      editorial queue as its own approvable origin.
--   2. Nothing tied a translation to the ledger, so "a translation whose
--      cost is not in the ledger" was representable, and the cap was
--      checked against a total that application discipline alone kept true.
--   3. Nothing decided who crossed the cap first, so two workers crossing
--      it together would both announce the halt into an append-only stream
--      that cannot deduplicate.
--
-- WHAT DELIBERATELY STAYS IN APPLICATION CODE: the ceiling and the cap
-- themselves. data-model.md and the 0002 table comment both record them as
-- configuration, so a CHECK (cost_microusd <= 20000) would turn a budget
-- change into a migration - and, far worse, would REFUSE TO RECORD A COST
-- THAT WAS ALREADY PAID. An over-ceiling call is money out of the door
-- whether or not we wanted to spend it; a schema that rejects the row
-- destroys the honest record of it and leaves the ledger reading low by
-- exactly the amount that most needed to be seen.
--
-- The database's contribution is the atomic counter instead. The
-- single-statement upsert below takes the month row's lock and returns the
-- post-increment total, which SELECT-then-INSERT cannot do at any isolation
-- level a busy pipeline runs at: two workers reading the same total and
-- then writing their own would each record the other's spend as if it had
-- not happened. With the upsert, whoever gets the lock second sees the
-- first one's money.

------------------------------------------------------------------------------
-- One translation per item and locale.
------------------------------------------------------------------------------

-- Fail fast rather than surface a bare duplicate-key error from the index
-- build: translation rows are immutable AND undeletable (I-3 tooling, the
-- translation_immutable trigger), so duplicates cannot be cleaned up by an
-- operator on a whim - reconciling them is a founder decision about which
-- paid translation is the record. Pre-release every environment is empty
-- and this never fires.
do $$
begin
    if exists (
        select 1 from translation
         group by source_item_id, target_locale
        having count(*) > 1
    ) then
        raise exception 'more than one translation exists for a (source_item, locale) pair: reconcile them before applying 0005 - translation rows are immutable, so choosing which paid translation is the record is a founder decision, not a migration''s';
    end if;
end;
$$;

-- A translation is bought with real money and read by the editorial queue
-- as an approvable origin. Two of them for one item and locale is a double
-- charge AND a duplicated review decision, so the pair is unique.
--
-- Consequence worth naming: a deliberate RE-translation of the same item
-- into the same locale - a better model, a revised prompt - is now refused
-- too. That is the correct default while nothing distinguishes a re-run
-- from a re-translation; when the editorial flow needs one, it needs an
-- explicit supersession marker (and this index made partial on it), which
-- is a schema decision to take deliberately rather than a hole to leave
-- open. Corrections do not need it today: withdrawal frees an origin for
-- re-approval (0002), so a correction re-approves the translation on
-- record rather than buying another one.
create unique index translation_one_per_item_locale
    on translation (source_item_id, target_locale);

comment on index translation_one_per_item_locale is
    'One paid translation per retrieved item and target locale: a re-run, a retry after a partial failure or a second worker cannot buy the same translation twice, nor put two approvable origins for one item and locale in the editorial queue.';

------------------------------------------------------------------------------
-- The lower bound travels with the cost.
------------------------------------------------------------------------------

-- Without this column the ledger's unmetered_attempts (below) could only
-- ever be moved by the unbilled-spend path, and would stay 0 forever for
-- every SUCCESSFUL translation - because a row-level trigger sees only
-- `new`, i.e. a translation row, and the count would not be on it. The
-- flag that says "the recorded cost is a lower bound" would then be
-- missing from precisely the case it is produced for.
--
-- Unlike cost_microusd this column has a default, and the asymmetry is
-- deliberate: an omitted COST is an unknown amount of money and must be a
-- rejected insert, whereas an omitted COUNT of unpriced attempts is the
-- ordinary outcome - every attempt was priced - and 0 states exactly that.
alter table translation
    add column unmetered_attempts integer not null default 0
        constraint translation_unmetered_attempts_not_negative
            check (unmetered_attempts >= 0);

comment on column translation.unmetered_attempts is
    'How many provider attempts on the way to this translation were accepted but never priced (a call that timed out mid-generation, an answer with no usable token usage). Non-zero means cost_microusd is a LOWER BOUND: the provider may well have billed for them and the amount cannot be known here.';

------------------------------------------------------------------------------
-- The ledger row the translation cannot exist without.
------------------------------------------------------------------------------

alter table translation_spend
    add column unmetered_attempts integer not null default 0
        constraint translation_spend_unmetered_attempts_not_negative
            check (unmetered_attempts >= 0),
    add column halted_at timestamptz;

comment on column translation_spend.unmetered_attempts is
    'Unpriced provider attempts accumulated over the month. Non-zero means spent_microusd is a LOWER BOUND on what the month really cost, so the cap is nearer than the ledger says and an operator should see it.';
comment on column translation_spend.halted_at is
    'When this month crossed the monthly cap and the pipeline stopped translating. Latched by a single conditional UPDATE, so exactly one transaction can set it and the halt is announced once per month, not once per tick.';

comment on table translation_spend is
    'Monthly translation spend ledger. Every translation insert moves it by trigger, so a translation whose cost is not in the ledger is unrepresentable; spend from calls that were billed but produced no translation is added directly. The month key is the calendar month IN UTC - every writer and reader derives it as date_trunc(''month'', now() at time zone ''utc'')::date, never from the session TimeZone, so an operator''s psql session and the app pool always land spend in the same row. Caps (per-article ceiling, monthly cap) remain configuration - the database supplies the atomic counter and the once-per-month halt latch, not the budget.';

-- Following the article_record_withdrawal_event precedent (0002): the
-- derived row is written by the database, in the same transaction as the
-- fact it derives from, and the application does NOT also write it. A Go
-- side that upserted here too would double every cost.
create function translation_record_spend() returns trigger
language plpgsql
as $$
begin
    -- One statement, so it takes the month row's lock and releases it at
    -- commit: concurrent translations serialise on the counter instead of
    -- overwriting each other's totals. unmetered_attempts accumulates
    -- alongside the cost because the two are one fact - what the month
    -- spent, and how confident that number is.
    --
    -- The month is pinned to UTC: a bare date_trunc + ::date on a
    -- timestamptz both work in the session's TimeZone GUC, which nothing in
    -- this repo pins - an operator session at another offset would key a
    -- different row for the same instant, and the cap check would read only
    -- one of the two ledgers. See the translation_spend table comment.
    insert into translation_spend (month, spent_microusd, unmetered_attempts)
    values (date_trunc('month', now() at time zone 'utc')::date, new.cost_microusd, new.unmetered_attempts)
    on conflict (month) do update
       set spent_microusd = translation_spend.spent_microusd + excluded.spent_microusd,
           unmetered_attempts = translation_spend.unmetered_attempts + excluded.unmetered_attempts;
    return null;
end;
$$;

comment on function translation_record_spend() is
    'Moves the monthly ledger by this translation''s cost and its unpriced-attempt count, in the same transaction as the insert (FR-006). The application never upserts the ledger on the success path.';

create trigger translation_spend_ledger
    after insert on translation
    for each row execute function translation_record_spend();

-- The halt is an event in the record, not a log line, and it must appear
-- exactly once per month - same reasoning as the withdrawal event, same
-- shape. The latching UPDATE decides who crossed the cap first (its WHERE
-- clause matches one transaction only), and this trigger turns that single
-- winning update into the single announcement. Emitting it from
-- application code as well would duplicate it; emitting it from
-- application code INSTEAD would make a halted month with no halt event
-- representable.
--
-- The cap itself is not in the payload: it is configuration, it may differ
-- from the value in force when the month is read back, and the fact worth
-- recording is that this month stopped at this total.
create function translation_record_halt_event() returns trigger
language plpgsql
as $$
begin
    insert into domain_event (type, payload)
    values ('pipeline.halted', jsonb_build_object(
        'month', new.month,
        'spent_microusd', new.spent_microusd,
        'unmetered_attempts', new.unmetered_attempts,
        'halted_at', new.halted_at
    ));
    return null;
end;
$$;

comment on function translation_record_halt_event() is
    'Writes the pipeline.halted domain event - which month, at what total, when - in the same transaction as the halt latch itself (FR-006).';

create trigger translation_spend_halt_event
    after update on translation_spend
    for each row
    when (old.halted_at is null and new.halted_at is not null)
    execute function translation_record_halt_event();
