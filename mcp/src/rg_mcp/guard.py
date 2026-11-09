"""Checks curation previews against the Bookshelf libraries (ebook and audio).

Bookshelf deletes a library book that drops out of an author's list on the next
author refresh, unless one of its editions was added manually. A curation change
that removes a work holding a file must be forced.
"""

from __future__ import annotations

from typing import Any

from .client import BookshelfClient

EDITION_CHUNK = 100


async def library_conflicts(clients: list[BookshelfClient], previews: list[dict[str, Any]]) -> dict[str, Any]:
    """Find removed works that are books in any Bookshelf library.

    Returns {"checked": [instance names], "with_files": [...], "monitored": [...],
    "would_delete": [...]}; each entry names the instance, author, work, Bookshelf
    book id and file count. would_delete is unmonitored, fileless books without a
    manually added edition, which the next author refresh removes.
    """
    removing = [p for p in previews if p.get("removed")]
    out: dict[str, Any] = {"checked": [c.name for c in clients], "with_files": [], "monitored": [], "would_delete": []}
    if not removing:
        return out
    for bs in clients:
        await _check(bs, removing, out)
    return out


async def _check(bs: BookshelfClient, removing: list[dict[str, Any]], out: dict[str, Any]) -> None:
    authors = await bs.get("author") or []
    by_foreign = {str(a.get("foreignAuthorId")): a for a in authors}
    unmonitored: list[dict[str, Any]] = []
    for p in removing:
        author = by_foreign.get(str(p["author_id"]))
        if author is None:
            continue  # Not in this library.
        books = await bs.get("book", authorId=author["id"]) or []
        by_work = {str(b.get("foreignBookId")): b for b in books}
        for w in p["removed"]:
            book = by_work.get(str(w["id"]))
            if book is None:
                continue
            files = (book.get("statistics") or {}).get("bookFileCount", 0)
            entry = {
                "instance": bs.name,
                "author_id": p["author_id"],
                "author": p.get("name") or author.get("authorName"),
                "work_id": w["id"],
                "title": w.get("title") or book.get("title"),
                "reason": w.get("reason", ""),
                "bookshelf_book_id": book.get("id"),
                "files": files,
                "monitored": bool(book.get("monitored")),
            }
            if files:
                out["with_files"].append(entry)
            elif entry["monitored"]:
                out["monitored"].append(entry)
            else:
                unmonitored.append(entry)

    manual = await _manually_added(bs, [e["bookshelf_book_id"] for e in unmonitored])
    out["would_delete"].extend(e for e in unmonitored if e["bookshelf_book_id"] not in manual)


async def _manually_added(bs: BookshelfClient, book_ids: list[int]) -> set[int]:
    """Return the books with a manually added edition."""
    manual: set[int] = set()
    for i in range(0, len(book_ids), EDITION_CHUNK):
        editions = await bs.get("edition", bookId=book_ids[i : i + EDITION_CHUNK]) or []
        manual.update(e["bookId"] for e in editions if e.get("manualAdd"))
    return manual


def summarize_previews(previews: list[dict[str, Any]], limit: int = 50) -> list[dict[str, Any]]:
    """Trim each preview's work lists to `limit` entries, keeping totals."""
    out = []
    for p in previews:
        s = {k: p.get(k) for k in ("author_id", "name", "mode", "served", "curated")}
        for key in ("removed", "added"):
            works = p.get(key) or []
            s[key] = works[:limit]
            if len(works) > limit:
                s[f"{key}_total"] = len(works)
        out.append(s)
    return out


def trim_conflicts(conflicts: dict[str, Any], limit: int = 50) -> dict[str, Any]:
    """Cap each conflict list at `limit` entries, keeping totals."""
    out = dict(conflicts)
    for key in ("with_files", "monitored", "would_delete"):
        entries = conflicts.get(key) or []
        out[key] = entries[:limit]
        if len(entries) > limit:
            out[f"{key}_total"] = len(entries)
    return out
