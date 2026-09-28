"""ffmpeg invocation in audio.py (#6). subprocess.run is faked; ffmpeg never runs."""

import os

import pytest

import audio

WAV = b"RIFF\x24\x00\x00\x00WAVEfmt " + b"\x00" * 16
HEADERS = {
    "wav": WAV,
    "mp3_id3": b"ID3\x04\x00\x00\x00\x00\x00\x00" + b"\x00" * 16,
    "mp3_frame": b"\xff\xfb\x90\x64" + b"\x00" * 16,
    "ogg": b"OggS\x00\x02" + b"\x00" * 16,
    "flac": b"fLaC\x00\x00\x00\x22" + b"\x00" * 16,
    "m4a": b"\x00\x00\x00\x20ftypM4A " + b"\x00" * 16,
    "webm": b"\x1a\x45\xdf\xa3\x9f\x42\x86\x81" + b"\x00" * 16,
    "aac_adts": b"\xff\xf1\x50\x80" + b"\x00" * 16,
    "aiff": b"FORM\x00\x00\x00\x20AIFFCOMM" + b"\x00" * 16,
}
EXPECTED_FORMAT = {
    "wav": "wav",
    "mp3_id3": "mp3",
    "mp3_frame": "mp3",
    "ogg": "ogg",
    "flac": "flac",
    "m4a": "mov",
    "webm": "matroska",
    "aac_adts": "aac",
    "aiff": "aiff",
}


@pytest.fixture
def ffmpeg_calls(monkeypatch):
    calls = []

    def fake_run(cmd, *args, **kwargs):
        calls.append(list(cmd))
        with open(cmd[-1], "wb") as fh:
            fh.write(b"OggS")
        return None

    monkeypatch.setattr(audio.subprocess, "run", fake_run)
    return calls


def _input_options(cmd):
    """The options that apply to the input: everything before -i."""
    return cmd[: cmd.index("-i")]


def test_protocol_whitelist_and_input_restrictions(tmp_path, ffmpeg_calls):
    src = tmp_path / "voice.wav"
    src.write_bytes(WAV)
    audio.convert_to_opus_ogg(str(src), str(tmp_path / "out.ogg"))
    cmd = ffmpeg_calls[0]
    opts = _input_options(cmd)
    assert "-protocol_whitelist" in opts
    assert opts[opts.index("-protocol_whitelist") + 1] == "file"
    assert "-f" in opts, "input format must be explicit so ffmpeg does not probe (e.g. into hls/concat)"
    assert opts[opts.index("-f") + 1] == "wav"
    assert "-nostdin" in cmd


def test_input_passed_as_explicit_file_url(tmp_path, ffmpeg_calls):
    src = tmp_path / "-looks-like-an-option.wav"
    src.write_bytes(WAV)
    audio.convert_to_opus_ogg(str(src), str(tmp_path / "out.ogg"))
    cmd = ffmpeg_calls[0]
    assert cmd[cmd.index("-i") + 1] == "file:" + os.path.abspath(src)


@pytest.mark.parametrize("kind", sorted(HEADERS))
def test_input_format_from_magic_bytes(tmp_path, ffmpeg_calls, kind):
    # The extension is deliberately wrong: the format comes from the content.
    src = tmp_path / "input.bin"
    src.write_bytes(HEADERS[kind])
    audio.convert_to_opus_ogg(str(src), str(tmp_path / "out.ogg"))
    opts = _input_options(ffmpeg_calls[0])
    assert opts[opts.index("-f") + 1] == EXPECTED_FORMAT[kind]


@pytest.mark.parametrize(
    "content",
    [
        b"#EXTM3U\n#EXT-X-VERSION:3\nfile:///etc/passwd\n",
        b"ffconcat version 1.0\nfile '/etc/passwd'\n",
        b"[playlist]\nFile1=http://example.invalid/x\n",
        b"<?xml version='1.0'?><MPD></MPD>",
        b"",
        b"just some text",
    ],
)
def test_unrecognised_input_rejected_before_ffmpeg(tmp_path, ffmpeg_calls, content):
    src = tmp_path / "voice.mp3"
    src.write_bytes(content)
    with pytest.raises(ValueError):
        audio.convert_to_opus_ogg(str(src), str(tmp_path / "out.ogg"))
    assert ffmpeg_calls == []


def test_output_format_explicit(tmp_path, ffmpeg_calls):
    src = tmp_path / "voice.wav"
    src.write_bytes(WAV)
    out = tmp_path / "out.ogg"
    audio.convert_to_opus_ogg(str(src), str(out))
    cmd = ffmpeg_calls[0]
    after_input = cmd[cmd.index("-i") + 2 :]
    assert after_input[after_input.index("-f") + 1] == "ogg"
    assert cmd[-1] == os.path.abspath(out)


def test_temp_output_in_requested_dir(tmp_path, ffmpeg_calls):
    src = tmp_path / "voice.wav"
    src.write_bytes(WAV)
    outdir = tmp_path / "outbox"
    outdir.mkdir()
    result = audio.convert_to_opus_ogg_temp(str(src), output_dir=str(outdir))
    assert os.path.dirname(result) == str(outdir)
    assert result.endswith(".ogg")
