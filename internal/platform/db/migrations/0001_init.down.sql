-- Reverses 0001_init. Destroys provenance evidence: never run against an
-- environment holding real retrieved content.

drop view article_provenance;

drop table domain_event;
drop table reader_place;
drop table article_place;
drop table article;
drop function is_entitled(uuid, text);
drop table consent;
drop table account;
drop table translation;
drop table source_item;
drop table source;
drop table place;
drop table language;

drop function raise_immutable();
