-- Reverses 0035_preferred_route_must_be_publishable.

alter table cashback.merchant_network
    drop constraint merchant_network_preferred_is_publishable;

alter table cashback.merchant_network
    drop column can_attribute;
