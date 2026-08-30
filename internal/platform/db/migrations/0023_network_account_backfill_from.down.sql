-- Reverses 0023_network_account_backfill_from. The constraint goes first:
-- dropping the column would take it with it, but naming both says what is
-- being removed rather than leaving a reader to know that rule.
--
-- Rolling back DISCARDS every operator's answer to "how far back does this
-- account read". Rolling forward again leaves every row null, which the
-- poller refuses by name - so the accounts stop polling loudly rather than
-- resuming from a start nobody chose.
alter table cashback.network_account
    drop constraint network_account_backfill_from_not_ahead;

alter table cashback.network_account
    drop column backfill_from;
