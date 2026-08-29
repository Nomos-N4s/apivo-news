-- Reverses 0021_event_deliveries. Destroys every subscriber's memory of
-- what it has consumed: positions, completed deliveries and the operator
-- queue of parked ones. The stream itself is untouched, so a re-applied
-- subscriber layer re-reads it from the start and leans entirely on
-- handler idempotence (consumer rule 1) - correct, but every handler runs
-- over the whole history again, and any parked failure awaiting an
-- operator is forgotten rather than resolved.
--
-- Dropping the three tables is the whole reversal: their triggers, checks
-- and indexes fall with them, 0021 created no trigger on any other table
-- (the 0013 lesson), and raise_immutable() belongs to 0001 and stays. The
-- order is the reverse of creation; nothing here references anything else
-- in the set, so it is convention, not dependency.

drop table event_dead_letter;
drop table event_delivery;
drop table subscriber_checkpoint;
