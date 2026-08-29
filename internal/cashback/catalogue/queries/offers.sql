-- Catalogue queries (T029). One query so far: the read the click-out path
-- (T048) is built on. Later catalogue work (browse, merchant detail) adds
-- its own queries here; this file stays the click-out's.
--
-- "Live" is the schema's own definition, and every leg of the chain has a
-- veto (0011):
--
--   * the offer's validity window: valid_from has passed and valid_to has
--     not. The moment is a query parameter, not now() - an index predicate
--     cannot say "now", so offer_validity_window_idx covers the whole
--     window via coalesce(valid_to, 'infinity'), and this query uses
--     exactly that shape to stay on the index;
--   * the route: merchant_network.status = 'active'. A retailer that left
--     one network may still be live on another, but not through this row;
--   * the retailer: merchant.status = 'active' - whether we publish them
--     at all, independent of any single route;
--   * the network: network.active. A network is born inactive and a
--     half-configured one must never issue clicks.
--
-- Money and rates stay integral end to end (C-6): rate_bps and
-- member_share_bps are basis points, rate_fixed_minor is minor units with
-- its explicit ISO-4217 currency. Nothing here casts, rounds or converts.

-- name: GetLiveOffer :one
-- The rate band a click is about to be issued against, with the two facts
-- the redirect and the snapshot need from its neighbours: the network's
-- identity and click-reference parameter (FR-021), and the merchant's
-- identity. Every offer column is returned because the WHOLE band as
-- published at click time is snapshotted onto the click (FR-013).
--
-- No rows means "not live" - missing, expired, or with an inactive link in
-- the chain - and the endpoint maps that to its 409 (contract: POST
-- /clickouts). Which leg failed is deliberately not distinguished here:
-- the click must not be created either way, and the distinction would cost
-- the filtered index this query is shaped for.
select
    o.id,
    o.merchant_network_id,
    o.rate_kind,
    o.rate_bps,
    o.rate_fixed_minor,
    o.currency,
    o.member_share_bps,
    o.conditions,
    o.exclusions,
    o.valid_from,
    o.valid_to,
    o.deeplink_template,
    n.id as network_id,
    n.click_ref_param,
    m.id as merchant_id,
    m.slug as merchant_slug
from cashback.offer o
join cashback.merchant_network mn on mn.id = o.merchant_network_id
join cashback.merchant m on m.id = mn.merchant_id
join cashback.network n on n.id = mn.network_id
where o.id = sqlc.arg(id)
  and o.valid_from <= sqlc.arg(at)::timestamptz
  and coalesce(o.valid_to, 'infinity'::timestamptz) > sqlc.arg(at)::timestamptz
  and mn.status = 'active'
  and m.status = 'active'
  and n.active;
