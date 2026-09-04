-- 0033_network_reporting_lag_minutes: how far behind the present a network
-- reports (T292, 004 Phase G).
--
-- The forward sweep asks for a window ending now. A network whose data is
-- behind answers that window cleanly and emptily for the part it has not
-- reached, and the cursor advances anyway. Nothing is lost - the trailing
-- sweep walks forward and re-reads every period exactly once - but it does
-- so about a hundred days later, so a member waits a quarter for a credit
-- earned today and no error stream shows it.
--
-- networks.Limits.ReportingLag is what stops the forward window at
-- now - lag. This column is where the value belongs: a lag is a fact about
-- the network, not about the adapter's release, and it is the same kind of
-- fact as max_query_window_days and rate_limit_per_minute beside it. A
-- network that changes its reporting delay should be one UPDATE and a
-- restart, not a code change.
--
-- MINUTES, for the reason 0026 moved the throttle to per-minute: it is the
-- unit that can express the truth. A lag is usually hours and occasionally
-- a day; minutes holds both exactly, holds a network that reports every
-- ninety minutes, and reads the same way as the column above it. Days would
-- round a two-hour lag to zero or to a whole day, and both are wrong in the
-- direction that costs money - zero re-opens the defect, a day delays every
-- credit by a day.
--
-- DEFAULT 0, and zero is the ORDINARY value here rather than the dangerous
-- one. The two columns beside it are constrained above zero because zero is
-- what an unset row carries and a zero window or rate stops ingestion dead.
-- A network with no reporting lag is simply one that answers up to the
-- moment, which is what every existing row means and what every adapter
-- declared before this column existed. So every row backfills to 0 and no
-- deployment changes behaviour.
--
-- Negative is refused: a network cannot report the future, and a negative
-- lag would push the forward window's end past now - which is precisely the
-- clock-skew case nextForwardWindow already refuses to produce.
alter table cashback.network
    add column reporting_lag_minutes integer not null default 0
        constraint network_reporting_lag_not_negative
            check (reporting_lag_minutes >= 0);

comment on column cashback.network.reporting_lag_minutes is
    'How far behind the present this network reports, in minutes. The forward sweep ends its window at now minus this, so the cursor never passes ground the network has not covered. Zero means the network answers up to the moment, which is the ordinary case; negative is refused because a network cannot report the future.';
