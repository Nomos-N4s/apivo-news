-- 0019_operator_role: C-4 tightened from "a named human" to "a named
-- operator".
--
-- This does for payouts exactly what 0002 did for articles. There, I-1's
-- "an article cannot exist without a named human approver" became "the
-- approver must hold the editor role". Here, C-4's "a payout row cannot
-- exist without a non-null named human approver" becomes "the approver
-- must hold the OPERATOR role":
--
--   * the NOT NULL on payout.approved_by (0014) makes an UNAPPROVED payout
--     unrepresentable;
--   * payout_insert_guard below makes an UNAUTHORISED approver
--     unrepresentable too.
--
-- Migration 0002 constrained account.role to ('reader', 'editor'), so an
-- operator is currently not even expressible and the authority behind every
-- operator surface has nowhere to live. That constraint is widened first.
--
-- THE LOCKING READ IS NOT OPTIONAL. payout_insert_guard reads the
-- approver's role FOR SHARE, exactly as article_insert_guard does, for
-- exactly the reason recorded there: foreign-key locks cover only key
-- columns, so a concurrent demotion of this very account could otherwise
-- commit unseen and the trigger would authorise against a stale role. This
-- was a real bug once. It is not re-derived per table; the pattern is
-- copied along with its reasoning.

------------------------------------------------------------------------------
-- The role itself.
------------------------------------------------------------------------------

alter table account
    drop constraint account_role_known;

alter table account
    add constraint account_role_known check (role in ('reader', 'editor', 'operator'));

-- History has to fit the tightened invariant, or the invariant is only
-- about tomorrow. 0002 faced the same question for articles and answered
-- by PROMOTING legacy approvers, because "the named human who approved
-- this article" and "an editor" were the same authority under a different
-- name.
--
-- That answer is wrong here, and the difference is the point: promoting an
-- account to operator grants the authority to release money, and a
-- migration must not hand that out on its own initiative. So this refuses
-- instead, names the count, and makes a person decide. Pre-release every
-- environment is empty and this never fires; it exists so the migration
-- cannot gamble on that.
do $$
declare
    unauthorised bigint;
begin
    select count(*) into unauthorised
      from cashback.payout p
      join account a on a.id = p.approved_by
     where a.role <> 'operator';
    if unauthorised > 0 then
        raise exception '% existing payout(s) were approved by an account that does not hold the operator role: decide explicitly who those approvers are before applying 0019 - this migration will not grant money-releasing authority on its own', unauthorised;
    end if;
end;
$$;

comment on column account.role is
    'What this person may do: readers read, editors approve articles, operators release money. Every authority is checked by the database (article_insert_guard, payout_insert_guard, is_entitled), never by application code alone.';

------------------------------------------------------------------------------
-- C-4 tightened: releasing money is an operator decision.
------------------------------------------------------------------------------

create function cashback.payout_insert_guard() returns trigger
language plpgsql
as $$
declare
    approver_role text;
begin
    -- The role is read WITH A ROW LOCK (FOR SHARE), not with a plain
    -- snapshot read. Foreign-key locks cover only key columns, so a
    -- concurrent demotion of this very account could otherwise commit
    -- unseen and this trigger would approve against a stale role. FOR
    -- SHARE conflicts with the demotion UPDATE's row lock, so the two
    -- transactions serialize in either order, and under READ COMMITTED a
    -- locking read sees the latest committed version of the row:
    -- whichever side commits second sees the other's write and raises
    -- (here, or in account_role_guard). A reader can never be recorded
    -- as having released money.
    select a.role
      into approver_role
      from public.account a
     where a.id = new.approved_by
       for share;
    -- No row means a null or nonexistent approver; fall through so those
    -- keep their natural SQLSTATEs - 23502 from the NOT NULL, 23503 from
    -- the foreign key. Only the ROLE rule lives here.
    if approver_role is not null and approver_role <> 'operator' then
        raise exception 'cashback.payout.approved_by must hold the operator role (C-4): releasing money is an operator decision, not an editorial one';
    end if;
    return new;
end;
$$;

comment on function cashback.payout_insert_guard() is
    'BEFORE INSERT guard on cashback.payout: the approver must hold the operator role at approval time (C-4 tightened). Reads the role FOR SHARE so a concurrent demotion cannot slip past it.';

create trigger payout_insert_guard
    before insert on cashback.payout
    for each row execute function cashback.payout_insert_guard();

-- A role check on INSERT alone would be bypassable by an UPDATE that
-- renames the approver afterwards. It is not, because 0014's payout_guard
-- already freezes approved_by for the life of the row - the two halves are
-- what make C-4 a rule about the payout rather than about one statement,
-- and neither is sufficient alone.

------------------------------------------------------------------------------
-- The role stays provable over history.
------------------------------------------------------------------------------

-- Guard v2. The editor rule from 0002 is carried verbatim; the operator
-- rule is its exact counterpart. While an operator's payout approvals are
-- on record, the authority that released that money cannot be taken away -
-- otherwise the C-4 chain would point at an account whose right to approve
-- is no longer demonstrable, which is precisely the question an auditor
-- asks about a payment.
--
-- Concurrency is covered by the FOR SHARE reads in the insert guards: an
-- in-flight approval holds a share lock on the account row, so this
-- demotion UPDATE waits for it and then re-checks against the committed
-- payout.
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
    if old.role = 'operator'
        and new.role is distinct from old.role
        and exists (
            select 1
              from cashback.payout p
             where p.approved_by = old.id
        ) then
        raise exception 'account role is frozen while payout approvals reference it (C-4): money already released requires a demonstrable operator';
    end if;
    return new;
end;
$$;

comment on function account_role_guard() is
    'Blocks demoting an editor whose approvals or withdrawals are on record (I-1), and an operator whose payout approvals are on record (C-4), keeping every recorded decision attributable to a verifiable holder of the authority behind it.';
