# Permanent, curated metadata store

Status: phases 0, 1 and 2a deployed; phase 2b in progress (off by default).

## Problem

rreading-glasses was a cache: gzipped JSON blobs keyed `a<id>`/`w<id>`/
`b<id>` with TTLs, rebuilt from Hardcover on expiry. That caused two kinds
of damage (details in `tmp/junk-metadata.md`):

- **Shrinking author lists.** A refresh rebuilt each list from scratch,
  and any upstream failure (429s, unstable paging, budget) produced a short
  list. Bookshelf deletes library books missing from the list.
- **Junk we can't keep out.** Hardcover author catalogues are mostly box
  sets, study guides, translations, fan works and misattributions; default
  editions are often foreign or wrong. All cleanup lived in Bookshelf and
  in bookshelf-mcp scripts. The next refresh from Hardcover could undo it
  or add junk back.

On top of that, every refresh spent Hardcover requests; the Supporter
key's 50k/day cap was hit on 2026-11-07.

## Goals

1. Hardcover is a source we sync from, not the system of record. What we
   serve never shrinks because of an upstream failure.
2. Curation (hide, merge, retitle, pick edition, allowlist) is stored here,
   wins over upstream, and survives every refresh.
3. Hardcover spend is bounded and predictable.
4. Bookshelf keeps working unchanged: the Readarr-compatible API stays.
   Two instances (ebook and audio) read from this server.

Non-goals: the Goodreads build (`cmd/rggr`), staying rebaseable on upstream
(dropped 2026-11-07).

## Phases

### 0. Stop the bleeding (done)

Stale-while-revalidate, a daily budget, additive author refresh, failed
walks don't mark the refresh done. See CLAUDE.md fix 5.

### 1. Normalized store (write-through + backfill)

New tables in the same CNPG database, next to `cache`. Rows are never
deleted by sync; only curation hides things.

```
authors        id, name, data jsonb, created_at, updated_at
works          id, title, full_title, short_title, release_date,
               rating_count, data jsonb, created_at, updated_at
editions       id, work_id, title, language, format, isbn13, asin,
               is_ebook, listed, data jsonb, created_at, updated_at
work_authors   work_id, author_id, position          -- credit order
author_works   author_id, work_id, first_seen
series         id, title, data jsonb, created_at, updated_at
series_works   series_id, work_id, position, sort, is_primary
```

- `data` holds the resource JSON as we serve it, minus nested relations;
  the columns are the fields curation and lookups query.
- `author_works` is exactly the served author list, so phase 2 can serve
  from the store and match today's responses.
- `editions.listed` marks editions included in the work's `Books` (today's
  denormalized subset). Seeing a work doesn't list all its editions.
- No foreign keys: rows arrive in any order (a book before its work).

**Write-through.** `StoreCache` wraps the existing cache. Every `Set` of
an author, work, book or series blob also upserts the normalized rows.
`Expire`/`Delete` don't touch the store. Responses are unchanged in this
phase.

**Backfill.** On startup, if it hasn't run yet, a background job streams
every `a`/`w`/`b`/`s` row from `cache` (expired ones too; nothing ever
deleted them) through the same upsert path. It makes no Hardcover requests.
`rghc backfill` reruns it.

**Migrations.** Numbered SQL files embedded in the binary, applied at
startup and tracked in `schema_migrations`.

The store holds our mapped resources, not raw Hardcover payloads. Fields
the mapping drops (`audio_seconds`, `users_count`, alternative titles,
contributor roles on works) come back in phase 3, when the syncer stores
raw payloads.

### 2. Curation and serving from the store

Tables:

```
author_curation  author_id, mode ('rules'|'allowlist'), merged_into, note
work_curation    work_id, status ('canon'|'hidden'|null), merged_into,
                 title, edition_id, primary_author_id, reason, source,
                 updated_at
edition_curation edition_id, language, hidden, reason
rule_verdicts    work_id|edition_id, rule, verdict, reason, computed_at
curation_log     at, kind, entity_id, before, after, source
```

Every curation change is logged with before/after, so imports and bulk
edits can be audited and undone.

The effective view is upstream, then rules, then manual curation; manual
always wins.

- **Allowlist mode** (default for library authors): the author list is
  only `canon` works. **Rules mode** filters with the rule verdicts.
- **Rules**, re-run offline from stored data: junk-title regex (from
  `pick.py`), foreign-title detection (from `foreign.py`), default-edition
  scoring (prefer a title matching the work's title; penalize audio,
  foreign scripts, marketing suffixes), primary author = first `Author`
  credit, same-normalized-title siblings.
- **Merge** B into A: B's editions are served under A, B leaves author
  lists, ISBN/ASIN lookups of B's editions resolve to A.
- Hidden works stay fetchable by ID (`/work/<id>`), so hand-added books
  don't break; they only leave author lists and search.
- Serving: curation is overlaid on the cached author and work responses in
  the handler (`internal/serve.go`), behind `--serve-curated` (off by
  default). With no curation touching an author the response is
  byte-identical. Rules mode only removes works curation names, so a store
  that lags the cache can't shrink a list. The cache keeps upstream data. A
  curation failure returns an error rather than an uncurated list. Works
  curation adds, and editions merged in, come from the store. Not done yet:
  ISBN/ASIN and `/book/<id>` lookups of merged works, and search.

Import the existing curation: `canon.tsv` / `match.py` output, the
`keep-<aid>.txt` lists, `siblings.tsv`, the edition picks.

**MCP** (new, Python, like bookshelf-mcp) over an admin HTTP API on its
own port with a bearer token. Tools: list an author's works with verdicts,
set mode, mark canon/hidden, merge, set title/edition/language, preview.

**Danger:** switching an author to allowlist removes every non-canon work
from its list, and Bookshelf deletes those entries on the next author
refresh. The preview tool must diff against the Bookshelf library first
(books with files, monitored books), and the switch should refuse
when it would drop a book that has a file unless forced.

The admin port is exposed on a LAN-only ingress (decided 2026-11-07).

**Admin API (2a).** Separate listener, `--admin-port` (8789), off unless
`ADMIN_TOKEN` or `ADMIN_TOKEN_FILE` is set; every request needs
`Authorization: Bearer <token>`. JSON in and out, errors as
`{"error": ...}` with 400/401/404/413/500.

```
GET  /admin/authors?q=&limit=                     search by name or ID
GET  /admin/authors/{id}?status=&q=&limit=&offset= candidate works + verdicts
GET  /admin/authors/{id}/preview                  served vs curated diff
GET  /admin/works?q=&author=&limit=
GET  /admin/works/{id}                            editions, credits, merges
GET  /admin/editions?isbn=|asin=
GET  /admin/log?kind=&id=&limit=
POST /admin/curation                              {source, authors, works, editions}
```

`POST /admin/curation` takes partial patches keyed by `id`: absent fields
are kept, null or `""` clears. The whole request is one transaction;
merge chains, self-merges, unknown IDs and editions from unrelated works
are rejected. No-op patches don't log. `dry_run: true` applies the patches,
returns previews (served vs curated) for every affected author, and rolls
back. `GET .../preview?mode=allowlist` previews a mode without switching.

The MCP (`mcp/`, Python, `rg-mcp`) dry-runs every write, matches removed
works against both Bookshelf libraries (ebook and audio, read-only) and
refuses when a book with files would drop, unless forced. Canon adds a work for every
credited author; `primary_author_id` restricts it to one.

Open: whether Bookshelf follows a work whose ForeignId was merged (needs a
test).

Split: **2a** curation tables, audit log, admin API, preview (nothing
served changes); **2b** serve from the store with curation applied;
**2c** rules and verdicts; MCP alongside 2a.

### Changes Bookshelf can observe

Upgrading or patching Bookshelf? Check these first.

- **`IsEbook` (2026-11-08):** Hardcover's `eBook` format used to map to
  false and now maps to true (`hcIsEbook`). It only changes as entries
  refresh (editions 180d, works 90d, or a `?full=1` bust), so true and false
  coexist for months, in cached responses and in `editions.is_ebook`. How
  Bookshelf uses the field hasn't been verified.

### 3. Budgeted syncer

Replace TTL-driven refresh with a scheduler: refresh the oldest rows first,
library authors first, within the budget. Use bulk queries
(`books(where: {id: {_in: [...]}})`) to cut request count. Store raw
payloads and `last_seen_upstream` per row. Persist the budget counter in
Postgres so restarts don't reset it.
