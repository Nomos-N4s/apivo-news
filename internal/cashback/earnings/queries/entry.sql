-- name: CreateEntry :one
-- Open an entry for a report, in the state the network's own report puts it.
--
-- No state is checked here: entry_state_known and entry_hold_rule_iff_held do
-- it in the schema, and CanOpen does it in Go before the transaction starts.
-- What this statement is for is the constraint no caller can enforce -
-- entry_one_per_report - which makes exactly-once crediting a property of the
-- database rather than of the poller's memory.
insert into cashback.entry (
    account_id, brand_id, network_transaction_id, click_id,
    state, amount_minor, currency, hold_rule, reversal_of_id
) values (
    sqlc.arg(account_id), sqlc.arg(brand_id), sqlc.arg(network_transaction_id),
    sqlc.narg(click_id), sqlc.arg(state), sqlc.arg(amount_minor),
    sqlc.arg(currency), sqlc.narg(hold_rule), sqlc.narg(reversal_of_id)
)
returning id, account_id, brand_id, network_transaction_id, click_id,
          state, amount_minor, currency, hold_rule, reversal_of_id, created_at;

-- name: MoveEntry :one
-- Move an entry from one state to another, if it is still in the state the
-- caller read.
--
-- The `state = @from_state` in the WHERE is the whole point. Two pollers, or
-- a poller and an operator, can read one entry at the same instant and decide
-- different futures for it; without this clause both writes succeed and the
-- second silently overwrites the first, leaving an entry whose state matches
-- neither of the two transitions recorded against it. With it, the second
-- finds no row and its caller is told, before the ledger has been asked to
-- move anything.
--
-- It is not a substitute for the transition table. This refuses a move from a
-- state the entry has left; CanFollow refuses a move that was never lawful
-- from any state, which is a different mistake and gets a different answer.
update cashback.entry
   set state = sqlc.arg(to_state),
       -- A held entry names the rule that held it and an entry that is not
       -- held names none (entry_hold_rule_iff_held). Releasing therefore has
       -- to clear it in the same statement that changes the state, or the row
       -- is momentarily unrepresentable and the constraint refuses the write.
       hold_rule = sqlc.narg(hold_rule)
 where id = sqlc.arg(id)
   and state = sqlc.arg(from_state)
returning id, account_id, brand_id, network_transaction_id, click_id,
          state, amount_minor, currency, hold_rule, reversal_of_id, created_at;

-- name: RecordTransition :one
-- Record what moved, why, and which posting carried it.
--
-- ledger_transfer_ref is not null in the schema (D7): a state recorded with
-- no posting behind it is precisely the disagreement between the wallet and
-- the ledger that C-1 exists to prevent. So this statement cannot be called
-- before the transfer exists, and the caller's ordering - post, then record -
-- is the schema's requirement rather than a convention.
insert into cashback.entry_transition (
    entry_id, from_state, to_state, ledger_transfer_ref, reason, actor_id
) values (
    sqlc.arg(entry_id), sqlc.narg(from_state), sqlc.arg(to_state),
    sqlc.arg(ledger_transfer_ref), sqlc.narg(reason), sqlc.narg(actor_id)
)
returning id, entry_id, from_state, to_state, ledger_transfer_ref, reason,
          actor_id, occurred_at;

-- name: LinkLedgerTransfer :one
-- Record the join C-7 is answered from: this transition, this entry, this
-- transfer.
--
-- Separate from the transition row rather than folded into it, because this
-- is the seam that makes the ledger swappable (data-model 2.6): Apivo stores
-- the join and nothing else about a posting. The composite foreign key means
-- a link naming a transition of a different entry, or a different transfer
-- than that transition recorded, is unrepresentable rather than merely wrong.
insert into cashback.ledger_link (transition_id, entry_id, ledger_transfer_ref)
values (sqlc.arg(transition_id), sqlc.arg(entry_id), sqlc.arg(ledger_transfer_ref))
returning transition_id, entry_id, ledger_transfer_ref, posted_at;

-- name: GetEntry :one
-- Read one entry as it stands now.
select id, account_id, brand_id, network_transaction_id, click_id,
       state, amount_minor, currency, hold_rule, reversal_of_id, created_at
  from cashback.entry
 where id = sqlc.arg(id);

-- name: ConfirmedEntriesFor :many
-- Every confirmed entry a member holds in one currency, oldest first, locked.
--
-- This is what a withdrawal reserves FROM. It returns the whole set rather
-- than the few rows a particular amount needs, and it takes a row lock on all
-- of them, and both of those are the point:
--
--   * FOR UPDATE is what makes two withdrawal requests for one member
--     sequential. The second blocks here, and by the time it reads, the first
--     has moved its entries out of confirmed - so the second sees what is
--     actually left rather than what was there when it started. Without it
--     both could select the same entries and both would post a reservation
--     for money that only exists once.
--   * The whole set, because which rows a given amount needs is a decision
--     about running totals, and a decision about money is one this repository
--     makes in Go where it can be read and tested, not in a window function
--     that cannot be locked in the same statement anyway (Postgres refuses
--     FOR UPDATE beside one).
--
-- Oldest first is not arbitrary either: cashback is paid out in the order it
-- was earned, so a member's oldest confirmed money leaves first and nothing
-- can sit at the back of the queue indefinitely.
--
-- One currency, because an account holds one currency (C-6) and a withdrawal
-- is denominated in one. Confirmed money in another currency is a different
-- balance and a different withdrawal.
select id, account_id, brand_id, network_transaction_id, click_id,
       state, amount_minor, currency, hold_rule, reversal_of_id, created_at
  from cashback.entry
 where account_id = sqlc.arg(account_id)
   and currency = sqlc.arg(currency)
   and state = 'confirmed'
 order by created_at, id
   for update;
