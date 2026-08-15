-- 0003_reader_feed_index: an index shaped like the reader feed query.
--
-- The front page orders by (published_at, id) descending and reads only
-- published-and-visible rows, paging by keyset rather than offset. The
-- existing article_published_at_idx (0001) covers the leading column alone,
-- so the tie-break column and the withdrawal predicate still cost a sort
-- and a recheck. This index matches the query exactly - both ordering
-- columns, and the same partial predicate - so the feed and every keyset
-- page after it read straight from the index in order.
--
-- Nothing here changes semantics: an index adds no constraint and relaxes
-- none. Published-and-visible remains
-- `published_at IS NOT NULL AND withdrawn_at IS NULL` (FR-016), stated once
-- more here so the index can never drift from the rule it serves.

create index article_visible_feed_idx
    on article (published_at desc, id desc)
    where published_at is not null and withdrawn_at is null;

comment on index article_visible_feed_idx is
    'Backs the reader front page: keyset pagination on (published_at, id) descending over published-and-visible articles only.';
