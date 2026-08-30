-- The click store (T063, FR-013, FR-020..023): the tracked redirect as a
-- row, and the way a reported reference finds its way back to one.
--
-- Two statements, and between them they carry the whole of what a later
-- credit rests on. The row is append-only (0012), so neither of them ever
-- updates: a click is what happened at one instant, and the rate the member
-- was promised at that instant is snapshotted onto it rather than read from
-- the offer as it stands when the money is finally paid (FR-013).

-- name: InsertClick :one
-- Record one click, before its redirect is issued (FR-020).
--
-- clicked_at is the column's own default rather than a supplied instant, for
-- the reason detected_at is on the unattributed queue: the row's clock is
-- what an auditor reads, and a caller's would be a second answer to when
-- this happened. It is returned, so the caller reports the instant the row
-- carries and not the one it guessed.
--
-- account_id has no default and no null to fall back on. That is FR-023 in
-- the schema: an anonymous click is unrepresentable, so a click can never
-- later be adopted by an account that was not the one that made it.
--
-- The unique constraint on click_ref is deliberately NOT swallowed here. A
-- collision at 128 bits is not something to retry past quietly - it means
-- either the entropy source is broken or a caller is re-using a reference,
-- and both are defects that must surface rather than cost a member the
-- credit whose reference was taken.
insert into cashback.click (
    click_ref, account_id, offer_id,
    rate_snapshot, member_share_bps_snapshot, context_digest
)
values (
    sqlc.arg(click_ref), sqlc.arg(account_id), sqlc.arg(offer_id),
    sqlc.arg(rate_snapshot), sqlc.arg(member_share_bps_snapshot), sqlc.narg(context_digest)
)
returning id, click_ref, account_id, offer_id, clicked_at,
          rate_snapshot, member_share_bps_snapshot, context_digest;

-- name: GetClickByRef :one
-- The click a reported reference names, or no row.
--
-- This is the attribution join, seen from the click's side: a network echoes
-- back the reference it was given, and the credit that follows may only rest
-- on the click that reference belongs to (C-2).
--
-- Matched EXACTLY, with no normalisation of any kind. No trimming, no case
-- folding, no unescaping. Every one of those would widen the set of network
-- strings that resolve to a member's click, and the reference is the only
-- thing standing between one member's purchase and another's credit - so a
-- reference that does not match this row byte for byte is not this click's,
-- whatever it looks like. A reported reference that matches nothing is
-- ordinary and is the caller's to queue as unattributed (FR-034).
select id, click_ref, account_id, offer_id, clicked_at,
       rate_snapshot, member_share_bps_snapshot, context_digest
  from cashback.click
 where click_ref = sqlc.arg(click_ref);
