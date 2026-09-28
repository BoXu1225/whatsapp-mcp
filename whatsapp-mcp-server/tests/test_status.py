"""Bridge health and data freshness (#7).

requests.get is replaced per test; nothing leaves the process. The token and
databases are fakes under tmp_path.
"""

import ast
import os
import pathlib
from datetime import datetime, timezone

import pytest
import requests
from conftest import ALICE, BOB

import main
import status
import whatsapp

TOKEN = "ef" * 32
NEWEST = "2024-01-02T10:05:00Z"  # b1 in seeded_db
DB_MTIME = datetime(2024, 1, 3, 8, 0, 0, tzinfo=timezone.utc)

HEALTHY = {
    "connected": True,
    "logged_in": True,
    "last_event": "2024-01-02T10:06:00Z",
    "started_at": "2024-01-02T09:00:00Z",
    "version": "0.2.0",
}


class FakeResponse:
    def __init__(self, status_code=200, payload=None):
        self.status_code = status_code
        self._payload = payload

    def json(self):
        if self._payload is None:
            raise ValueError("no JSON")
        return self._payload


@pytest.fixture
def token(seeded_db):
    """bridge_token next to the seeded messages.db; the DB mtime pinned to DB_MTIME."""
    store = pathlib.Path(seeded_db.path).parent
    (store / "bridge_token").write_text(TOKEN + "\n")
    ts = DB_MTIME.timestamp()
    os.utime(seeded_db.path, (ts, ts))
    return store


def answer(monkeypatch, response=None, exc=None):
    """Make requests.get return `response` (or raise `exc`); returns the recorded calls."""
    calls = []

    def fake_get(url, *args, **kwargs):
        calls.append({"url": url, **kwargs})
        if exc is not None:
            raise exc
        return response

    monkeypatch.setattr(requests, "get", fake_get)
    return calls


# --- get_status ---------------------------------------------------------------


def test_healthy_bridge(token, monkeypatch):
    calls = answer(monkeypatch, FakeResponse(200, HEALTHY))
    result = main.get_status()

    assert len(calls) == 1
    assert calls[0]["url"] == f"{whatsapp.WHATSAPP_API_BASE_URL}/health"
    assert calls[0]["headers"] == {"X-Bridge-Token": TOKEN}
    assert calls[0]["timeout"] == 2

    assert result["bridge"] == "up"
    assert result["connected"] is True
    assert result["logged_in"] is True
    assert result["last_event"] == "2024-01-02T10:06:00Z"
    assert result["started_at"] == "2024-01-02T09:00:00Z"
    assert result["version"] == "0.2.0"
    assert result["newest_message"] == NEWEST
    assert "up" in result["summary"]


def test_bridge_down_falls_back_to_db(token, monkeypatch):
    answer(monkeypatch, exc=requests.ConnectionError("refused"))
    result = main.get_status()

    assert result["bridge"] == "down"
    assert result["connected"] is False
    assert result["newest_message"] == NEWEST
    assert result["db_modified"] == "2024-01-03T08:00:00Z"
    summary = result["summary"]
    assert "bridge down since" in summary.lower()
    assert "2024-01-03T08:00:00Z" in summary
    assert f"data as of {NEWEST}" in summary


def test_bridge_timeout_is_down(token, monkeypatch):
    answer(monkeypatch, exc=requests.Timeout("slow"))
    assert main.get_status()["bridge"] == "down"


def test_missing_token_is_down_without_request(seeded_db, monkeypatch):
    calls = answer(monkeypatch, FakeResponse(200, HEALTHY))
    result = main.get_status()
    assert calls == []
    assert result["bridge"] == "down"
    assert "bridge_token" in result["error"]
    assert result["newest_message"] == NEWEST


def test_token_rejected(token, monkeypatch):
    answer(monkeypatch, FakeResponse(401))
    result = main.get_status()
    assert result["bridge"] == "error"
    assert "401" in result["error"]
    assert result["newest_message"] == NEWEST


def test_bridge_running_but_disconnected(token, monkeypatch):
    answer(monkeypatch, FakeResponse(200, {**HEALTHY, "connected": False}))
    result = main.get_status()
    assert result["bridge"] == "disconnected"
    assert "not connected" in result["summary"].lower()


def test_bridge_logged_out(token, monkeypatch):
    answer(monkeypatch, FakeResponse(200, {**HEALTHY, "connected": True, "logged_in": False}))
    result = main.get_status()
    assert result["bridge"] == "logged_out"
    assert "qr" in result["summary"].lower()


def test_no_database(monkeypatch):
    answer(monkeypatch, exc=requests.ConnectionError("refused"))
    result = main.get_status()
    assert result["bridge"] == "down"
    assert result["newest_message"] is None
    assert result["db_modified"] is None
    assert "no message database" in result["summary"].lower()


def test_empty_database(messages_db, monkeypatch):
    answer(monkeypatch, exc=requests.ConnectionError("refused"))
    result = main.get_status()
    assert result["newest_message"] is None
    assert result["db_modified"] is not None


def test_newest_message_across_offsets(messages_db, monkeypatch):
    """Rows written with different UTC offsets are compared as instants."""
    answer(monkeypatch, exc=requests.ConnectionError("refused"))
    messages_db.add_chat(ALICE, "Alice Example", datetime(2024, 1, 2, 12, 0, tzinfo=timezone.utc))
    import sqlite3

    with sqlite3.connect(messages_db.path) as conn:
        # 11:30+02:00 is 09:30Z; 10:00+00:00 is later.
        conn.execute("INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES "
                     "('x1', ?, 's', 'a', '2024-01-02 11:30:00+02:00', 0), "
                     "('x2', ?, 's', 'b', '2024-01-02 10:00:00.123456789+00:00', 0)", (ALICE, ALICE))
    assert main.get_status()["newest_message"] == "2024-01-02T10:00:00Z"


def test_db_mtime_includes_wal(token, monkeypatch):
    """With WAL, recent writes land in messages.db-wal, not the main file."""
    answer(monkeypatch, exc=requests.ConnectionError("refused"))
    wal = pathlib.Path(token) / "messages.db-wal"
    wal.write_bytes(b"")
    later = datetime(2024, 1, 4, 0, 0, 0, tzinfo=timezone.utc).timestamp()
    os.utime(wal, (later, later))
    assert main.get_status()["db_modified"] == "2024-01-04T00:00:00Z"


# --- freshness header on list tools --------------------------------------------


def test_list_messages_header_when_down(token, monkeypatch):
    answer(monkeypatch, exc=requests.ConnectionError("refused"))
    out = main.list_messages(chat_jid=ALICE)
    first, rest = out.split("\n", 1)
    assert "bridge down" in first.lower()
    assert NEWEST in first
    assert "ID: a1" in rest and "ID: a1" not in first


def test_list_messages_header_when_up(token, monkeypatch):
    answer(monkeypatch, FakeResponse(200, HEALTHY))
    first = main.list_messages(chat_jid=ALICE).split("\n", 1)[0]
    assert "bridge up" in first.lower()
    assert NEWEST in first


def test_list_chats_header(token, monkeypatch):
    answer(monkeypatch, exc=requests.ConnectionError("refused"))
    result = main.list_chats()
    assert isinstance(result[0], str)
    assert "bridge down" in result[0].lower()
    assert [c.jid for c in result[1:]][0] == BOB


def test_list_awaiting_reply_header(token, monkeypatch):
    answer(monkeypatch, FakeResponse(200, HEALTHY))
    result = main.list_awaiting_reply()
    assert "bridge up" in result[0].lower()
    assert [c.jid for c in result[1:]] == [BOB, ALICE]


def test_header_is_one_line(token, monkeypatch):
    answer(monkeypatch, exc=requests.ConnectionError("refused"))
    assert "\n" not in status.freshness_header()


def test_header_never_breaks_the_tool(seeded_db, monkeypatch):
    """A failing health check (even an unexpected error) still returns the data."""
    monkeypatch.setattr(status, "bridge_health", lambda: (_ for _ in ()).throw(RuntimeError("boom")))
    result = main.list_chats()
    assert isinstance(result[0], str)
    assert len(result) == 4


def test_tool_errors_still_raised():
    with pytest.raises(whatsapp.WhatsAppDBError):
        main.list_messages()


# --- every HTTP call has a timeout ---------------------------------------------

SERVER_DIR = pathlib.Path(__file__).resolve().parent.parent
HTTP_METHODS = {"get", "post", "put", "patch", "delete", "head", "options", "request"}


def test_every_requests_call_has_a_timeout():
    missing = []
    for path in SERVER_DIR.glob("*.py"):
        tree = ast.parse(path.read_text(), filename=str(path))
        for node in ast.walk(tree):
            if (
                isinstance(node, ast.Call)
                and isinstance(node.func, ast.Attribute)
                and node.func.attr in HTTP_METHODS
                and isinstance(node.func.value, ast.Name)
                and node.func.value.id == "requests"
                and not any(k.arg == "timeout" for k in node.keywords)
            ):
                missing.append(f"{path.name}:{node.lineno}")
    assert missing == []
