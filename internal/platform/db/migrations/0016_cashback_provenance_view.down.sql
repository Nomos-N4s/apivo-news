-- Reverses 0016_cashback_provenance_view. The rows survive; only the two
-- questions an auditor could ask in one query go away.

drop view cashback.ledger_zero_sum;
drop function cashback.ledger_net_minor(text);
drop function cashback.ledger_currencies();
drop function cashback.ledger_balance_relation();

drop view cashback.provenance;
