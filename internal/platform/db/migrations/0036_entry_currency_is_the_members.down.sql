-- Reverses 0036_entry_currency_is_the_members.

alter table cashback.network_account
    drop column reports_currency;

alter table cashback.entry
    drop constraint entry_currency_is_the_members;

alter table cashback.participation
    drop constraint participation_account_currency_unique;
