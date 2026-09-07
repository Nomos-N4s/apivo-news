-- 0039_a_verification_names_its_verifier: when a person verifies a payout
-- destination, the row says which person (B6, #548, FR-051 with FR-061).
--
-- 0014 recorded a verification as when and how, together, and that was the
-- whole of it while nothing could perform one. ADR-0006 settled how the
-- alpha verifies: an operator confirms ownership out of band and records
-- that they did. That makes verification an operator action, and every
-- other operator action in this schema names the human behind it -
-- unattributed_transaction.resolved_by, withdrawal_request.decided_by -
-- because FR-061 makes the audit record part of the action. A verification
-- that let money move and named nobody would be the one exception, and the
-- one hardest to defend later.
--
-- NULLABLE, and independently of the pair beside it. That is deliberate and
-- it is what keeps ADR-0006's own revisit trigger open: a destination
-- verified by a payment provider at tokenisation has a method and an
-- instant and no human at all, and folding this into
-- payout_destination_verification_all_or_none would make that unstorable.
-- So the pair stays a pair, and this column carries one rule of its own -
-- a verifier without a verification is not a state, because there is
-- nothing for them to have verified.
--
-- Frozen with the rest of the verification. The existing guard already
-- refuses to re-date or re-method one that stands; this joins it, so the
-- name on a verification cannot be changed to somebody else's after the
-- money has moved on the strength of it.

alter table cashback.payout_destination
    add column verified_by uuid references public.account (id),
    add constraint payout_destination_verifier_verified
        check (verified_by is null or verified_at is not null);

comment on column cashback.payout_destination.verified_by is
    'The operator who verified this destination, when a person did (FR-061). Null where nobody did: a verification performed by a payment provider at tokenisation has a method and an instant and no human, and that is a state this column is deliberately shaped to allow.';

comment on constraint payout_destination_verifier_verified on cashback.payout_destination is
    'A verifier without a verification is not a state: there would be nothing for them to have verified.';

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
            or new.verified_method is distinct from old.verified_method
            or new.verified_by is distinct from old.verified_by) then
        raise exception 'payout destination verification is one-way and final (FR-051): it is the evidence a withdrawal was allowed to name it, and who performed it is part of that evidence (FR-061)';
    end if;
    return new;
end;
$$;

comment on function cashback.payout_destination_guard() is
    'Freezes a destination''s owner, rail and details reference after insert; verification - its instant, its method and the person who performed it - may only move from absent to recorded, never back and never re-dated.';
