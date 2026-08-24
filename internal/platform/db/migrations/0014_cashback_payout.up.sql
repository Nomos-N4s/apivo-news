-- 0014_cashback_payout: destinations, withdrawal requests and payouts.
--
-- This is where money leaves. Two invariants are enforced here, and both
-- are enforced the way article.approved_by enforces I-1 - by making the
-- illegal state unrepresentable rather than by checking for it:
--
--   C-4  A payout row cannot exist without a non-null named human
--        approver. The row IS the approval.
--   C-5  Every outbound payout carries a unique idempotency key with a
--        database uniqueness constraint, derived deterministically from
--        the withdrawal request. A retry cannot create a second payout.
--
-- C-5 goes one step further than the data model requires. The key is not a
-- caller-supplied string that happens to be unique: it is a GENERATED
-- column derived from request_id, so a retry cannot mint a new key even by
-- accident, which is how duplicate payouts actually happen (D8). The
-- database, not the caller, is the authority on it - exactly as it is on
-- source_item.content_hash.
--
-- Two composite foreign keys carry rules a single-column key cannot:
--
--   * a withdrawal may only name a destination belonging to the requesting
--     member;
--   * a payout may only pay the amount and currency of the request it
--     settles. Not "should" - cannot.
--
-- 0019 tightens C-4 further: the approver must hold the operator role,
-- checked with a locking read, exactly as article_insert_guard checks the
-- editor role. Until then the NOT NULL is the whole of it.

------------------------------------------------------------------------------
-- Destinations: money only goes where the member has proved it belongs.
------------------------------------------------------------------------------

create table cashback.payout_destination (
    id uuid primary key default gen_random_uuid(),
    account_id uuid not null references public.account (id),
    kind text not null
        constraint payout_destination_kind_known check (kind in ('sepa', 'manual', 'stub')),
    -- A reference into the store that holds the actual bank details, never
    -- the details themselves. Losing this database must not be losing
    -- anybody's IBAN.
    details_ref text not null
        constraint payout_destination_details_ref_not_blank check (btrim(details_ref) <> ''),
    verified_at timestamptz,
    verified_method text
        constraint payout_destination_verified_method_not_blank
            check (verified_method is null or btrim(verified_method) <> ''),
    created_at timestamptz not null default now(),
    -- Verification is when and how, together. "Verified, method unknown" is
    -- not a verification anyone could defend later (FR-051).
    constraint payout_destination_verification_all_or_none
        check (num_nonnulls(verified_at, verified_method) in (0, 2)),
    -- The pair withdrawal_request keys on, so ownership travels with the
    -- foreign key instead of being re-checked by every caller.
    constraint payout_destination_id_account_unique unique (id, account_id)
);

comment on table cashback.payout_destination is
    'Where a member may be paid. Never holds raw bank details - only a reference to them. An unverified destination cannot be named by a withdrawal request (FR-051).';
comment on column cashback.payout_destination.details_ref is
    'A reference into the store holding the actual payout details. Deliberately NOT the details: the money schema should never be the thing that leaks an IBAN.';
comment on column cashback.payout_destination.verified_at is
    'When the member proved this destination is theirs (FR-051). One-way: a verification is never withdrawn or re-dated, so a request that passed the check cannot later be reasoned about as if it had not.';

create index payout_destination_account_id_idx on cashback.payout_destination (account_id);

-- What a destination IS - whose it is, what rail it uses, what it points at
-- - is frozen after insert, and verification only ever moves from absent to
-- recorded. Otherwise a verified destination could be quietly re-pointed at
-- somebody else's account between the check and the payment.
create function cashback.payout_destination_guard() returns trigger
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

create trigger payout_destination_guard
    before update on cashback.payout_destination
    for each row execute function cashback.payout_destination_guard();

------------------------------------------------------------------------------
-- Withdrawal requests: the reservation exists before the review (D9).
------------------------------------------------------------------------------

create table cashback.withdrawal_request (
    id uuid primary key default gen_random_uuid(),
    account_id uuid not null references public.account (id),
    destination_id uuid not null,
    amount_minor bigint not null
        constraint withdrawal_request_amount_positive check (amount_minor > 0),
    currency char(3) not null
        constraint withdrawal_request_currency_iso4217_format check (currency ~ '^[A-Z]{3}$'),
    state text not null default 'awaiting_approval'
        constraint withdrawal_request_state_known
            check (state in ('awaiting_approval', 'approved', 'rejected', 'paid', 'failed')),
    requested_at timestamptz not null default now(),
    -- D9: requesting moves the amount from confirmed to reserved
    -- immediately, before any human looks at it. The double-spend window is
    -- between request and approval, and this column is what closes it - so
    -- a request that reserved nothing cannot be written.
    reserved_transfer_ref text not null
        constraint withdrawal_request_reserved_transfer_ref_not_blank
            check (btrim(reserved_transfer_ref) <> ''),
    decided_by uuid references public.account (id),
    decided_at timestamptz,
    decision_reason text
        constraint withdrawal_request_decision_reason_not_blank
            check (decision_reason is null or btrim(decision_reason) <> ''),
    constraint withdrawal_request_reserved_transfer_ref_unique unique (reserved_transfer_ref),
    -- A decision is who and when, together.
    constraint withdrawal_request_decision_all_or_none
        check (num_nonnulls(decided_by, decided_at) in (0, 2)),
    -- Anything that is no longer awaiting approval has been decided by
    -- somebody: a request cannot slide into approved without a decider.
    constraint withdrawal_request_decided_before_leaving_the_queue
        check (state = 'awaiting_approval' or decided_at is not null),
    constraint withdrawal_request_decided_after_request
        check (decided_at is null or decided_at >= requested_at),
    -- A refusal that records no reason is a refusal the member cannot be
    -- told about and an operator cannot be held to.
    constraint withdrawal_request_rejection_has_reason
        check (state <> 'rejected' or (decision_reason is not null and btrim(decision_reason) <> '')),
    -- Ownership, in the key: a member can only withdraw to their own
    -- destination, and no caller has to remember to check.
    constraint withdrawal_request_destination_is_the_members
        foreign key (destination_id, account_id)
        references cashback.payout_destination (id, account_id),
    -- The pair payout keys on, so a payout can never pay an amount the
    -- request did not ask for.
    constraint withdrawal_request_id_amount_unique unique (id, amount_minor, currency)
);

comment on table cashback.withdrawal_request is
    'A member asking to be paid. The reservation transfer already exists when the row is written (D9): the double-spend window is between request and approval, and it is closed with the ledger rather than with a lock.';
comment on column cashback.withdrawal_request.reserved_transfer_ref is
    'The ledger transfer that moved this amount from confirmed to reserved at request time (D9). NOT NULL: a request that reserved nothing could be approved twice over the same balance.';
comment on column cashback.withdrawal_request.decided_by is
    'The named human who approved or rejected this request. The payout row carries the approval that releases money (C-4); this records the decision on the request itself.';

create index withdrawal_request_account_idx on cashback.withdrawal_request (account_id, requested_at desc);
-- The operator queue (FR-060) is exactly this partial index.
create index withdrawal_request_awaiting_idx on cashback.withdrawal_request (requested_at)
    where state = 'awaiting_approval';

-- FR-051: an unverified destination cannot receive money, so it cannot be
-- named by a request either.
--
-- The destination is read WITH A ROW LOCK (FOR SHARE), the same pattern
-- article_insert_guard uses for the approver's role. Verification is
-- one-way today, so a plain read could not go stale in the dangerous
-- direction - but the lock costs one row and keeps this guard correct if
-- that ever changes, and it serialises against a concurrent write to the
-- destination either way. Getting this wrong once already cost a real bug
-- in the news schema; the pattern is not re-litigated per table.
create function cashback.withdrawal_request_guard() returns trigger
language plpgsql
as $$
declare
    destination_verified_at timestamptz;
begin
    select d.verified_at
      into destination_verified_at
      from cashback.payout_destination d
     where d.id = new.destination_id
       for share;
    -- No row means a nonexistent destination; fall through so the foreign
    -- key raises it with its natural SQLSTATE.
    if found and destination_verified_at is null then
        raise exception 'a withdrawal request cannot name an unverified payout destination (FR-051): the member has not proved it is theirs';
    end if;
    return new;
end;
$$;

comment on function cashback.withdrawal_request_guard() is
    'BEFORE INSERT/UPDATE guard: the named payout destination must already be verified (FR-051). Reads the destination FOR SHARE, as article_insert_guard reads the approver''s role.';

create trigger withdrawal_request_guard
    before insert or update of destination_id on cashback.withdrawal_request
    for each row execute function cashback.withdrawal_request_guard();

------------------------------------------------------------------------------
-- Payouts: C-4 and C-5.
------------------------------------------------------------------------------

create table cashback.payout (
    id uuid primary key default gen_random_uuid(),
    -- ADR-0004's fourth tenant-boundary record. A payout is the one place
    -- the brand is not an abstraction: the payout descriptor a member sees
    -- on their statement, the legal entity behind it and the rail account
    -- it leaves from are all brand-scoped. Created with the table, and
    -- frozen by payout_guard below - a settled payment cannot be
    -- reattributed to another brand afterwards.
    brand_id text not null
        constraint payout_brand_id_not_blank check (btrim(brand_id) <> ''),
    -- Two uniqueness statements, deliberately. idempotency_key below is
    -- derived from this column, so each implies the other today and one
    -- index would do. They are both kept because they say different things:
    -- "one payout per request" is the business rule, and "one payout per
    -- idempotency key" is the contract with the ledger and the rail. If the
    -- derivation ever changes - a rail-scoped or attempt-scoped key is the
    -- obvious future - dropping either now would silently weaken C-5 then.
    -- The cost is one extra unique index on a table that takes a handful of
    -- rows a month.
    request_id uuid not null
        constraint payout_one_per_request unique,
    -- C-4. The row IS the approval, exactly as article.approved_by is for
    -- news: a payout with no named approver is unrepresentable, not merely
    -- rejected by a service.
    approved_by uuid not null references public.account (id),
    -- C-5. Derived by the database from the request, never supplied: a
    -- retry reuses the key because it cannot do anything else (D8). The
    -- unique constraint then makes the second attempt a 23505 rather than
    -- a second payment.
    idempotency_key text
        generated always as ('payout:' || request_id::text) stored
        not null
        constraint payout_idempotency_key_unique unique,
    amount_minor bigint not null
        constraint payout_amount_positive check (amount_minor > 0),
    currency char(3) not null
        constraint payout_currency_iso4217_format check (currency ~ '^[A-Z]{3}$'),
    rail text not null
        constraint payout_rail_not_blank check (btrim(rail) <> ''),
    rail_reference text
        constraint payout_rail_reference_not_blank
            check (rail_reference is null or btrim(rail_reference) <> ''),
    state text not null default 'submitted'
        constraint payout_state_known check (state in ('submitted', 'settled', 'failed')),
    submitted_at timestamptz not null default now(),
    settled_at timestamptz,
    constraint payout_settled_iff_settlement_time
        check ((state = 'settled') = (settled_at is not null)),
    constraint payout_settled_after_submission
        check (settled_at is null or settled_at >= submitted_at),
    -- A payout pays the request's own amount and currency. The composite
    -- key is also the foreign key to the request, so there is exactly one
    -- statement of what was owed and it is the one that was approved.
    constraint payout_pays_the_requested_amount
        foreign key (request_id, amount_minor, currency)
        references cashback.withdrawal_request (id, amount_minor, currency)
);

comment on table cashback.payout is
    'One outbound money movement. C-4: approved_by is NOT NULL, so an unapproved payout is unrepresentable. C-5: the idempotency key is generated from the request, so a retry cannot mint a new one and the unique constraint turns a double submit into a 23505.';
comment on column cashback.payout.brand_id is
    'Which brand paid this money out (ADR-0004). The payout descriptor, the legal entity and the rail account are all brand-scoped, and the column is frozen once the row exists.';
comment on column cashback.payout.approved_by is
    'C-4. The named human who approved this payout. Enforced by the database, not by application code - the row IS the approval.';
comment on column cashback.payout.idempotency_key is
    'C-5. Generated from request_id and passed unchanged to the ledger and the payout rail (D8). A caller cannot supply it: derivation is what makes a retry safe, and a caller-chosen key is exactly how a retry stops being one.';
comment on column cashback.payout.rail is
    'Which payout rail carried this money. The alpha ships a manual rail alongside any real one, and both enforce C-4 and C-5 identically (FR-052).';

-- The row IS the approval, so what was approved is frozen the moment it
-- exists. Without this, C-4 would be a rule about INSERT only: a payout
-- could be created with a legitimate approver and then updated to name
-- somebody else, or to pay a different amount on a different rail, and
-- every check that ran at insert time would be describing a row that no
-- longer exists. 0019 makes the approver's authority a role check, and a
-- role check on INSERT alone would be bypassable the same way.
--
-- What may still move is what a rail actually does after submission: the
-- state, when it settled, and the reference the rail gave back.
create function cashback.payout_guard() returns trigger
language plpgsql
as $$
begin
    if new.id is distinct from old.id
        or new.request_id is distinct from old.request_id
        or new.brand_id is distinct from old.brand_id
        or new.approved_by is distinct from old.approved_by
        or new.amount_minor is distinct from old.amount_minor
        or new.currency is distinct from old.currency
        or new.rail is distinct from old.rail
        or new.submitted_at is distinct from old.submitted_at then
        raise exception 'a payout''s approval is frozen (C-4): who approved it, which brand paid, what it pays and on which rail cannot change once the row exists - a different payment is a different request';
    end if;
    if old.state = 'settled' and new.state is distinct from old.state then
        raise exception 'a settled payout is terminal: money that has left cannot be un-settled';
    end if;
    return new;
end;
$$;

comment on function cashback.payout_guard() is
    'Freezes the approval a payout row IS - approver, brand, request, amount, currency, rail and submission time - leaving only the rail outcome (state, settled_at, rail_reference) mutable, and making settled terminal.';

create trigger payout_guard
    before update on cashback.payout
    for each row execute function cashback.payout_guard();
create trigger payout_no_delete
    before delete on cashback.payout
    for each row execute function public.raise_immutable();
create trigger payout_no_truncate
    before truncate on cashback.payout
    for each statement execute function public.raise_immutable();

create index payout_approved_by_idx on cashback.payout (approved_by);
create index payout_state_idx on cashback.payout (state, submitted_at desc);
