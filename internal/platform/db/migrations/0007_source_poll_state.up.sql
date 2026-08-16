-- 0007_source_poll_state: the poll loop's memory, on the source row.
--
-- T013 (#16): the feed poll loop asks each source conditionally, so it must
-- remember the validators the source last handed out; and the sources list
-- (#86) shows an operator how each source's last poll went, so the outcome
-- lives where that endpoint will read it. Every column here is operational
-- state, overwritten on the next poll of the source - none of it is
-- evidence. The retrieval record itself stays where the invariants put it:
-- source_item holds what was retrieved and when (I-2, I-3), domain_event
-- holds the item.retrieved trail, and nothing here duplicates either.

alter table source
    add column etag text not null default '',
    add column last_modified text not null default '',
    add column last_polled_at timestamptz,
    add column last_poll_error text,
    add column last_poll_retrieved integer not null default 0,
    add column last_poll_duplicates integer not null default 0,
    add column next_poll_not_before timestamptz;

comment on column source.etag is
    'The source''s own ETag from its last answer, sent back as If-None-Match on the next conditional GET. Empty when the source has never stated one.';
comment on column source.last_modified is
    'The source''s own Last-Modified from its last answer, carried verbatim as an opaque token and sent back as If-Modified-Since on the next conditional GET. Empty when the source has never stated one.';
comment on column source.last_polled_at is
    'When the poll loop last completed an attempt on this source, success or failure alike. Null until the first poll.';
comment on column source.last_poll_error is
    'Why the last poll of this source failed, URLs already redacted by the fetcher; null when it succeeded. Overwritten each cycle.';
comment on column source.last_poll_retrieved is
    'How many new items the last poll stored, overwritten each cycle. The last poll''s outcome only - history lives in domain_event/source_item, not here.';
comment on column source.last_poll_duplicates is
    'How many items the last poll recognised as already on record, overwritten each cycle. The last poll''s outcome only - history lives in domain_event/source_item, not here.';
comment on column source.next_poll_not_before is
    'When a Retry-After the source asked for expires: no replica polls this source before it. Written from the source''s own ask on a rate limit, cleared by the next completed attempt; null when the source has no standing ask.';
