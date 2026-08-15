-- 0004_withdrawn_origin_index: indexes shaped like the correction lookup.
--
-- 0002 made the one-per-origin uniqueness partial on `withdrawn_at IS NULL`,
-- which is exactly right for the rule it enforces - withdrawal frees an
-- origin for a corrected re-approval - but it leaves the complementary set,
-- the withdrawn rows, with no origin index at all.
--
-- That set is not cold data. The review queue reads it on every page: for
-- the origins it lists it fetches their withdrawal history, which is how a
-- correction candidate is told apart from a first approval, and that lookup
-- filters `withdrawn_at IS NOT NULL` by origin. Without these indexes it
-- degrades to a sequential scan of article that grows with every withdrawal
-- ever recorded - and withdrawals are never deleted (I-5), so it only ever
-- grows.
--
-- Two indexes rather than one because an article has exactly one origin
-- column set (article_exactly_one_origin), so each stays as small as the
-- history it actually covers, and the query's OR over the two columns can
-- be answered by a bitmap OR of both.
--
-- Nothing here changes semantics: an index adds no constraint and relaxes
-- none.

create index article_withdrawn_per_translation
    on article (translation_id)
    where translation_id is not null and withdrawn_at is not null;

comment on index article_withdrawn_per_translation is
    'Backs the review queue''s correction lookup: the withdrawal history of a translated origin. Complements article_one_per_translation, which covers the non-withdrawn rows only.';

create index article_withdrawn_per_source_item
    on article (source_item_id)
    where source_item_id is not null and withdrawn_at is not null;

comment on index article_withdrawn_per_source_item is
    'Backs the review queue''s correction lookup: the withdrawal history of an untranslated origin. Complements article_one_per_source_item, which covers the non-withdrawn rows only.';
