-- 0029_reconciliation_difference_identity: every difference says which line
-- or which report it is about, and says it once per run.
--
-- 0015 gave a paid_not_reported difference an amount and a currency and
-- nothing else: money on the statement matching no report has no report to
-- name, so it named nothing. That is exact and useless. An operator looking
-- at "250 EUR, paid, not reported" cannot tell which line of the statement
-- it is, and two such lines for the same amount are the same row twice.
-- statement_transaction_id is the statement's own identifier for the line,
-- and paid_not_reported must carry it. The other two kinds name a report,
-- whose external_id IS the line, so for them the column stays null - the
-- shape rule keeps saying exactly the facts that make a row its kind and
-- none that would contradict it.
--
-- The two partial unique indexes are what lets detection be re-run. A run's
-- differences are derived from an immutable statement and the reports as
-- they stand, so detection is repeatable by construction - but a repeat
-- that inserted its findings again would double the queue. With one row per
-- (run, report) and one per (run, statement line), a repeat inserts what is
-- new and skips what is there, resolved or not: a difference an operator
-- has already worked through is not raised at them a second time.
--
-- The tightened check is safe to add: nothing has produced a difference
-- before this migration (detection is what follows it), so no row exists to
-- fail it.

alter table cashback.reconciliation_difference
    add column statement_transaction_id text
        constraint reconciliation_difference_statement_transaction_id_not_blank
            check (statement_transaction_id is null or btrim(statement_transaction_id) <> '');

comment on column cashback.reconciliation_difference.statement_transaction_id is
    'The statement''s own identifier for the line, carried only by paid_not_reported: money matching no report has no report to name, and without this an operator could not tell two such lines apart. For the kinds that name a report, the report''s external_id is the line, and this stays null.';

alter table cashback.reconciliation_difference
    drop constraint reconciliation_difference_shape_matches_kind;

alter table cashback.reconciliation_difference
    add constraint reconciliation_difference_shape_matches_kind check (
        case kind
            -- The statement paid a different amount than the report says.
            when 'amount_mismatch' then network_transaction_id is not null
                and statement_transaction_id is null
                and expected_minor is not null
                and actual_minor is not null
                and expected_minor <> actual_minor
            -- A report the statement never paid: an expectation, no payment.
            when 'reported_not_paid' then network_transaction_id is not null
                and statement_transaction_id is null
                and expected_minor is not null
                and actual_minor is null
            -- Money on the statement matching no report: a payment, a line
            -- it came from, and nothing that expected it.
            when 'paid_not_reported' then network_transaction_id is null
                and statement_transaction_id is not null
                and expected_minor is null
                and actual_minor is not null
            else false
        end
    );

create unique index reconciliation_difference_one_per_report
    on cashback.reconciliation_difference (run_id, network_transaction_id)
    where network_transaction_id is not null;

create unique index reconciliation_difference_one_per_line
    on cashback.reconciliation_difference (run_id, statement_transaction_id)
    where statement_transaction_id is not null;
