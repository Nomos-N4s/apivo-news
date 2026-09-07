-- Reverses 0034_click_backs_one_credit.

drop index cashback.entry_click_id_idx;

create index entry_click_id_idx on cashback.entry (click_id) where click_id is not null;
