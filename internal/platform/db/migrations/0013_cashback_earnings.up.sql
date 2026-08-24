-- 0013_cashback_earnings: entries, their history, and the ledger seam.
--
--   C-2  A cashback credit cannot exist without a reference to exactly one
--        network transaction record and, through it, at most one click
--        record. Credits with no evidence are unrepresentable.
--
-- C-2 is one NOT NULL foreign key, and that is the whole enforcement: an
-- entry that cites no evidence fails with 23502 before any application code
-- has an opinion. Two further keys make the citation honest rather than
-- merely present:
--
--   * a composite key on (click_id, account_id) into click, so a credit can
--     only cite a click that belongs to the member being credited;
--   * a partial unique index on network_transaction_id, so one report
--     produces at most one credit - exactly-once, from the schema.
--
-- D7 governs the history: every transition writes a ledger transfer, and no
-- state is ever recorded without its posting. entry_transition therefore
-- requires the transfer reference, and is append-only: the record of how
-- money moved is evidence in the same sense the network report is.

------------------------------------------------------------------------------
-- Entries: the member-facing unit of earning (C-2).
------------------------------------------------------------------------------

create table cashback.entry (
    id uuid primary key default gen_random_uuid(),
    account_id uuid not null references public.account (id),
    -- ADR-0004's third tenant-boundary record: an entry is money owed to a
    -- member BY A BRAND. Created with the table rather than added later, so
    -- it exists at every point in the migration history where an entry
    -- does, and no default - a brand literal in a migration is what the
    -- constitution forbids, and a row whose brand nobody stated is a row
    -- nobody can scope later.
    brand_id text not null
        constraint entry_brand_id_not_blank check (btrim(brand_id) <> ''),
    -- C-2. Not nullable, not deferrable, not optional: this column is the
    -- invariant.
    network_transaction_id uuid not null references cashback.network_transaction (id),
    -- Null only for an entry an operator attributed by hand from the
    -- unattributed queue (FR-034); the network reported no click reference
    -- and a human decided whose purchase it was.
    click_id uuid references cashback.click (id),
    state text not null
        constraint entry_state_known
            check (state in ('held', 'pending', 'confirmed', 'reserved', 'paid', 'reversed')),
    amount_minor bigint not null
        constraint entry_amount_positive check (amount_minor > 0),
    currency char(3) not null
        constraint entry_currency_iso4217_format check (currency ~ '^[A-Z]{3}$'),
    hold_rule text,
    reversal_of_id uuid references cashback.entry (id),
    created_at timestamptz not null default now(),
    -- A held entry names the rule that held it, and an entry that is not
    -- held names none: US7's operator queue is "every entry whose hold_rule
    -- is set", and that has to be the same set as "every held entry".
    -- Releasing clears the rule; entry_transition keeps the record of why
    -- it was held in the first place.
    constraint entry_hold_rule_iff_held
        check ((state = 'held') = (hold_rule is not null and btrim(hold_rule) <> '')),
    -- A reversing entry is a reversal: it never masquerades as a credit in
    -- some other state (SC-010).
    constraint entry_reversal_is_reversed
        check (reversal_of_id is null or state = 'reversed'),
    constraint entry_not_own_reversal
        check (reversal_of_id is null or reversal_of_id <> id),
    -- Exactly-once crediting, from the schema rather than from the poller's
    -- memory: one report backs at most one entry, credit or reversal alike.
    --
    -- This was a partial index excluding reversals, on the reasoning that a
    -- reversal cites the reversing report. It does - and that reversing
    -- report is a DIFFERENT row, because a status change is a new
    -- superseding row (C-3), never an edit. So the exclusion bought nothing
    -- and cost the guarantee: two reversal entries could cite one reversing
    -- report and debit a member twice for one clawback.
    constraint entry_one_per_report unique (network_transaction_id),
    -- And an original is reversed at most once. Without this, two reversal
    -- entries can cite the same credit - two debits, one clawback - and
    -- each would look correct on its own.
    constraint entry_reversed_at_most_once unique (reversal_of_id),
    -- The pair 0012 prepared: the click cited must belong to the member
    -- being credited. Without it, a credit could rest on a real click by
    -- somebody else, which is evidence of the wrong thing.
    constraint entry_click_belongs_to_member
        foreign key (click_id, account_id) references cashback.click (id, account_id)
);

comment on table cashback.entry is
    'A member''s unit of earning. C-2 lives on network_transaction_id: a credit with no evidence cannot be inserted. Reversal never edits an entry - a reversing entry is inserted beside it (SC-010).';
comment on column cashback.entry.brand_id is
    'Which brand owes this earning (ADR-0004). A tenant boundary would fall here, and entry_guard freezes it: an already paid or reversed entry cannot be moved across one.';
comment on column cashback.entry.network_transaction_id is
    'C-2. The single network report this credit rests on, and through its click_ref the click that earned it. NOT NULL, so a credit with no evidence is unrepresentable.';
comment on column cashback.entry.click_id is
    'The click that earned this credit. Null only for an entry an operator attributed by hand from the unattributed queue; the composite foreign key guarantees that when it is set, the click belongs to this member.';
comment on column cashback.entry.amount_minor is
    'The member''s share in minor units, always positive and always with an explicit currency (C-6). A reversal is a separate entry, not a negative amount, so the sign never has to be interpreted.';
comment on column cashback.entry.hold_rule is
    'Which rule held this entry back from crediting (US7). Set exactly while the entry is held; the transition history keeps the record after release.';
comment on column cashback.entry.reversal_of_id is
    'The entry this one reverses (SC-010). The original is left exactly as it was, so the auditable pair survives.';

create index entry_account_state_idx on cashback.entry (account_id, state);
create index entry_network_transaction_idx on cashback.entry (network_transaction_id);
create index entry_click_id_idx on cashback.entry (click_id) where click_id is not null;
create index entry_held_idx on cashback.entry (created_at) where state = 'held';

-- Everything that makes an entry a money record is frozen after insert.
-- The state column moves - that is what the state machine is - and
-- hold_rule moves with it on release. Nothing else may change, and paid
-- and reversed are terminal: an entry that has been paid can never be
-- walked back into a payable state, which is how the same money gets paid
-- twice.
create function cashback.entry_guard() returns trigger
language plpgsql
as $$
begin
    if new.id is distinct from old.id
        or new.account_id is distinct from old.account_id
        or new.brand_id is distinct from old.brand_id
        or new.network_transaction_id is distinct from old.network_transaction_id
        or new.click_id is distinct from old.click_id
        or new.amount_minor is distinct from old.amount_minor
        or new.currency is distinct from old.currency
        or new.reversal_of_id is distinct from old.reversal_of_id
        or new.created_at is distinct from old.created_at then
        raise exception 'cashback entry identity, evidence, brand and amount are frozen (C-2): a correction is a new entry, never an edit';
    end if;
    if old.state in ('paid', 'reversed') and new.state is distinct from old.state then
        raise exception 'cashback entry state % is terminal: a paid or reversed entry never becomes payable again', old.state;
    end if;
    return new;
end;
$$;

comment on function cashback.entry_guard() is
    'Freezes an entry''s member, brand, evidence, amount and currency after insert, and makes the paid and reversed states terminal. Only the state machine''s own columns may move.';

-- C-2 completed. The NOT NULL foreign key says a credit cites evidence;
-- this says the credit cites ALL of the evidence there is. Where the
-- network reported a click reference, the entry must name the click it
-- belongs to, and that click must be the one the network named - not
-- merely some click of the same member's, which the composite foreign key
-- alone would happily accept.
--
-- click_id may still be null where the report carries no reference: that
-- is the operator-attributed case (FR-034), where a human decided whose
-- purchase it was and there is no click evidence to cite. The asymmetry is
-- the point - the database requires the evidence that exists, and does not
-- invent the evidence that does not.
--
-- This matters because entry_guard freezes click_id: an entry created
-- without its click can never be repaired, only reversed.
create function cashback.entry_evidence_guard() returns trigger
language plpgsql
as $$
declare
    reported_click_ref text;
    cited_click_ref text;
begin
    select nt.click_ref
      into reported_click_ref
      from cashback.network_transaction nt
     where nt.id = new.network_transaction_id;
    -- No row means a nonexistent report; fall through so the foreign key
    -- raises it with its natural SQLSTATE.
    if not found then
        return new;
    end if;

    if reported_click_ref is not null and new.click_id is null then
        raise exception 'cashback.entry must cite the click the network reported (C-2): report % names click reference %, so the credit cannot be attributed without it',
            new.network_transaction_id, reported_click_ref;
    end if;

    if reported_click_ref is not null and new.click_id is not null then
        select c.click_ref into cited_click_ref
          from cashback.click c
         where c.id = new.click_id;
        if found and cited_click_ref is distinct from reported_click_ref then
            raise exception 'cashback.entry cites click reference % but the network reported % (C-2): the credit would rest on a click the network never mentioned',
                cited_click_ref, reported_click_ref;
        end if;
    end if;
    return new;
end;
$$;

comment on function cashback.entry_evidence_guard() is
    'BEFORE INSERT guard on cashback.entry: where the network reported a click reference, the entry must cite that exact click (C-2). A null click_id stays legal only for the operator-attributed case, where the report names no reference at all.';

create trigger entry_evidence_guard
    before insert on cashback.entry
    for each row execute function cashback.entry_evidence_guard();

create trigger entry_guard
    before update on cashback.entry
    for each row execute function cashback.entry_guard();
create trigger entry_no_delete
    before delete on cashback.entry
    for each row execute function public.raise_immutable();
create trigger entry_no_truncate
    before truncate on cashback.entry
    for each statement execute function public.raise_immutable();

------------------------------------------------------------------------------
-- Transition history: no state without its posting (D7).
------------------------------------------------------------------------------

create table cashback.entry_transition (
    id uuid primary key default gen_random_uuid(),
    entry_id uuid not null references cashback.entry (id),
    -- Null on the transition that created the entry.
    from_state text
        constraint entry_transition_from_state_known
            check (from_state is null
                or from_state in ('held', 'pending', 'confirmed', 'reserved', 'paid', 'reversed')),
    to_state text not null
        constraint entry_transition_to_state_known
            check (to_state in ('held', 'pending', 'confirmed', 'reserved', 'paid', 'reversed')),
    -- D7: a transition without a transfer reference is rejected. A state
    -- recorded with no posting behind it is precisely the disagreement
    -- between the wallet and the ledger that C-1 exists to prevent.
    ledger_transfer_ref text not null
        constraint entry_transition_transfer_ref_not_blank
            check (btrim(ledger_transfer_ref) <> ''),
    reason text
        constraint entry_transition_reason_not_blank
            check (reason is null or btrim(reason) <> ''),
    -- The operator who caused this transition, where a human caused it.
    actor_id uuid references public.account (id),
    occurred_at timestamptz not null default now(),
    constraint entry_transition_states_differ
        check (from_state is null or from_state <> to_state),
    -- The triple ledger_link joins on, so a posting cannot be attached to a
    -- transition of a different entry, nor claim a different transfer than
    -- the transition it belongs to.
    constraint entry_transition_id_entry_transfer_unique
        unique (id, entry_id, ledger_transfer_ref)
);

comment on table cashback.entry_transition is
    'APPEND ONLY. One row per state change, each carrying the ledger transfer that made it real (D7). The history of how a member''s money moved is evidence, not a log.';
comment on column cashback.entry_transition.ledger_transfer_ref is
    'The ledger transfer this transition wrote. NOT NULL by design (D7): no state is ever recorded without its posting, and no posting without its state.';

create index entry_transition_entry_id_idx on cashback.entry_transition (entry_id, occurred_at);
create index entry_transition_transfer_ref_idx on cashback.entry_transition (ledger_transfer_ref);

-- D7, actually enforced rather than asserted in a comment.
--
-- Until this trigger existed, the migration claimed "no state is ever
-- recorded without its posting" and the schema permitted exactly that: a
-- bare `update cashback.entry set state = 'paid'` moved a member's money
-- into a paid state with no transition row and no ledger transfer behind
-- it. A rule stated in a comment and absent from the schema is the failure
-- mode Principle VIII exists to prevent, and this file was committing it.
--
-- The shape is 0006's: an AFTER UPDATE constraint trigger, DEFERRABLE
-- INITIALLY DEFERRED. Deferred matters twice, exactly as it does there. It
-- lets the state change and its transition row commit together in one
-- transaction, written in either order. And because the check runs at
-- COMMIT, a test that moves an entry inside a rolled-back transaction is
-- undisturbed: what never commits is never checked.
--
-- What it asserts, precisely: for the hop just made, a transition row
-- exists recording that same hop. What it therefore catches: a state moved
-- with no transition at all, and a state moved with a transition recording
-- some other hop. What it does not catch: repeating a hop that was already
-- recorded once - reaching reserved a second time reuses the first
-- (confirmed -> reserved) row. That residual case cannot be reached in
-- practice, because returning from reserved to confirmed is itself a hop
-- this trigger requires a row for; it is named here rather than papered
-- over, because a guard whose comment overclaims is worse than no guard.
create function cashback.entry_state_requires_transition() returns trigger
language plpgsql
as $$
begin
    if not exists (
        select 1
          from cashback.entry_transition et
         where et.entry_id = new.id
           and et.from_state is not distinct from old.state
           and et.to_state = new.state
    ) then
        raise exception 'cashback entry % moved from % to % with no entry_transition recording it (D7): every state change carries a ledger transfer, and a state with no posting behind it is the wallet and the ledger disagreeing',
            new.id, old.state, new.state;
    end if;
    return null;
end;
$$;

comment on function cashback.entry_state_requires_transition() is
    'COMMIT-time check (deferred constraint trigger) that a change to entry.state is accompanied by an entry_transition row recording that same hop (D7). Without it the transition table is documentation rather than enforcement.';

create constraint trigger entry_state_requires_transition
    after update of state on cashback.entry
    deferrable initially deferred
    for each row
    when (old.state is distinct from new.state)
    execute function cashback.entry_state_requires_transition();

create trigger entry_transition_immutable
    before update or delete on cashback.entry_transition
    for each row execute function public.raise_immutable();
create trigger entry_transition_no_truncate
    before truncate on cashback.entry_transition
    for each statement execute function public.raise_immutable();

------------------------------------------------------------------------------
-- The unattributed queue (FR-034).
------------------------------------------------------------------------------

create table cashback.unattributed_transaction (
    id uuid primary key default gen_random_uuid(),
    network_transaction_id uuid not null references cashback.network_transaction (id)
        constraint unattributed_one_per_report unique,
    detected_at timestamptz not null default now(),
    resolved_by uuid references public.account (id),
    resolved_reason text,
    resolved_at timestamptz,
    -- A resolution is who, when and why, together: a half-recorded
    -- resolution is not an audit trail.
    constraint unattributed_resolution_all_or_none
        check (num_nonnulls(resolved_by, resolved_reason, resolved_at) in (0, 3)),
    constraint unattributed_resolved_reason_not_blank
        check (resolved_reason is null or btrim(resolved_reason) <> ''),
    constraint unattributed_resolved_after_detection
        check (resolved_at is null or resolved_at >= detected_at)
);

comment on table cashback.unattributed_transaction is
    'A network report with no matching click (FR-034). It is queued for an operator and NEVER auto-credited: the row exists so the money is visible, not so it is paid.';

create index unattributed_open_idx on cashback.unattributed_transaction (detected_at)
    where resolved_at is null;

-- The row is the record that this report was SEEN and went unattributed.
-- Which report, and when it was noticed, are therefore evidence and frozen;
-- only the resolution is a decision anyone may still make. Without this, a
-- queue row could be quietly re-homed onto a different report and the
-- history of what an operator actually looked at would be gone.
create function cashback.unattributed_transaction_guard() returns trigger
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

create trigger unattributed_transaction_guard
    before update on cashback.unattributed_transaction
    for each row execute function cashback.unattributed_transaction_guard();

------------------------------------------------------------------------------
-- The ledger seam (ADR-0002).
------------------------------------------------------------------------------

create table cashback.ledger_link (
    transition_id uuid not null,
    entry_id uuid not null,
    ledger_transfer_ref text not null
        constraint ledger_link_transfer_ref_not_blank
            check (btrim(ledger_transfer_ref) <> ''),
    posted_at timestamptz not null default now(),
    -- One posting per transition: the key IS the rule.
    constraint ledger_link_pkey primary key (transition_id),
    constraint ledger_link_transfer_ref_unique unique (ledger_transfer_ref),
    -- The composite key keeps a posting attached to a transition of the
    -- same entry AND to the same transfer that transition recorded. Both
    -- tables carry ledger_transfer_ref, so without the third column they
    -- could disagree - the transition naming one transfer and its posting
    -- another - and the provenance view would answer with whichever it
    -- happened to read. Declaring it in the key rather than checking it in
    -- a trigger means the disagreement is unrepresentable, on insert and on
    -- update alike.
    constraint ledger_link_matches_transition
        foreign key (transition_id, entry_id, ledger_transfer_ref)
        references cashback.entry_transition (id, entry_id, ledger_transfer_ref)
);

comment on table cashback.ledger_link is
    'The join between an entry transition and the ledger transfer that carried it (ADR-0002). Balances live in the ledger; this table is the seam that makes C-7 one query and keeps the ledger swappable.';
comment on column cashback.ledger_link.ledger_transfer_ref is
    'The ledger''s own transfer reference, unique here so one transfer can never be claimed by two transitions.';

create index ledger_link_entry_id_idx on cashback.ledger_link (entry_id);

-- The seam that makes C-7 a single query is itself evidence: an auditor
-- reading the provenance view is reading these rows. A posting that could
-- be rewritten or removed after the fact would make that answer worth no
-- more than the last person to edit it, so the same raise_immutable()
-- guards that protect entry_transition protect its postings. A correction
-- is a new transition with a new transfer, never an edit here.
create trigger ledger_link_immutable
    before update or delete on cashback.ledger_link
    for each row execute function public.raise_immutable();
create trigger ledger_link_no_truncate
    before truncate on cashback.ledger_link
    for each statement execute function public.raise_immutable();
