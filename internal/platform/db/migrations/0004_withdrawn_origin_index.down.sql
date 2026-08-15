-- Reverses 0004: drops the withdrawn-origin indexes. Nothing breaks - the
-- review queue's correction lookup still returns the same rows, it just
-- scans article to find them.

drop index article_withdrawn_per_source_item;
drop index article_withdrawn_per_translation;
