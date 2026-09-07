-- What this deployment is connected to (T228, FR-092, ADR-0003).
--
-- The read behind GET /ops/networks. Everything here is configuration
-- rather than money, which is why it is a plain read with no decision
-- attached and no event: an operator looking at this is working out why
-- nothing is happening, not changing anything.
--
-- Here rather than in the networks module for the reason the other operator
-- reads are here: this crosses every network and every publisher account,
-- and the networks module's own statements are scoped to the one account a
-- poller is acting for. An operator has no such scope.

-- name: ListConnectedNetworks :many
-- Every seeded network and the publisher accounts hanging off it.
--
-- A LEFT JOIN, deliberately. A `cashback.network` row with NO account is not
-- a broken read - it is one of the states this endpoint exists to make
-- visible: the row is seeded, the driver may even be shipped, and nothing
-- polls because `connect-network` was never run for a publisher. That
-- deployment logs "no publisher account is connected" once at start-up and
-- then looks exactly like a quiet network forever.
--
-- The credential_ref travels because it NAMES A KEY, never a value
-- (ADR-0003): it is the string an operator checks their env file for. What
-- must not travel is whether that key has anything in it - that is the
-- composition root's answer, not the database's, and it comes back as a
-- boolean beside this row rather than as anything derived from a secret.
--
-- Ordered by (network, publisher) so two runs of the same deployment read
-- the same way. Unpaginated: the set is bounded by what an operator seeded
-- and what NETWORKS names, not by member activity, so a cursor here would be
-- ceremony over a handful of rows.
select
    n.id as network_id,
    n.display_name,
    n.click_ref_param,
    n.max_query_window_days,
    n.rate_limit_per_minute,
    n.reporting_lag_minutes,
    n.active as network_active,
    a.id as account_id,
    a.external_publisher_id,
    a.credential_ref,
    a.cursor_at,
    a.trailing_cursor_at,
    a.backfill_from,
    a.reports_currency,
    a.active as account_active
  from cashback.network n
  left join cashback.network_account a on a.network_id = n.id
 order by n.id, a.external_publisher_id;
