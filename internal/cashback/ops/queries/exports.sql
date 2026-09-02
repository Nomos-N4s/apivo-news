-- The accounting exports (T114, FR-062): two journals, read by window.
--
-- Both are read newest-last in the order the rows were written, bounded by
-- a page size the caller sets one past what it will accept - so "too large
-- to export in one document" is answered by what came back, never guessed.
-- Both windows are half-open, [from, to), so two adjacent exports share no
-- row and miss none.

-- name: ExportTransitions :many
-- The ledger journal: every movement of a member's money, as the earnings
-- module recorded it, with the transfer reference that pins it to the
-- ledger's own record. The entry is joined for whose money, under which
-- brand, on which report, and how much - the transition carries the states
-- and the transfer, the entry carries the money (0013).
select t.id, t.entry_id, e.account_id, e.brand_id, e.network_transaction_id,
       t.from_state, t.to_state, e.amount_minor, e.currency,
       t.ledger_transfer_ref, t.reason, t.actor_id, t.occurred_at
  from cashback.entry_transition t
  join cashback.entry e on e.id = t.entry_id
 where t.occurred_at >= sqlc.arg(from_at)
   and t.occurred_at < sqlc.arg(to_at)
 order by t.occurred_at, t.id
 limit sqlc.arg(page_size);

-- name: ExportDifferences :many
-- The reconciliation journal: every difference detected in the window, with
-- the statement it came from - which publisher account at which network,
-- covering which period - the transaction in the network's own terms, the
-- figures, and the decision if one was made. LEFT JOIN on the report,
-- because paid_not_reported names none.
select d.id, d.run_id, r.network_account_id, na.network_id, na.external_publisher_id,
       r.statement_period_start, r.statement_period_end,
       d.kind, d.network_transaction_id, nt.external_id, d.statement_transaction_id,
       d.expected_minor, d.actual_minor, d.currency, d.detected_at,
       d.resolution, d.resolved_by, d.resolved_reason, d.resolved_at
  from cashback.reconciliation_difference d
  join cashback.reconciliation_run r on r.id = d.run_id
  join cashback.network_account na on na.id = r.network_account_id
  left join cashback.network_transaction nt on nt.id = d.network_transaction_id
 where d.detected_at >= sqlc.arg(from_at)
   and d.detected_at < sqlc.arg(to_at)
 order by d.detected_at, d.id
 limit sqlc.arg(page_size);
