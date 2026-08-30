-- Publisher account cursor queries (T055). Where a poller has got to, and
-- the one way it is allowed to say it has got further.
--
-- Two cursors, and the schema already states how they relate (0011): the
-- trailing cursor walks a window that has ALREADY been read, so it cannot
-- exist before the main cursor does and cannot run ahead of it. Ahead means
-- a restart advanced the wrong one, which is how a window gets skipped.
--
-- Both advances below are CONDITIONAL on the cursor still being where the
-- caller read it. That is not defensive coding, it is the whole durability
-- argument: a window is persisted and its cursor advanced in ONE
-- transaction, so a crash rolls both back together and the window is simply
-- read again (FR-031). Two pollers racing would otherwise both commit,
-- and the second would move the cursor past a window only the first had
-- written - which is a window skipped, silently, forever.

-- name: GetNetworkAccountCursors :one
-- Where this account has got to, where it started, and the identity a poll
-- needs to build its adapter's retrieval facts.
--
-- backfill_from comes back on every poll rather than only on the first,
-- because BOTH sweeps read it: the forward one until its cursor exists, and
-- the trailing one until its own does - which is about a hundred days later
-- (0023). Locked FOR UPDATE, because the caller is about
-- to read a window and advance a cursor on the strength of what it says: an
-- unlocked read would let a second poller pick the same window, and both
-- would then find their conditional advance rejected after doing all the
-- work of fetching it.
select
    na.id,
    na.network_id,
    na.external_publisher_id,
    na.cursor_at,
    na.trailing_cursor_at,
    na.backfill_from,
    na.active
from cashback.network_account na
where na.id = sqlc.arg(id)
for update;

-- name: AdvanceNetworkAccountCursor :one
-- Move the main cursor, but only from where the caller found it.
--
-- `is not distinct from` rather than `=`, because the first advance moves it
-- from NULL and `null = null` is null, not true - so an `=` comparison would
-- silently match nothing and every first poll would report a lost race.
--
-- No rows back means somebody else moved it first. That is a lost race
-- rather than a failure: the window this caller read has been read by
-- another poller, and the right response is to stop and let it be.
update cashback.network_account
set cursor_at = sqlc.arg(advance_to)
where id = sqlc.arg(id)
  and cursor_at is not distinct from sqlc.narg(advance_from)
returning cursor_at, trailing_cursor_at;

-- name: AdvanceNetworkAccountTrailingCursor :one
-- Move the trailing cursor the same way, under the same condition.
--
-- The schema refuses a value ahead of the main cursor, so this statement
-- carries no such check of its own: restating a constraint in a WHERE clause
-- produces a silent no-row answer where the database would have raised a
-- named refusal, and the two would then have to be kept in step forever.
update cashback.network_account
set trailing_cursor_at = sqlc.arg(advance_to)
where id = sqlc.arg(id)
  and trailing_cursor_at is not distinct from sqlc.narg(advance_from)
returning cursor_at, trailing_cursor_at;

-- name: GetNetworkAccountByPublisher :one
-- The account row an adapter's configuration names, found by the natural key
-- an operator actually holds.
--
-- The composition root is handed a network and a publisher identifier - the
-- two an operator reads off the network's own dashboard - and the poller
-- needs the row id, because that is what owns the cursors and what every
-- evidence row carries in network_account_id. Nothing but the database can
-- make that translation, and it is made ONCE at wiring rather than at every
-- poll: an id that changed between polls would move a cursor belonging to
-- another account.
--
-- No FOR UPDATE. This read decides whether to register a job, not what to
-- write, and holding a row lock for the life of the process would block
-- every poll the job then makes.
--
-- `active` and `backfill_from` come back so the caller can say what it
-- found. The poller refuses an inactive account and an account with no start
-- on its own - 0011 makes an account born inactive precisely so a
-- half-configured one cannot fetch, and 0023 leaves the start null until an
-- operator sets it - so both are for the log line at startup rather than for
-- a decision here.
select
    na.id,
    na.network_id,
    na.external_publisher_id,
    na.backfill_from,
    na.active
from cashback.network_account na
where na.network_id = sqlc.arg(network_id)
  and na.external_publisher_id = sqlc.arg(external_publisher_id);
