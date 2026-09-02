-- Reverses 0028_reconciliation_statement_once.
--
-- Dropping the constraint and the column loses nothing: the digest is
-- derived from raw_statement, which stays, and every run stays with it.
-- What comes back is the possibility of importing one statement twice.

alter table cashback.reconciliation_run
    drop constraint reconciliation_run_statement_once;

alter table cashback.reconciliation_run
    drop column statement_digest;
