-- Withdrawal query (T021, FR-016).
--
-- Withdrawal ends publication and preserves every record: the article row,
-- its approval, its attribution and the retrieved evidence all stay exactly
-- as they were (I-5). Nothing is deleted, and there is deliberately no way
-- to erase an approval.
--
-- The audit event is NOT written here. Migration 0002 writes
-- `article.withdrawn` by trigger, in the same transaction as the update, so
-- the record of who withdrew what and why cannot be lost to application
-- discipline - and must not be duplicated by it either.

-- name: WithdrawArticle :one
-- The one-way withdrawal transition and its verdict, in ONE statement.
--
-- The lifecycle rule is that only a published article can be withdrawn, and
-- only once. Attempting the write and then reading back why it did nothing
-- would answer from a second snapshot: an article published in between
-- those two statements reads as published-and-not-withdrawn, and the
-- refusal would be reported as "already withdrawn", which is simply untrue.
-- So the attempt and the classification are one statement over one locked
-- row.
--
-- `target` takes the row lock; under READ COMMITTED that also re-reads the
-- latest committed version, so the flags below describe the article as it
-- actually is, not as it was when the statement was planned. Holding the
-- lock is what makes them still true when the update runs, and what makes
-- two concurrent withdrawals produce exactly one 200 and one 409 without
-- either meeting article_guard's exception.
--
-- The three withdrawal columns move together, which is what
-- `article_withdrawal_all_or_none` requires: a partial withdrawal is
-- unrepresentable.
with target as (
    select id, published_at, withdrawn_at
      from article
     where id = sqlc.arg(article_id)::uuid
       for update
),
withdrawn as (
    update article
       set withdrawn_at = now(),
           withdrawn_by = sqlc.arg(withdrawn_by)::uuid,
           withdrawal_reason = sqlc.arg(reason)::text
     where id = (
        select id
          from target
         where published_at is not null
           and withdrawn_at is null
     )
    returning id, withdrawn_at, withdrawn_by, withdrawal_reason
)
-- No row at all means no such article. A row with was_published false is an
-- approval that was never released; with was_withdrawn true, one whose
-- publication has already ended. Otherwise the withdrawal happened and the
-- joined columns carry it.
select
    (t.published_at is not null)::boolean as was_published,
    (t.withdrawn_at is not null)::boolean as was_withdrawn,
    w.id                       as article_id,
    w.withdrawn_at,
    w.withdrawn_by,
    w.withdrawal_reason
from target t
left join withdrawn w on w.id = t.id;
