-- Approval, publication and lifecycle queries (T020).
--
-- Approval IS the article insert (I-1): there is no draft state and no path
-- that creates an article without a named editor. The database enforces the
-- rules - the editor role by trigger, one article per non-withdrawn origin
-- by partial unique index - and these queries carry the intent to it. The
-- endpoint mirrors the verdicts as 403 and 409; it never pre-checks them,
-- because a pre-check that disagreed with the database would be worse than
-- no pre-check at all.

-- name: SourceItemTitle :one
-- The retrieved title of an untranslated origin, read inside the approval
-- transaction. An untranslated article renders source_item.original_title
-- as its headline, so approving an item whose feed provided no title would
-- publish a headline-less article; the contract rejects it with 400. No row
-- means the named origin does not exist.
select original_title
from source_item
where id = sqlc.arg(source_item_id)::uuid;

-- name: ApproveArticle :one
-- The approval. published_at is set in the same statement for the
-- publish-now path, so an immediately published article is one row write:
-- now() is the transaction timestamp, which is also approved_at's default,
-- so `article_published_after_approval` holds by construction.
insert into article (
    translation_id,
    source_item_id,
    approved_by,
    attribution_block,
    published_at
)
values (
    sqlc.narg(translation_id)::uuid,
    sqlc.narg(source_item_id)::uuid,
    sqlc.arg(approved_by)::uuid,
    sqlc.arg(attribution)::text,
    case when sqlc.arg(publish)::boolean then now() else null end
)
returning id, approved_by, approved_at, published_at;

-- name: PublishArticle :one
-- The one-way publish transition, expressed as a guarded UPDATE: the
-- `published_at is null` predicate means a second attempt matches no row
-- rather than racing the article_guard trigger into a 500. Two concurrent
-- callers therefore produce exactly one 200 and one 409, decided by the
-- database.
update article
   set published_at = now()
 where id = sqlc.arg(article_id)::uuid
   and published_at is null
returning id, approved_by, approved_at, published_at;

-- name: ArticleLifecycle :one
-- Where an article stands, for turning "the guarded update matched nothing"
-- into the right status code: no row at all is a 404, a row is a 409 whose
-- reason these two timestamps name.
select published_at, withdrawn_at
from article
where id = sqlc.arg(article_id)::uuid;

-- name: RecordDomainEvent :exec
-- Appends to the audit stream. Called inside the same transaction as the
-- write it describes, so the event and the fact it records commit together
-- or not at all.
insert into domain_event (type, payload)
values (sqlc.arg(type)::text, sqlc.arg(payload)::jsonb);
