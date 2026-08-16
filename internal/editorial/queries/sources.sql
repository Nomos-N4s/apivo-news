-- Source-list queries (#86).
--
-- The sources screen makes the licensing invariant visible: which feeds
-- the crawler polls, under which CURRENT terms, and how the last poll of
-- each went. Current is the operative word - this list reads the mutable
-- source row and says so, because the legal basis of anything already
-- retrieved is the snapshot on source_item (I-4), which the provenance
-- endpoint serves and this one deliberately does not.

-- name: ListSources :many
-- One page of registered sources: newest registration first, keyset-
-- paginated on (created_at, id) descending - the queue's pagination shape
-- on this table's own timestamp. Offset pagination is not used here either,
-- for the same reason as the queue: registrations arrive while an operator
-- pages, and an offset would silently skip or repeat rows.
--
-- The active filter is a tri-state narg: absent lists everything, true the
-- polled feeds, false the paused ones - pausing is the operator's one
-- switch (0002's source.active) and this list is the only place it can be
-- observed.
--
-- The cursor is atomic for the queue's reason: a row comparison against a
-- NULL half yields UNKNOWN and would filter every row out. The endpoint
-- rejects malformed cursors with 400 before reaching here.
--
-- No index backs (created_at, id): the table is bounded by hand-registered
-- feeds, so a scan is cheaper than the index would ever pay back.
select
    id,
    name,
    url,
    language_code,
    jurisdiction,
    licence_terms,
    usage_rule,
    permission_evidence,
    active,
    last_polled_at,
    created_at
from source
where (
        sqlc.narg(active)::boolean is null
        or active = sqlc.narg(active)::boolean
  )
  and (
        sqlc.narg(cursor_created_at)::timestamptz is null
        or sqlc.narg(cursor_row_id)::uuid is null
        or (created_at, id)
            < (sqlc.narg(cursor_created_at)::timestamptz, sqlc.narg(cursor_row_id)::uuid)
  )
order by created_at desc, id desc
limit sqlc.arg(row_limit);

-- name: UpdateSource :one
-- One PATCH as one statement (#118). Each narg is "not supplied": coalesce
-- keeps the current value, so any subset of name, url, active and
-- licence_terms updates without a per-combination query.
--
-- The pre-image comes from a LOCKING read, and must. Under READ COMMITTED
-- an UPDATE that blocks on a concurrent committed update re-fetches its
-- target row when it resumes (EvalPlanQual), but an ordinary join scan of
-- the same table keeps answering from the statement's ORIGINAL snapshot -
-- precisely the snapshot the race made stale. A pre-image read that way
-- is the value from before the wait rather than the value the write
-- actually replaced: the intervening version disappears from an
-- append-only stream, and a patch restating what another editor just set
-- looks like an edit. `for update` waits for that writer and then reads
-- the latest committed row, so old and new are the true adjacent pair.
--
-- No row answers pgx.ErrNoRows, which the store maps to ErrSourceNotFound;
-- a url colliding with another registration raises source_url_key, mapped
-- to ErrDuplicateSourceURL like the registration path.
with old as (
    select id, name, url, active, licence_terms
      from source
     where id = sqlc.arg(id)::uuid
       for update
)
update source
   set name = coalesce(sqlc.narg(name)::text, source.name),
       url = coalesce(sqlc.narg(url)::text, source.url),
       active = coalesce(sqlc.narg(active)::boolean, source.active),
       licence_terms = coalesce(sqlc.narg(licence_terms)::text, source.licence_terms)
  from old
 where source.id = old.id
returning
    source.id,
    source.name,
    source.url,
    source.language_code,
    source.jurisdiction,
    source.licence_terms,
    source.usage_rule,
    source.permission_evidence,
    source.active,
    source.last_polled_at,
    source.created_at,
    old.name as old_name,
    old.url as old_url,
    old.active as old_active,
    old.licence_terms as old_licence_terms;

-- name: DeleteSource :execrows
-- Deletion is refused by the database wherever evidence exists: the
-- source_item FK carries no ON DELETE clause, so a source with retrieved
-- items raises 23503 and the store turns that verdict into the 409 naming
-- the evidence count. Zero rows deleted means no such source.
delete from source where id = sqlc.arg(id)::uuid;

-- name: CountSourceEvidence :one
-- How many retrieved items hold this source in the provenance chain - the
-- honest figure the delete refusal names.
select count(*) from source_item where source_id = sqlc.arg(source_id)::uuid;

-- name: LastPollCycle :one
-- The last poll cycle as the poll state records it (0007): how much the
-- last poll of each ACTIVE source retrieved, how much the content
-- fingerprint deduplicated (FR-014), and which feeds failed, by name.
-- Summing per-source last-poll counters is what makes the screen's cycle
-- numbers real readings rather than invented figures; paused sources are
-- excluded because their state describes a poll no longer running.
--
-- The failure list is ordered by name so the same broken feeds read the
-- same way on every refresh.
select
    coalesce(sum(last_poll_retrieved), 0)::bigint as retrieved,
    coalesce(sum(last_poll_duplicates), 0)::bigint as duplicates,
    coalesce(
        array_agg(name order by name) filter (where last_poll_error is not null),
        '{}'
    )::text[] as failures
from source
where active;
