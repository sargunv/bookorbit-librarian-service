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

1. Discover book IDs through the BookOrbit API and enqueue durable SQLite jobs.
   Each book is processed once per policy version; discovery does not repeatedly
   charge for completed or review-marked books.
2. Search configured providers, with the current title, authors, ISBN and book
   ID. Successful evidence from a partial provider failure remains usable, with
   a warning attached to the proposal. Complete search failures retry.
3. Ask the primary model to select a matching record and fields. Ambiguity and
   contradictory selections can escalate to a configured fallback. Unresolved
   identities enter `review`.
4. Persist the candidates, decision, token usage, field provenance and proposed
   patch before applying changes. Models select evidence; they cannot supply new
   bibliographic facts. The first version selects fields from one matched
   record.
5. Save original files with SHA-256 hashes and a metadata manifest, outside the
   scanned library. Respect locked fields and preserve edition fields unless the
   model finds evidence for the specific edition.
6. Apply metadata and covers through BookOrbit and use its file writer.
   Optionally organize files and catalog paths together using a recovery
   journal.

The service sends bibliographic metadata and basenames to the configured model
endpoint, not book contents or full filesystem paths. Source descriptions and
filenames are treated as untrusted evidence. Decisions are probabilistic; the
recorded evidence and originals make them inspectable and reversible.

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

| Variable                          | Meaning / default                                                      |
| --------------------------------- | ---------------------------------------------------------------------- |
| `ADMIN_TOKEN`                     | Required, at least 24 characters; bearer token for operations          |
| `BOOKORBIT_URL`                   | Required HTTP(S) base URL, with optional `/api/v1` suffix              |
| `BOOKORBIT_MAGIC_LINK_TOKEN`      | Required opaque application login token                                |
| `MODEL_BASE_URL`, `MODEL_API_KEY` | Required OpenAI chat completions endpoint and key                      |
| `MODEL_PRIMARY`                   | `claude/haiku-5-5`; use a name supported by your proxy                 |
| `MODEL_REQUEST_INTERVAL`          | `7s` between primary/fallback requests across workers; range `0s`–`1m` |
| `MODEL_FALLBACK`                  | Optional model used for ambiguity or invalid primary selections        |
| `METADATA_PROVIDERS`              | `hardcover,goodreads`; provider keys already configured in BookOrbit   |
| `LIBRARY_ID`                      | `1`                                                                    |
| `LIBRARY_ROOT`                    | Required absolute library root, matching BookOrbit paths               |
| `ORIGINALS_ROOT`                  | Required non-overlapping backup directory outside the scanned root     |
| `BOOKORBIT_DATABASE_URL`          | Required only for file organization                                    |
| `STATE_PATH`                      | `/data/librarian.db`                                                   |
| `LISTEN_ADDR`                     | `:8080`                                                                |
| `WORKERS`                         | `1`, maximum `4`; applying changes is serialized                       |
| `MAX_ATTEMPTS`                    | `3`; automatic retries preserve saved proposals                        |
| `JOB_TIMEOUT`                     | `8m`, allowed range `1m`–`30m`                                         |
| `AUTO_APPLY`                      | `false`; otherwise safe proposals finish as `planned`                  |
| `ORGANIZE_FILES`                  | `false`                                                                |
| `AUTO_DISCOVER`                   | `false`; discover on startup and periodically when enabled             |
| `DISCOVERY_INTERVAL`              | `1h`, minimum `1m`                                                     |

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
podman exec homelab-bookorbit-librarian /librarian request POST /v1/jobs '{"bookIds":[42,43]}'
podman exec homelab-bookorbit-librarian /librarian request POST /v1/jobs '{}'
podman exec homelab-bookorbit-librarian /librarian request GET '/v1/jobs?status=review'
podman exec homelab-bookorbit-librarian /librarian request GET /v1/jobs/1
podman exec homelab-bookorbit-librarian /librarian request POST /v1/jobs/1/retry
podman exec homelab-bookorbit-librarian /librarian request POST /v1/pause
podman exec homelab-bookorbit-librarian /librarian request POST /v1/resume
```

`SERVICE_URL` overrides the client's default `http://127.0.0.1:8080`.
`HEALTH_URL` similarly overrides the health command's default `/healthz` URL.
`GET /healthz` is public and checks SQLite availability. All `/v1/` endpoints
require the admin bearer token. `POST /v1/jobs` with `{}` discovers the whole
configured library; `{"limit":10}` bounds discovery. Listing returns the latest
100 jobs, optionally filtered by status. Retrieve a job to inspect its complete
proposal and resulting moves/merges. There is no web UI or manual patch editor.

States are `pending`, `running`, `review`, `planned`, `applied`, `failed`, and
`skipped`. Expired leases recover after interruption. Explicit retries of
failed, review or planned jobs gather fresh evidence; automatic retries reuse
saved proposals. Pause stops claiming jobs and discovery after current work; it
is in memory and resets on restart. Stop the unit for a durable maintenance
pause.

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
detector. Fixtures cover API wire formats and auth, evidence/edition gates,
backups, paths, collisions, lease recovery and authenticated operations. There
is no external service access in these tests. Deployment requires a small
real-book exercise against the exact BookOrbit version and filesystem.
