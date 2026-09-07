-- 0034_click_backs_one_credit: one click backs at most one CREDIT.
--
-- (specs/004-multi-network-linkwise/data-model.md describes this change as
-- 0033; that number was taken by the reporting lag before this landed.)
--
-- cashback.entry already carries "one report backs one credit"
-- (entry_one_per_report, 0032). The click had no such rule: 0013 gave it an
-- ordinary index. With one network that is harmless, because a click
-- reference is unique (click_ref_unique) and only one network ever echoes
-- it back. With two, a reference reported by BOTH networks matches the same
-- click twice, each report is a different network_transaction_id, so
-- entry_one_per_report is satisfied by each - and the member is credited
-- twice for one purchase. One network can do it too, by reporting one
-- click's purchase under two transaction ids.
--
-- So the rule becomes what it was always meant to be: one click earns one
-- CREDIT. The second report is not lost - the crediting path recognises
-- this refusal by name and queues it for an operator, exactly as it queues
-- a reference that matched nothing (FR-034).
--
-- Reversals are excluded for the reason 0032 excludes them: a reversal cites
-- the click of the credit it undoes, and must be allowed to. A credit being
-- reversed at most once is entry_reversed_at_most_once's rule, untouched.
--
-- The "is not null" predicate is kept although Postgres treats NULLs as
-- distinct in a unique index anyway: an operator-attributed entry has no
-- click (0013's own comment), and the predicate says so in the schema
-- rather than in a comment.
--
-- The name is kept, as 0032 kept entry_one_per_report: a unique index
-- reports its own name on refusal, and open.go recognises it by that name.

drop index cashback.entry_click_id_idx;

create unique index entry_click_id_idx
    on cashback.entry (click_id)
 where click_id is not null and reversal_of_id is null;

comment on index cashback.entry_click_id_idx is
    'One click backs at most one CREDIT (spec 004, T200). A second report citing a credited click is queued for an operator rather than credited again. Reversals are excluded because a reversal cites the click of the credit it undoes; entry_reversed_at_most_once keeps a credit reversed at most once. Operator-attributed entries have no click and are outside the rule.';
