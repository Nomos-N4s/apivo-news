-- Reverses 0014_cashback_payout. Destroys the record of who approved which
-- payment, which is the C-4 evidence itself: never run against an
-- environment that has paid anybody.

drop trigger payout_guard on cashback.payout;
drop function cashback.payout_guard();
drop table cashback.payout;

drop trigger withdrawal_request_guard on cashback.withdrawal_request;
drop function cashback.withdrawal_request_guard();
drop table cashback.withdrawal_request;

drop trigger payout_destination_guard on cashback.payout_destination;
drop function cashback.payout_destination_guard();
drop table cashback.payout_destination;
