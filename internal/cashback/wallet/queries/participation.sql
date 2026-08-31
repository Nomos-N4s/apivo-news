-- name: ParticipationFor :one
-- The member's own opt-in record (T080, FR-001).
--
-- No brand predicate. One deployment serves one brand (ADR-0004) and the
-- account is the primary key, so a brand filter here could only ever be the
-- deployment's own - and the day it were not, it would hide a member's
-- record from them rather than showing them somebody else's. The column is
-- carried for the tenant boundary a later deployment draws; it is not a
-- filter this read may apply on its own authority.
select account_id, brand_id, opted_in_at, terms_version, status, left_at, default_currency
  from cashback.participation
 where account_id = sqlc.arg(account_id);

-- name: OptIntoParticipation :one
-- Opt in, or re-join after leaving, in one statement (T080, FR-002).
--
-- The conflict clause is what makes re-joining a TRANSITION rather than a
-- second row: participation is keyed on the account, so a member who left
-- and came back updates the row they already have. 0017's guard permits
-- exactly that move - left -> active re-states opted_in_at, terms_version
-- and default_currency together - and refuses every other edit to them, so
-- this statement is the only shape of write the schema will accept.
--
-- The `where participation.status = 'left'` is the concurrency control and
-- the 409, both. ON CONFLICT DO UPDATE takes the row lock before it
-- evaluates the predicate, so two simultaneous opt-ins serialise: the
-- second sees the first's committed row, finds it active, updates nothing
-- and returns nothing. "Already opted in" therefore arrives as zero rows
-- rather than as a duplicate record, and it arrives that way whether the
-- previous opt-in was a second ago or a year ago.
--
-- brand_id is deliberately absent from the update. The guard freezes it,
-- so re-stating it would raise on any deployment that had since been
-- rebranded - and freezing it is right: the row records which brand this
-- member accepted terms from, and that is not a fact a later configuration
-- may rewrite.
--
-- left_at goes back to null because the check constraint requires it:
-- status = 'left' iff left_at is not null, so an active row carrying a
-- leaving date is unrepresentable rather than merely wrong.
insert into cashback.participation (
    account_id, brand_id, terms_version, default_currency, status
) values (
    sqlc.arg(account_id), sqlc.arg(brand_id), sqlc.arg(terms_version), sqlc.arg(default_currency), 'active'
)
on conflict (account_id) do update
   set opted_in_at = now(),
       terms_version = excluded.terms_version,
       default_currency = excluded.default_currency,
       status = 'active',
       left_at = null
 where participation.status = 'left'
returning account_id, brand_id, opted_in_at, terms_version, status, left_at, default_currency;

-- name: LeaveParticipation :one
-- Leave cashback (T080, FR-003).
--
-- A status and a date, never a delete: 0017's guard refuses the delete
-- outright, because the entries, payouts and evidence built on this row
-- outlive the preference by law and by accounting.
--
-- `status = 'active'` narrows this to the call that actually closes the
-- participation. A member who is already gone matches nothing, which is
-- what lets the caller tell "this request closed it" from "it was closed
-- before" - and it must be able to tell, because only the first of those
-- announces cashback.participation.ended. Answering the second from this
-- statement instead would publish a leaving event every time a client
-- retried a DELETE.
update cashback.participation
   set status = 'left',
       left_at = now()
 where account_id = sqlc.arg(account_id)
   and status = 'active'
returning account_id, brand_id, opted_in_at, terms_version, status, left_at, default_currency;
