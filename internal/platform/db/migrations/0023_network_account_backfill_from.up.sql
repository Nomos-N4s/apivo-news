-- 0023_network_account_backfill_from: where a publisher account's history
-- starts, kept beside the two cursors it seeds.
--
-- The poller reads one window at a time and advances a durable cursor over
-- it (FR-031). An account nobody has polled has no cursor, so the first
-- window has to start somewhere, and nothing in the system can work out
-- where: too recent silently skips history nobody notices is missing, too
-- old asks a network for years of it. Only the operator who connected the
-- account knows.
--
-- It is a COLUMN rather than process configuration, and the reason is the
-- trailing sweep rather than the forward one. The re-read walks from this
-- instant until its own cursor is first written, which cannot happen until
-- the forward cursor has run a whole lag ahead of it - about a hundred days
-- (ADR-0003). For that whole period the value is live state, not a seed. A
-- process-global one would then be a single value for every account this
-- process polls: connecting a second account and setting the key for it
-- would move the FIRST account's re-read floor forward, and the span
-- between the two starts would never be re-read. The trailing sweep is the
-- only path from pending to confirmed, so everything in that span would sit
-- pending forever, with nothing logged and nothing wrong anywhere.
--
-- Nullable, and deliberately so. It is not "unset means now": an account
-- with no start is one nobody has finished connecting, and the poller
-- refuses it by name rather than inventing a first window. Existing rows
-- get null for exactly that reason.
alter table cashback.network_account
    add column backfill_from timestamptz;

comment on column cashback.network_account.backfill_from is
    'The instant this account''s first poll reads from, and the floor the trailing re-read walks from until its own cursor exists. Set by the operator who connects the account; NULL means nobody has said, and the poller refuses to guess (FR-031, ADR-0003).';

-- A start after the cursor would be a floor the forward sweep has already
-- walked past, which says the two disagree about what has been read. It is
-- only checkable against the cursor - "not in the future" cannot be a check
-- constraint, because now() is not immutable - so the poller refuses that
-- half itself, at the one moment it has a clock.
alter table cashback.network_account
    add constraint network_account_backfill_from_not_ahead
        check (
            backfill_from is null
            or cursor_at is null
            or backfill_from <= cursor_at
        );
