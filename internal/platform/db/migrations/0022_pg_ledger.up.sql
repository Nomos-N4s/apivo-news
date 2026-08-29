-- 0022_pg_ledger: the documented exit route, made real - a double-entry
-- ledger in three tables inside this database's own walls.
--
-- ADR-0002 adopts Blnk as the money substrate and names its honest cost:
-- C-1 - postings sum to zero per currency - is the one invariant that
-- leaves Apivo's schema. The accepted mitigation is an exit route, "three
-- tables, a zero-sum trigger and a uniqueness constraint", kept implemented
-- behind the Ledger port so the adoption is reversible in days rather than
-- months. This migration is those three tables, that trigger and that
-- constraint; the Go adapter over them lives in
-- internal/cashback/wallet/postgres.
--
--   C-1  postings sum to zero per currency within every transfer, enforced
--        by a DEFERRED constraint trigger on ledger.posting: judged at
--        COMMIT, so a transfer's postings may arrive in any order inside
--        their transaction and are refused as a whole when they do not
--        balance. Checked twice by design - the wallet port refuses an
--        unbalanced transfer before any I/O, and this trigger refuses one
--        written straight in SQL behind the port's back.
--   C-5  the UNIQUE constraint on the idempotency key: of N concurrent
--        posts of one key the index lets exactly one insert commit, and
--        the adapter resolves the losers into replays or conflicts, never
--        into a second recording of the same money.
--   C-6  bigint minor units beside a format-checked char(3) ISO-4217
--        currency; no numeric, float or money column exists here.
--
-- Postings and transfers are financial history in the same sense
-- source_item is legal evidence: UPDATE, DELETE and TRUNCATE raise. A
-- correction is a new transfer that reverses the old one, never an edit.
--
-- The schema is deliberately self-contained - every function and foreign
-- key it uses lives inside it - so the whole schema can be lifted into a
-- database of its own (the ADR-0002 S1 fallback shape) without dragging a
-- single object from public or cashback along.
--
-- THE C-1 CHECK CAN BE POINTED HERE. 0016's cashback.ledger_zero_sum view
-- reads <schema>.balances - the columns currency and balance - in whatever
-- schema cashback.ledger_schema() resolves to (0020). ledger.balances
-- below presents exactly those columns, derived from the postings, so
--
--   select set_config('cashback.ledger_schema', 'ledger', true);
--
-- points the deployed zero-sum check at THIS ledger with no view rewritten
-- and no migration run; a deployment that swaps Blnk out makes the same
-- choice permanent with ALTER DATABASE ... SET cashback.ledger_schema =
-- 'ledger' and keeps the same continuous check.

create schema ledger;

comment on schema ledger is
    'The exit-route double-entry ledger (ADR-0002): accounts, transfers and postings, reached only through the wallet Ledger port. Self-contained on purpose, so it can move to its own database unchanged. Point the C-1 check at it by setting cashback.ledger_schema to ''ledger''.';

------------------------------------------------------------------------------
-- Grants: the cashback domain reads and writes the ledger, nobody else.
------------------------------------------------------------------------------

-- PUBLIC gets nothing. A new schema grants nothing by default; saying so
-- explicitly keeps the intent readable next to the grants that follow.
revoke all on schema ledger from public;

-- cashback_domain is 0010's NOLOGIN group role: the wallet adapter runs
-- inside the cashback product, so the ledger is granted to the same role
-- the rest of that product's access already flows through.
grant usage on schema ledger to cashback_domain;

-- Declared before the tables exist so every table below is covered without
-- each CREATE having to remember a grant. UPDATE and DELETE are granted
-- deliberately, exactly as 0010 reasons: immutability is enforced by
-- TRIGGERS, which no grant can bypass, rather than by withheld privileges,
-- which a later GRANT could quietly undo. The identity sequence behind
-- ledger.posting.id needs no grant of its own - identity columns carry
-- their sequence as part of the table, unlike serial.
alter default privileges in schema ledger
    grant select, insert, update, delete on tables to cashback_domain;

------------------------------------------------------------------------------
-- Immutability guard.
------------------------------------------------------------------------------

-- A copy in this schema rather than a reference to public.raise_immutable()
-- (which 0012 and 0013 reuse), for the self-containment stated above: the
-- one message this schema raises must not be the one object that ties it to
-- public when the schema is lifted out - and its wording can then say what
-- is actually being protected here.
create function ledger.raise_immutable() returns trigger
language plpgsql
as $$
begin
    raise exception 'table % is immutable: % is not allowed (postings and transfers are financial history; a correction is a reversing transfer)',
        tg_table_name, tg_op;
end;
$$;

comment on function ledger.raise_immutable() is
    'Trigger guard: rows in this schema are financial history and are never updated, deleted or truncated. A mistake is corrected by a new transfer that reverses it, which leaves both the mistake and the correction on the record.';

------------------------------------------------------------------------------
-- Accounts: one identity, one currency (C-6).
------------------------------------------------------------------------------

create table ledger.account (
    id text primary key
        constraint account_id_not_blank check (btrim(id) <> ''),
    currency char(3) not null
        constraint account_currency_iso4217_format check (currency ~ '^[A-Z]{3}$'),
    created_at timestamptz not null default now(),
    -- Redundant beside the primary key on purpose: it is the target the
    -- posting foreign key below aims at, which is what makes a posting in
    -- a currency its account does not hold unrepresentable rather than
    -- merely checked.
    constraint account_identity_carries_currency unique (id, currency)
);

comment on table ledger.account is
    'One ledger account: an identity issued by the wallet adapter''s EnsureAccount and the single currency it is denominated in (C-6). Insert-only - an issued id stays issued, and a rewritten currency would re-denominate every posting already recorded against it.';
comment on column ledger.account.id is
    'The identity the wallet adapter derives from (AccountRef, currency). Opaque to everything above the port: nothing outside the adapter parses it, so the derivation can change without stranding a caller.';
comment on column ledger.account.currency is
    'ISO-4217 code of the one currency this account holds. A member holding two currencies holds two accounts; no operation ever spans them implicitly (C-6).';
comment on column ledger.account.created_at is
    'When the account was first ensured. Bookkeeping for an operator; nothing derives from it.';

create trigger account_immutable
    before update or delete on ledger.account
    for each row execute function ledger.raise_immutable();
create trigger account_no_truncate
    before truncate on ledger.account
    for each statement execute function ledger.raise_immutable();

------------------------------------------------------------------------------
-- Transfers: one atomic movement, one idempotency key (C-5).
------------------------------------------------------------------------------

create table ledger.transfer (
    -- Minted by the database so a transfer written straight in SQL gets a
    -- reference the same way an adapter-posted one does. The prefix is for
    -- the human reading a domain row's ledger_transfer_ref, not for
    -- parsing: the port declares every reference opaque.
    ref text primary key default ('transfer_' || gen_random_uuid()::text)
        constraint transfer_ref_not_blank check (btrim(ref) <> ''),
    idempotency_key text not null
        constraint transfer_idempotency_key_not_blank check (btrim(idempotency_key) <> ''),
    reference text not null default '',
    metadata jsonb not null default '{}'::jsonb
        constraint transfer_metadata_is_object check (jsonb_typeof(metadata) = 'object'),
    posted_at timestamptz not null default now(),
    -- C-5's database half, and the whole concurrency story: however many
    -- posts of one key race, this index lets exactly one commit. The
    -- adapter never advises around it - the losers read the winner back
    -- and answer with its reference or with a conflict.
    constraint transfer_one_per_idempotency_key unique (idempotency_key)
);

comment on table ledger.transfer is
    'One atomic movement of money: the identity its postings hang from, the idempotency key that makes retrying it safe (C-5), and when it was recorded. Immutable: the postings are the substance, and a transfer whose annotations could drift after commit would be a record that changes under its auditor.';
comment on column ledger.transfer.ref is
    'The minted reference the domain stores (entry_transition.ledger_transfer_ref, withdrawal_request.reserved_transfer_ref). Text, opaque and never blank: the seam between the domain schema and the ledger is a reference, not a foreign key (D7).';
comment on column ledger.transfer.idempotency_key is
    'The caller-derived key that makes Post safe to retry, compared byte for byte. Unique in the database, so the concurrent-replay race is resolved by this index and nowhere else.';
comment on column ledger.transfer.reference is
    'The domain record that caused the transfer, as free text for whoever reads the ledger directly. A courtesy pointer, not the seam: the authoritative join runs the other way, from the domain rows that store ref.';
comment on column ledger.transfer.metadata is
    'Free-form string-to-string annotations stored with the transfer, for reading the ledger directly. Part of replay identity: the same key under different metadata is a conflict, not a replay.';
comment on column ledger.transfer.posted_at is
    'When the ledger recorded the transfer: the one instant every posting of the transfer carries, and the instant History windows select on.';

create trigger transfer_immutable
    before update or delete on ledger.transfer
    for each row execute function ledger.raise_immutable();
create trigger transfer_no_truncate
    before truncate on ledger.transfer
    for each statement execute function ledger.raise_immutable();

------------------------------------------------------------------------------
-- Postings: the double-entry atom (C-1, C-6).
------------------------------------------------------------------------------

create table ledger.posting (
    -- The identity doubles as recording order: History breaks posted_at
    -- ties by it, so two transfers recorded at one instant still read back
    -- in the order the ledger accepted them.
    id bigint generated always as identity primary key,
    transfer_ref text not null references ledger.transfer (ref),
    account_id text not null,
    -- Signed, not a direction flag beside a magnitude: positive raises the
    -- account's balance, negative lowers it, and the illegal state - a
    -- "credit" whose amount disagrees with its own sign - cannot be
    -- written. Zero is refused: a posting that moves nothing records
    -- nothing, and the likeliest way one appears is a caller that forgot
    -- to set an amount.
    amount_minor bigint not null
        constraint posting_amount_moves_money check (amount_minor <> 0),
    currency char(3) not null
        constraint posting_currency_iso4217_format check (currency ~ '^[A-Z]{3}$'),
    -- Aimed at account's (id, currency) pair rather than its primary key,
    -- so money of a currency the account does not hold has no
    -- representation here at all - an account is one currency by
    -- definition (C-6), and this is that definition as a foreign key.
    constraint posting_account_holds_currency
        foreign key (account_id, currency) references ledger.account (id, currency)
);

comment on table ledger.posting is
    'One signed movement of money on one account: the double-entry atom. A balance is the sum of an account''s postings at the moment it is asked for, never a stored figure (D7), and within a transfer the postings of each currency sum to zero (C-1, the deferred trigger below). Immutable.';
comment on column ledger.posting.id is
    'Recording order. History orders by the transfer''s posted_at and breaks ties by this value, so a frozen clock cannot shuffle the record.';
comment on column ledger.posting.transfer_ref is
    'The transfer this posting belongs to. Every posting has one; the zero-sum trigger judges the postings of a transfer together.';
comment on column ledger.posting.account_id is
    'The account the amount moves on, as issued by EnsureAccount.';
comment on column ledger.posting.amount_minor is
    'The signed movement in minor units of the posting''s explicit currency (C-6). Positive raises the account''s balance, negative lowers it, zero cannot be written.';
comment on column ledger.posting.currency is
    'ISO-4217 code the amount is denominated in. Pinned to the account''s own currency by the composite foreign key, so it exists to make each row self-describing and to give the zero-sum trigger its per-currency grouping.';

-- The zero-sum trigger sums by (transfer_ref, currency); replay detection
-- reads a transfer's postings back by transfer_ref alone. One index serves
-- both.
create index posting_transfer_ref_idx on ledger.posting (transfer_ref);
-- Balance sums by account; History reads by account in recording order.
create index posting_account_id_idx on ledger.posting (account_id, id);

create trigger posting_immutable
    before update or delete on ledger.posting
    for each row execute function ledger.raise_immutable();
create trigger posting_no_truncate
    before truncate on ledger.posting
    for each statement execute function ledger.raise_immutable();

------------------------------------------------------------------------------
-- C-1: the zero-sum trigger.
------------------------------------------------------------------------------

-- The shape is 0006's: an AFTER INSERT constraint trigger, DEFERRABLE
-- INITIALLY DEFERRED, so the judgement runs at COMMIT when the transfer's
-- postings are all present, whatever order the transaction wrote them in.
-- Immediate judgement would refuse every first posting of every transfer -
-- one row alone never sums to zero.
--
-- Per row rather than per statement, which re-checks a currency once per
-- posting that touched it. That redundancy is bought deliberately: a
-- constraint trigger cannot use transition tables, and the cost is a few
-- indexed sums on a transfer three postings long.
create function ledger.posting_zero_sum() returns trigger
language plpgsql
as $$
declare
    net bigint;
begin
    select coalesce(sum(p.amount_minor), 0)
      into net
      from ledger.posting p
     where p.transfer_ref = new.transfer_ref
       and p.currency = new.currency;
    if net <> 0 then
        raise exception 'transfer % does not sum to zero in %: postings net to % minor units; a transfer that creates or destroys money is refused whole (C-1)',
            new.transfer_ref, new.currency, net;
    end if;
    return null;
end;
$$;

comment on function ledger.posting_zero_sum() is
    'COMMIT-time check (deferred constraint trigger) that a transfer''s postings sum to zero within each currency it touches (C-1). The wallet port runs the same check before any I/O; this is the half that catches SQL written behind the port''s back, and the half that makes the invariant the database''s rather than a convention.';

create constraint trigger posting_zero_sum
    after insert on ledger.posting
    deferrable initially deferred
    for each row execute function ledger.posting_zero_sum();

------------------------------------------------------------------------------
-- Balances: derived, never stored (D7).
------------------------------------------------------------------------------

-- The plural name is load-bearing: 0020's cashback.ledger_balance_relation()
-- resolves <schema>.balances, because that is what Blnk calls its balance
-- table - so this view answering to the same name is what lets the setting
-- swing the C-1 check from Blnk's schema to this one without touching the
-- check. The bigint cast keeps the column integer-typed end to end (C-6);
-- a single account whose postings could not fit an int64 balance raises
-- here rather than reading back wrapped around.
create view ledger.balances as
select
    a.id       as account_id,
    a.currency as currency,
    coalesce(sum(p.amount_minor), 0)::bigint as balance
from ledger.account a
left join ledger.posting p on p.account_id = a.id
group by a.id, a.currency;

comment on view ledger.balances is
    'Every account''s balance as the sum of its postings at the moment of the query - there is nothing here a stored figure could drift from (D7). Presents the currency and balance columns the C-1 zero-sum check reads (0016/0020), under the relation name that check resolves.';
