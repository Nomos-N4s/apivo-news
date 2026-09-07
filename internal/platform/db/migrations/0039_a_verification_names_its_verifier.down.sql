-- Reverses 0039_a_verification_names_its_verifier.
--
-- The guard function is restored to its 0014 body verbatim, not merely to
-- something that behaves like it: the round-trip test compares routine
-- bodies, and a down that leaves a differently-worded function behind is the
-- same class of rot as a forgotten DROP.

create or replace function cashback.payout_destination_guard() returns trigger
language plpgsql
as $$
begin
    if new.id is distinct from old.id
        or new.account_id is distinct from old.account_id
        or new.kind is distinct from old.kind
        or new.details_ref is distinct from old.details_ref
        or new.created_at is distinct from old.created_at then
        raise exception 'a payout destination is frozen after creation (FR-051): a different destination is a new row, verified on its own merits';
    end if;
    if old.verified_at is not null
        and (new.verified_at is distinct from old.verified_at
            or new.verified_method is distinct from old.verified_method) then
        raise exception 'payout destination verification is one-way and final (FR-051): it is the evidence a withdrawal was allowed to name it';
    end if;
    return new;
end;
$$;

comment on function cashback.payout_destination_guard() is
    'Freezes a destination''s owner, rail and details reference after insert; verification may only move from absent to recorded, never back and never re-dated.';

alter table cashback.payout_destination
    drop constraint payout_destination_verifier_verified;

alter table cashback.payout_destination
    drop column verified_by;
