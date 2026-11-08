"""Checks curation previews against the Bookshelf libraries (ebook and audio).

Bookshelf deletes library books that drop out of an author's list on the next
author refresh, files included in its bookkeeping. A curation change that
removes a work holding a file must be forced.
"""

from __future__ import annotations

from typing import Any

from .client import BookshelfClient


async def library_conflicts(clients: list[BookshelfClient], previews: list[dict[str, Any]]) -> dict[str, Any]:
    """Find removed works that are books in any Bookshelf library.

    Returns {"checked": [instance names], "with_files": [...], "monitored": [...]};
    each entry names the instance, author, work, Bookshelf book id and file count.
    """
    removing = [p for p in previews if p.get("removed")]
    out: dict[str, Any] = {"checked": [c.name for c in clients], "with_files": [], "monitored": []}
    if not removing:
        return out
    for bs in clients:
        await _check(bs, removing, out)
    return out


async def _check(bs: BookshelfClient, removing: list[dict[str, Any]], out: dict[str, Any]) -> None:
    authors = await bs.get("author") or []
    by_foreign = {str(a.get("foreignAuthorId")): a for a in authors}
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
