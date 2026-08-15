-- Reverses 0005_translation_spend_ledger, restoring the 0002 state of the
-- cost lineage: the ledger stops being written by the database, the month
-- stops being able to latch a halt, and a second paid translation of the
-- same item and locale becomes representable again.
--
-- Nothing is lost that the ledger itself holds - spent_microusd keeps every
-- amount the trigger accumulated - but the lower-bound counts and the halt
-- times go with their columns, and the pipeline.halted events already in
-- the append-only stream are left exactly where they are: they record
-- something that really happened.

drop trigger translation_spend_halt_event on translation_spend;
drop function translation_record_halt_event();

drop trigger translation_spend_ledger on translation;
drop function translation_record_spend();

alter table translation_spend
    drop column halted_at,
    drop column unmetered_attempts; -- drops its CHECK with it

comment on table translation_spend is
    'Monthly translation spend ledger, updated in the same transaction as each translation insert. Caps (per-article ceiling, monthly cap) are configuration; the translation module refuses work once the ledger reaches the cap and emits a pipeline.halted domain event.';

alter table translation drop column unmetered_attempts; -- drops its CHECK with it

drop index translation_one_per_item_locale;
