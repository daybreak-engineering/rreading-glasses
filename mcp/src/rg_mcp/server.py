"""MCP server for curating the rreading-glasses metadata store."""

from __future__ import annotations

import functools
import json
from typing import Annotated, Any, Literal

from mcp.server.mcpserver import MCPServer
from mcp.server.mcpserver.exceptions import ToolError
from mcp_types import ToolAnnotations
from pydantic import Field

from .client import AdminClient, BookshelfClient, CurationError, Settings
from .guard import library_conflicts, summarize_previews

INSTRUCTIONS = """\
Curate the book metadata rreading-glasses serves to Bookshelf (Hardcover IDs:
authors, works = Bookshelf books, editions). Curation is stored there, wins over
Hardcover, and survives refreshes. Every change is logged with before/after.

Concepts:
- Author mode: `rules` serves the upstream list minus hidden works plus canon
  ones; `allowlist` serves only works marked `canon`.
- Work status: `canon`, `hidden`, or unset. Canon adds a work for every
  credited author; set `primary_author_id` to keep it with one author.
- Merge work B into A: B leaves author lists, its editions are served under A.
  Targets can't themselves be merged.
- Overrides: work title and default edition; edition language and hidden.

Flow: `search_authors` -> `get_author_works` (filter by status) -> `get_work`
for editions -> write tools -> `get_curation_log` to audit.
`preview_author(mode="allowlist")` shows what switching would drop.

Safety: every write is dry-run first and checked against the Bookshelf library.
It is refused if it would drop a library book that has files, unless
`force=true`. Bookshelf deletes books that leave an author's list.
Use `dry_run=true` on any write tool to see the effect without applying it.
"""

mcp = MCPServer("rreading-glasses", instructions=INSTRUCTIONS)

_settings: Settings | None = None
_admin: AdminClient | None = None
_bookshelf: BookshelfClient | None = None
_bookshelf_set = False


def settings() -> Settings:
    global _settings
    if _settings is None:
        _settings = Settings.from_env()
    return _settings


def admin() -> AdminClient:
    global _admin
    if _admin is None:
        _admin = AdminClient(settings())
    return _admin


def bookshelf() -> BookshelfClient | None:
    global _bookshelf, _bookshelf_set
    if not _bookshelf_set:
        s = settings()
        _bookshelf = BookshelfClient(s) if s.bookshelf_url and s.bookshelf_api_key else None
        _bookshelf_set = True
    return _bookshelf


def tool(**kwargs: Any) -> Any:
    """Register a tool whose result is returned as one compact JSON text block."""

    def deco(fn: Any) -> Any:
        @functools.wraps(fn)
        async def wrapper(*args: Any, **kw: Any) -> str:
            try:
                res = await fn(*args, **kw)
            except (CurationError, ValueError, OSError) as e:
                raise ToolError(str(e)) from e
            if res is None:
                res = {"ok": True}
            return json.dumps(res, separators=(",", ":"), ensure_ascii=False)

        mcp.add_tool(wrapper, **kwargs)
        return fn

    return deco


RO = ToolAnnotations(read_only_hint=True, open_world_hint=False)
IDEMPOTENT = ToolAnnotations(read_only_hint=False, destructive_hint=False, idempotent_hint=True, open_world_hint=False)
# Curation can remove works from author lists, which Bookshelf acts on.
REMOVING = ToolAnnotations(read_only_hint=False, destructive_hint=True, idempotent_hint=True, open_world_hint=False)

Force = Annotated[bool, Field(description="Apply even if it drops library books that have files")]
DryRun = Annotated[bool, Field(description="Preview the effect and library conflicts without applying")]
Note = Annotated[str | None, Field(description="Why; stored with the curation")]
Source = Annotated[str, Field(description="Recorded in the curation log")]


async def _apply(body: dict[str, Any], force: bool, dry_run: bool) -> dict[str, Any]:
    """Dry-run a curation request, check it against the library, then apply it."""
    body = {k: v for k, v in body.items() if v}
    if settings().read_only and not dry_run:
        raise CurationError("read-only mode (RG_READ_ONLY): pass dry_run=true")

    dry = await admin().curate({**body, "dry_run": True})
    previews = dry.get("previews") or []
    conflicts = await library_conflicts(bookshelf(), previews)
    removes = any(p.get("removed") for p in previews)
    summary = summarize_previews(previews)
    report = {"previews": summary, "library": conflicts}

    if dry_run:
        return {"dry_run": True, "result": _counts(dry), **report}
    if not force:
        if conflicts["with_files"]:
            raise CurationError(
                "refused: would drop library books that have files (pass force=true to apply anyway): "
                + json.dumps(conflicts["with_files"], separators=(",", ":"), ensure_ascii=False)
            )
        if removes and not conflicts["checked"]:
            raise CurationError(
                "refused: this removes works from author lists and BOOKSHELF_URL/BOOKSHELF_API_KEY aren't set, "
                "so the library can't be checked (pass force=true to apply anyway)"
            )

    res = await admin().curate(body)
    out: dict[str, Any] = {"result": _counts(res), "previews": summary}
    if conflicts["with_files"] or conflicts["monitored"]:
        out["library"] = conflicts
    return out


def _counts(res: dict[str, Any]) -> dict[str, Any]:
    return {k: res.get(k, 0) for k in ("authors", "works", "editions", "unchanged")}


def _patch(entity_id: int, fields: dict[str, Any], clear: list[str] | None) -> dict[str, Any]:
    p: dict[str, Any] = {"id": entity_id}
    p.update({k: v for k, v in fields.items() if v is not None})
    for k in clear or []:
        if k in p and k != "id":
            raise ValueError(f"{k} is both set and cleared")
        p[k] = None
    return p


# Reads


@tool(annotations=RO)
async def search_authors(
    query: Annotated[str, Field(description="Name substring or exact Hardcover author ID; empty lists all")] = "",
    limit: Annotated[int, Field(ge=1, le=200)] = 20,
) -> Any:
    """Find authors in the store, largest served lists first, with their curation mode."""
    return await admin().get("authors", q=query, limit=limit)


@tool(annotations=RO)
async def get_author_works(
    author_id: int,
    status: Annotated[
        Literal["canon", "hidden", "none", "served", "excluded"] | None,
        Field(description="none = uncurated; served/excluded = kept/dropped with curation applied"),
    ] = None,
    query: Annotated[str | None, Field(description="Title substring")] = None,
    limit: Annotated[int, Field(ge=1, le=5000)] = 100,
    offset: Annotated[int, Field(ge=0)] = 0,
) -> Any:
    """List an author's candidate works (served list, credits, overrides), most-rated first.

    Each work shows whether it's in today's served list, the author's credit
    position, whether curation keeps it and why not, its curation, best edition,
    edition count and languages.
    """
    return await admin().get(f"authors/{author_id}", status=status, q=query, limit=limit, offset=offset)


@tool(annotations=RO)
async def get_work(work_id: int) -> Any:
    """Get a work with credits, series, curation, works merged into it and all known editions."""
    return await admin().get(f"works/{work_id}")


@tool(annotations=RO)
async def search_works(
    query: Annotated[str | None, Field(description="Title substring")] = None,
    author_id: Annotated[int | None, Field(description="Only works listing or crediting this author")] = None,
    limit: Annotated[int, Field(ge=1, le=500)] = 50,
) -> Any:
    """Find works by title and/or author."""
    if not query and not author_id:
        raise ValueError("pass query and/or author_id")
    return await admin().get("works", q=query, author=author_id, limit=limit)


@tool(annotations=RO)
async def find_editions(isbn: str | None = None, asin: str | None = None) -> Any:
    """Find editions by ISBN-13 or ASIN (exactly one)."""
    return await admin().get("editions", isbn=isbn, asin=asin)


@tool(annotations=RO)
async def get_curation_log(
    kind: Literal["author", "work", "edition"] | None = None,
    entity_id: int | None = None,
    limit: Annotated[int, Field(ge=1, le=1000)] = 50,
) -> Any:
    """Recent curation changes with before/after, newest first."""
    return await admin().get("log", kind=kind, id=entity_id, limit=limit)


@tool(annotations=RO)
async def preview_author(
    author_id: int,
    mode: Annotated[
        Literal["rules", "allowlist"] | None, Field(description="Preview this mode instead of the current one")
    ] = None,
    limit: Annotated[int, Field(ge=1, le=5000, description="Max works listed per side")] = 200,
) -> Any:
    """Compare an author's served list with the curated one, and flag library books it would drop."""
    p = await admin().get(f"authors/{author_id}/preview", mode=mode)
    conflicts = await library_conflicts(bookshelf(), [p])
    return {**summarize_previews([p], limit)[0], "library": conflicts}


# Writes


@tool(annotations=REMOVING)
async def set_author_mode(
    author_id: int,
    mode: Literal["rules", "allowlist"],
    note: Note = None,
    force: Force = False,
    dry_run: DryRun = False,
    source: Source = "mcp",
) -> Any:
    """Switch an author between rules and allowlist mode."""
    patch = _patch(author_id, {"mode": mode, "note": note}, None)
    return await _apply({"source": source, "authors": [patch]}, force, dry_run)


@tool(annotations=REMOVING)
async def set_works_status(
    work_ids: Annotated[list[int], Field(min_length=1)],
    status: Annotated[Literal["canon", "hidden", "clear"], Field(description="clear removes the status")],
    note: Note = None,
    force: Force = False,
    dry_run: DryRun = False,
    source: Source = "mcp",
) -> Any:
    """Mark works canon or hidden, or clear their status. All or nothing."""
    value = None if status == "clear" else status
    patches = [{"id": w, "status": value, **({"note": note} if note is not None else {})} for w in work_ids]
    return await _apply({"source": source, "works": patches}, force, dry_run)


@tool(annotations=REMOVING)
async def merge_works(
    source_ids: Annotated[list[int], Field(min_length=1, description="Works to merge away")],
    target_id: Annotated[int, Field(description="Work they're served as; must not itself be merged")],
    note: Note = None,
    force: Force = False,
    dry_run: DryRun = False,
    source: Source = "mcp",
) -> Any:
    """Merge duplicate works into one: sources leave author lists, their editions serve under the target."""
    patches = [{"id": w, "merged_into": target_id, **({"note": note} if note is not None else {})} for w in source_ids]
    return await _apply({"source": source, "works": patches}, force, dry_run)


@tool(annotations=REMOVING)
async def update_work(
    work_id: int,
    title: Annotated[str | None, Field(description="Title override")] = None,
    edition_id: Annotated[int | None, Field(description="Default edition (of this work or one merged into it)")] = None,
    primary_author_id: Annotated[int | None, Field(description="Only this author lists the work")] = None,
    note: Note = None,
    clear: Annotated[
        list[Literal["title", "edition_id", "primary_author_id", "status", "merged_into", "note"]] | None,
        Field(description="Fields to reset to upstream"),
    ] = None,
    force: Force = False,
    dry_run: DryRun = False,
    source: Source = "mcp",
) -> Any:
    """Override a work's title, default edition or primary author, or clear overrides (unmerge, unhide)."""
    fields = {"title": title, "edition_id": edition_id, "primary_author_id": primary_author_id, "note": note}
    patch = _patch(work_id, fields, clear)
    if len(patch) == 1:
        raise ValueError("nothing to change")
    return await _apply({"source": source, "works": [patch]}, force, dry_run)


@tool(annotations=IDEMPOTENT)
async def update_edition(
    edition_id: int,
    language: Annotated[str | None, Field(description="Language override, e.g. eng")] = None,
    hidden: bool | None = None,
    note: Note = None,
    clear: list[Literal["language", "note"]] | None = None,
    dry_run: DryRun = False,
    source: Source = "mcp",
) -> Any:
    """Override an edition's language or hide it."""
    patch = _patch(edition_id, {"language": language, "hidden": hidden, "note": note}, clear)
    if len(patch) == 1:
        raise ValueError("nothing to change")
    return await _apply({"source": source, "editions": [patch]}, False, dry_run)


@tool(annotations=REMOVING)
async def curate(
    authors: Annotated[
        list[dict[str, Any]] | None,
        Field(description='Patches like {"id": 1, "mode": "allowlist", "merged_into": null, "note": "..."}'),
    ] = None,
    works: Annotated[
        list[dict[str, Any]] | None,
        Field(
            description='Patches like {"id": 1, "status": "canon"|"hidden"|null, "merged_into": 2, "title": "...", '
            '"edition_id": 3, "primary_author_id": 4, "note": "..."}'
        ),
    ] = None,
    editions: Annotated[
        list[dict[str, Any]] | None,
        Field(description='Patches like {"id": 1, "language": "eng", "hidden": true, "note": "..."}'),
    ] = None,
    source: Source = "mcp",
    force: Force = False,
    dry_run: DryRun = False,
) -> Any:
    """Apply many patches in one transaction (bulk edits, imports).

    Absent fields are unchanged; null or "" clears. Use a descriptive source
    for imports, e.g. "import:canon.tsv".
    """
    if not (authors or works or editions):
        raise ValueError("pass authors, works and/or editions")
    return await _apply({"source": source, "authors": authors, "works": works, "editions": editions}, force, dry_run)


def main() -> None:
    mcp.run()
