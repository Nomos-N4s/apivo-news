-- Reverses 0011_cashback_catalogue. Dropped in dependency order, so a
-- forgotten reference fails here rather than leaving a half-catalogue
-- behind. Destroys every merchant, route, rate band and imported payload.

drop table cashback.offer;
drop table cashback.merchant_place;
drop table cashback.merchant_copy;
drop table cashback.merchant_network;
drop table cashback.merchant;
drop table cashback.network_account;
drop table cashback.network;
