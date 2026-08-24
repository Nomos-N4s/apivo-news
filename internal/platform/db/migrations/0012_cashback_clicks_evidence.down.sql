-- Reverses 0012_cashback_clicks_evidence. Destroys the click records and
-- the network evidence every credit rests on: never run against an
-- environment holding real money records.

drop table cashback.network_transaction;
drop function cashback.network_transaction_guard();

drop table cashback.click;
