-- 0021_event_deliveries: durable delivery tracking for event subscribers.
--
-- The dispatcher in internal/platform/events reads the append-only
-- domain_event stream and delivers it at-least-once
-- (contracts/events.md). Until now everything it remembered between polls
-- lived in process memory - the right posture for the stream itself, whose
-- idempotent handlers absorb a replay, but it leaves three facts with
-- nowhere durable to live:
--
--   * where each subscriber has read to, so a restart resumes instead of
--     replaying the whole stream through every handler;
--   * which deliveries have completed. This is the delivery idempotency
--     key of the event contract, keyed (subscriber, event): a process
--     that handled an event and crashed before advancing its checkpoint
--     redelivers into a recorded no-op instead of a second side effect;
--   * which deliveries have failed for good. Consumer rule 4: a handler
--     that fails after its retry budget moves the delivery to a
--     dead-letter table that an operator queue surfaces. A silently
--     dropped event is a defect.
--
-- All three tables are platform infrastructure in public, beside the
-- domain_event stream they track. They are deliberately NOT granted to
-- cashback_domain: a product appends to the stream and handles events in
-- process, while reading and writing this bookkeeping is the platform
-- dispatcher's job alone - a handler that could edit its own delivery
-- record could also unmake the no-op guarantee it depends on.

------------------------------------------------------------------------------
-- Where each subscriber has read to.
------------------------------------------------------------------------------

create table subscriber_checkpoint (
    subscriber text primary key
        constraint subscriber_checkpoint_subscriber_not_blank check (btrim(subscriber) <> ''),
    occurred_at timestamptz not null,
    event_id uuid not null,
    updated_at timestamptz not null default now()
);

comment on table subscriber_checkpoint is
    'How far each named subscriber has read the domain_event stream, as a position in its (occurred_at, id) order. Losing a row is safe by design: delivery is at-least-once, the subscriber re-reads from the start and event_delivery absorbs the replay - so this table is a resumption optimisation, never the correctness record.';
comment on column subscriber_checkpoint.subscriber is
    'The subscriber this position belongs to. Positions are per subscriber on purpose: one slow or blocked subscriber must not hold another back.';
comment on column subscriber_checkpoint.occurred_at is
    'The time axis of the position: the occurred_at of the last event this subscriber''s contiguous prefix covers.';
comment on column subscriber_checkpoint.event_id is
    'The id breaking occurred_at ties, completing the position. Deliberately NOT a foreign key to domain_event: this is a cursor, not a reference - a replay resets it and a fresh subscriber starts before the first event, neither of which names a row.';
comment on column subscriber_checkpoint.updated_at is
    'When the position last advanced. Operational visibility only: a subscriber whose position has not moved while the stream has is stuck.';

------------------------------------------------------------------------------
-- The delivery idempotency key: completed deliveries.
------------------------------------------------------------------------------

create table event_delivery (
    subscriber text not null
        constraint event_delivery_subscriber_not_blank check (btrim(subscriber) <> ''),
    event_id uuid not null references domain_event (id),
    attempts integer not null
        constraint event_delivery_attempts_positive check (attempts > 0),
    delivered_at timestamptz not null default now(),
    constraint event_delivery_pkey primary key (subscriber, event_id)
);

comment on table event_delivery is
    'IMMUTABLE. One row per completed delivery of one event to one named subscriber - the delivery idempotency key of contracts/events.md. The primary key is the guarantee: a redelivery finds the row and becomes a recorded no-op instead of a second side effect. The row is written AFTER the handler returns, so a crash between the two redelivers the event; closing that window entirely needs the handler''s own idempotence (consumer rule 1), which this table narrows but can never replace.';
comment on column event_delivery.subscriber is
    'The named subscriber this delivery completed for. Each subscriber records its own deliveries: the same event is delivered once to every subscriber of its type, independently.';
comment on column event_delivery.event_id is
    'The delivered event. The foreign key can hold because domain_event is append-only: a delivery of an event that never happened is unrepresentable.';
comment on column event_delivery.attempts is
    'How many handler attempts the completed delivery took, the dead-lettered ones included when an operator requeue resolved it. Attempts spent in a run that crashed before completing are not in the count - only the run that completed records.';
comment on column event_delivery.delivered_at is
    'When the delivery completed. Together with attempts, the recorded outcome consumer rule 1 asks handlers to keep.';

-- The record that makes redelivery a no-op must not be quietly unmade or
-- forged: an edited row would either replay a side effect or fake a
-- delivery that never ran. Same guard as domain_event itself (0001).
create trigger event_delivery_immutable
    before update or delete on event_delivery
    for each row execute function raise_immutable();
create trigger event_delivery_no_truncate
    before truncate on event_delivery
    for each statement execute function raise_immutable();

------------------------------------------------------------------------------
-- The dead-letter table: deliveries that spent their retry budget.
------------------------------------------------------------------------------

create table event_dead_letter (
    subscriber text not null
        constraint event_dead_letter_subscriber_not_blank check (btrim(subscriber) <> ''),
    event_id uuid not null references domain_event (id),
    event_type text not null
        constraint event_dead_letter_event_type_not_blank check (btrim(event_type) <> ''),
    subject uuid,
    occurred_at timestamptz not null,
    attempts integer not null
        constraint event_dead_letter_attempts_positive check (attempts > 0),
    last_error text not null
        constraint event_dead_letter_last_error_not_blank check (btrim(last_error) <> ''),
    parked_at timestamptz not null default now(),
    requeued_at timestamptz,
    constraint event_dead_letter_pkey primary key (subscriber, event_id)
);

comment on table event_dead_letter is
    'Deliveries parked after spending their retry budget - consumer rule 4''s dead-letter table, and the operator queue that surfaces it. A row here blocks its own (type, subject) lane for its subscriber until an operator requeues it, because delivering a later event of the same subject past a parked earlier one would reorder the subject''s history. Rows are resolved by deletion once a requeued delivery completes: this table is the open operator queue, not an audit log - the durable outcome lands in event_delivery and the audit stream stays domain_event (data-model 2.9).';
comment on column event_dead_letter.subscriber is
    'The subscriber whose delivery is parked. The same event may be parked for one subscriber and long delivered for another.';
comment on column event_dead_letter.event_id is
    'The event whose delivery is parked. It stays in the stream untouched; this row is about one subscriber''s failure to consume it.';
comment on column event_dead_letter.event_type is
    'The event''s type, copied from the stream so the blocked (type, subject) lane can be read without a join on every poll.';
comment on column event_dead_letter.subject is
    'The event''s subject, copied from the stream; null for a subjectless event. With event_type it names the lane this row blocks.';
comment on column event_dead_letter.occurred_at is
    'The event''s stream position (with event_id), so requeued deliveries are re-attempted in stream order.';
comment on column event_dead_letter.attempts is
    'Total handler attempts so far, across the original delivery and every requeue that failed again.';
comment on column event_dead_letter.last_error is
    'What the final attempt reported - the first thing an operator reads when deciding what to do with the row.';
comment on column event_dead_letter.parked_at is
    'When the delivery (last) entered the queue. A requeue that fails again re-parks and refreshes this.';
comment on column event_dead_letter.requeued_at is
    'When an operator asked for redelivery; null while the row waits for one. A requeued row no longer blocks its lane query, because the next dispatcher pass re-attempts it before touching anything behind it.';

-- At most one parked head per (subscriber, type, subject) lane, enforced by
-- the database rather than by the dispatcher remembering it: only the first
-- failure on a lane parks - everything behind it is held, not attempted -
-- and two dispatchers racing past each other cannot park a second head.
-- The coalesce folds the null subject into a lane key of its own; the nil
-- uuid is unreachable as a real subject because the outbox writer stores an
-- absent subject as null, never as zeros.
create unique index event_dead_letter_one_parked_per_lane
    on event_dead_letter (subscriber, event_type, coalesce(subject, '00000000-0000-0000-0000-000000000000'::uuid))
    where requeued_at is null;
