-- Reverses 0015_cashback_reconciliation. Destroys the imported statements
-- that evidence what a network actually paid: never run against an
-- environment holding real accounting records.

drop table cashback.reconciliation_difference;
drop table cashback.reconciliation_run;

-- Added here rather than in 0012, so it is removed here too.
alter table cashback.network_transaction
    drop constraint network_transaction_id_account_unique;
