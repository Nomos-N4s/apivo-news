-- Reverses 0026_network_rate_limit_per_minute.
--
-- LOSSY, and unavoidably so: that is the whole reason the column moved.
-- A rate below sixty a minute is less than one a second, and a per-second
-- integer constrained above zero cannot hold it - so those rows come back
-- as 1, which is FASTER than the network permits rather than slower.
--
-- Rounding up rather than down is deliberate even though it is the unsafe
-- direction, because zero would violate the check and fail the rollback
-- outright, leaving an operator mid-migration with no way back. A rate
-- that is wrong and visible beats a migration that cannot complete; the
-- comment below is what tells whoever rolls back to re-check the value.
alter table cashback.network
    rename constraint network_rate_limit_per_minute_positive to network_rate_limit_positive;

alter table cashback.network
    rename column rate_limit_per_minute to rate_limit_per_second;

update cashback.network
   set rate_limit_per_second = greatest(1, rate_limit_per_second / 60);

comment on column cashback.network.rate_limit_per_second is
    'The request rate this network documents, in calls per second. Any network slower than one call a second was rounded UP to 1 by the rollback from 0026 and is now faster than its documentation permits: re-check every row against the network''s published limit before polling.';
