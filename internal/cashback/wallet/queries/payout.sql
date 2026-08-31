-- name: SettledPayoutsFor :one
-- What a member has actually been paid, in one currency (T078).
--
-- The one figure GET /wallet returns that is NOT a balance. Every other
-- total is projected from the ledger, because a balance is what an account
-- holds now; this is money that has left the business entirely, so no
-- account holds it and there is nothing to project. It is read from the
-- payments themselves.
--
-- That is not the stored balance the contract forbids. A settled payout is
-- evidence of a payment that happened, in the same way an entry is evidence
-- of a credit - it is written once and never revised. What the contract
-- refuses is a running total somebody maintains beside the postings, which
-- drifts; a sum over immutable rows cannot.
--
-- SETTLED only. A submitted payout is money in flight and a failed one is
-- money that came back, and counting either would tell a member they had
-- been paid before - or despite - the rail saying so. The state column is
-- the rail's own verdict, and this asks for exactly the one that means the
-- money arrived.
--
-- Joined through withdrawal_request because that is where the member is
-- named: a payout names the request it pays, and the composite foreign key
-- on (request_id, amount_minor, currency) means the amount summed here is
-- the amount that was approved rather than one the payout could restate.
--
-- coalesce, because a member who has never been paid must read as zero
-- rather than as null - and the cast keeps sum()'s numeric out of Go, where
-- C-6 says no fractional money type may appear.
select coalesce(sum(payout.amount_minor), 0)::bigint as paid_minor
  from cashback.payout payout
  join cashback.withdrawal_request request on request.id = payout.request_id
 where request.account_id = sqlc.arg(account_id)
   and payout.currency = sqlc.arg(currency)
   and payout.state = 'settled';
