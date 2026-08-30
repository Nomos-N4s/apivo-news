-- Reverses 0024_unattributed_queue_guards, restoring 0013's function body
-- verbatim: the queue row becomes deletable and truncatable again, and a
-- recorded resolution becomes erasable again.
--
-- Verbatim includes 0013's comment, because the round trip compares function
-- bodies and because the reasoning is what a reader of the restored function
-- needs. The triggers are dropped before the function is replaced, though
-- neither depends on the other: a plpgsql body is a string to PostgreSQL, so
-- the order is written down rather than enforced.
drop trigger unattributed_transaction_no_delete on cashback.unattributed_transaction;
drop trigger unattributed_transaction_no_truncate on cashback.unattributed_transaction;

create or replace function cashback.unattributed_transaction_guard() returns trigger
language plpgsql
as $$
begin
    if new.id is distinct from old.id
        or new.network_transaction_id is distinct from old.network_transaction_id
        or new.detected_at is distinct from old.detected_at then
        raise exception 'an unattributed transaction names which report went unattributed and when it was noticed (FR-034): both are frozen, only the resolution may be recorded';
    end if;
    return new;
end;
$$;

comment on function cashback.unattributed_transaction_guard() is
    'Freezes which report an unattributed queue row refers to and when it was detected; only resolved_by, resolved_reason and resolved_at may change.';
