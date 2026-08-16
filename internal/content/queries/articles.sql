-- name: ListPublishedArticles :many
-- Published-and-visible (FR-016): withdrawal ends publication, so
-- withdrawn articles never reach readers.
select * from article
where published_at is not null and withdrawn_at is null
order by published_at desc
limit $1;
