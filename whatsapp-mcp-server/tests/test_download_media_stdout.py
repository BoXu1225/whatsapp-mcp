"""download_media must not write to stdout: it is the MCP stdio transport."""

import pytest

import whatsapp


class FakeResponse:
    def __init__(self, status_code, body=None, text=""):
        self.status_code = status_code
        self._body = body
        self.text = text

    def json(self):
        return self._body


@pytest.mark.parametrize(
    "response",
    [
        FakeResponse(200, {"success": True, "path": "/tmp/fake.jpg"}),
        FakeResponse(200, {"success": False, "message": "nope"}),
        FakeResponse(500, text="boom"),
    ],
)
def test_download_media_keeps_stdout_clean(monkeypatch, capsys, response):
    monkeypatch.setattr(whatsapp, "_bridge_post", lambda endpoint, payload: response)
    whatsapp.download_media("m1", "15550000001@s.whatsapp.net")
    captured = capsys.readouterr()
    assert captured.out == ""
    assert captured.err != ""


def test_download_media_unexpected_error_keeps_stdout_clean(monkeypatch, capsys):
    def boom(endpoint, payload):
        raise KeyError("x")

    monkeypatch.setattr(whatsapp, "_bridge_post", boom)
    assert whatsapp.download_media("m1", "15550000001@s.whatsapp.net") is None
    assert capsys.readouterr().out == ""
