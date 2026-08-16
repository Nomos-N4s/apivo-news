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
