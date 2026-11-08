import json
from typing import Any

import httpx
import pytest
from mcp.server.mcpserver.exceptions import ToolError

from rg_mcp import server
from rg_mcp.client import AdminClient, BookshelfClient, Settings

AUTHOR = 100
WORK_WITH_FILE = 1
WORK_MONITORED = 2
WORK_PLAIN = 3


class Fake:
    """Records requests and serves handlers keyed by (method, path)."""

    def __init__(self, auth: tuple[str, str]) -> None:
        self.auth = auth
        self.routes: dict[tuple[str, str], Any] = {}
        self.calls: list[tuple[str, str, dict[str, str], Any]] = []

    def __call__(self, req: httpx.Request) -> httpx.Response:
        assert req.headers[self.auth[0]] == self.auth[1]
        body = json.loads(req.content) if req.content else None
        params = dict(req.url.params)
        self.calls.append((req.method, req.url.path, params, body))
        handler = self.routes.get((req.method, req.url.path))
        if handler is None:
            return httpx.Response(404, json={"error": "HTTP 404: Not Found"})
        res = handler(params, body) if callable(handler) else handler
        return res if isinstance(res, httpx.Response) else httpx.Response(200, json=res)

    def posts(self) -> list[Any]:
        return [b for m, _, _, b in self.calls if m == "POST"]


def preview(removed: list[int]) -> dict[str, Any]:
    return {
        "author_id": AUTHOR,
        "name": "Author",
        "mode": "allowlist",
        "served": 3,
        "curated": 3 - len(removed),
        "removed": [{"id": w, "title": f"Work {w}", "reason": "not canon"} for w in removed],
        "added": [],
    }


def curation_handler(removed: list[int]):
    def handle(_: dict[str, str], body: dict[str, Any]) -> dict[str, Any]:
        res = {"authors": len(body.get("authors") or []), "works": len(body.get("works") or []), "editions": 0}
        if body.get("dry_run"):
            return {**res, "dry_run": True, "previews": [preview(removed)]}
        return res

    return handle


@pytest.fixture
def admin(monkeypatch: pytest.MonkeyPatch) -> Fake:
    f = Fake(("Authorization", "Bearer tok"))
    s = Settings(admin_url="http://rg", admin_token="tok", bookshelf_url="http://bs", bookshelf_api_key="k")
    monkeypatch.setattr(server, "_settings", s)
    monkeypatch.setattr(server, "_admin", AdminClient(s, transport=httpx.MockTransport(f)))
    return f


@pytest.fixture
def bookshelf(monkeypatch: pytest.MonkeyPatch, admin: Fake) -> Fake:
    f = Fake(("X-Api-Key", "k"))
    f.routes[("GET", "/api/v1/author")] = [{"id": 7, "foreignAuthorId": str(AUTHOR), "authorName": "Author"}]
    f.routes[("GET", "/api/v1/book")] = lambda p, _: (
        [
            {"id": 70, "foreignBookId": str(WORK_WITH_FILE), "monitored": True, "statistics": {"bookFileCount": 1}},
            {"id": 71, "foreignBookId": str(WORK_MONITORED), "monitored": True, "statistics": {"bookFileCount": 0}},
        ]
        if p == {"authorId": "7"}
        else []
    )
    monkeypatch.setattr(server, "_bookshelf", BookshelfClient(server.settings(), transport=httpx.MockTransport(f)))
    monkeypatch.setattr(server, "_bookshelf_set", True)
    return f


async def call(tool: str, **args: Any) -> Any:
    res = await server.mcp.call_tool(tool, args)
    assert not res.is_error and len(res.content) == 1, res
    return json.loads(res.content[0].text)


async def call_err(tool: str, **args: Any) -> str:
    with pytest.raises(ToolError) as e:
        await server.mcp.call_tool(tool, args)
    return str(e.value)


async def test_refuses_dropping_books_with_files(admin: Fake, bookshelf: Fake) -> None:
    admin.routes[("POST", "/admin/curation")] = curation_handler([WORK_WITH_FILE, WORK_PLAIN])
    err = await call_err("set_author_mode", author_id=AUTHOR, mode="allowlist")
    assert "files" in err and '"bookshelf_book_id":70' in err
    assert [b.get("dry_run") for b in admin.posts()] == [True], "only the dry run was sent"

    res = await call("set_author_mode", author_id=AUTHOR, mode="allowlist", force=True)
    assert res["result"]["authors"] == 1
    assert admin.posts()[-1] == {"source": "mcp", "authors": [{"id": AUTHOR, "mode": "allowlist"}]}
    assert res["library"]["with_files"][0]["work_id"] == WORK_WITH_FILE


async def test_monitored_without_files_is_a_warning(admin: Fake, bookshelf: Fake) -> None:
    admin.routes[("POST", "/admin/curation")] = curation_handler([WORK_MONITORED])
    res = await call("set_works_status", work_ids=[WORK_MONITORED], status="hidden", note="study guide")
    assert res["library"]["monitored"][0]["bookshelf_book_id"] == 71
    assert admin.posts()[-1]["works"] == [{"id": WORK_MONITORED, "status": "hidden", "note": "study guide"}]


async def test_dry_run_doesnt_apply(admin: Fake, bookshelf: Fake) -> None:
    admin.routes[("POST", "/admin/curation")] = curation_handler([WORK_WITH_FILE])
    res = await call("merge_works", source_ids=[WORK_WITH_FILE], target_id=9, dry_run=True)
    assert res["dry_run"] is True
    assert res["library"]["with_files"][0]["work_id"] == WORK_WITH_FILE
    assert len(admin.posts()) == 1


async def test_removals_need_bookshelf_or_force(admin: Fake, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(server, "_bookshelf", None)
    monkeypatch.setattr(server, "_bookshelf_set", True)
    admin.routes[("POST", "/admin/curation")] = curation_handler([WORK_PLAIN])
    assert "can't be checked" in await call_err("set_works_status", work_ids=[WORK_PLAIN], status="hidden")

    # Nothing removed: no library check needed.
    admin.routes[("POST", "/admin/curation")] = curation_handler([])
    res = await call("set_works_status", work_ids=[WORK_PLAIN], status="canon")
    assert res["result"]["works"] == 1


async def test_read_only(admin: Fake, bookshelf: Fake, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(server, "_settings", Settings(admin_url="http://rg", admin_token="tok", read_only=True))
    admin.routes[("POST", "/admin/curation")] = curation_handler([])
    assert "read-only" in await call_err("update_edition", edition_id=5, hidden=True)
    res = await call("update_edition", edition_id=5, hidden=True, dry_run=True)
    assert res["dry_run"] is True


async def test_update_work_clear(admin: Fake, bookshelf: Fake) -> None:
    admin.routes[("POST", "/admin/curation")] = curation_handler([])
    await call("update_work", work_id=4, title="Better", clear=["merged_into", "status"])
    assert admin.posts()[-1]["works"] == [{"id": 4, "title": "Better", "merged_into": None, "status": None}]
    assert "both set and cleared" in await call_err("update_work", work_id=4, title="x", clear=["title"])
    assert "nothing to change" in await call_err("update_work", work_id=4)


async def test_preview_author_and_errors(admin: Fake, bookshelf: Fake) -> None:
    admin.routes[("GET", f"/admin/authors/{AUTHOR}/preview")] = lambda p, _: {**preview([WORK_WITH_FILE]), **p}
    res = await call("preview_author", author_id=AUTHOR, mode="allowlist")
    assert res["mode"] == "allowlist"
    assert res["library"]["with_files"][0]["files"] == 1

    err = await call_err("get_work", work_id=404)
    assert "HTTP 404" in err


async def test_settings_from_env(monkeypatch: pytest.MonkeyPatch, tmp_path: Any) -> None:
    tok = tmp_path / "tok"
    tok.write_text("secret\n")
    monkeypatch.setenv("RG_ADMIN_URL", "http://rg/")
    monkeypatch.delenv("RG_ADMIN_TOKEN", raising=False)
    monkeypatch.setenv("RG_ADMIN_TOKEN_FILE", str(tok))
    s = Settings.from_env()
    assert (s.admin_url, s.admin_token, s.bookshelf_url) == ("http://rg", "secret", "")
