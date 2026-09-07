-- name: RecordUnmatchedReference :one
-- Record that this stored report named a reference matching no click, if it
-- did.
--
-- FR-034's other half. RecordUnattributedReport in the networks module covers
-- the reports a network attached NO reference to; that statement says in its
-- own comment why it cannot ask this question - cashback.click belongs to
-- this path, not to ingestion - and this is the read it was shaped to carry.
--
-- The predicate is the STATEMENT'S, not the caller's, for the same reason
-- the sibling gives: the stored columns are the authority on what the network
-- said and what Apivo recorded, and they are the same columns
-- entry_evidence_guard reads when it later decides whether a credit may omit
-- its click (0013). A second implementation in Go is where the two would
-- eventually disagree - as a member never paid, or as money in no queue at
-- all.
--
-- `not exists` rather than a left join, because the question is whether ANY
-- click carries the reference and not which one. click_ref is unique on
-- cashback.click (0012), so the two answer alike today; the difference is
-- that this one keeps answering alike if that constraint is ever relaxed.
--
-- ON CONFLICT rather than a caught 23505, for the reason the sibling gives: a
-- whole window shares one transaction, and a failed statement aborts it, so a
-- raw violation on the third of forty reports takes the other thirty-seven
-- down with it. The constraint is NAMED, so a foreign key failure still
-- raises - a queue row naming a report that does not exist is not something
-- to swallow.
--
-- No rows back covers three outcomes that mean the same thing to a caller:
-- the report carried no reference at all (the sibling's half), its reference
-- matched a click, or its observation is already recorded.
insert into cashback.unattributed_transaction (network_transaction_id)
select nt.id
  from cashback.network_transaction nt
 where nt.id = sqlc.arg(network_transaction_id)
   and nt.click_ref is not null
   and not exists (
       select 1
         from cashback.click c
        where c.click_ref = nt.click_ref
   )
on conflict on constraint unattributed_one_per_report do nothing
returning id, network_transaction_id, detected_at;

-- name: RecordCreditedClickReference :one
-- Record that this stored report named a click that already backs a credit,
-- if it did.
--
-- The third way a report can be money nobody can be credited for (spec 004,
-- T202): its reference round-tripped and matched a click, and that click
-- already earned its one credit (entry_click_id_idx, 0034). A second
-- network echoing the same reference does this, and so does one network
-- reporting one purchase under two transaction ids. The report is queued
-- for an operator exactly as an unmatched one is - the money stays visible,
-- and nobody is paid twice for one click.
--
-- The predicate is the STATEMENT'S, as RecordUnmatchedReference's is and
-- for the same reason: the stored columns decide, not a caller that has
-- just been refused and believes it knows why. A report whose click backs
-- no credit yet is not queued by this, whatever the caller thinks.
--
-- Reversals do not count as the click's credit, mirroring the index.
insert into cashback.unattributed_transaction (network_transaction_id)
select nt.id
  from cashback.network_transaction nt
 where nt.id = sqlc.arg(network_transaction_id)
   and nt.click_ref is not null
   and exists (
       select 1
         from cashback.click c
         join cashback.entry e on e.click_id = c.id and e.reversal_of_id is null
        where c.click_ref = nt.click_ref
   )
on conflict on constraint unattributed_one_per_report do nothing
returning id, network_transaction_id, detected_at;

-- name: RecordForeignCurrencyReference :one
-- Record that this stored report is in a currency its member cannot be paid
-- in, if it is (FR-109, spec 004).
--
-- The fourth way a report can be money nobody can be credited for: its
-- reference matched a click, and the click's member is in cashback in a
-- currency other than the report's - or in none at all, which
-- entry_currency_is_the_members (0036) refuses alike. The report is queued
-- for an operator exactly as an unmatched one is; the row names the
-- report, and the report names its currency.
--
-- The predicate is the STATEMENT'S, as its two siblings' are: the stored
-- columns decide, not a caller that was just refused and believes it knows
-- why. A report whose currency IS the member's is not queued by this.
insert into cashback.unattributed_transaction (network_transaction_id)
select nt.id
  from cashback.network_transaction nt
  join cashback.click c on c.click_ref = nt.click_ref
  left join cashback.participation p on p.account_id = c.account_id
 where nt.id = sqlc.arg(network_transaction_id)
   and (p.account_id is null or p.default_currency <> nt.currency)
on conflict on constraint unattributed_one_per_report do nothing
returning id, network_transaction_id, detected_at;

-- name: RecordForeignNetworkReference :one
-- Record that this stored report named a click another network issued, if
-- it did (FR-096, FR-098, spec 004).
--
-- The fifth way a report can be money nobody can be credited for: its
-- reference is a real click's, and that click was issued through a network
-- other than the one now reporting it. A retailer on two networks, whose
-- second network echoes a reference the first one sent the member out
-- with, does this - and so does a member's link pasted somewhere a second
-- network tracks. The click's own network is the only one whose report of
-- this reference is this click's (click.network_id, 0037), so the report is
-- queued for an operator exactly as an unmatched one is: the money stays
-- visible, and no network is credited for a purchase it did not send.
--
-- The predicate is the STATEMENT'S, as its siblings' are: the stored
-- columns decide. A report whose reference names no click at all is
-- RecordUnmatchedReference's and not this one's, and a report from the
-- issuing network is not queued by this, whatever the caller was answered.
-- Until the queue carries a reason column, which statement wrote the row is
-- the only record of why; the join is what an operator would run to find
-- out.
insert into cashback.unattributed_transaction (network_transaction_id)
select nt.id
  from cashback.network_transaction nt
  join cashback.click c on c.click_ref = nt.click_ref
 where nt.id = sqlc.arg(network_transaction_id)
   and c.network_id <> nt.network_id
on conflict on constraint unattributed_one_per_report do nothing
returning id, network_transaction_id, detected_at;
