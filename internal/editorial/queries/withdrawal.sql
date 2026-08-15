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
-- The one-way withdrawal transition, expressed as a guarded UPDATE. The two
-- predicates carry the whole lifecycle rule: only a published article can be
-- withdrawn, and only once. Expressing them here rather than as a read
-- followed by a write means two concurrent withdrawals produce exactly one
-- 200 and one 409, decided by the database, and neither ever meets
-- article_guard's exception - the losing statement simply matches no row.
--
-- The three withdrawal columns move together, which is what
-- `article_withdrawal_all_or_none` requires: a partial withdrawal is
-- unrepresentable.
update article
   set withdrawn_at = now(),
       withdrawn_by = sqlc.arg(withdrawn_by)::uuid,
       withdrawal_reason = sqlc.arg(reason)::text
 where id = sqlc.arg(article_id)::uuid
   and published_at is not null
   and withdrawn_at is null
returning id, withdrawn_at, withdrawn_by, withdrawal_reason;
