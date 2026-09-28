"""Tests for the HTTP calls to the bridge REST API (send / download).

requests.post is replaced by a recorder per test; nothing leaves the process.
All numbers and paths are fake.
"""

import os

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


def test_missing_token_download_error_not_on_stdout(bridge_store, posts, capsys):
    """stdout is the MCP stdio transport; the token error must go to stderr."""
    (bridge_store / "bridge_token").unlink()
    whatsapp.download_media("m1", RECIPIENT + "@s.whatsapp.net")
    out, err = capsys.readouterr()
    assert out == ""
    assert "bridge_token" in err


# --- #2: send allowlist ----------------------------------------------------


def test_send_file_outside_outbox_rejected(bridge_store, posts, tmp_path):
    secret = tmp_path / "secret.env"
    secret.write_text("KEY=fake")
    ok, msg = whatsapp.send_file(RECIPIENT, str(secret))
    assert not ok
    assert "outbox" in msg
    assert posts == []


def test_send_file_dotdot_traversal_rejected(bridge_store, posts, tmp_path):
    (tmp_path / "secret.env").write_text("KEY=fake")
    sneaky = os.path.join(str(bridge_store / "outbox"), "..", "..", "secret.env")
    ok, msg = whatsapp.send_file(RECIPIENT, sneaky)
    assert not ok
    assert posts == []


def test_send_file_symlink_out_of_outbox_rejected(bridge_store, posts, tmp_path):
    secret = tmp_path / "secret.env"
    secret.write_text("KEY=fake")
    link = bridge_store / "outbox" / "innocent.jpg"
    link.symlink_to(secret)
    ok, msg = whatsapp.send_file(RECIPIENT, str(link))
    assert not ok
    assert posts == []


def test_send_file_outbox_sibling_prefix_rejected(bridge_store, posts):
    """store/outbox-evil must not pass a plain string-prefix check against store/outbox."""
    evil = bridge_store / "outbox-evil"
    evil.mkdir()
    f = evil / "a.jpg"
    f.write_bytes(b"x")
    ok, _ = whatsapp.send_file(RECIPIENT, str(f))
    assert not ok
    assert posts == []


def test_send_file_inside_outbox_accepted(bridge_store, posts):
    f = bridge_store / "outbox" / "photo.jpg"
    f.write_bytes(b"\xff\xd8fake")
    ok, _ = whatsapp.send_file(RECIPIENT, str(f))
    assert ok
    assert posts[0]["json"]["media_path"] == os.path.realpath(f)


def test_send_file_extra_allowed_dir_from_env(bridge_store, posts, tmp_path, monkeypatch):
    extra = tmp_path / "shared"
    extra.mkdir()
    other = tmp_path / "other"
    other.mkdir()
    monkeypatch.setenv("WHATSAPP_SEND_ALLOWED_DIRS", os.pathsep.join([str(extra), str(other)]))
    f = other / "doc.pdf"
    f.write_bytes(b"%PDF")
    ok, _ = whatsapp.send_file(RECIPIENT, str(f))
    assert ok
    assert len(posts) == 1


def test_send_audio_outside_outbox_rejected(bridge_store, posts, tmp_path):
    f = tmp_path / "voice.ogg"
    f.write_bytes(b"OggS")
    ok, _ = whatsapp.send_audio_message(RECIPIENT, str(f))
    assert not ok
    assert posts == []


def test_send_audio_converted_file_lands_in_outbox(bridge_store, posts, monkeypatch):
    """ffmpeg output must stay sendable: it is written inside the outbox, then removed."""
    import audio

    src = bridge_store / "outbox" / "voice.wav"
    src.write_bytes(b"RIFF\x24\x00\x00\x00WAVEfmt ")

    def fake_run(cmd, *args, **kwargs):
        out = cmd[-1]
        with open(out, "wb") as fh:
            fh.write(b"OggS")
        return None

    monkeypatch.setattr(audio.subprocess, "run", fake_run)
    ok, _ = whatsapp.send_audio_message(RECIPIENT, str(src))
    assert ok
    sent = posts[0]["json"]["media_path"]
    outbox = os.path.realpath(bridge_store / "outbox")
    assert sent.startswith(outbox + os.sep)
    assert sent.endswith(".ogg")
    assert not os.path.exists(sent), "temporary converted file should be removed after sending"


def test_relative_allowed_dir_ignored(bridge_store, posts, tmp_path, monkeypatch, capsys):
    """Relative WHATSAPP_SEND_ALLOWED_DIRS entries would depend on the cwd; they are skipped with a warning."""
    work = tmp_path / "work"
    work.mkdir()
    f = work / "doc.pdf"
    f.write_bytes(b"%PDF")
    monkeypatch.chdir(work)
    monkeypatch.setenv("WHATSAPP_SEND_ALLOWED_DIRS", os.pathsep.join([".", "work"]))
    ok, _ = whatsapp.send_file(RECIPIENT, str(f))
    assert not ok
    assert posts == []
    out, err = capsys.readouterr()
    assert out == ""
    assert "absolute" in err
