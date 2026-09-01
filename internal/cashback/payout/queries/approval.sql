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

-- name: RecordPayoutOutcome :one
-- Move a payout to where the rail says it ended up.
--
-- Narrowed on the state the caller read, so two retries racing one payout
-- leave one recorded outcome rather than the second overwriting the first -
-- the same conditional-write shape every decision in this schema uses.
--
-- settled_at is now() for a settlement and null otherwise, computed here
-- rather than passed in because payout_settled_iff_settlement_time ties the
-- two together: a settled payout without an instant, or an unsettled one
-- with, is a row the database refuses. One statement decides both, so they
-- cannot disagree.
--
-- payout_guard permits exactly this and nothing else: the state, the
-- settlement instant and the rail's reference may move; who approved it,
-- what it pays and on which rail may not, and a settled payout is terminal.
update cashback.payout
   set state = sqlc.arg(to_state),
       settled_at = case when sqlc.arg(to_state)::text = 'settled' then now() else null end,
       rail_reference = coalesce(sqlc.narg(rail_reference), rail_reference)
 where id = sqlc.arg(id)
   and state = sqlc.arg(from_state)
returning id, brand_id, request_id, approved_by, idempotency_key,
          amount_minor, currency, rail, rail_reference, state,
          submitted_at, settled_at;

-- name: LockPayoutForRequest :one
-- The payout a request has, held against a second retry of the same one.
--
-- FOR UPDATE because a retry both reads the payout's state and decides what
-- to do about it, and two retries arriving together must not both conclude
-- that the rail was never asked. The withdrawal request is locked by the
-- caller first, so the order is always request then payout and there is no
-- pair of locks to take the wrong way round.
select id, brand_id, request_id, approved_by, idempotency_key,
       amount_minor, currency, rail, rail_reference, state,
       submitted_at, settled_at
  from cashback.payout
 where request_id = sqlc.arg(request_id)
   for update;

-- name: MarkWithdrawalPaid :one
-- Move a request to paid, and touch NOTHING else.
--
-- Deliberately not RecordWithdrawalDecision, which sets decided_at to now().
-- A settlement is an OBSERVATION, not a decision: the human decided when
-- they approved, and overwriting their instant with a sweep's clock would
-- lose the one fact C-4 exists to keep. The approval already set decided_by
-- and decided_at, so withdrawal_request_decided_before_leaving_the_queue and
-- withdrawal_request_decision_all_or_none stay satisfied without writing
-- either again.
--
-- Narrowed on approved for the reason every other decision here is: a
-- request already paid, refused or failed matches nothing and its caller is
-- told, rather than two writes both succeeding and one being lost.
update cashback.withdrawal_request
   set state = 'paid'
 where id = sqlc.arg(id)
   and state = 'approved'
returning id, account_id, destination_id, amount_minor, currency, state,
          requested_at, reserved_transfer_ref, decided_by, decided_at,
          decision_reason;

-- name: SubmittedPayouts :many
-- Every payment the rail has taken and not finished, oldest first.
--
-- The sweep's work list. It returns the REQUEST ids rather than the payout
-- rows, because settling one re-reads and locks it anyway (a row read here
-- and acted on later is a row that may have moved in between), and because
-- the request is what the rest of the settlement path is keyed on.
--
-- Oldest first so a backlog drains in the order members have been waiting,
-- and a payment that is stuck stays at the front where it is noticed rather
-- than being lapped by newer ones.
select request_id
  from cashback.payout
 where state = 'submitted'
 order by submitted_at;
