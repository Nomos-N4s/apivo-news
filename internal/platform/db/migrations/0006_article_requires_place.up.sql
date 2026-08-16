-- 0006_article_requires_place: an article must name at least one place.
--
-- The front page is scoped by place (FR-009): ListFrontPage shows an
-- article only where an article_place row joins it to a followed place.
-- An article with no such row is therefore invisible to every reader on
-- every front page - published, approved, attributed, and unreachable,
-- silently, behind a 200 and an empty list. That state served no purpose
-- and carried a real cost, so it becomes unrepresentable: the decision is
-- the database's, not a field somebody remembers to pass (I-1 spirit,
-- same reasoning as the named-approver rule).
--
-- The shape is an AFTER INSERT constraint trigger, DEFERRABLE INITIALLY
-- DEFERRED, not a NOT NULL column: places are many-to-many and the schema
-- already models them as article_place rows, so the rule is "at least one
-- row exists", which no column constraint can say. DEFERRED matters
-- twice. It lets the article insert and its article_place rows commit
-- together in one transaction - the approval writes both, in either
-- order. And because the check runs at COMMIT, a test that seeds an
-- article inside a rolled-back transaction is undisturbed: what never
-- commits is never checked.
--
-- article_place rows themselves are insert-only in practice (nothing
-- deletes them), but no trigger freezes them yet; the guard here covers
-- the article's birth, which is the path the product actually has.

create function article_requires_place() returns trigger
language plpgsql
as $$
begin
    if not exists (
        select 1 from article_place where article_id = new.id
    ) then
        raise exception 'article % names no place (FR-009): an article with no article_place row can appear on no front page, so it must be inserted in the same transaction as at least one', new.id;
    end if;
    return null;
end;
$$;

comment on function article_requires_place() is
    'COMMIT-time check (deferred constraint trigger) that an inserted article has at least one article_place row: the front page is scoped by place, so a placeless article would be unreachable by every reader (FR-009).';

create constraint trigger article_requires_place
    after insert on article
    deferrable initially deferred
    for each row execute function article_requires_place();
