-- Reverses 0033_network_reporting_lag_minutes.
--
-- Lossy in one direction only, and worth naming: any lag an operator set is
-- gone, and every network reverts to being read as though it reports up to
-- the moment. That is the behaviour the whole codebase had before the
-- column existed, so nothing breaks - but a network that genuinely reports
-- late will again have its cursor advanced past ground it has not covered,
-- and the transactions in that span will not be seen until the trailing
-- sweep reaches them.
--
-- Re-record the lags before rolling forward again: the values are not
-- recoverable from anything else in the schema.
alter table cashback.network
    drop column reporting_lag_minutes;
