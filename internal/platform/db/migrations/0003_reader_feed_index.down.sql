-- Reverses 0003: drops the reader feed index. The feed keeps working -
-- article_published_at_idx (0001) still covers the leading column - it just
-- sorts again.

drop index article_visible_feed_idx;
