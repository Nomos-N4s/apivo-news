-- 0037_click_carries_its_network: a click records the route and the network
-- it was issued through, pinned by key so the two cannot disagree with the
-- offer (spec 004, FR-096, FR-098).
--
-- (specs/004-multi-network-linkwise/data-model.md describes this change
-- under 0037, the last of the four integrity migrations.)
--
-- Attribution has to ask which network issued a click. Until now that
-- answer was three tables away - click.offer_id, offer.merchant_network_id,
-- merchant_network.network_id - and nothing on the crediting path asked it.
-- With one network that is harmless, because only one network ever echoes
-- a reference back. With two, a reference reported by the wrong network
-- matches the click all the same, and the first report to arrive is the
-- one credited, whichever network it came from.
--
-- 0011 refused to repeat the network on the offer, and said why: the two
-- could disagree. That objection stands and is answered the way 0012
-- answered it for click_id_account_unique - not by trusting the copy but
-- by making disagreement unrepresentable. Two composite foreign keys, over
-- two unique keys that are each redundant against a primary key and exist
-- only to be referenced: the click's route must be the offer's route, and
-- the click's network must be the route's network. The value is
-- denormalised; its truth is a key.
--
-- Two columns rather than one, because network_id alone cannot be pinned:
-- nothing joins a click to a network except through its route, so the route
-- is recorded too. It is also the route the rate snapshot came from, which
-- is worth having beside it.
--
-- cashback.click is append-only (0012); its immutability trigger is stood
-- down for the one statement that backfills rows written before these
-- columns existed, and stood back up in the same transaction. The backfill
-- derives both values from the join the columns replace, which is the only
-- answer the old rows can give and is the same answer the keys then hold
-- them to.

alter table cashback.offer
    add constraint offer_id_merchant_network_unique unique (id, merchant_network_id);

alter table cashback.merchant_network
    add constraint merchant_network_id_network_unique unique (id, network_id);

alter table cashback.click
    add column merchant_network_id uuid,
    add column network_id text;

alter table cashback.click disable trigger click_immutable;

update cashback.click c
   set merchant_network_id = o.merchant_network_id,
       network_id          = mn.network_id
  from cashback.offer o
  join cashback.merchant_network mn on mn.id = o.merchant_network_id
 where o.id = c.offer_id;

alter table cashback.click enable trigger click_immutable;

alter table cashback.click
    alter column merchant_network_id set not null,
    alter column network_id set not null,
    add constraint click_route_matches_offer
        foreign key (offer_id, merchant_network_id)
        references cashback.offer (id, merchant_network_id),
    add constraint click_network_matches_route
        foreign key (merchant_network_id, network_id)
        references cashback.merchant_network (id, network_id);

-- The attribution predicate's index: which network's clicks a reference is
-- looked up among (FR-096), and later the ops queue's filter by network.
create index click_network_id_idx on cashback.click (network_id);

comment on column cashback.click.merchant_network_id is
    'The route (retailer through one network) the member clicked - the route the rate snapshot came from. Pinned to the offer by click_route_matches_offer, so it cannot name a route the offer is not on.';
comment on column cashback.click.network_id is
    'The network this click was issued through, and the only network whose report of this reference is this click''s (FR-096). Pinned to the route by click_network_matches_route, so it cannot name a network the route does not belong to.';
