-- Resolving a difference (T112, US6, FR-061): a named human, a verdict and
-- a reason, recorded together, once.

-- name: ResolveDifference :one
-- Close one difference: who, what was decided, why, and when, in one
-- statement, because the schema demands all four or none.
--
-- `resolved_at is null` in the WHERE is what makes two operators deciding
-- the same row at the same moment safe without a lock: the second statement
-- waits on the first's row lock, re-reads after it commits, finds the column
-- set and matches nothing - so the second caller is told the row was taken
-- rather than quietly overwriting the first operator's verdict.
--
-- resolved_at is now() rather than a supplied instant: the row's own clock
-- is the one an auditor reads.
update cashback.reconciliation_difference d
   set resolved_by = sqlc.arg(resolved_by),
       resolved_reason = sqlc.arg(resolved_reason),
       resolution = sqlc.arg(resolution),
       resolved_at = now()
 where d.id = sqlc.arg(id)
   and d.resolved_at is null
returning d.id, d.run_id, d.kind, d.network_transaction_id, d.statement_transaction_id,
          d.expected_minor, d.actual_minor, d.currency, d.detected_at,
          d.resolved_by, d.resolved_reason, d.resolution, d.resolved_at;

-- name: ClassifyDifference :one
-- Why a row could not be resolved: it does not exist (no rows), or somebody
-- already decided it (a resolution, returned so the 409 can say who and
-- what). Read in the caller's transaction, so what it reports is what the
-- update saw.
select d.id, d.resolved_by, d.resolved_reason, d.resolution, d.resolved_at
  from cashback.reconciliation_difference d
 where d.id = sqlc.arg(id);
