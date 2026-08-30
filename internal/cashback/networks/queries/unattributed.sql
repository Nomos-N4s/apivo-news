-- The unattributed queue (T058, FR-034): recording that a report went
-- unattributed, and asking which of those records is still work.
--
-- The two halves are deliberately different kinds of thing. What was SEEN is
-- written once and never edited - 0013 freezes which report a row names and
-- when it was noticed, and 0024 stops the row being deleted or its
-- resolution erased. Whether it is still WORK is a property of the
-- transaction NOW, and a later report cannot make an earlier observation
-- false any more than a reversal makes a confirmation false (C-3). So that
-- half is a read.

-- name: RecordUnattributedReport :one
-- Record that this stored report went unattributed, if it did.
--
-- The predicate is the STATEMENT'S, not the caller's, for the reason
-- content_digest is computed by a trigger rather than supplied: the column
-- the database stored is the authority on what the network said, and it is
-- the same column entry_evidence_guard reads when it later decides whether a
-- credit may omit its click (0013). A second implementation in Go is where
-- the two would eventually disagree - as a member never paid, or as money in
-- no queue at all.
--
-- Null and nothing else. click_ref is constrained not to be blank (0012), so
-- `is null` is total and a report has no third state to be in.
--
-- A reference that matches no CLICK is also "no matching click" under
-- FR-034, and it is deliberately not asked here. cashback.click belongs to
-- the click-out and earnings path, not to this module: matching a reference
-- to a click is the attribution step, and this package's job ends at what
-- the network said. Asking it from here would also be wrong today in a way
-- that is easy to miss - no code writes a click yet, so the join would
-- report every attributed report as unattributed. T067 owns that half, and
-- the read below is shaped to carry it when it arrives.
--
-- ON CONFLICT rather than a caught 23505, for the reason
-- InsertNetworkTransactionIfNew gives: a whole window shares one
-- transaction, and a failed statement aborts it, so a raw violation on the
-- third of forty reports takes the other thirty-seven down with it and
-- leaves the cursor where it was - the window re-read forever. The
-- constraint is NAMED, so a foreign key failure still raises: a queue row
-- naming a report that does not exist is not something to swallow.
--
-- No rows back means the report carried a reference, or its observation is
-- already stored. Both mean there is nothing to do, so both answer alike.
insert into cashback.unattributed_transaction (network_transaction_id)
select nt.id
  from cashback.network_transaction nt
 where nt.id = sqlc.arg(network_transaction_id)
   and nt.click_ref is null
on conflict on constraint unattributed_one_per_report do nothing
returning id, network_transaction_id, detected_at;

-- name: ListOpenUnattributedReports :many
-- The unattributed work an operator still has (FR-034, FR-060).
--
-- Three conditions, and each earns its place.
--
-- Nobody has resolved it. That is the operator's own decision, and it rides
-- the partial index 0013 created for exactly this read.
--
-- The report it names is still the TIP of its chain. This is the whole of
-- automatic resolution, and it is the same NOT EXISTS that
-- GetCurrentNetworkTransaction defines "current" with - two definitions of
-- the current row is how a screen comes to show one amount while an entry
-- cites another. It needs no resolved_by to invent, which matters because
-- there is nothing to invent one FROM: resolved_by references an account and
-- the all-or-none check demands all three columns, so "the network resolved
-- it itself" has no representation. It also makes the money right for free -
-- the open row IS the tip, so its own columns are the current ones - and it
-- makes one open row per transaction fall out, because an earlier
-- observation closes exactly when its successor is recorded.
--
-- Nothing has been credited against it. This is the belt. An operator action
-- writes the entry and the resolution together, but a report already
-- credited must never come back as work even if that resolution were lost.
-- It rides entry_one_per_report.
--
-- Ordered by (detected_at, id): every row one poll records shares
-- detected_at exactly, because the poller reads its clock once per poll, so
-- the id tiebreak is what makes this a total order rather than a page that
-- skips or repeats rows.
select
    u.id,
    u.detected_at,
    nt.id as network_transaction_id,
    nt.network_id,
    nt.network_account_id,
    nt.external_id,
    nt.status,
    nt.sale_amount_minor,
    nt.commission_minor,
    nt.currency,
    nt.transacted_at,
    nt.retrieved_at,
    -- Which resolutions are legal, derived from immutable evidence rather
    -- than stored. Where the network named no reference an operator may
    -- attribute by hand; where it named one that matched no click,
    -- entry_evidence_guard refuses a null click_id and there is no click to
    -- cite, so dismissing is the only lawful outcome. Every row this module
    -- records today is the first kind; T067 will record the second into this
    -- same table, and this column is what lets the operator surface tell
    -- them apart without a kind column and without a migration.
    (nt.click_ref is null)::boolean as attributable
  from cashback.unattributed_transaction u
  join cashback.network_transaction nt on nt.id = u.network_transaction_id
 where u.resolved_at is null
   and not exists (
       select 1 from cashback.network_transaction s where s.supersedes_id = nt.id
   )
   and not exists (
       select 1 from cashback.entry e where e.network_transaction_id = nt.id
   )
   and (u.detected_at, u.id) > (sqlc.arg(after_detected_at)::timestamptz, sqlc.arg(after_id)::uuid)
 order by u.detected_at, u.id
 limit sqlc.arg(page_size);

-- name: GetOpenUnattributedReport :one
-- The same question about one row, asked again inside the transaction of the
-- action itself.
--
-- Deliberately the same three conditions rather than a lookup by id. An
-- operator's page is minutes or hours old, and between the render and the
-- click a poll may have superseded the report with an attributed one. No
-- rows means the answer has changed, which the caller reports as such - it
-- does not mean "not found".
--
-- This is what stops a stale page becoming a second credit. An entry may
-- legally cite a superseded report - entry_evidence_guard reads click_ref
-- from whichever report the entry names - so an operator attributing a
-- superseded root and the automatic path crediting the attributed tip would
-- write two entries for one purchase, past entry_one_per_report, because
-- those are two different reports.
select
    u.id,
    u.detected_at,
    nt.id as network_transaction_id,
    nt.network_id,
    nt.network_account_id,
    nt.external_id,
    nt.status,
    nt.sale_amount_minor,
    nt.commission_minor,
    nt.currency,
    nt.transacted_at,
    nt.retrieved_at,
    (nt.click_ref is null)::boolean as attributable
  from cashback.unattributed_transaction u
  join cashback.network_transaction nt on nt.id = u.network_transaction_id
 where u.id = sqlc.arg(id)
   and u.resolved_at is null
   and not exists (
       select 1 from cashback.network_transaction s where s.supersedes_id = nt.id
   )
   and not exists (
       select 1 from cashback.entry e where e.network_transaction_id = nt.id
   );
