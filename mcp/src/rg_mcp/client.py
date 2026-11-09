"""Async clients for the rreading-glasses admin API and (read-only) Bookshelf."""

from __future__ import annotations

import logging
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import httpx

# Per-request INFO logs are noise on the MCP stderr channel.
logging.getLogger("httpx").setLevel(logging.WARNING)

_TRUE = {"1", "true", "yes", "on"}


class CurationError(Exception):
    """Raised for API failures and refused writes."""


# Bookshelf instances fed by this server: (name, env prefix).
BOOKSHELF_INSTANCES = (("ebook", "BOOKSHELF"), ("audio", "BOOKSHELF_AUDIO"))


@dataclass(frozen=True)
class Bookshelf:
    name: str
    url: str
    api_key: str


@dataclass(frozen=True)
class Settings:
    admin_url: str
    admin_token: str
    bookshelves: tuple[Bookshelf, ...] = ()
    read_only: bool = False
    timeout: float = 120.0

    @classmethod
    def from_env(cls) -> Settings:
        url = os.environ.get("RG_ADMIN_URL", "").strip()
        token = os.environ.get("RG_ADMIN_TOKEN", "").strip()
        token_file = os.environ.get("RG_ADMIN_TOKEN_FILE", "").strip()
        if not token and token_file:
            token = Path(token_file).expanduser().read_text().strip()
        if not url or not token:
            raise CurationError("RG_ADMIN_URL and RG_ADMIN_TOKEN (or RG_ADMIN_TOKEN_FILE) must be set")
        bookshelves = []
        for name, prefix in BOOKSHELF_INSTANCES:
            bs_url = os.environ.get(f"{prefix}_URL", "").strip().rstrip("/")
            key = os.environ.get(f"{prefix}_API_KEY", "").strip()
            if bool(bs_url) != bool(key):
                raise CurationError(f"set both {prefix}_URL and {prefix}_API_KEY, or neither")
            if bs_url:
                bookshelves.append(Bookshelf(name, bs_url, key))
        return cls(
            admin_url=url.rstrip("/"),
            admin_token=token,
            bookshelves=tuple(bookshelves),
            read_only=os.environ.get("RG_READ_ONLY", "").lower() in _TRUE,
            timeout=float(os.environ.get("RG_TIMEOUT", "120")),
        )


def _clean_params(params: dict[str, Any]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for k, v in params.items():
        if v is None:
            continue
        out[k] = ("true" if v else "false") if isinstance(v, bool) else v
    return out


def _format_error(resp: httpx.Response) -> str:
    try:
        body = resp.json()
    except ValueError:
        body = resp.text
    if isinstance(body, dict):
        detail = str(body.get("error") or body.get("message") or body)
    else:
        detail = str(body)
    return f"{resp.request.method} {resp.request.url.path} -> HTTP {resp.status_code}: {detail[:2000]}"


class _Client:
    def __init__(self, base_url: str, headers: dict[str, str], timeout: float, transport: Any = None):
        self._http = httpx.AsyncClient(base_url=base_url, headers=headers, timeout=timeout, transport=transport)

    async def aclose(self) -> None:
        await self._http.aclose()

    async def request(self, method: str, path: str, params: dict[str, Any] | None = None, json: Any = None) -> Any:
        try:
            resp = await self._http.request(method, path, params=_clean_params(params or {}), json=json)
        except httpx.HTTPError as e:
            raise CurationError(f"{method} {path} failed: {type(e).__name__} {e}".rstrip()) from e
        if resp.status_code >= 400:
            raise CurationError(_format_error(resp))
        return resp.json() if resp.content else None


class AdminClient(_Client):
    """rreading-glasses /admin API."""

    def __init__(self, settings: Settings, transport: httpx.AsyncBaseTransport | None = None):
        super().__init__(
            settings.admin_url,
            {"Authorization": f"Bearer {settings.admin_token}", "Accept": "application/json"},
            settings.timeout,
            transport,
        )

    async def get(self, path: str, **params: Any) -> Any:
        return await self.request("GET", f"/admin/{path.lstrip('/')}", params=params)

    async def curate(self, body: dict[str, Any]) -> Any:
        return await self.request("POST", "/admin/curation", json=body)

    async def run_rules(self) -> Any:
        return await self.request("POST", "/admin/rules/run")


class BookshelfClient(_Client):
    """Read-only Bookshelf v1 API, used to check curation against the library."""

    def __init__(self, bs: Bookshelf, timeout: float = 120.0, transport: httpx.AsyncBaseTransport | None = None):
        self.name = bs.name
        super().__init__(bs.url, {"X-Api-Key": bs.api_key, "Accept": "application/json"}, timeout, transport)

    async def get(self, path: str, **params: Any) -> Any:
        return await self.request("GET", f"/api/v1/{path.lstrip('/')}", params=params)
