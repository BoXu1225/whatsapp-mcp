"""Bridge health and data freshness (#7).

get_status() asks the bridge's GET /api/health (2 s timeout) and adds what the
message database says: the newest stored message and when the database was
last written. When the bridge is down, those two are the best answer to "how
stale is this?". freshness_header() condenses the same into one line for the
list tools.
"""

import os
import sqlite3
import time
from datetime import datetime, timezone
from typing import Any, Dict, Optional, Tuple

import requests

import whatsapp

HEALTH_TIMEOUT = 2  # seconds, for the get_status tool
# The header runs on every list call: a hung bridge may cost at most
# HEADER_TIMEOUT, and at most once per HEADER_CACHE_SECONDS.
HEADER_TIMEOUT = 0.5
HEADER_CACHE_SECONDS = 5.0

_now = time.monotonic
_header_cache: Optional[Tuple[str, float, str]] = None  # (db path, expires, header)


def clear_header_cache() -> None:
    global _header_cache
    _header_cache = None


def _utc(ts: Optional[float]) -> Optional[str]:
    if ts is None:
        return None
    return datetime.fromtimestamp(ts, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def bridge_health(timeout: float = HEALTH_TIMEOUT) -> Tuple[Optional[Dict[str, Any]], Optional[str]]:
    """(health JSON, None) if the bridge answered, else (None, reason).

    The reason starts with "down:" when the bridge could not be reached and
    "error:" when it answered but not with a usable health response.
    """
    try:
        headers = whatsapp._bridge_headers()
    except whatsapp.BridgeTokenError as e:
        return None, f"down: {e}"
    try:
        resp = requests.get(f"{whatsapp.WHATSAPP_API_BASE_URL}/health", headers=headers, timeout=timeout)
    except requests.RequestException as e:
        return None, f"down: bridge not reachable at {whatsapp.WHATSAPP_API_BASE_URL} ({type(e).__name__})"
    if resp.status_code != 200:
        hint = " (token rejected; restart the MCP server or bridge?)" if resp.status_code == 401 else ""
        return None, f"error: bridge answered HTTP {resp.status_code}{hint}"
    try:
        data = resp.json()
    except ValueError:
        return None, "error: bridge sent an invalid health response (an older bridge without /api/health?)"
    if not isinstance(data, dict):
        return None, "error: bridge sent an invalid health response"
    return data, None


def db_freshness() -> Tuple[Optional[str], Optional[str]]:
    """(newest message time, database last-modified time), both RFC 3339 UTC or None.

    Timestamps are compared as instants (SQLite parses the stored offsets), so
    rows written with different UTC offsets still order correctly.
    """
    path = whatsapp.MESSAGES_DB_PATH
    if not os.path.isfile(path):
        return None, None
    # With WAL, recent writes sit in -wal until a checkpoint.
    mtimes = [os.path.getmtime(p) for p in (path, path + "-wal") if os.path.isfile(p)]
    modified = _utc(max(mtimes)) if mtimes else None
    newest = None
    try:
        conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
        try:
            row = conn.execute(
                "SELECT MAX(CASE WHEN typeof(timestamp) = 'integer' THEN timestamp"
                " ELSE CAST(strftime('%s', timestamp) AS INTEGER) END) FROM messages"
            ).fetchone()
        finally:
            conn.close()
        if row and row[0] is not None:
            newest = _utc(row[0])
    except sqlite3.Error:
        pass
    return newest, modified


def get_status(timeout: float = HEALTH_TIMEOUT) -> Dict[str, Any]:
    """Bridge state plus database freshness, with a one-sentence summary.

    bridge is one of: "up", "disconnected" (running, not connected to
    WhatsApp), "logged_out" (needs a QR scan), "down" (not reachable) or
    "error" (answered, but not usably).
    """
    health, problem = bridge_health(timeout)
    newest, modified = db_freshness()
    result: Dict[str, Any] = {
        "bridge": "down",
        "connected": False,
        "logged_in": None,
        "last_event": None,
        "started_at": None,
        "version": None,
        "newest_message": newest,
        "db_modified": modified,
    }
    data_as_of = f"data as of {newest} (newest stored message)" if newest else (
        "no messages stored yet" if modified else "no message database found"
    )

    if health is None:
        kind, _, detail = problem.partition(": ")
        result["bridge"] = kind
        result["error"] = detail
        if kind == "down":
            since = f"bridge down since about {modified} (last database write)" if modified else "bridge down"
            result["summary"] = f"{since}; {data_as_of}. Newer messages are missing until the bridge is started. {detail}"
        else:
            result["summary"] = f"Bridge not usable: {detail}; {data_as_of}."
        return result

    connected = bool(health.get("connected"))
    logged_in = health.get("logged_in")
    result.update(
        connected=connected,
        logged_in=logged_in,
        last_event=health.get("last_event"),
        started_at=health.get("started_at"),
        version=health.get("version"),
    )
    last_event = f"last event {result['last_event']}" if result["last_event"] else "no events yet"
    if logged_in is False:
        result["bridge"] = "logged_out"
        result["summary"] = f"Bridge running but logged out of WhatsApp: run it in the foreground and scan the QR code; {data_as_of}."
    elif not connected:
        result["bridge"] = "disconnected"
        result["summary"] = f"Bridge running but not connected to WhatsApp ({last_event}); {data_as_of}."
    else:
        result["bridge"] = "up"
        result["summary"] = f"Bridge up and connected ({last_event}); {data_as_of}."
    return result


def freshness_header() -> str:
    """One line for the top of list output, cached briefly. Never raises."""
    global _header_cache
    path, now = whatsapp.MESSAGES_DB_PATH, _now()
    if _header_cache and _header_cache[0] == path and now < _header_cache[1]:
        return _header_cache[2]
    header = _build_header()
    _header_cache = (path, now + HEADER_CACHE_SECONDS, header)
    return header


def _build_header() -> str:
    try:
        s = get_status(HEADER_TIMEOUT)
    except Exception as e:  # the header must never break the tool it decorates
        return f"[status unknown: {type(e).__name__}]"
    newest = s["newest_message"] or "none"
    if s["bridge"] == "up":
        return f"[bridge up · data as of {newest}]"
    if s["bridge"] == "down":
        since = f" since ~{s['db_modified']}" if s["db_modified"] else ""
        return f"[WARNING: bridge down{since} · data as of {newest}; newer messages are missing]"
    return f"[WARNING: bridge {s['bridge'].replace('_', ' ')} · data as of {newest}; may be stale]"
