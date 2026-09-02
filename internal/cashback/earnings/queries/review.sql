-- Reviewing held credits: the queue an operator works, and the two reads a
-- decision on one row needs (T119, US7 scenario 3, FR-061).

-- name: ListHeldEntries :many
-- The held queue, oldest first, keyset-paged on (created_at, id). Each row
-- carries the rule that held it and the reason the rule gave - the opening
-- transition's reason, which is where hold rules write it - beside what the
-- network reported, so an operator decides from one screen.
--
-- A held credit an operator has already rejected is excluded rather than
-- moved: a rejection is a reversing entry beside the credit (SC-010), and
-- the credit's own row keeps the state it was rejected in. "Still to
-- decide" is therefore "held, and nothing undoes it".
select e.id, e.account_id, e.brand_id, e.network_transaction_id, e.click_id,
       e.hold_rule, e.amount_minor, e.currency, e.created_at,
       coalesce((select t.reason
                   from cashback.entry_transition t
                  where t.entry_id = e.id and t.to_state = 'held'
                  order by t.occurred_at desc, t.id desc
                  limit 1), '')::text as hold_reason,
       nt.network_id, nt.external_id, nt.status as report_status,
       nt.sale_amount_minor, nt.commission_minor, nt.transacted_at
  from cashback.entry e
  join cashback.network_transaction nt on nt.id = e.network_transaction_id
 where e.state = 'held'
   and e.reversal_of_id is null
   and not exists (select 1 from cashback.entry r where r.reversal_of_id = e.id)
   and (e.created_at, e.id) > (sqlc.arg(after_created_at), sqlc.arg(after_id))
 order by e.created_at, e.id
 limit sqlc.arg(page_size);

-- name: LockEntry :one
-- One entry, locked for the decision about to be recorded against it. Two
-- operators deciding one held credit at the same instant serialise here, and
-- the second reads what the first did.
select id, account_id, brand_id, network_transaction_id, click_id,
       state, amount_minor, currency, hold_rule, reversal_of_id, created_at
  from cashback.entry
 where id = sqlc.arg(id)
   for update;

-- name: ReversalOf :one
-- The reversing entry that undoes a credit, if one exists. A credit is
-- reversed at most once (entry_reversed_at_most_once), so :one is exact.
select id
  from cashback.entry
 where reversal_of_id = sqlc.arg(entry_id);

-- name: HeldTransition :one
-- The transition that most recently put an entry in held: the cause a
-- release derives its posting key from (D8). An entry can be held more
-- than once in its life - pending to held and out again - and a key that
-- named only the entry would make the second release a replay of the first,
-- which the ledger would answer by moving nothing.
select id
  from cashback.entry_transition
 where entry_id = sqlc.arg(entry_id)
   and to_state = 'held'
 order by occurred_at desc, id desc
 limit 1;

-- name: LatestTransition :one
-- The most recent transition an entry made, read back so what a review
-- announces - who, why, when, which transfer - is what the row holds.
select id, entry_id, from_state, to_state, ledger_transfer_ref, reason,
       actor_id, occurred_at
  from cashback.entry_transition
 where entry_id = sqlc.arg(entry_id)
 order by occurred_at desc, id desc
 limit 1;
