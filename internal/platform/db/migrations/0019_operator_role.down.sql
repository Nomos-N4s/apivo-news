-- Reverses 0019_operator_role, restoring the 0002 role model: two roles,
-- and no database-enforced authority behind a payout. Destroys the
-- structural half of C-4; the NOT NULL on payout.approved_by survives, so
-- an unapproved payout stays unrepresentable, but an unauthorised approver
-- becomes representable again.

-- Role guard back to its 0002 body.
create or replace function account_role_guard() returns trigger
language plpgsql
as $$
begin
    if old.role = 'editor'
        and new.role is distinct from old.role
        and exists (
            select 1
              from article ar
             where ar.approved_by = old.id
                or ar.withdrawn_by = old.id
        ) then
        raise exception 'account role is frozen while editorial decisions reference it (I-1): recorded approvals and withdrawals require a demonstrable editor';
    end if;
    return new;
end;
$$;

comment on function account_role_guard() is
    'Blocks demoting an editor whose approvals or withdrawals are on record, keeping every recorded editorial decision attributable to a verifiable editor (I-1).';

drop trigger payout_insert_guard on cashback.payout;
drop function cashback.payout_insert_guard();

-- Accounts holding the operator role cannot fit the narrowed constraint,
-- and this migration will not guess what they should become instead: a
-- demotion to reader would silently strip the authority behind money that
-- has already been released, and a promotion to editor would silently
-- grant editorial authority nobody conferred. Reassign them explicitly
-- first. Pre-release every environment is empty and this never fires.
do $$
begin
    if exists (select 1 from account where role = 'operator') then
        raise exception 'accounts hold the operator role: reassign them explicitly before reverting 0019 - this migration will not choose a replacement role for a person who has released money';
    end if;
end;
$$;

alter table account
    drop constraint account_role_known;

alter table account
    add constraint account_role_known check (role in ('reader', 'editor'));

comment on column account.role is
    'What this person may do: readers read, editors approve. Approval authority is checked by the database (article_insert_guard, is_entitled), never by application code alone.';
