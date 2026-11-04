# BookOrbit Librarian Service

A resumable metadata curation worker and BookOrbit client, using source evidence
and inexpensive models.

## Project map

- `cmd/librarian`: service entry point and operational commands.
- `internal/bookorbit`: authenticated upstream API adapter.
- `internal/curator`: model judgments and evidence-constrained metadata patches.
- `internal/domain`: shared typed records.
- Other internal packages: configuration, durable jobs, HTTP operations, file
  organization.

## Dev tool commands

Use `mise run check`, `mise run test`, and `mise run build`. There is no CI gate
yet.

## Project invariants

- Credentials and real library records never belong in Git or logs. Generic test
  fixtures only.
- A model selects supplied evidence; it cannot manufacture factual metadata or
  identifiers.
- File organization must stay within the configured library root. Preserve
  source files; never overwrite a collision.
- Errors are errors, not negative matches. Persist work and outcomes before
  marking jobs complete.
- All deployment-specific configuration comes from environment variables.
