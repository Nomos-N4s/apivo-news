-- One merchant's page: who they are, and every rate band we publish for
-- them right now (T104, US5 scenario 3, FR-011).
--
-- Two reads rather than one join, and the reason is what each of them is
-- allowed to mean. "No such merchant" and "no rate band today" are
-- different answers to a member - the first is a 404, the second is a page
-- saying this retailer pays nothing at the moment - and a single join
-- returns zero rows for both. A retailer between rate bands would 404 as
-- though we had never heard of them.
--
-- Copy is a third read, and it is CopyForMerchants (browse.sql) unchanged:
-- language and place are independent axes (constitution VII), so the
-- language resolution that browsing uses is the one a merchant page uses,
-- called with one merchant instead of many.

-- name: MerchantBySlug :one
-- The retailer at this address, if we publish them at all.
--
-- status = 'active' is the whole of the contract's "404 unknown or
-- inactive": a paused retailer keeps its slug, its rows and its history,
-- and stops being reachable. Deliberately says nothing about routes or
-- bands - whether there is anything to click is the second query's
-- question, and folding it in here is what would turn a rate gap into a
-- 404.
select
    m.id,
    m.slug,
    m.country,
    m.source_language_code,
    m.status
  from cashback.merchant m
 where m.slug = sqlc.arg(slug)
   and m.status = 'active';

-- name: PublishedBands :many
-- Every band in force for this retailer at one moment, with the conditions
-- and the exclusions each one carries (FR-011).
--
-- EVERY band, which is US5 scenario 3 exactly: a retailer whose rate varies
-- by what you buy publishes several bands at once, and showing one of them
-- is how a member ends up expecting 8% on an order that pays 2%. The
-- conditions and exclusions ride with each band rather than being collected
-- into one page-level note, because they qualify one band and not the
-- others.
--
-- On the PREFERRED route only. merchant_network.preferred is the schema's
-- own answer to "which route does the catalogue publish" (0011), held to
-- one row per retailer by a partial unique index and set on a retailer's
-- first route at import. Listing every route's bands would show the same
-- retailer twice at two rates, one of which no click will ever be issued
-- against.
--
-- The moment is a parameter rather than now() for the same reason
-- GetLiveOffer takes one: one instant pins the whole page, instead of a
-- read of the clock per band that can straddle an edge and render a
-- catalogue that contradicts itself. coalesce(valid_to, 'infinity') is the
-- shape offer_validity_window_idx is built on, so "still open" and "closes
-- later" stay one thing here, in the index, and in the click-out read.
--
-- Neither deeplink_template nor the route's network is selected. Both are
-- how a click is issued rather than anything a member is shown - the
-- click-out path reads them by offer id at the moment it issues the click
-- (FR-013) - and which network pays a retailer's commission is a
-- commercial fact a merchant page has no reason to publish.
select
    o.id,
    o.rate_kind,
    o.rate_bps,
    o.rate_fixed_minor,
    o.currency,
    o.member_share_bps,
    o.conditions,
    o.exclusions,
    o.valid_from,
    o.valid_to
  from cashback.offer o
  join cashback.merchant_network mn on mn.id = o.merchant_network_id
  join cashback.network n on n.id = mn.network_id
 where mn.merchant_id = sqlc.arg(merchant_id)
   and mn.preferred
   and mn.status = 'active'
   and n.active
   and o.valid_from <= sqlc.arg(at)::timestamptz
   and coalesce(o.valid_to, 'infinity'::timestamptz) > sqlc.arg(at)::timestamptz
 order by o.valid_from desc, o.id;
