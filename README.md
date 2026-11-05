# BookOrbit Librarian Service

A Go worker and operational client that curates a BookOrbit library using
bibliographic source records and inexpensive language models. It searches
through BookOrbit's existing metadata providers, asks a model to judge identity
and select fields, then builds changes directly from the selected evidence. It
can write metadata back into book files and group alternate formats into
organized folders.

Built against BookOrbit 3.6.0. This is an independent project with no CI gate
yet. The file organizer depends on BookOrbit's PostgreSQL schema; review
compatibility before upgrading BookOrbit.

## How it works

1. Discover books and enqueue durable SQLite jobs. An inventory-wide discovery
   call considers titles, authors, identifiers, filenames, nearby folder names,
   and existing series fields as uncertain hints. It finds related works even
   when series fields are absent or dirty. The resulting groups are editorial
   hypotheses, not bibliographic assertions.
2. Search BookOrbit's configured providers for each volume, retaining linked
   edition records and making an independent title/author search without old
   provider IDs or ISBNs. This avoids a bad existing link pinning all evidence
   to the wrong work or language. Partial results carry warnings; search
   failures retry.
3. For each discovered group, present labeled contact sheets of current and
   candidate covers to the vision model (default Haiku). It describes visual
   families, visible text and defects. Calls describe at most 48 target covers;
   larger groups share an overview of all current covers in every batch. This
   bounds response size without splitting the final group-wide decision.
4. Give those observations and source records to the metadata model (default
   Terra). It resolves the actual works and series inside the provisional group,
   selects fields across matching providers and chooses coordinated covers for
   related works without forcing every group member into the same series. Both
   tasks run for every group with images; there is no confidence or review
   ladder.
5. Save the entire group plan in one SQLite transaction before any edits.
   Sibling jobs reuse it. Each field records its provider; edition fields
   require explicit edition evidence. Locked fields and good current values are
   kept.
6. Back up originals outside the scanned library, apply metadata and covers
   through BookOrbit, write into the book files, then organize files and catalog
   paths using a recovery journal.

The service sends bibliographic metadata, basenames, nearby folder names and
cover contact sheets to its model endpoint. It does not send book contents or
full filesystem paths. Models select supplied evidence, not new bibliographic
facts or invented cover art. If no suitable coordinated family exists,
appropriate current covers can remain. Translations, guides and omnibuses are
considered on their own merits.

The role split was informed by a small live comparison on identical library
examples: both models recognized the same cover family, but Haiku proposed
artwork that wasn't supplied and made an inconsistent edition selection. Terra
kept the edition uncertain and distinguished visible evidence from catalog
claims. This is a concrete design observation, not a broad accuracy benchmark.
The old Polished collection is useful context, not a gold-standard evaluation
set. Saved source records, visual observations and decisions make the results
inspectable; original files remain available for rollback.

## Run

Install [mise](https://mise.jdx.dev), then:

```sh
mise trust
mise install
mise run check
mise run test
mise run build
```

Configuration comes entirely from environment variables; see `.env.example`. The
service does not implicitly read dotenv files. Supply them through your process
manager or container runtime. Start with `bin/librarian serve`.

For a container:

```sh
podman build --build-arg VERSION=local -t localhost/bookorbit-librarian .
podman run --rm --env-file /private/librarian.env \
  --user 1026:100 --read-only --tmpfs /tmp \
  -v /private/librarian-state:/data \
  -v /your/books-parent:/books \
  localhost/bookorbit-librarian
```

Join the network hosting BookOrbit and PostgreSQL, and use the same book paths
as BookOrbit sees. Mount the library and originals writable; keep SQLite on
local storage. The container UID needs access to both mounts. The Dockerfile's
Go image is pinned to the Linux amd64 manifest; replace it with a suitable
pinned manifest when building for another architecture. Use `/librarian health`
for OCI health.

## Configuration

| Variable                          | Meaning / default                                                    |
| --------------------------------- | -------------------------------------------------------------------- |
| `ADMIN_TOKEN`                     | Required, at least 24 characters; bearer token for operations        |
| `BOOKORBIT_URL`                   | Required HTTP(S) base URL, with optional `/api/v1` suffix            |
| `BOOKORBIT_MAGIC_LINK_TOKEN`      | Required opaque application login token                              |
| `MODEL_BASE_URL`, `MODEL_API_KEY` | Required OpenAI chat completions endpoint and key                    |
| `MODEL_VISION`                    | `claude/haiku-5-5`; use a name supported by your proxy               |
| `MODEL_REQUEST_INTERVAL`          | `7s` between model requests across workers; range `0s`–`1m`          |
| `MODEL_METADATA`                  | `chatgpt/gpt-6.1-terra`; reconciles sources and chooses covers       |
| `METADATA_PROVIDERS`              | `hardcover,goodreads`; provider keys already configured in BookOrbit |
| `LIBRARY_ID`                      | `1`                                                                  |
| `LIBRARY_ROOT`                    | Required absolute library root, matching BookOrbit paths             |
| `ORIGINALS_ROOT`                  | Required non-overlapping backup directory outside the scanned root   |
| `BOOKORBIT_DATABASE_URL`          | Required only for file organization                                  |
| `STATE_PATH`                      | `/data/librarian.db`                                                 |
| `LISTEN_ADDR`                     | `:8080`                                                              |
| `WORKERS`                         | `1`, maximum `4`; applying changes is serialized                     |
| `MAX_ATTEMPTS`                    | `3`; automatic retries preserve saved proposals                      |
| `JOB_TIMEOUT`                     | `20m`, allowed range `1m`–`30m`                                      |
| `AUTO_APPLY`                      | `false`; otherwise proposals finish as `planned`                     |
| `ORGANIZE_FILES`                  | `false`                                                              |
| `AUTO_DISCOVER`                   | `false`; discover on startup and periodically when enabled           |
| `DISCOVERY_INTERVAL`              | `1h`, minimum `1m`                                                   |

Use a dedicated shared BookOrbit account with library editor access and
`library_edit_metadata` and `library_download` permissions. The client exchanges
its opaque magic-link token for access tokens and renews on HTTP 401.
Application magic links work with BookOrbit's local password authentication
disabled.

Use a restricted proxy key with model allowlists, request limits and a budget.
There is no currency budget calculation inside this worker. HTTP/model failures
are errors, not evidence of a missing book. The TypeSafe experimental decision
model is not used in this version.

## Operations

The same binary provides an authenticated HTTP client. Inside the container it
uses the service's `ADMIN_TOKEN` without exposing that token in command
arguments:

```sh
podman exec homelab-bookorbit-librarian /librarian request GET /v1/status
podman exec homelab-bookorbit-librarian /librarian request GET /v1/discovery
podman exec homelab-bookorbit-librarian /librarian request POST /v1/discovery
podman exec homelab-bookorbit-librarian /librarian request POST /v1/jobs '{"bookIds":[42,43]}'
podman exec homelab-bookorbit-librarian /librarian request POST /v1/jobs '{}'
podman exec homelab-bookorbit-librarian /librarian request GET '/v1/jobs?status=failed'
podman exec homelab-bookorbit-librarian /librarian request GET /v1/jobs/1
podman exec homelab-bookorbit-librarian /librarian request POST /v1/jobs/1/retry
podman exec homelab-bookorbit-librarian /librarian request POST /v1/pause
podman exec homelab-bookorbit-librarian /librarian request POST /v1/resume
```

`GET /v1/discovery` shows the persisted library-wide groups and rationales.
`POST /v1/discovery` rebuilds them from the current whole inventory. Discovery
otherwise reuses its saved snapshot until new book IDs appear; metadata writes
and file moves do not repeatedly trigger global model calls. Books with no
useful relationship hints can remain isolated until better catalog evidence is
available.

`SERVICE_URL` overrides the client's default `http://127.0.0.1:8080`.
`HEALTH_URL` similarly overrides the health command's default `/healthz` URL.
`GET /healthz` is public and checks SQLite availability. All `/v1/` endpoints
require the admin bearer token. `POST /v1/jobs` with `{}` discovers the whole
configured library; `{"limit":10}` bounds discovery. Listing returns the latest
100 jobs, optionally filtered by status. Retrieve a job to inspect its complete
proposal and resulting moves/merges. There is no web UI or manual patch editor.

States are `pending`, `running`, `planned`, `applied`, `failed`, and `skipped`.
Expired leases recover after interruption. Explicit retries of failed or planned
jobs gather fresh evidence; automatic retries reuse saved proposals. Pause stops
claiming jobs and discovery after current work; it is in memory and resets on
restart. Stop the unit for a durable maintenance pause.

## Activity page

The read-only dashboard presents recorded activity as structured rows: book
thumbnail, outcome, added/updated field badges, metadata completeness before and
after, and source providers. Filters isolate major updates, cover replacements,
description changes, provider-link-only edits, unchanged metadata and unresolved
errors. Open a row for its cached LLM explanation and complete field/source
diff; there is no separate prose stream. Retry events remain visible and are
marked recovered after a later successful outcome.

Completeness counts nine populated fields (title, authors, description,
language, publisher, publication date/year, page count, ISBN and genres).
Optional series/subtitle and provider links are excluded. It measures missing
data, not correctness. A major update changes at least three bibliographic
fields, or a cover plus a bibliographic field. Dashboard counters use each job's
latest outcome; filter counts count activity events. Historical completeness is
derived from saved proposals when it was not recorded on the event.

Cover thumbnails are served privately from BookOrbit with a five-minute cache.
New cover replacements save small before/after images under `activity-covers/`
next to the SQLite database. They persist across restarts. Earlier before images
cannot be reconstructed; the UI labels the fallback as the current cover.
Capture failures do not block curation. The page refreshes every 30 seconds
while entries are closed; pagination and open details pause refresh.

Set `UI_PROXY_TOKEN` to a separate secret of at least 24 characters. Put the
service behind an authenticating reverse proxy such as Authelia and inject
`X-Librarian-UI-Token` only after authentication. That capability grants the
activity page only; the `/v1/` admin API still requires `ADMIN_TOKEN`. An admin
bearer token can also read the page. With no UI token configured the page is
disabled. Never expose a header-trusting proxy without authentication.

`BOOKORBIT_PUBLIC_URL` enables links back to books. `MODEL_SUMMARY` defaults to
the configured vision model; `ACTIVITY_SUMMARY_INTERVAL` defaults to `5m` and
must be at least `1m`. Summaries are generated from new recorded outcomes, saved
in SQLite, and reused across requests. Page refreshes never call a model.
Summary failures do not block curation; raw activity remains available.

Activity is an append-only private SQLite log, separate from sparse operational
stdout. Existing job outcomes are imported on first startup; past retry
transitions that were overwritten before this feature cannot be reconstructed.
Activity entries can include titles and bibliographic changes, but never
credentials or book contents. Preserve the SQLite database along with originals
and BookOrbit backups. LLM summaries are a reading guide; saved events are the
record of what happened.

## File organization and recovery

Back up BookOrbit's database and application data before enabling organization.
Disable its scheduled scans, filesystem watcher and automatic file renaming. Use
`book_per_folder` organization and `file_write_all_files=true`. Do not start a
manual scan while the worker is moving files. Startup schedules may be cached,
so apply library settings through BookOrbit or restart it after SQL changes.

Files are laid out as `Author/[Series]/Title [book-ID]/Title.ext`. Stable IDs
avoid ambiguous collisions. A duplicate extension gets a file-ID suffix; files
are never overwritten. Alternate formats are grouped only with matching
Hardcover edition IDs, or matching ISBN plus normalized title, language and
authors, within one library folder. Different or uncertain editions stay
separate. Empty source folders may remain. Grouped catalog entries lose the
removed entry's user state; this service does not preserve progress,
annotations, ratings or collections when merging. Actual distinct files are
retained, including duplicate formats.

The PostgreSQL role needs `CONNECT` on the database, `USAGE` on `public`,
`SELECT` on `books`, `book_files`, `book_metadata`, `libraries`,
`library_folders`, `book_authors`, `authors`, `scan_jobs`; `UPDATE` on `books`,
`book_files`; `UPDATE(id)` on `libraries`; and `DELETE` on `books`. It does not
need superuser access or schema mutation permissions. Additional grants required
by future BookOrbit schema changes should be reviewed narrowly.

Moves claim destinations with hard links before unlinking the source. When NAS
ownership prevents linking an untouched source, a verified temporary copy
supplies the destination link. Source and destination must be on the same
filesystem. Journals under `ORIGINALS_ROOT/journals` record hashes and both
paths before moves. Recovery reconciles them against committed catalog paths,
retains the matching file and stops on ambiguity. Do not edit these journals by
hand or run two service instances against the same library.

Original copies live at `ORIGINALS_ROOT/books/<book-ID>/`, with a
`manifest.json` that records the original catalog metadata, source paths and
file hashes. Backups are copies, not hard links to files being edited. For
rollback, stop the worker and BookOrbit, preserve the current state, restore the
matching database/application backup, then restore original book bytes to the
paths in the manifests. Verify those paths stay under your library root. SQLite
is the job/evidence store and must be backed up too (stop the service or use
SQLite's backup API).

A library with unprocessed or unresolved flat files is not yet safe for a folder
scan. Keep scans disabled through the initial pass; resolve remaining jobs
before resuming them. New files can be added through BookOrbit uploads, then
discovered by the worker. Do not treat an old "Polished" collection as trusted
completion.

## Development

`mise run fix` applies formatting and lint fixes. `mise run check` checks
formatting, module tidiness and lint; `mise run test` runs tests with the race
detector. Fixtures cover API wire formats and auth, series plan reuse, visual
inputs, field/edition sources, backups, paths, collisions, lease recovery and
authenticated operations. There is no external service access in these tests.
Deployment requires a small real-book exercise against the exact BookOrbit
version and filesystem.
