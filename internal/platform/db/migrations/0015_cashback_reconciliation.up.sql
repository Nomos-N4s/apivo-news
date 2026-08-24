-- 0015_cashback_reconciliation: what the network says it paid, against
-- what was actually received.
--
-- C-3 names imported statements alongside network reports and clicks:
-- "Network transaction records, click records and IMPORTED STATEMENTS
-- reject UPDATE, DELETE and TRUNCATE." A statement is the counterparty's
-- own account of the money, and an editable one is worth nothing in a
-- dispute - so reconciliation_run gets the same raise_immutable() guards
-- source_item has.
--
-- The differences it produces are the opposite: they exist to be worked
-- through and resolved by a named operator (US6), so that table is
-- mutable, with the resolution recorded all-or-none the way every other
-- resolution in this schema is.

------------------------------------------------------------------------------
-- The imported statement (immutable, C-3).
------------------------------------------------------------------------------

create table cashback.reconciliation_run (
    id uuid primary key default gen_random_uuid(),
    network_account_id uuid not null references cashback.network_account (id),
    statement_period_start timestamptz not null,
    statement_period_end timestamptz not null,
    imported_at timestamptz not null default now(),
    imported_by uuid not null references public.account (id),
    raw_statement jsonb not null,
    constraint reconciliation_run_period_ordered
        check (statement_period_end > statement_period_start),
    -- The pair reconciliation_difference keys on, so a difference cannot
    -- be filed against a run for one publisher account while citing a
    -- report that belongs to another.
    constraint reconciliation_run_id_account_unique unique (id, network_account_id)
);

comment on table cashback.reconciliation_run is
    'IMMUTABLE (C-3). One import of a network''s payment statement: which publisher account, which period, by whom, and the statement verbatim. It is the counterparty''s own account of the money, and an editable one would be worth nothing in a dispute.';
comment on column cashback.reconciliation_run.imported_by is
    'The named human who imported this statement (US6). Reconciliation is an accounting act with a person behind it, not a background job with nobody''s name on it.';
comment on column cashback.reconciliation_run.raw_statement is
    'The statement exactly as the network supplied it. Differences are derived from this; the derivation can be re-run, the statement cannot be re-fetched.';

create index reconciliation_run_network_account_idx
    on cashback.reconciliation_run (network_account_id, statement_period_end desc);

create trigger reconciliation_run_immutable
    before update or delete on cashback.reconciliation_run
    for each row execute function public.raise_immutable();
create trigger reconciliation_run_no_truncate
    before truncate on cashback.reconciliation_run
    for each statement execute function public.raise_immutable();

-- The other half of that pair, on 0012's table. A reconciliation run is
-- scoped to one publisher account and so is every report it can possibly
-- disagree with, but nothing said so until now: a difference could cite a
-- report from a different network account and satisfy every foreign key on
-- the way. Declaring it as a key rather than checking it in a trigger makes
-- the cross-account row unrepresentable on insert and on update alike.
alter table cashback.network_transaction
    add constraint network_transaction_id_account_unique unique (id, network_account_id);

------------------------------------------------------------------------------
-- The differences (mutable: they exist to be resolved).
------------------------------------------------------------------------------

create table cashback.reconciliation_difference (
    id uuid primary key default gen_random_uuid(),
    run_id uuid not null,
    -- Carried so both keys below can pin it: the run's publisher account
    -- and the report's publisher account are the same account, by key.
    network_account_id uuid not null,
    kind text not null
        constraint reconciliation_difference_kind_known
            check (kind in ('reported_not_paid', 'paid_not_reported', 'amount_mismatch')),
    -- Null for money on the statement that matches no report at all, which
    -- is exactly the paid_not_reported case.
    network_transaction_id uuid,
    expected_minor bigint,
    actual_minor bigint,
    currency char(3) not null
        constraint reconciliation_difference_currency_iso4217_format
            check (currency ~ '^[A-Z]{3}$'),
    detected_at timestamptz not null default now(),
    resolved_by uuid references public.account (id),
    resolved_reason text
        constraint reconciliation_difference_resolved_reason_not_blank
            check (resolved_reason is null or btrim(resolved_reason) <> ''),
    resolved_at timestamptz,
    -- A resolution is who, when and why, together. Half of one is a queue
    -- item that looks handled and is not.
    constraint reconciliation_difference_resolution_all_or_none
        check (num_nonnulls(resolved_by, resolved_reason, resolved_at) in (0, 3)),
    constraint reconciliation_difference_resolved_after_detection
        check (resolved_at is null or resolved_at >= detected_at),
    -- Every kind of difference states exactly the facts that make it one,
    -- and none of the facts that would contradict it. The forbidden half
    -- is not decoration: an earlier version required only the fields each
    -- kind needs, so a paid_not_reported row - money on the statement
    -- matching no report - could still name a report and an expected
    -- amount, and a reported_not_paid row could carry an actual amount it
    -- was by definition never paid. Both read as coherent in an operator
    -- queue and mean nothing.
    constraint reconciliation_difference_shape_matches_kind check (
        case kind
            -- The statement paid a different amount than the reports say.
            when 'amount_mismatch' then network_transaction_id is not null
                and expected_minor is not null
                and actual_minor is not null
                and expected_minor <> actual_minor
            -- A report the statement never paid: there is an expectation
            -- and no payment.
            when 'reported_not_paid' then network_transaction_id is not null
                and expected_minor is not null
                and actual_minor is null
            -- Money on the statement matching no report: there is a
            -- payment and nothing that expected it.
            when 'paid_not_reported' then network_transaction_id is null
                and expected_minor is null
                and actual_minor is not null
            else false
        end
    ),
    -- The run and the report it disagrees with belong to the same
    -- publisher account, stated as keys rather than as a trigger.
    constraint reconciliation_difference_run_account
        foreign key (run_id, network_account_id)
        references cashback.reconciliation_run (id, network_account_id),
    constraint reconciliation_difference_transaction_account
        foreign key (network_transaction_id, network_account_id)
        references cashback.network_transaction (id, network_account_id)
);

comment on table cashback.reconciliation_difference is
    'One disagreement between what a network reported and what it paid (US6). Mutable on purpose: the row exists to be worked through, and the resolution records who decided what, when, and why.';
comment on column cashback.reconciliation_difference.expected_minor is
    'What the reported transactions add up to, in minor units (C-6). Null only for money on the statement that matches no report.';
comment on column cashback.reconciliation_difference.actual_minor is
    'What the statement actually paid, in minor units (C-6). Null only for a reported transaction the statement never paid.';

create index reconciliation_difference_run_idx on cashback.reconciliation_difference (run_id);
create index reconciliation_difference_transaction_idx
    on cashback.reconciliation_difference (network_transaction_id)
    where network_transaction_id is not null;
-- The operator queue (FR-060) is exactly this partial index.
create index reconciliation_difference_open_idx
    on cashback.reconciliation_difference (detected_at)
    where resolved_at is null;
