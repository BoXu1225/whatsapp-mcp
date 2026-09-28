"""download_media reports the bridge's answer: the local path and original
file name, or that a re-upload of expired media was requested (#19).

The bridge is never called: _bridge_post is replaced per test. Fake data only.
"""

import main
import whatsapp

CHAT = "15550000001@s.whatsapp.net"


class FakeResponse:
    def __init__(self, status_code, body=None, text=""):
        self.status_code = status_code
        self._body = body
        self.text = text or str(body)

    def json(self):
        return self._body


def _answer(monkeypatch, response):
    calls = []

    def fake_post(endpoint, payload):
        calls.append((endpoint, payload))
        return response

    monkeypatch.setattr(whatsapp, "_bridge_post", fake_post)
    return calls


def test_download_media_tool_returns_path_and_original_name(monkeypatch):
    _answer(monkeypatch, FakeResponse(200, {
        "success": True,
        "message": "Successfully downloaded document media",
        "filename": "3EB0AAAA.pdf",
        "original_filename": "Holiday Plan.pdf",
        "path": "/fake/store/chat/3EB0AAAA.pdf",
    }))
    result = main.download_media("3EB0AAAA", CHAT)
    assert result["success"] is True
    assert result["file_path"] == "/fake/store/chat/3EB0AAAA.pdf"
    assert result["original_filename"] == "Holiday Plan.pdf"


def test_download_media_tool_reports_retry_requested(monkeypatch):
    msg = "the media has expired on WhatsApp's servers; asked the sender's phone to upload it again, try again shortly"
    _answer(monkeypatch, FakeResponse(202, {"success": False, "retry_requested": True, "message": msg}))
    result = main.download_media("3EB0BBBB", CHAT)
    assert result["success"] is False
    assert result["retry_requested"] is True
    assert "try again" in result["message"]


def test_download_media_tool_passes_bridge_error(monkeypatch):
    _answer(monkeypatch, FakeResponse(500, {"success": False, "message": "Failed to download media: not a media message"}))
    result = main.download_media("3EB0CCCC", CHAT)
    assert result["success"] is False
    assert "not a media message" in result["message"]


def test_download_media_function_still_returns_path_or_none(monkeypatch):
    _answer(monkeypatch, FakeResponse(202, {"success": False, "retry_requested": True, "message": "try again shortly"}))
    assert whatsapp.download_media("3EB0BBBB", CHAT) is None
    _answer(monkeypatch, FakeResponse(200, {"success": True, "path": "/fake/p.jpg"}))
    assert whatsapp.download_media("3EB0AAAA", CHAT) == "/fake/p.jpg"
