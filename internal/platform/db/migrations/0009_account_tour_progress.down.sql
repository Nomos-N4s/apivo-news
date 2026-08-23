-- Reverses 0009. The constraint goes with the column.
--
-- Dropping this loses every visitor's place in every tour. That is the
-- honest consequence and there is nothing to preserve it into: the value
-- is a cursor into a list that lives in the front end, so there is no
-- other table it could mean anything in.

alter table account
    drop column tour_progress;
