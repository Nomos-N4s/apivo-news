-- 0028_reconciliation_statement_once: the same statement, imported twice,
-- is one run.
--
-- 0015 made reconciliation_run immutable and said why: it is the
-- counterparty's own account of the money. It said nothing about importing
-- the same account of it twice, and an operator retrying an upload that
-- timed out after the commit does exactly that - and gets two runs. Two
-- runs are two sets of differences derived from one statement, an operator
-- queue that has doubled, and a report that must be resolved twice before
-- FR-043 counts it as received. On an immutable table there is no tidying
-- up afterwards, so the second run has to be impossible rather than
-- regrettable.
--
-- The digest is computed by the database from the statement it stores, so
-- it cannot disagree with it. Uniqueness is on what an operator means by
-- "this statement": which publisher account, which period, which content.
-- The same lines framed as a different period are a different statement
-- (and a mistake, but not one a constraint can see); a corrected statement
-- for the same period is a different digest and rightly a new run.
--
-- jsonb's text form is canonical - keys ordered, whitespace normalised - so
-- two uploads that differ only in formatting hash the same, and two that
-- differ in one amount do not.

alter table cashback.reconciliation_run
    add column statement_digest text not null
        generated always as (md5(raw_statement::text)) stored;

comment on column cashback.reconciliation_run.statement_digest is
    'md5 of the statement as stored, computed by the database from raw_statement. With the account and the period it is what makes a retried import the same run rather than a second one.';

alter table cashback.reconciliation_run
    add constraint reconciliation_run_statement_once
        unique (network_account_id, statement_period_start, statement_period_end, statement_digest);
