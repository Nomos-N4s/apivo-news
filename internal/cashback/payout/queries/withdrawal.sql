-- Withdrawal requests: what a member asked for, and the reservation that
-- already happened when they asked (D9).

-- name: CreateWithdrawalRequest :one
-- Write the request for a reservation that has already been posted.
--
-- The id is supplied rather than defaulted, and that is the one unusual thing
-- here. reserved_transfer_ref is NOT NULL, so the transfer has to exist
-- before this row can; and the transfer's idempotency key is derived from the
-- request (D8), so the request's id has to exist before the transfer. The
-- only way out of that circle is for the caller to mint the id, post under a
-- key derived from it, and then write this row - which is what
-- payout.Withdrawals does.
--
-- Nothing here re-checks ownership or verification. The composite foreign key
-- withdrawal_request_destination_is_the_members means a destination that is
-- not this member's cannot be named at all, and withdrawal_request_guard
-- refuses an unverified one with a locking read. Those are the checks; the
-- service reads them first only so a member gets a 403 or a 409 instead of a
-- constraint violation, never instead of the database enforcing them.
insert into cashback.withdrawal_request (
    id, account_id, destination_id, amount_minor, currency, reserved_transfer_ref
) values (
    sqlc.arg(id), sqlc.arg(account_id), sqlc.arg(destination_id),
    sqlc.arg(amount_minor), sqlc.arg(currency), sqlc.arg(reserved_transfer_ref)
)
returning id, account_id, destination_id, amount_minor, currency, state,
          requested_at, reserved_transfer_ref, decided_by, decided_at,
          decision_reason;

-- name: GetWithdrawalRequestForAccount :one
-- One request, if it is this member's.
--
-- Narrowed on the account for the reason GetPayoutDestinationForAccount is:
-- another member's request and a request that does not exist come back the
-- same way, so an id cannot be probed for existence.
select id, account_id, destination_id, amount_minor, currency, state,
       requested_at, reserved_transfer_ref, decided_by, decided_at,
       decision_reason
  from cashback.withdrawal_request
 where id = sqlc.arg(id)
   and account_id = sqlc.arg(account_id);

-- name: ListWithdrawalRequestsForAccount :many
-- Every request this member has made, newest first - the order a member reads
-- their own history in, and the order withdrawal_request_account_idx is built
-- for.
select id, account_id, destination_id, amount_minor, currency, state,
       requested_at, reserved_transfer_ref, decided_by, decided_at,
       decision_reason
  from cashback.withdrawal_request
 where account_id = sqlc.arg(account_id)
 order by requested_at desc, id desc;
