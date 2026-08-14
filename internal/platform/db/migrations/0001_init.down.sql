-- Reverses 0001_init. Destroys provenance evidence: never run against an
-- environment holding real retrieved content.

drop view article_provenance;

drop table domain_event;
drop table reader_place;
drop table article_place;
drop table article;
drop function article_guard();
drop function is_entitled(uuid, text);
drop table consent;
drop function consent_guard();
drop table account;
drop table translation;
drop table source_item;
drop function source_item_snapshot_terms();
drop table source;
drop table place;
drop table language;

drop function raise_immutable();

-- pgcrypto is deliberately NOT dropped: extensions are database-wide and
-- may predate this application or serve other consumers (Supabase
-- pre-provisions it). Rollback removes what this migration created, never
-- shared infrastructure it merely reused.
