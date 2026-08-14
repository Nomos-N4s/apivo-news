-- name: GetArticleProvenance :one
-- I-5: full provenance of one article - source, licence snapshot at
-- retrieval, model, prompt version and named approver - in a single query.
select * from article_provenance
where article_id = $1;

-- name: ListPublishedArticles :many
-- Published-and-visible (FR-016): withdrawal ends publication, so
-- withdrawn articles never reach readers.
select * from article
where published_at is not null and withdrawn_at is null
order by published_at desc
limit $1;
