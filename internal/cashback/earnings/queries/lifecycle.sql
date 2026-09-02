-- The earnings lifecycle: what the poller ingested, turned into what a
-- member is owed (#435). Three reads, one per step, each answering "what is
-- waiting" rather than deciding anything - the deciding is Go's, in
-- lifecycle.go, through the same matcher, share, hold rules and state
-- machine the journey test proved.
--
-- Every read speaks of a TRANSACTION through its current report, the tip of
-- its chain (0012): the row nothing supersedes. An entry cites the report it
-- was opened on, which may since have been restated, so "this transaction
-- already has an entry" and "the network now says confirmed" are both
-- questions about the chain, not about one row.

-- name: ReportsAwaitingCredit :many
-- Current reports that could become money and have not: they carry a click
-- reference, the network still intends to pay (pending or confirmed), no
-- entry cites any row of the transaction, and the report is not in the
-- unattributed queue - open or closed. Closed matters: a report an operator
-- dismissed is decided, and a scan that re-matched it every pass would be
-- re-asking a question a human already answered.
--
-- A report the network has declined or reversed before anything was
-- credited is money that never was, and is left alone: crediting it only
-- to reverse it would move money twice to say nothing. So is a report of no
-- commission: a share of nothing is not a credit (open.go refuses it), and
-- a restatement that puts a commission on it arrives as a new tip.
select nt.id, nt.network_id, nt.click_ref, nt.status,
       nt.sale_amount_minor, nt.commission_minor, nt.currency, nt.transacted_at
  from cashback.network_transaction nt
 where nt.click_ref is not null
   and nt.status in ('pending', 'confirmed')
   and nt.commission_minor > 0
   and not exists (
       select 1 from cashback.network_transaction s where s.supersedes_id = nt.id
   )
   and not exists (
       select 1
         from cashback.entry e
         join cashback.network_transaction cited on cited.id = e.network_transaction_id
        where cited.network_id = nt.network_id
          and cited.external_id = nt.external_id
   )
   and not exists (
       select 1 from cashback.unattributed_transaction u where u.network_transaction_id = nt.id
   )
 order by nt.retrieved_at, nt.id
 limit sqlc.arg(page_size);

-- name: BrandOfOffer :one
-- Which brand owes a credit earned through an offer: the route the offer
-- was published on names it (ADR-0004). An entry is money owed to a member
-- BY A BRAND, and the click carries the offer, not the brand.
select mn.brand_id
  from cashback.offer o
  join cashback.merchant_network mn on mn.id = o.merchant_network_id
 where o.id = sqlc.arg(offer_id);

-- name: EntriesAwaitingConfirmation :many
-- Pending credits whose transaction the network now reports confirmed. The
-- entry's own row may be an older word; the tip is the current one. Whether
-- a statement has accounted for it is Confirm's to ask (FR-043), not this
-- read's: a credit the network confirmed and no statement covers is still
-- waiting, and answered as such on every pass until one does.
select sqlc.embed(e), tip.id as current_report
  from cashback.entry e
  join cashback.network_transaction cited on cited.id = e.network_transaction_id
  join cashback.network_transaction tip
    on tip.network_id = cited.network_id
   and tip.external_id = cited.external_id
   and not exists (select 1 from cashback.network_transaction s where s.supersedes_id = tip.id)
 where e.state = 'pending'
   and e.reversal_of_id is null
   and tip.status = 'confirmed'
 order by e.created_at, e.id
 limit sqlc.arg(page_size);

-- name: EntriesAwaitingReversal :many
-- Credits whose transaction the network has since declined or reversed,
-- and that no reversing entry undoes yet. The tip must be a DIFFERENT row
-- from the one the entry cites: a reversal cites the superseding report as
-- its own evidence (C-3), and entry_one_per_report would refuse a second
-- entry on the original's. Held, pending and confirmed all hold money that
-- can go back; reserved and paid do not - money a withdrawal has claimed is
-- the clawback posture's (Q3), not this read's.
select sqlc.embed(e), tip.id as current_report, tip.status as current_status
  from cashback.entry e
  join cashback.network_transaction cited on cited.id = e.network_transaction_id
  join cashback.network_transaction tip
    on tip.network_id = cited.network_id
   and tip.external_id = cited.external_id
   and not exists (select 1 from cashback.network_transaction s where s.supersedes_id = tip.id)
 where e.state in ('held', 'pending', 'confirmed')
   and e.reversal_of_id is null
   and tip.status in ('declined', 'reversed')
   and tip.id <> e.network_transaction_id
   and not exists (select 1 from cashback.entry r where r.reversal_of_id = e.id)
 order by e.created_at, e.id
 limit sqlc.arg(page_size);
