-- Importing a catalogue (T105). The writes that turn a network's programme
-- list into retailers a member can click, and the one that notices a
-- retailer has gone.
--
-- Every statement here is keyed by something the schema already keeps
-- unique, because an import runs again on a schedule and the second run must
-- change nothing it did not mean to.

-- name: FindRouteByExternalID :one
-- The route this network's own id already names, if there is one.
--
-- This is the FIRST question an import asks about a retailer, and it has to
-- be: it is the only identity that cannot drift. A name can be edited by the
-- advertiser between two imports - "Gartenhaus" becomes "Gartenhaus DE" -
-- and matching on the slug that name produces would create a second merchant
-- for a retailer we already have, with its own history and its own offers.
-- The network's id for them does not move.
select
    mn.id,
    mn.merchant_id,
    mn.status,
    mn.preferred
from cashback.merchant_network mn
where mn.network_id = sqlc.arg(network_id)
  and mn.external_merchant_id = sqlc.arg(external_merchant_id);

-- name: FindMerchantBySlug :one
-- A retailer we already publish under this slug, and whether they already
-- have a route at this network.
--
-- Asked only when the network's own id is new to us, and it answers two
-- different questions at once because the caller has to tell them apart:
--
--   - no row: a retailer we have never seen. Create one.
--   - a row with no route at this network: the SAME retailer, reached
--     through a network we already import. Add a route to them - that is
--     what merchant_network exists for, and it is how a member gets the
--     better of two rates.
--   - a row that already has a route here: a DIFFERENT retailer whose name
--     happens to slug the same. Two programmes called "Fashion Store" are
--     two businesses, and merchant_network_one_route_per_network would
--     refuse the second route anyway. The caller falls back to a slug built
--     from the network's own id.
select
    m.id,
    m.slug,
    exists (
        select 1
        from cashback.merchant_network mn
        where mn.merchant_id = m.id
          and mn.network_id = sqlc.arg(network_id)
    ) as has_route_at_network
from cashback.merchant m
where m.slug = sqlc.arg(slug);

-- name: CreateMerchant :one
-- A retailer we have not seen before.
--
-- status is left at the column default, which is 'active'. That is a
-- decision about whether we publish this retailer AT ALL, and it is
-- deliberately not the same decision as whether a route works: a retailer
-- suspended on one network is a merchant_network status, and an operator who
-- has hidden a retailer must not have that undone by the next import.
insert into cashback.merchant (slug, country, source_language_code)
values (
    sqlc.arg(slug),
    sqlc.narg(country),
    sqlc.arg(source_language_code)
)
returning id, slug, country, source_language_code, status;

-- name: UpsertMerchantCopy :exec
-- The retailer's name in the language the network supplies it in.
--
-- Only the name. The port carries nothing else about a retailer, and a
-- summary invented from a programme description is copy no operator wrote -
-- the merchant_copy comment is explicit that a missing language is resolved
-- by labelled fallback and never by inventing text.
insert into cashback.merchant_copy (merchant_id, language_code, name)
values (sqlc.arg(merchant_id), sqlc.arg(language_code), sqlc.arg(name))
on conflict (merchant_id, language_code) do update
   set name = excluded.name;

-- name: UpsertRoute :one
-- The route itself: the network's own id for the retailer, what the network
-- last said about it, when we asked, and the payload the answer came in.
--
-- retrieved_at is written on EVERY run, including one that changed nothing
-- else, and that is not bookkeeping - it is how MarkRoutesNotSeen below
-- finds the routes this import did not return. A route whose timestamp did
-- not move is a route the network no longer lists.
--
-- brand_id is written once, on insert, and never updated. Which brand
-- publishes a route is an operator's decision (ADR-0004); an import that
-- rewrote it would move a retailer between tenants on a schedule.
insert into cashback.merchant_network (
    merchant_id, network_id, brand_id, external_merchant_id,
    retrieved_at, raw_payload, status, preferred
)
values (
    sqlc.arg(merchant_id),
    sqlc.arg(network_id),
    sqlc.arg(brand_id),
    sqlc.arg(external_merchant_id),
    sqlc.arg(retrieved_at),
    sqlc.arg(raw_payload),
    sqlc.arg(status),
    sqlc.arg(preferred)
)
on conflict (network_id, external_merchant_id) do update
   set retrieved_at = excluded.retrieved_at,
       raw_payload = excluded.raw_payload,
       status = excluded.status
returning id, merchant_id, status, preferred, retrieved_at;

-- name: MerchantHasPreferredRoute :one
-- Whether this retailer already has the route the catalogue publishes.
--
-- Asked before setting preferred on a new route, because
-- merchant_network_one_preferred is a partial unique index: a second
-- preferred route is refused by the database, not silently accepted. A
-- retailer's first route becomes the preferred one so the catalogue has
-- something to publish at all; after that, arbitration is an operator's.
select exists (
    select 1
    from cashback.merchant_network mn
    where mn.merchant_id = sqlc.arg(merchant_id)
      and mn.preferred
) as has_preferred;

-- name: MarkRoutesNotSeen :execrows
-- Routes at this network the import did not return: the retailer has left.
--
-- This is the statement absence-means-departure rests on, and it is why the
-- port makes a partial catalogue read an error rather than a short answer. A
-- read that stopped at retailer 400 of 5000 and ended quietly would bring
-- every one of the remaining 4600 through here, stop publishing their
-- offers, and empty the catalogue members see - from an import that reported
-- nothing wrong. The caller runs it only after an iteration that ended with
-- no error at all (contract rule 8).
--
-- It compares retrieved_at against the instant the import STARTED rather
-- than against a list of ids it saw. A list would be an array parameter the
-- size of the catalogue; a timestamp is the same answer in one comparison,
-- and it is already on the row for its own reasons.
--
-- Rows already at left_network are excluded so the row count is what
-- CHANGED, which is the number worth logging: an import that reports 4000
-- departures is one an operator should look at before it runs again.
update cashback.merchant_network
   set status = 'left_network'
 where network_id = sqlc.arg(network_id)
   and retrieved_at < sqlc.arg(import_started_at)
   and status <> 'left_network';
