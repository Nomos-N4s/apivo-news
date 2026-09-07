-- Reverses 0038_queued_report_says_why.
--
-- The guard function is restored to its 0024 body verbatim, not merely to
-- something that behaves like it: the round-trip test compares routine
-- bodies, and a down that leaves a differently-worded function behind is
-- the same class of rot as a forgotten DROP.

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

alter table cashback.unattributed_transaction
    drop constraint unattributed_reason_known;

alter table cashback.unattributed_transaction
    drop column reason;
