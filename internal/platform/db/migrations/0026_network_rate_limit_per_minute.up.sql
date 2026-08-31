-- 0026_network_rate_limit_per_minute: the unit networks actually publish
-- their throttle in (T137, ADR-0003).
--
-- 0011 recorded the throttle as `rate_limit_per_second integer`, from a
-- research note that put the reference network at six requests a second.
-- Reading Awin's own documentation while building their adapter found
-- otherwise: "a throttling system in place that limits the number of API
-- requests to 20 API calls per minute per user".
--
-- Twenty a minute is one request every three seconds. In a per-second
-- integer column constrained above zero, the smallest value that can be
-- stored is 1 - three times Awin's actual limit, and there is no smaller
-- one to choose. The column could not express the truth, so a deployment
-- configured from it would have polled at treble the permitted rate and
-- been throttled or blocked, with the row it read from looking correct.
--
-- Per MINUTE is the fix rather than a fractional per-second column,
-- because it is the unit the networks themselves publish: Awin says 20 a
-- minute, and a limit written the way its owner writes it is one an
-- operator can check against the documentation without arithmetic. A fast
-- network stays exact too - six a second is 360 a minute, a whole number.
--
-- The conversion is exact in this direction. Every existing row holds a
-- per-second count, and multiplying by sixty is the same throttle said
-- differently; no row changes meaning and none can fail the check, because
-- a positive integer times sixty is positive.
--
-- networks.RateLimiter takes a float64 per second, so the adapter divides
-- by sixty at construction. That division is the one place a unit bug
-- could hide, and it is exactly the bug RateLimiter.Rate() was made
-- observable to catch: the conformance suite compares what the limiter is
-- holding to against what the port declares.
alter table cashback.network
    rename column rate_limit_per_second to rate_limit_per_minute;

alter table cashback.network
    rename constraint network_rate_limit_positive to network_rate_limit_per_minute_positive;

update cashback.network
   set rate_limit_per_minute = rate_limit_per_minute * 60;

comment on column cashback.network.rate_limit_per_minute is
    'The request rate this network documents, in calls per minute - the unit networks publish (Awin: 20 a minute). Per minute rather than per second because a slow network''s real limit is a fraction of a request a second, which a positive integer column cannot hold: the adapter divides by sixty to build its limiter.';
