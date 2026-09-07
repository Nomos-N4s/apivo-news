-- Publishing a rate band on a route an operator names (#537). The one write
-- path onto cashback.offer that is not the fixture seed, and the reads that
-- keep it honest.
--
-- Everything here is keyed by things the schema already keeps unique - the
-- network's own merchant id (merchant_network_unique_per_network), the
-- offer's primary key - because an operator command is run again after a
-- typo, and the second run must be able to see what the first one did.

-- name: RouteForPublishing :one
-- The route a band would be published on, with the three legs a live offer
-- needs (GetLiveOffer): the route's own status, the retailer's, and whether
-- the network is switched on.
--
-- All three are read here rather than trusted, because a band on a route
-- that fails any of them is a band no click will ever be issued against -
-- GetLiveOffer refuses it - and the time to say so is before the row
-- exists, in the terminal of the person who can fix it, not at a member's
-- first click as a 409.
--
-- preferred rides along for the report and not for a refusal: a band on a
-- non-preferred route is clickable by id and invisible on the merchant
-- page (PublishedBands lists the preferred route only), which is a thing
-- to tell an operator and not a thing to forbid them.
select
    mn.id,
    mn.status,
    mn.preferred,
    m.id as merchant_id,
    m.slug as merchant_slug,
    m.status as merchant_status,
    n.active as network_active,
    n.click_ref_param
  from cashback.merchant_network mn
  join cashback.merchant m on m.id = mn.merchant_id
  join cashback.network n on n.id = mn.network_id
 where mn.network_id = sqlc.arg(network_id)
   and mn.external_merchant_id = sqlc.arg(external_merchant_id);

-- name: BandsInForceOnRoute :many
-- Every band on this route in force at one moment.
--
-- The moment is a parameter for the reason GetLiveOffer's is: the band
-- being published names its own start, and "already in force" has to be
-- judged at that instant rather than at the clock - a band that opens next
-- month is not a duplicate of one that closes tonight. The predicate is
-- the same shape as offer_validity_window_idx and as PublishedBands, so
-- "in force" means one thing across the index, the merchant page, the
-- click-out and this command.
--
-- Read twice by the publisher: before the write, to refuse a band identical
-- to one already in force; and after it, so the report shows what the
-- route now carries rather than what the operator assumed it did.
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
    o.valid_to,
    o.deeplink_template
  from cashback.offer o
 where o.merchant_network_id = sqlc.arg(route_id)
   and o.valid_from <= sqlc.arg(at)::timestamptz
   and coalesce(o.valid_to, 'infinity'::timestamptz) > sqlc.arg(at)::timestamptz
 order by o.valid_from, o.id;

-- name: CloseOffer :execrows
-- End a band at one moment, and only a band that is open at that moment on
-- this route.
--
-- Three conditions besides the id, and each is a refusal the caller reads
-- as "zero rows":
--
--   the route, so a --replaces naming a band on some other retailer's
--   route cannot close it - the id alone would;
--
--   valid_from strictly before the moment, because offer_validity_window
--   requires valid_to > valid_from and a band that opens at or after the
--   moment cannot be closed AT it without a window of nothing, which the
--   schema refuses with an error that would abort the whole publish;
--
--   still open at the moment, so a band already closed is not silently
--   re-closed at a different instant. A closed band is a fact about when a
--   rate stopped being offered, and this statement does not move it.
--
-- An UPDATE on a row that clicks reference, and safe for exactly that
-- reason: every click carries its own snapshot of the band (FR-013), so
-- closing the band changes nothing about what any click already earned.
update cashback.offer
   set valid_to = sqlc.arg(at)::timestamptz
 where id = sqlc.arg(id)
   and merchant_network_id = sqlc.arg(route_id)
   and valid_from < sqlc.arg(at)::timestamptz
   and coalesce(valid_to, 'infinity'::timestamptz) > sqlc.arg(at)::timestamptz;

-- name: InsertOffer :one
-- The band itself. Every money column arrives in the schema's own units -
-- basis points and minor units with an explicit currency (C-6) - and the
-- schema's offer_rate_kind_fields is what finally decides the shape is
-- legal; the publisher checks the same thing first only so the refusal
-- reads as a sentence rather than a constraint name.
insert into cashback.offer (
    merchant_network_id,
    rate_kind,
    rate_bps,
    rate_fixed_minor,
    currency,
    member_share_bps,
    conditions,
    exclusions,
    valid_from,
    valid_to,
    deeplink_template
) values (
    sqlc.arg(route_id),
    sqlc.arg(rate_kind),
    sqlc.arg(rate_bps),
    sqlc.arg(rate_fixed_minor),
    sqlc.arg(currency),
    sqlc.arg(member_share_bps),
    sqlc.arg(conditions),
    sqlc.arg(exclusions),
    sqlc.arg(valid_from)::timestamptz,
    sqlc.arg(valid_to),
    sqlc.arg(deeplink_template)
)
returning id;
