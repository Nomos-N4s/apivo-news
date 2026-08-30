-- 0024_unattributed_queue_guards: the unattributed queue is evidence, and
-- these are the two ways it could still be edited out of existence.
--
-- 0013 froze which report a queue row names and when it was noticed, and
-- said why: "The row is the record that this report was SEEN and went
-- unattributed... the history of what an operator actually looked at". Two
-- holes were left in that, and both are reachable today - each was confirmed
-- by exercising it against the migrated schema, not inferred from reading.
--
--   1. The row can be DELETED, and the table TRUNCATED. Every sibling in
--      0012 and 0013 refuses both - click, network_transaction, entry,
--      entry_transition and ledger_link all carry raise_immutable() - and
--      this row is evidence in exactly the same sense. Without the guard,
--      "delete the row that looks stale" is a legal repair for a row that
--      was never wrong, and the record of what an operator was shown is
--      gone.
--
--   2. A recorded resolution can be UN-WRITTEN. Setting resolved_by,
--      resolved_reason and resolved_at all back to null passes the existing
--      guard and passes unattributed_resolution_all_or_none, whose
--      num_nonnulls(...) in (0, 3) admits zero precisely so an unresolved
--      row is representable. FR-061 makes the audit record part of the
--      action; an action whose record can be erased is not audited. A
--      mistaken dismissal is corrected by appending to domain_event, where
--      every operator action already goes, not by rewriting the record that
--      it happened.
--
-- No new column and no new state. A queue row still says only: this report
-- was seen unattributed, at this instant, and here is who resolved it and
-- why, if anyone has. Whether it is still WORK is a question about the chain
-- and is answered by a read (T058), not by editing the row.

create trigger unattributed_transaction_no_delete
    before delete on cashback.unattributed_transaction
    for each row execute function public.raise_immutable();
create trigger unattributed_transaction_no_truncate
    before truncate on cashback.unattributed_transaction
    for each statement execute function public.raise_immutable();

create or replace function cashback.unattributed_transaction_guard() returns trigger
language plpgsql
as $$
begin
    if new.id is distinct from old.id
        or new.network_transaction_id is distinct from old.network_transaction_id
        or new.detected_at is distinct from old.detected_at then
        raise exception 'an unattributed transaction names which report went unattributed and when it was noticed (FR-034): both are frozen, only the resolution may be recorded';
    end if;
    -- Appended to, never erased. The all-or-none check still governs the
    -- shape of a resolution; this governs its direction.
    if old.resolved_at is not null and new.resolved_at is null then
        raise exception 'an unattributed transaction records who resolved it and why (FR-061): a resolution is appended to, never erased - correct a mistaken one by appending to domain_event';
    end if;
    return new;
end;
$$;

comment on function cashback.unattributed_transaction_guard() is
    'Freezes which report an unattributed queue row refers to and when it was detected, and refuses to let a recorded resolution be erased (FR-061); only resolved_by, resolved_reason and resolved_at may be written, and only once.';
