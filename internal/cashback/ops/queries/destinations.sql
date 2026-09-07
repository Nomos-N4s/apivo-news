-- The operator's half of payout destination verification (B6, #548,
-- FR-051 with FR-061, ADR-0006).
--
-- Every statement in the payout module's own destination.sql is narrowed on
-- the account, and its header says why: ownership travels with the key
-- rather than being re-checked by each caller, and a query taking an id
-- alone would be the one place a later caller forgets, on the one table
-- where forgetting sends a member's money elsewhere.
--
-- These take an id alone, and that is exactly why they are HERE and not
-- there. An operator acts across members by definition - it is what the
-- role is - so the ownership narrowing has nothing to narrow on and would
-- have to be faked with the destination's own account read a moment
-- earlier. Keeping these in the operator module leaves the member module's
-- rule true without exception, which is what makes it enforceable.
--
-- The same split the unattributed queue already has: the networks module
-- owns what was seen, this module owns what a person decided about it.

-- name: ListUnverifiedDestinations :many
-- The destinations waiting for somebody to verify them, oldest first.
--
-- The queue an operator works through. A destination is here from the
-- moment a member records it until somebody proves it is theirs, and no
-- withdrawal may name it in between (FR-051), so a row sitting here is a
-- member who cannot be paid.
--
-- Ordered by (created_at, id) for the reason the member's own list is:
-- created_at defaults to now() and two destinations recorded in one
-- transaction share an instant to the microsecond, so the id tiebreak is
-- what makes this a total order rather than a page that skips or repeats.
--
-- It carries NO details and cannot: the column holds a reference and the
-- details live in the vault (ADR-0006). What an operator does with this row
-- is open the reference there.
select
    d.id,
    d.account_id,
    d.kind,
    d.details_ref,
    d.created_at,
    a.email as account_email
  from cashback.payout_destination d
  join public.account a on a.id = d.account_id
 where d.verified_at is null
   and (d.created_at, d.id) > (sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_id)::uuid)
 order by d.created_at, d.id
 limit sqlc.arg(page_size);

-- name: GetDestinationForVerification :one
-- One destination as an operator sees it, whoever it belongs to.
--
-- Read before the write, and not only to be friendly about a repeat: the
-- update below cannot tell "no such destination" from "already verified",
-- and those are a not-found and a success that happened earlier.
select
    d.id,
    d.account_id,
    d.kind,
    d.details_ref,
    d.verified_at,
    d.verified_method,
    d.verified_by,
    d.created_at
  from cashback.payout_destination d
 where d.id = sqlc.arg(id);

-- name: VerifyDestinationAsOperator :one
-- Record that a named operator proved this destination belongs to its
-- member (FR-051, FR-061).
--
-- Narrowed on verified_at being null, exactly as the member's own statement
-- is and for the same reason: the table's guard raises on any attempt to
-- change a verification that stands, so a statement that did not exclude
-- verified rows would turn an ordinary repeat into a database error in the
-- middle of an operator's request. No rows back means somebody verified it
-- first, which the caller reads back rather than reporting as a failure.
--
-- verified_by references public.account, so "a named human" is the foreign
-- key's guarantee and not this module's (FR-061, 0039).
update cashback.payout_destination
   set verified_at = now(),
       verified_method = sqlc.arg(verified_method),
       verified_by = sqlc.arg(verified_by)
 where id = sqlc.arg(id)
   and verified_at is null
returning id, account_id, kind, details_ref, verified_at, verified_method, created_at, verified_by;
