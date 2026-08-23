-- 0009_account_tour_progress: where a person got to in each guided tour.
--
-- The tours walk editorial staff through the work — sign in, read the
-- queue, compare a translation against its original, approve — and a tour
-- that crosses four pages needs somewhere to keep its place between them.
--
-- ---------------------------------------------------------------------------
-- Why on `account` rather than in the browser
--
-- The browser was the first home for this, and it is the wrong one for a
-- product whose users are a handful of named people who will open it on a
-- laptop and then a phone. Progress in localStorage means the tour starts
-- over on the second device and vanishes when somebody clears site data,
-- and the person it restarts for is a co-founder who has already seen it.
--
-- It also makes an unanswerable question answerable: how far into the tour
-- people actually get. jsonb is queryable — `tour_progress ->> 'editor'`
-- groups as readily as a column would — so choosing a document here does
-- not trade away the reporting that a normalised table would give.
--
-- ---------------------------------------------------------------------------
-- Why a document and not a column per tour
--
-- There will be more tours than this one: the reader's first visit, place
-- selection, registration and consent. A column each means a migration
-- each, for a value that is a cursor into a list defined entirely in the
-- front end. The shape here is {tour_id: cursor}, where cursor is a step
-- index as text or the string 'done'.
--
-- The value is NOT interpreted by the database or by Go beyond being an
-- object. The front end owns what a cursor means, and it already refuses
-- to trust one: a tour that gained or lost steps since a cursor was
-- written leaves it pointing at nothing, so every value read back is
-- treated as a suggestion and an out-of-range one restarts the tour. That
-- check has to exist for localStorage regardless — this column is not
-- trusted any further than that one was.
--
-- Default '{}' rather than null: "no tours started" and "this column has
-- no value" are the same state, and one of them costs every reader a null
-- check for no gain.

alter table account
    add column tour_progress jsonb not null default '{}'::jsonb
        constraint account_tour_progress_is_object
            check (jsonb_typeof(tour_progress) = 'object');

comment on column account.tour_progress is
    'Where this person got to in each guided tour, as {tour_id: cursor}. The cursor is a step index as text, or ''done''. Written by the front end and not interpreted here beyond being an object: the tours are defined in the web app, and a cursor that no longer addresses a step is discarded on read rather than migrated.';
