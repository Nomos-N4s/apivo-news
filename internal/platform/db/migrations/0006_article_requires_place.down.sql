-- Reverses 0006_article_requires_place: an article with no article_place
-- row becomes representable again - and, with it, invisible to every
-- reader on every front page. Nothing already written is touched; the
-- rows the trigger required are ordinary article_place rows and stay.

drop trigger article_requires_place on article;
drop function article_requires_place();
