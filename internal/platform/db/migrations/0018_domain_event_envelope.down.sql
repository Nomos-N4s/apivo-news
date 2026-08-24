-- Reverses 0018_domain_event_envelope. Dropping the columns drops their
-- checks and the two indexes with them. Every event survives; only the
-- routing an event carries is lost, so a consumer that had been
-- deduplicating on idempotency_key would start accepting redeliveries.

-- Tolerant of already-missing objects: a down migration is run during
-- incident recovery, over databases that may have been partially applied
-- or hand-patched, and failing there because there was nothing left to
-- drop helps nobody. (Dropping the columns below would take these indexes
-- with them in any case; naming them keeps the rollback readable.)
drop index if exists domain_event_subject_idx;
drop index if exists domain_event_producer_idempotency_key_unique;

alter table domain_event
    drop column idempotency_key,
    drop column subject,
    drop column producer,
    drop column version;
