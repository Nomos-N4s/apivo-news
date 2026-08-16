-- Reverses 0007_source_poll_state. The dropped columns are operational
-- state only, rewritten on every poll of the source, so nothing is lost
-- that the next cycle would not recreate: the first poll after re-applying
-- runs unconditionally (no stored validators) and refills every value. The
-- retrieval record in source_item and domain_event is untouched.

alter table source
    drop column etag,
    drop column last_modified,
    drop column last_polled_at,
    drop column last_poll_error,
    drop column last_poll_retrieved,
    drop column last_poll_duplicates;
