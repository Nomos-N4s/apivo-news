-- Reverses 0013_cashback_earnings. Destroys every member entry, the
-- history of how their money moved and the ledger join: never run against
-- an environment holding real money records.

drop table cashback.ledger_link;

drop trigger unattributed_transaction_guard on cashback.unattributed_transaction;
drop function cashback.unattributed_transaction_guard();
drop table cashback.unattributed_transaction;

drop table cashback.entry_transition;

-- The D7 constraint trigger sits on entry, not on the transition table it
-- checks, so it does not fall with the drop above and must go explicitly
-- before the function it executes can.
drop trigger entry_state_requires_transition on cashback.entry;
drop function cashback.entry_state_requires_transition();

drop table cashback.entry;
drop function cashback.entry_evidence_guard();
drop function cashback.entry_guard();
