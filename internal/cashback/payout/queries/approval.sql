-- Approving a withdrawal: the payout row IS the approval (C-4), and its key
-- is the database's to derive (C-5).

-- name: LockWithdrawalRequestForDecision :one
-- Read one request for an operator to decide, holding it against a second
-- operator deciding the same one.
--
-- FOR UPDATE rather than the plain read GetWithdrawalRequestForAccount makes:
-- two operators can open the approval queue at the same instant, and the
-- second must wait here and then see the state the first left. Without it
-- both would read awaiting_approval and both would try to approve; C-5's
-- unique key would catch the second, but as a constraint violation in the
-- middle of a money movement rather than as an answer.
--
-- Not narrowed on an account. An operator acts on another member's request
-- by definition, and the authority for that is the operator role - checked by
-- the database in payout_insert_guard, not by a WHERE clause here.
select id, account_id, destination_id, amount_minor, currency, state,
       requested_at, reserved_transfer_ref, decided_by, decided_at,
       decision_reason
  from cashback.withdrawal_request
 where id = sqlc.arg(id)
   for update;

-- name: RecordWithdrawalDecision :one
-- Move a request out of the approval queue, recording who decided and when.
--
-- Narrowed on the state the caller read, so a request that has already been
-- decided matches nothing and its caller is told - the same conditional-write
-- shape MoveEntry uses, and for the same reason: two writes that both succeed
-- would leave one decision recorded and the other silently lost.
--
-- decided_at is now() rather than a value passed in, because
-- withdrawal_request_decided_after_request compares it against requested_at
-- and the database is the only clock both are read from.
update cashback.withdrawal_request
   set state = sqlc.arg(to_state),
       decided_by = sqlc.arg(decided_by),
       decided_at = now(),
       decision_reason = sqlc.narg(decision_reason)
 where id = sqlc.arg(id)
   and state = sqlc.arg(from_state)
returning id, account_id, destination_id, amount_minor, currency, state,
          requested_at, reserved_transfer_ref, decided_by, decided_at,
          decision_reason;

-- name: BrandOfReservedEntries :many
-- Which brand the entries this request reserved were earned under
-- (ADR-0004).
--
-- Read from the ENTRIES rather than from today's configuration, and that is
-- the whole point: a payout records which brand paid, payout_guard freezes
-- it, and a deployment that is rebranded between a member earning and being
-- paid must not have the payment attributed to the new one.
--
-- The seam is D9's reservation transfer, exactly as the provenance view uses
-- it (0016): the entries a payout pays are the entries whose reserving
-- transition carries the request's reserved_transfer_ref.
--
-- Returns the DISTINCT brands rather than one, so a caller sees a request
-- spanning two - which is not payable as one payout - instead of picking
-- whichever row came back first.
select distinct e.brand_id
  from cashback.entry_transition et
  join cashback.entry e on e.id = et.entry_id
 where et.ledger_transfer_ref = sqlc.arg(reserved_transfer_ref)
   and et.to_state = 'reserved'
 order by e.brand_id;

-- name: CreatePayout :one
-- Write the payout. The row IS the approval (C-4).
--
-- idempotency_key is absent from the column list because it is GENERATED
-- ALWAYS from request_id: the database derives it and a caller cannot supply
-- one (D8). It comes back in the RETURNING clause because that derived value
-- is what must be handed to the rail - computing the same string in Go would
-- be a second authority on the one thing C-5 rests on.
--
-- rail_reference is absent too. It does not exist yet: this row is written
-- BEFORE the rail is contacted, so that the unique key is claimed before any
-- money can move. A second approval then fails on 23505 rather than making a
-- second payment, which is the whole of C-5.
insert into cashback.payout (
    request_id, brand_id, approved_by, amount_minor, currency, rail
) values (
    sqlc.arg(request_id), sqlc.arg(brand_id), sqlc.arg(approved_by),
    sqlc.arg(amount_minor), sqlc.arg(currency), sqlc.arg(rail)
)
returning id, brand_id, request_id, approved_by, idempotency_key,
          amount_minor, currency, rail, rail_reference, state,
          submitted_at, settled_at;

-- name: RecordPayoutSubmission :one
-- Record what the rail gave back for a payout already written.
--
-- Narrowed on rail_reference being null, so recording a reference twice
-- matches nothing rather than overwriting the first. The first reference is
-- the one an auditor follows out of this system; a later write replacing it
-- would break that trail silently.
update cashback.payout
   set rail_reference = sqlc.arg(rail_reference)
 where id = sqlc.arg(id)
   and rail_reference is null
returning id, brand_id, request_id, approved_by, idempotency_key,
          amount_minor, currency, rail, rail_reference, state,
          submitted_at, settled_at;

-- name: GetPayoutForRequest :one
-- The payout a request already has, if it has one.
--
-- Asked after a 23505 on CreatePayout: the unique key did its job, and what
-- the operator needs to be told is which payout already exists rather than
-- that a constraint fired.
select id, brand_id, request_id, approved_by, idempotency_key,
       amount_minor, currency, rail, rail_reference, state,
       submitted_at, settled_at
  from cashback.payout
 where request_id = sqlc.arg(request_id);
