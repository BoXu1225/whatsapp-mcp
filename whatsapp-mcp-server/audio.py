import os
import subprocess
import tempfile


def probe_audio_format(path):
    """Return the ffmpeg demuxer for an audio file, chosen from its magic bytes.

    ffmpeg is always given this format explicitly (plus -protocol_whitelist
    file), so it never auto-detects a playlist-style input (HLS, ffconcat, ...)
    that could make it open other local files or URLs.

    Raises:
        ValueError: If the content is not a recognised audio container.
    """
    with open(path, "rb") as fh:
        head = fh.read(16)
    if head[:4] == b"RIFF" and head[8:12] == b"WAVE":
        return "wav"
    if head[:4] == b"OggS":
        return "ogg"
    if head[:4] == b"fLaC":
        return "flac"
    if head[:3] == b"ID3":
        return "mp3"
    if head[:4] == b"FORM" and head[8:12] in (b"AIFF", b"AIFC"):
        return "aiff"
    if head[4:8] == b"ftyp":  # MP4 / M4A / MOV / 3GP
        return "mov"
    if head[:4] == b"\x1a\x45\xdf\xa3":  # EBML: Matroska / WebM
        return "matroska"
    if len(head) >= 2 and head[0] == 0xFF and head[1] & 0xE0 == 0xE0:
        # MPEG audio frame sync; layer bits 00 mean an AAC ADTS stream.
        return "aac" if (head[1] >> 1) & 0x03 == 0 else "mp3"
    raise ValueError(f"Unrecognised or unsupported audio format: {path}")


def convert_to_opus_ogg(input_file, output_file=None, bitrate="32k", sample_rate=24000):
    """
    Convert an audio file to Opus format in an Ogg container.
    
    Args:
        input_file (str): Path to the input audio file
        output_file (str, optional): Path to save the output file. If None, replaces the
                                    extension of input_file with .ogg
        bitrate (str, optional): Target bitrate for Opus encoding (default: "32k")
        sample_rate (int, optional): Sample rate for output (default: 24000)
    
    Returns:
        str: Path to the converted file
        
    Raises:
        FileNotFoundError: If the input file doesn't exist
        ValueError: If the input is not a recognised audio format
        RuntimeError: If the ffmpeg conversion fails
    """
    if not os.path.isfile(input_file):
        raise FileNotFoundError(f"Input file not found: {input_file}")
    
    # If no output file is specified, replace the extension with .ogg
    if output_file is None:
        output_file = os.path.splitext(input_file)[0] + ".ogg"
    
    # Ensure the output directory exists
    output_dir = os.path.dirname(output_file)
    if output_dir and not os.path.exists(output_dir):
        os.makedirs(output_dir)
    
    input_format = probe_audio_format(input_file)

    # Build the ffmpeg command. The input options restrict ffmpeg to the one
    # local file: only the file protocol, an explicit demuxer (no probing), and
    # the input given as a file: URL so its name can't be read as an option
    # or another protocol.
    cmd = [
        "ffmpeg",
        "-nostdin",
        "-hide_banner",
        "-protocol_whitelist", "file",
        "-f", input_format,
        "-i", "file:" + os.path.abspath(input_file),
        "-vn",
        "-c:a", "libopus",
        "-b:a", bitrate,
        "-ar", str(sample_rate),
        "-application", "voip",  # Optimize for voice
        "-vbr", "on",           # Variable bitrate
        "-compression_level", "10",  # Maximum compression
        "-frame_duration", "60",     # 60ms frames (good for voice)
        "-f", "ogg",
        "-y",                        # Overwrite output file if it exists
        os.path.abspath(output_file)
    ]

    try:
        # Run the ffmpeg command and capture output
        process = subprocess.run(  # noqa: F841
            cmd,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            check=True
        )
        return os.path.abspath(output_file)
    except subprocess.CalledProcessError as e:
        raise RuntimeError(f"Failed to convert audio. You likely need to install ffmpeg {e.stderr}")


def convert_to_opus_ogg_temp(input_file, bitrate="32k", sample_rate=24000, output_dir=None):
    """
    Convert an audio file to Opus format in an Ogg container and store in a temporary file.
    
    Args:
        input_file (str): Path to the input audio file
        bitrate (str, optional): Target bitrate for Opus encoding (default: "32k")
        sample_rate (int, optional): Sample rate for output (default: 24000)
        output_dir (str, optional): Directory for the temporary file (default: the
                                    system temp dir). Pass the bridge outbox so the
                                    result can be sent.
    
    Returns:
        str: Path to the temporary file with the converted audio
        
    Raises:
        FileNotFoundError: If the input file doesn't exist
        RuntimeError: If the ffmpeg conversion fails
    """
    # Create a temporary file with .ogg extension
    temp_file = tempfile.NamedTemporaryFile(suffix=".ogg", delete=False, dir=output_dir)
    temp_file.close()
    
    try:
        # Convert the audio
        convert_to_opus_ogg(input_file, temp_file.name, bitrate, sample_rate)
        return temp_file.name
    except Exception as e:
        # Clean up the temporary file if conversion fails
        if os.path.exists(temp_file.name):
            os.unlink(temp_file.name)
        raise e


if __name__ == "__main__":
    # Example usage
    import sys
    
    if len(sys.argv) < 2:
        print("Usage: python audio.py input_file [output_file]")
        sys.exit(1)
    
    input_file = sys.argv[1]
    
    try:
        result = convert_to_opus_ogg_temp(input_file)
        print(f"Successfully converted to: {result}")
    except Exception as e:
        print(f"Error: {e}")
        sys.exit(1)
