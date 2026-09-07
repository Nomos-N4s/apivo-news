-- Reverses 0037_click_carries_its_network.

drop index cashback.click_network_id_idx;

alter table cashback.click
    drop constraint click_network_matches_route,
    drop constraint click_route_matches_offer,
    drop column network_id,
    drop column merchant_network_id;

alter table cashback.merchant_network
    drop constraint merchant_network_id_network_unique;

alter table cashback.offer
    drop constraint offer_id_merchant_network_unique;
