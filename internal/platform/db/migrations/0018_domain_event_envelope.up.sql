-- 0018_domain_event_envelope: routing fields on the shared event stream.
--
-- domain_event is (id, type, payload, occurred_at) and carries none of the
-- routing the cross-product event contract needs. Cashback and news
-- communicate only through this stream (constitution, "Products"), so the
-- envelope has to say which producer wrote an event, which schema version
-- it is in, which subject it concerns, and whether it has been seen before.
--
-- The table is APPEND ONLY, protected by row-level and statement-level
-- triggers since 0001. That is not relaxed here, not even briefly. Two
-- properties of ADD COLUMN make it unnecessary:
--
--   * a column added with a CONSTANT default does not rewrite the table
--     (PostgreSQL 11 and later): the default is recorded in the catalog
--     and materialised as rows are read, so no existing row is touched;
--   * DDL does not fire row-level triggers in any case, so even the
--     scanning form could not trip the immutability guards.
--
-- The consequence worth stating plainly: there is NO BACKFILL here, and no
-- UPDATE statement anywhere in this migration. Every pre-existing row keeps
-- its physical identity, and append-only holds continuously - before,
-- during and after.
--
-- The defaults are chosen so the two existing writers keep working
-- UNMODIFIED (internal/ingestion/store.go and
-- internal/editorial/queries/approval.sql, both of which insert only type
-- and payload): version 1 is the schema they write, and 'news' is the
-- producer they belong to, which is also correct for every row already in
-- the table.

alter table domain_event
    add column version integer not null default 1
        constraint domain_event_version_positive check (version > 0),
    add column producer text not null default 'news'
        constraint domain_event_producer_not_blank check (btrim(producer) <> ''),
    add column subject uuid,
    add column idempotency_key text
        constraint domain_event_idempotency_key_not_blank
            check (idempotency_key is null or btrim(idempotency_key) <> '');

comment on column domain_event.version is
    'The schema version of payload for this event type. Consumers read it before the payload, so a producer can change shape without a coordinated deploy.';
comment on column domain_event.producer is
    'Which product domain emitted this event. Defaults to news, which is correct for every row written before this migration and for both existing writers.';
comment on column domain_event.subject is
    'The entity this event is about. Per-subject ordering is the ONLY ordering the stream guarantees - consumers that assume a global order are relying on something this table does not promise.';
comment on column domain_event.idempotency_key is
    'A producer-chosen key making redelivery a no-op: a second attempt to append the same event violates the partial unique index below instead of duplicating it. Nullable, because the events written before this migration had no such key and no delivery to be idempotent about.';

-- Scoped by producer, and partial on the key.
--
-- Producer scoping matters because this migration's whole purpose is to
-- make the stream carry more than one producer. An idempotency key is
-- described as producer-chosen, so two products picking the same string -
-- and "entry.credited:2026-08" is not an exotic choice - would block each
-- other's appends with a unique violation that looks exactly like a
-- redelivery. Each producer's key space is its own; that is the only
-- scoping that matches who chooses the value.
--
-- Partial, so the pre-existing rows and every writer that does not need
-- idempotency are simply not in the index. Redelivery then fails with
-- 23505, which a consumer treats as "already accepted" rather than as an
-- error - at-least-once delivery made harmless (D10).
create unique index domain_event_producer_idempotency_key_unique
    on domain_event (producer, idempotency_key)
    where idempotency_key is not null;

-- Per-subject ordering is what the stream guarantees, so it is what the
-- index serves: the whole history of one subject, in order, without
-- scanning the stream.
create index domain_event_subject_idx on domain_event (subject, occurred_at)
    where subject is not null;
