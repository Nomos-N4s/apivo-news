-- Deriving a run's differences (T111, US6).
--
-- The derivation itself is Go: the statement's lines are read by the same
-- code that read them at import, so detection and import cannot disagree
-- about what a line is. This file is the two reads it needs and the one
-- write it makes.

-- name: GetReconciliationRun :one
-- One imported statement, whole: the frame (account, period, who, when) and
-- the document the differences are derived from.
select id, network_account_id, statement_period_start, statement_period_end,
       imported_by, imported_at, raw_statement, statement_digest
  from cashback.reconciliation_run
 where id = sqlc.arg(id);

-- name: CurrentReportsForStatement :many
-- The reports a statement is judged against: the CURRENT row of every
-- transaction the statement names, whatever its period, plus every current
-- confirmed report for the statement's period, named or not.
--
-- Current, not every row: a superseded report is what the network used to
-- say, and comparing a payment against it would raise a difference the
-- network has already corrected. The tip of a chain is the row nothing
-- supersedes (0012).
--
-- Two populations in one read, because they are the two sides of the
-- comparison. A line names a transaction; its current report is what tells
-- whether the amount matches, and that report may be outside the period
-- (a late payment for an earlier purchase is still a payment for it). A
-- confirmed report in the period is money the network owes; if no line
-- names it, it was not paid. Pending reports are not owed yet and are not
-- expected on a statement; declined and reversed ones are owed nothing.
select nt.id, nt.external_id, nt.status, nt.commission_minor, nt.currency, nt.transacted_at
  from cashback.network_transaction nt
 where nt.network_account_id = sqlc.arg(network_account_id)
   and not exists (
       select 1 from cashback.network_transaction s where s.supersedes_id = nt.id
   )
   and (nt.external_id = any(sqlc.arg(statement_transaction_ids)::text[])
        or (nt.status = 'confirmed'
            and nt.transacted_at >= sqlc.arg(period_start)
            and nt.transacted_at < sqlc.arg(period_end)))
 order by nt.transacted_at, nt.external_id;

-- name: RecordDifference :one
-- Write one difference, unless this run already has one for the same
-- report or the same statement line (0029). No rows means it was there.
--
-- ON CONFLICT DO NOTHING with no target, because there are two ways to be
-- already there - the report's index and the line's - and either is the
-- same answer: this finding is recorded, resolved or not, and is not raised
-- at an operator again.
insert into cashback.reconciliation_difference
    (run_id, network_account_id, kind, network_transaction_id, statement_transaction_id,
     expected_minor, actual_minor, currency)
values (sqlc.arg(run_id), sqlc.arg(network_account_id), sqlc.arg(kind),
        sqlc.narg(network_transaction_id), sqlc.narg(statement_transaction_id),
        sqlc.narg(expected_minor), sqlc.narg(actual_minor), sqlc.arg(currency))
on conflict do nothing
returning id, detected_at;
