"""End-to-end tool contract (#36): what an MCP client sees, through the SDK.

The server runs in-process and a real MCP client session talks to it over the
SDK's in-memory transport. The test lists the tools and calls a representative
set against the fake databases, and compares the wire format (tool names,
descriptions, input/output schemas, result content) with
tests/snapshots/tool_contract.json. An SDK upgrade that changes anything an
agent sees fails here.

Nothing leaves the process: the bridge health check gets a fake response, the
send functions are replaced with a recorder, and the autouse fixture in
conftest.py blocks any other HTTP request.

Regenerate the snapshot after an intended change with:
    UPDATE_TOOL_CONTRACT=1 uv run pytest tests/test_tool_contract.py
and review the diff.
"""

import json
import os
import pathlib
import time
from contextlib import asynccontextmanager
from datetime import datetime, timezone

import anyio
import pytest
import requests
from conftest import ALICE, _t

import main
import status
import whatsapp

SNAPSHOT = pathlib.Path(__file__).parent / "snapshots" / "tool_contract.json"
UPDATE = os.environ.get("UPDATE_TOOL_CONTRACT") == "1"

TOKEN = "ab" * 32
DB_MTIME = datetime(2024, 1, 3, 8, 0, 0, tzinfo=timezone.utc)
HEALTHY = {
    "connected": True,
    "logged_in": True,
    "last_event": "2024-01-02T10:06:00Z",
    "started_at": "2024-01-02T09:00:00Z",
    "version": "0.2.0",
}
# A non-ASCII chat name, so a change in how JSON text escapes it shows up.
UNICODE_CHAT = "120363000000000099@g.us"


@asynccontextmanager
async def client_session():
    """An initialized MCP client session connected in-memory to main.mcp.

    mcp 2.x: mcp.Client accepts the server object directly. mcp 1.x: the
    in-memory helper takes the low-level server.
    """
    try:
        from mcp import Client
    except ImportError:
        from mcp.shared.memory import create_connected_server_and_client_session

        async with create_connected_server_and_client_session(main.mcp._mcp_server) as session:
            yield session
    else:
        async with Client(main.mcp) as client:
            yield client


def wire(model):
    """A protocol object as it goes over the wire (camelCase keys, no nulls)."""
    return model.model_dump(mode="json", by_alias=True, exclude_none=True)


def tool_contract(tool):
    t = wire(tool)
    return {
        "description": t.get("description"),
        "inputSchema": t.get("inputSchema"),
        "outputSchema": t.get("outputSchema"),
        "annotations": t.get("annotations"),
    }


def call_contract(result):
    r = wire(result)
    return {
        "isError": r.get("isError", False),
        "content": r.get("content", []),
        "structuredContent": r.get("structuredContent"),
    }


class FakeResponse:
    status_code = 200

    def json(self):
        return HEALTHY


@pytest.fixture
def utc(monkeypatch):
    """Message times are printed in local time; pin it so the snapshot is stable."""
    monkeypatch.setenv("TZ", "UTC")
    time.tzset()
    yield
    monkeypatch.undo()
    time.tzset()


@pytest.fixture
def bridge(contacts_db, monkeypatch, utc):
    """Fake bridge: token file, healthy /api/health, send functions recorded."""
    store = pathlib.Path(contacts_db.path).parent
    (store / "bridge_token").write_text(TOKEN + "\n")
    contacts_db.add_chat(UNICODE_CHAT, "Zoë's Café 🎉", _t(9))
    contacts_db.add_message("z1", UNICODE_CHAT, "15550000002", "très bien 👍", _t(9))
    ts = DB_MTIME.timestamp()
    os.utime(contacts_db.path, (ts, ts))

    def fake_get(url, *args, **kwargs):
        assert url.endswith("/health"), url
        return FakeResponse()

    monkeypatch.setattr(requests, "get", fake_get)
    status.clear_header_cache()

    sent = []

    def recorder(recipient, payload):
        sent.append((recipient, payload))
        return True, "sent"

    monkeypatch.setattr(main, "whatsapp_send_message", recorder)
    monkeypatch.setattr(main, "whatsapp_send_file", recorder)
    monkeypatch.setattr(main, "whatsapp_audio_voice_message", recorder)
    yield sent
    status.clear_header_cache()


CALLS = [
    ("get_status", {}),
    ("list_chats", {}),
    ("list_chats", {"query": "Alice", "include_last_message": False}),
    ("list_messages", {"chat_jid": ALICE}),
    ("list_messages", {"query": "hello", "limit": 5}),
    ("list_awaiting_reply", {"include_groups": True}),
    ("search_contacts", {"query": "Example", "limit": 2}),
    ("get_contact_chats", {"jid": ALICE}),
    ("get_direct_chat_by_contact", {"sender_phone_number": "15550009999"}),
    ("get_chat", {"chat_jid": UNICODE_CHAT}),
    ("get_last_interaction", {"jid": ALICE}),
    ("get_message_context", {"message_id": "a2", "before": 1, "after": 1}),
    ("send_message", {"recipient": "15550009999", "message": "hello"}),
]


async def _exercise():
    async with client_session() as session:
        listed = await session.list_tools()
        tools = {t.name: tool_contract(t) for t in listed.tools}
        calls = []
        for name, args in CALLS:
            result = await session.call_tool(name, args)
            calls.append({"tool": name, "arguments": args, "result": call_contract(result)})
        return {"tools": tools, "calls": calls}


def _check(key, actual):
    snapshot = json.loads(SNAPSHOT.read_text(encoding="utf-8")) if SNAPSHOT.exists() else {}
    if UPDATE:
        snapshot[key] = actual
        SNAPSHOT.parent.mkdir(exist_ok=True)
        SNAPSHOT.write_text(json.dumps(snapshot, indent=2, ensure_ascii=False, sort_keys=True) + "\n", encoding="utf-8")
        return
    assert key in snapshot, f"no {key!r} in {SNAPSHOT.name}; run with UPDATE_TOOL_CONTRACT=1"
    assert actual == snapshot[key]


def test_tool_contract(bridge):
    observed = anyio.run(_exercise)

    # The same checks the snapshot encodes, spelled out for the important cases.
    assert set(observed["tools"]) == {
        "get_status", "search_contacts", "list_messages", "list_chats", "list_awaiting_reply", "get_chat",
        "get_direct_chat_by_contact", "get_contact_chats", "get_last_interaction", "get_message_context",
        "send_message", "send_file", "send_audio_message", "download_media", "request_history",
    }
    results = {(c["tool"], json.dumps(c["arguments"], sort_keys=True)): c["result"] for c in observed["calls"]}

    rejected = results[("send_message", json.dumps({"message": "hello", "recipient": "15550009999"}))]
    assert rejected["isError"] is False
    assert json.loads(rejected["content"][0]["text"])["success"] is False
    assert "allow_unknown" in rejected["content"][0]["text"]
    assert bridge == []  # nothing was sent

    chats = json.loads(results[("list_chats", "{}")]["content"][0]["text"])
    assert set(chats) == {"status", "chats"}
    assert chats["status"].startswith("[bridge up")

    messages = results[("list_messages", json.dumps({"chat_jid": ALICE}))]["content"][0]["text"]
    assert messages.startswith("[bridge up") and "ID: a1" in messages

    _check("tools", observed["tools"])
    _check("calls", observed["calls"])


def test_tool_error_contract(utc, tmp_path, caplog):
    """A tool that raises (no message database) is an is_error result with the error text.

    The exception is also logged with its traceback, for the MCP server log.
    """

    async def call():
        async with client_session() as session:
            return call_contract(await session.call_tool("list_chats", {}))

    status.clear_header_cache()
    result = anyio.run(call)
    assert result["isError"] is True
    assert result["content"][0]["text"].startswith("Error executing tool list_chats: ")
    assert "messages" in result["content"][0]["text"]
    logged = [r for r in caplog.records if r.exc_info and isinstance(r.exc_info[1], whatsapp.WhatsAppDBError)]
    assert len(logged) == 1 and "list_chats" in logged[0].getMessage()
    for block in result["content"]:
        block["text"] = block["text"].replace(str(tmp_path), "<tmp>")
    _check("error", result)
