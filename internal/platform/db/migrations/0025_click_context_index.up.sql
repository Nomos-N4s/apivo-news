-- 0025_click_context_index: the index the per-context click rule reads
-- (T066, US7 scenario 1, FR-022).
--
-- 0012 gave the click table `click_account_clicked_at_idx (account_id,
-- clicked_at desc)` for exactly this shape of question - "how many clicks
-- has this member made lately?" - and the per-member half of the rule rides
-- it. The other half asks the same question of a DEVICE, and had nothing to
-- ride: a count over context_digest was a sequential scan of every click
-- ever recorded, run on the click-out path, which is the one request a
-- member is waiting on before a redirect.
--
-- Partial, because the column is nullable and a click with no context to
-- digest records none (FR-022). Those rows are not a context and can never
-- satisfy the rule's WHERE, so keeping them out of the index costs nothing
-- and keeps it the size of the clicks that actually carry a digest.
--
-- Descending on clicked_at, matching its sibling: the rule reads the most
-- recent window, and the oldest click IN that window is what says when the
-- limit lifts (the Retry-After a 429 owes the member).
create index click_context_clicked_at_idx
    on cashback.click (context_digest, clicked_at desc)
    where context_digest is not null;

comment on index cashback.click_context_clicked_at_idx is
    'The per-context half of the click rule (US7 scenario 1). Partial on context_digest is not null: a click with no context digested is not a device and can never match the rule.';
