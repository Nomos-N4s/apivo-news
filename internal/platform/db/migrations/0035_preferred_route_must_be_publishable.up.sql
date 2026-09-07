-- 0035_preferred_route_must_be_publishable: the published route must be
-- alive and able to carry a click reference.
--
-- (specs/004-multi-network-linkwise/data-model.md describes this change
-- under 0035 after 0034 took the click rule.)
--
-- merchant_network_one_preferred (0011) guarantees AT MOST one preferred
-- route per retailer. It says nothing about whether that route is usable:
-- a route that has left the network, or one whose network cannot carry our
-- click reference back to us, may hold the published slot while a live
-- route sits beside it - and because the index only forbids a second,
-- nothing takes over. The first network to import a retailer owned its
-- published rate permanently, including after it stopped carrying the
-- retailer at all.
--
-- Two additions. can_attribute is contract rule 11 (spec 004,
-- contracts/ports.md): a route states whether a click through it carries
-- our reference back. It defaults to true because every route that exists
-- today can be attributed - Awin's click_ref_param is a network-wide fact -
-- so true is the truth for the whole table and needs no backfill; false is
-- a value an importer sets deliberately, from a fact its adapter learned.
-- A default of false would unpublish the entire catalogue on migration.
--
-- The check then makes demotion an explicit act: a route cannot be
-- preferred unless it is active and attributable, so whoever writes a
-- status change on the published route withdraws its preference first and
-- hands the slot to a survivor (FR-100) - the importer does exactly that -
-- rather than a dead or unattributable row quietly outranking a live one.
--
-- What is deliberately NOT a constraint: that a retailer with a publishable
-- route has a published one (a cross-row assertion a check cannot see), and
-- that the preferred route's NETWORK is active (another table). Both live
-- in the demotion path and in an operator listing (T210) that makes a
-- violation visible rather than silent.

alter table cashback.merchant_network
    add column can_attribute boolean not null default true;

-- Rows the rule already condemns: a route that paused or left the network
-- while holding the published slot, which is exactly the defect this
-- migration exists for. Their preference is withdrawn here rather than the
-- migration failing on them, and nothing is promoted in their place: which
-- survivor takes over is the importer's decision at its next run, recorded
-- as an event, and until then the retailer is visibly unpublished
-- (RetailersPublishingNothing) rather than silently publishing a dead
-- route.
update cashback.merchant_network
   set preferred = false
 where preferred
   and status <> 'active';

alter table cashback.merchant_network
    add constraint merchant_network_preferred_is_publishable
    check (not preferred or (status = 'active' and can_attribute));

comment on column cashback.merchant_network.can_attribute is
    'Whether a click through this route carries our click reference back to us (contract rule 11). False is not broken: the member clicks, buys, and the network pays the publisher - it just cannot say whose purchase it was. A route that cannot be attributed can never be the published one, because publishing a rate we cannot honour is worse than publishing nothing.';

comment on constraint merchant_network_preferred_is_publishable
    on cashback.merchant_network is
    'The published route must be usable: active, and able to carry a click reference. Demotion is then forced to be an explicit act with a successor (FR-100), rather than a dead or unattributable row quietly outranking a live one.';
