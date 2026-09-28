"""request_history asks the bridge for older messages of a chat (#20).

requests.post / _bridge_post are replaced per test; nothing leaves the process.
"""

import pytest
import requests

import main
import whatsapp

TOKEN = "cd" * 32
CHAT = "15550000001@s.whatsapp.net"


class FakeResponse:
    def __init__(self, status_code, body=None, text=""):
        self.status_code = status_code
        self._body = body
        self.text = text or str(body)

    def json(self):
        if self._body is None:
            raise ValueError("no json")
        return self._body


@pytest.fixture
def bridge_store(tmp_path, monkeypatch):
    store = tmp_path / "store"
    store.mkdir()
    (store / "bridge_token").write_text(TOKEN)
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(store / "messages.db"))
    return store


def test_request_history_posts_with_token_and_timeout(bridge_store, monkeypatch):
    calls = []

    def fake_post(url, *args, **kwargs):
        calls.append({"url": url, **kwargs})
        return FakeResponse(202, {"success": True, "message": "requested", "request_id": "REQ-1",
                                  "oldest_message_id": "M1", "count": 30})

    monkeypatch.setattr(requests, "post", fake_post)
    result = whatsapp.request_history(CHAT, 30)
    assert calls[0]["url"].endswith("/api/history")
    assert calls[0]["json"] == {"chat_jid": CHAT, "count": 30}
    assert calls[0]["headers"]["X-Bridge-Token"] == TOKEN
    assert calls[0].get("timeout")
    assert result["success"] is True
    assert result["request_id"] == "REQ-1"


def test_request_history_tool_explains_async(monkeypatch):
    monkeypatch.setattr(whatsapp, "_bridge_post", lambda endpoint, payload: FakeResponse(
        202, {"success": True, "message": "requested", "request_id": "REQ-2", "oldest_message_id": "M1", "count": 50}))
    result = main.request_history(CHAT)
    assert result["success"] is True
    assert result["request_id"] == "REQ-2"
    assert "asynchronous" in result["message"].lower() or "arrive" in result["message"].lower()


@pytest.mark.parametrize("count", [0, -5, 51, 1000])
def test_request_history_rejects_bad_count_without_calling_bridge(monkeypatch, count):
    def boom(endpoint, payload):
        raise AssertionError("bridge called")

    monkeypatch.setattr(whatsapp, "_bridge_post", boom)
    result = main.request_history(CHAT, count)
    assert result["success"] is False
    assert "count" in result["message"]


def test_request_history_rejects_empty_chat(monkeypatch):
    monkeypatch.setattr(whatsapp, "_bridge_post", lambda e, p: pytest.fail("bridge called"))
    assert main.request_history("", 10)["success"] is False


def test_request_history_passes_bridge_error(monkeypatch):
    monkeypatch.setattr(whatsapp, "_bridge_post", lambda endpoint, payload: FakeResponse(
        404, {"success": False, "message": "no stored messages in chat"}))
    result = main.request_history(CHAT, 10)
    assert result["success"] is False
    assert "no stored messages" in result["message"]


def test_request_history_bridge_down(monkeypatch, capsys):
    def down(endpoint, payload):
        raise requests.ConnectionError("refused")

    monkeypatch.setattr(whatsapp, "_bridge_post", down)
    result = main.request_history(CHAT, 10)
    assert result["success"] is False
    assert capsys.readouterr().out == ""
