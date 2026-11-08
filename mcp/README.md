# rg-mcp

MCP server for curating the rreading-glasses metadata store through its admin
API (`docs/design.md`, phase 2). Curation is stored in rreading-glasses, wins
over Hardcover and survives refreshes. Every change is logged with before and
after.

## Configuration

| Variable              | Required | Description                                          |
|-----------------------|----------|------------------------------------------------------|
| `RG_ADMIN_URL`        | yes      | Admin API base, e.g. `https://rg-admin.lan`          |
| `RG_ADMIN_TOKEN`      | yes*     | Bearer token (the server's `ADMIN_TOKEN`)            |
| `RG_ADMIN_TOKEN_FILE` | yes*     | File holding the token, instead of `RG_ADMIN_TOKEN`  |
| `BOOKSHELF_URL`       | no       | Ebook Bookshelf base URL, for the library check      |
| `BOOKSHELF_API_KEY`   | no       | Ebook Bookshelf API key                              |
| `BOOKSHELF_AUDIO_URL` | no       | Audiobook Bookshelf base URL                         |
| `BOOKSHELF_AUDIO_API_KEY` | no   | Audiobook Bookshelf API key                      |
| `RG_READ_ONLY`        | no       | `true` allows only reads and dry runs                |
| `RG_TIMEOUT`          | no       | Request timeout in seconds (default 120)             |

## Library check

Both Bookshelf instances (ebook and audio) take their metadata from the same
rreading-glasses server, and each deletes books that drop out of an author's
list on its next author refresh. Every write is first sent as a dry run,
which returns previews of the affected authors. Removed works are matched
against each library by `foreignAuthorId` / `foreignBookId`:

- a removed book **with files** refuses the write unless `force=true`;
- a removed book that is only monitored is reported as a warning;
- if either instance isn't configured, any write that removes works needs
  `force=true`.

Bookshelf is only read, never written.

## Use with Claude Code

```bash
claude mcp add rreading-glasses \
  --env RG_ADMIN_URL=https://rg-admin.lan \
  --env RG_ADMIN_TOKEN_FILE=/path/to/token \
  --env BOOKSHELF_URL=http://bookshelf.lan \
  --env BOOKSHELF_API_KEY=your-key \
  --env BOOKSHELF_AUDIO_URL=http://bookshelf-audio.lan \
  --env BOOKSHELF_AUDIO_API_KEY=your-audio-key \
  -- uv --directory /path/to/rreading-glasses/mcp run rg-mcp
```

## Tools

| Area   | Tools |
|--------|-------|
| Read   | `search_authors`, `get_author_works`, `get_work`, `search_works`, `find_editions`, `get_curation_log`, `preview_author` |
| Write  | `set_author_mode`, `set_works_status`, `merge_works`, `update_work`, `update_edition`, `curate` (bulk patches) |

Write tools take `dry_run` and `force`, plus a `source` recorded in the log
(default `mcp`; use e.g. `import:canon.tsv` for imports).

## Development

```bash
uv sync
uv run pytest
uv run ruff check . && uv run ruff format --check .
```
