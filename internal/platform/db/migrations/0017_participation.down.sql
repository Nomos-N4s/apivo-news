-- Reverses 0017_participation. Dropping the table discards which terms
-- each member accepted and when: never run against an environment holding
-- real members.

drop trigger participation_guard on cashback.participation;
drop function cashback.participation_guard();

drop table cashback.participation;
