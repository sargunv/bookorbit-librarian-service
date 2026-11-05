-- Review against the installed BookOrbit schema, then run with an administrator:
-- psql --set=role=your_librarian_role --file=ops/grants.sql
-- Extend the original organizer grants only to book-related relations.
\set ON_ERROR_STOP on
SELECT DISTINCT format('GRANT SELECT ON TABLE public.%I TO %I;',rel.relname,:'role')
FROM pg_constraint c JOIN pg_class rel ON rel.oid=c.conrelid
JOIN pg_namespace ns ON ns.oid=rel.relnamespace
WHERE c.contype='f' AND c.confrelid='public.books'::regclass AND ns.nspname='public'
\gexec
GRANT SELECT ON genres,collections TO :"role";
GRANT UPDATE(locked_fields) ON book_metadata TO :"role";
SELECT DISTINCT format('GRANT INSERT,UPDATE ON TABLE public.%I TO %I;',rel.relname,:'role')
FROM pg_constraint c JOIN pg_class rel ON rel.oid=c.conrelid
JOIN pg_namespace ns ON ns.oid=rel.relnamespace
WHERE c.contype='f' AND c.confrelid='public.books'::regclass AND ns.nspname='public'
  AND rel.relname NOT IN ('book_files','book_metadata','book_authors','book_genres',
  'book_provider_ids','book_series_memberships','book_covers','book_metadata_fetch_queue',
  'chapter_lookup_queue','book_chapter_jobs','book_duplicate_dismissals',
  'book_duplicate_group_members','book_duplicate_groups','book_duplicate_pairs','book_duplicate_scan_keys')
\gexec
