"""Tests for the HTTP calls to the bridge REST API (send / download).

requests.post is replaced by a recorder per test; nothing leaves the process.
All numbers and paths are fake.
"""

import pytest
import requests

import whatsapp

TOKEN = "ab" * 32
RECIPIENT = "15550000001"


class FakeResponse:
    def __init__(self, status_code=200, payload=None):
        self.status_code = status_code
        self._payload = payload if payload is not None else {"success": True, "message": "ok", "path": "/fake/path"}
        self.text = str(self._payload)

    def json(self):
        return self._payload


@pytest.fixture
def bridge_store(tmp_path, monkeypatch):
    """A fake bridge store dir: MESSAGES_DB_PATH inside it, a token file and an outbox."""
    store = tmp_path / "store"
    store.mkdir()
    (store / "outbox").mkdir(mode=0o700)
    (store / "bridge_token").write_text(TOKEN)
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(store / "messages.db"))
    monkeypatch.delenv("WHATSAPP_SEND_ALLOWED_DIRS", raising=False)
    return store


@pytest.fixture
def posts(monkeypatch):
    """Record requests.post calls and answer 200 / success."""
    calls = []

    def fake_post(url, *args, **kwargs):
        calls.append({"url": url, **kwargs})
        return FakeResponse()

    monkeypatch.setattr(requests, "post", fake_post)
    return calls


# --- #1: token header ------------------------------------------------------


def test_send_message_sends_token_header(bridge_store, posts):
    ok, _ = whatsapp.send_message(RECIPIENT, "hello")
    assert ok
    assert len(posts) == 1
    assert posts[0]["headers"]["X-Bridge-Token"] == TOKEN


def test_send_message_sets_timeout(bridge_store, posts):
    whatsapp.send_message(RECIPIENT, "hello")
    assert posts[0].get("timeout")


def test_download_media_sends_token_header(bridge_store, posts):
    path = whatsapp.download_media("m1", RECIPIENT + "@s.whatsapp.net")
    assert path == "/fake/path"
    assert posts[0]["headers"]["X-Bridge-Token"] == TOKEN
    assert posts[0].get("timeout")


def test_send_file_sends_token_header(bridge_store, posts):
    f = bridge_store / "outbox" / "photo.jpg"
    f.write_bytes(b"\xff\xd8fake")
    ok, _ = whatsapp.send_file(RECIPIENT, str(f))
    assert ok
    assert posts[0]["headers"]["X-Bridge-Token"] == TOKEN


def test_token_read_at_call_time(bridge_store, posts):
    """Rotating the token file (bridge restart) is picked up without re-import."""
    whatsapp.send_message(RECIPIENT, "one")
    (bridge_store / "bridge_token").write_text("cd" * 32 + "\n")
    whatsapp.send_message(RECIPIENT, "two")
    assert posts[1]["headers"]["X-Bridge-Token"] == "cd" * 32


def test_missing_token_gives_clear_error(bridge_store, posts):
    (bridge_store / "bridge_token").unlink()
    ok, msg = whatsapp.send_message(RECIPIENT, "hello")
    assert not ok
    assert "bridge_token" in msg
    assert posts == []


def test_missing_token_download_returns_none(bridge_store, posts):
    (bridge_store / "bridge_token").unlink()
    assert whatsapp.download_media("m1", RECIPIENT + "@s.whatsapp.net") is None
    assert posts == []
