-- 0032_entry_one_credit_per_report: one CREDIT per report; a reversal may
-- cite the report of the credit it undoes.
--
-- 0013 made entry_one_per_report a full unique on network_transaction_id,
-- reasoning that a reversal always cites a DIFFERENT row - the superseding
-- report that carried the network's reversal (C-3) - so excluding reversals
-- bought nothing. That holds for every reversal the network causes. It does
-- not hold for the one an operator causes: rejecting a held credit (US7
-- scenario 3, T119) undoes it without the network having restated anything,
-- so there is no superseding report, and the only evidence the reversing
-- entry can rest on (C-2, not null) is the credit's own. Under 0013 that
-- insert was refused, and the insert is the rejection.
--
-- So the rule becomes what it was meant to be: one report earns one CREDIT.
-- What 0013 feared - two reversal entries debiting a member twice for one
-- clawback - is refused by entry_reversed_at_most_once, which is unique on
-- reversal_of_id and untouched here. A reversal citing a report, credit or
-- reversal alike, still undoes exactly one entry exactly once.
--
-- The name is kept. open.go recognises the refusal by it, and a unique
-- index reports its name the way a constraint does.

alter table cashback.entry drop constraint entry_one_per_report;

create unique index entry_one_per_report
    on cashback.entry (network_transaction_id)
 where reversal_of_id is null;

comment on index cashback.entry_one_per_report is
    'Exactly-once crediting: one report backs at most one CREDIT. A reversal may cite the report of the credit it undoes (an operator''s rejection has no superseding report to cite); entry_reversed_at_most_once keeps a credit reversed at most once.';
