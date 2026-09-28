# WhatsApp MCP Server

A Model Context Protocol (MCP) server for WhatsApp: search and read your personal WhatsApp messages (including images, videos, documents and audio), search your contacts, see which chats are waiting for your reply, and send messages and files to people or groups.

It connects to your **personal WhatsApp account** directly via the WhatsApp Web multi-device API (using the [whatsmeow](https://github.com/tulir/whatsmeow) library). All your messages are stored locally in a SQLite database and only sent to an LLM (such as Claude) when the agent accesses them through tools (which you control).

> **About this fork.** This is a maintained fork of [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) by Luke Harries, which is no longer maintained. It adds an authenticated, loopback-only bridge API, a send allowlist, recipient checks before sending, correct contact/LID handling, bridge health reporting, tests and CI. See [CHANGELOG.md](CHANGELOG.md) for what changed, including changes to tool output.

![WhatsApp MCP](./example-use.png)

> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/): prompt injection in an incoming message could lead to private data exfiltration. See [Security](#security) for what the bridge does to limit this.

## Installation

Tested on macOS. Linux should work the same way; Windows is untested (see [Windows](#windows)).

### Prerequisites

- Go (the version in `whatsapp-bridge/go.mod`, or let Go fetch it) and a C compiler (go-sqlite3 uses cgo; on macOS, the Xcode command line tools)
- Python 3.11 or newer
- [uv](https://docs.astral.sh/uv/), install with `curl -LsSf https://astral.sh/uv/install.sh | sh`
- An MCP client: Claude Desktop, Claude Code, Cursor, ...
- FFmpeg (_optional_), only needed to send audio as voice messages. Voice messages must be `.ogg` Opus; with FFmpeg installed, other audio formats are converted automatically. Without it you can still send audio files with `send_file`.

### Steps

1. **Clone this repository**

   ```bash
   git clone https://github.com/BoXu1225/whatsapp-mcp.git
   cd whatsapp-mcp
   ```

2. **Log in: run the bridge in the foreground once**

   ```bash
   scripts/bridge.sh fg
   ```

   This builds `whatsapp-bridge/whatsapp-bridge` and runs it. The first time, it prints a QR code: scan it with WhatsApp on your phone (Settings > Linked Devices > Link a Device). Once it says it is connected, stop it with Ctrl+C. The session is kept in `whatsapp-bridge/store/`, so later starts don't need a QR code unless WhatsApp logs the device out (for example after the phone has been offline for a long time, or if you remove the linked device).

   On start the bridge creates `store/bridge_token` (the API token, see [Security](#security)) and `store/outbox/` (the only place files can be sent from by default).

3. **Run the bridge in the background**

   ```bash
   scripts/bridge.sh start
   ```

   `scripts/bridge.sh` manages the bridge. It works through a symlink, e.g. `ln -s "$PWD/scripts/bridge.sh" ~/.local/bin/wa-bridge`.

   | Command | What it does |
   | --- | --- |
   | `start` | Build if the Go sources changed, then start in the background (no-op if running). Logs to `whatsapp-bridge/bridge.log`, rotated to `bridge.log.1` over 10 MB. |
   | `stop` / `restart` | Stop / stop then start. |
   | `status` | Whether the process is running. |
   | `health` | Ask the running bridge for `/api/health`: connected, logged in, last event, start time, version. Exits 0 only if connected and logged in. |
   | `logs` | Follow the log. |
   | `fg` | Run in the foreground, e.g. to scan a new QR code. |

   The script sets `umask 077`, so the log and anything the bridge creates stay owner-only. To log message content (off by default), run the binary directly with `-debug`: `cd whatsapp-bridge && ./whatsapp-bridge -debug`.

   The bridge listens on `127.0.0.1:8080` only. It must be running for new messages to arrive and for sending and media downloads; the read tools work from the local database even when it isn't, and say so (see `get_status`).

4. **Connect your MCP client**

   The server runs with `uv --directory <repo>/whatsapp-mcp-server run main.py`. Use the full path to `uv` (from `which uv`) and to your clone.

   For **Claude Code**:

   ```bash
   claude mcp add whatsapp -- "$(which uv)" --directory "$PWD/whatsapp-mcp-server" run main.py
   ```

   For **Claude Desktop** (`~/Library/Application Support/Claude/claude_desktop_config.json`) or **Cursor** (`~/.cursor/mcp.json`):

   ```json
   {
     "mcpServers": {
       "whatsapp": {
         "command": "/path/to/uv",
         "args": ["--directory", "/path/to/whatsapp-mcp/whatsapp-mcp-server", "run", "main.py"]
       }
     }
   }
   ```

   Then restart the client.

### Windows

Untested. go-sqlite3 needs cgo, which is off by default on Windows: install a C compiler (for example via [MSYS2](https://www.msys2.org/), adding `ucrt64\bin` to `PATH`), then build with `go env -w CGO_ENABLED=1` and `go build -o whatsapp-bridge.exe .` in `whatsapp-bridge/`. `scripts/bridge.sh` needs a Unix shell. Without cgo you'll see `Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work.`

## Architecture Overview

This application consists of two main components:

1. **Go WhatsApp Bridge** (`whatsapp-bridge/`): A Go application that connects to WhatsApp's web API, handles authentication via QR code, and stores message history in SQLite. It serves as the bridge between WhatsApp and the MCP server.

2. **Python MCP Server** (`whatsapp-mcp-server/`): A Python server implementing the Model Context Protocol (MCP), which provides standardized tools for Claude to interact with WhatsApp data and send/receive messages.

### Data Storage

- Message history is in `whatsapp-bridge/store/messages.db` (tables `chats` and `messages`); the WhatsApp session, contacts and LID map are in whatsmeow's `whatsapp-bridge/store/whatsapp.db`.
- Downloaded media goes to `whatsapp-bridge/store/<chat>/`; files to send go in `whatsapp-bridge/store/outbox/`.
- `store/` is private data and is git-ignored.

### Schema migrations

`messages.db` carries a `schema_version` table. On start the bridge applies pending migrations in order, each in its own transaction. Before the first one runs on a database with data it saves a copy as `store/messages.db.bak-<version>-<UTC timestamp>` (mode 0600). If a migration fails, its changes are rolled back and the bridge exits with status 1, naming the backup. Current migrations:

1. Timestamps are stored in UTC (existing rows are converted).
2. `messages.sender_alt` holds the sender's other address (phone JID for a LID sender, and vice versa) when known.
3. A 1:1 chat is keyed by the person's LID JID when the LID is known, else by their phone JID. Phone-number and LID copies of the same chat are merged (also later, when a message reveals the mapping; the first such merge in a run backs up `messages.db` first). This step and the next run once the device store has loaded, and only when logged in.
4. Senders are full JIDs without device part (`user@server`). In a 1:1 chat the other person uses the chat's JID. Your own messages use your LID in LID chats and your phone JID elsewhere, including groups. Bare numbers whose server can't be determined are left as they were.

To upgrade, rebuild and restart the bridge. Restart the MCP server too. Until the bridge has migrated the database, time filters may be off by the UTC offset. Downloaded media of a re-keyed chat stays in the old `store/<phone JID>/` folder, where `download_media` with the new chat JID still finds it. Downgrading is not supported; to go back, restore the `.bak` file as `store/messages.db`.

## Usage

### MCP Tools

Claude can access the following tools to interact with WhatsApp:

- **get_status**: Whether the bridge is running and connected, when it last saw a WhatsApp event, and how fresh the stored data is (newest message, last database write). Use it when results look stale
- **search_contacts**: Search contacts by name or number, including contacts you have no chat with. Returns `{contacts, total_matches, truncated}` (default `limit` 50, with a note when truncated); each contact has separate `jid`, `phone`, `lid`, `name` and `chat_jid` fields, and a LID is never reported as a phone number
- **list_messages**: Retrieve messages with optional filters (date range, chat, sender, text, `media_only`, `media_type`) and context. One line per message, oldest first, each with chat name and JID, message ID and sender name. Context is off by default when `chat_jid` is set; with context, matches are marked `>>`
- **list_chats**: List available chats with metadata and each chat's newest message
- **list_awaiting_reply**: Chats whose newest message isn't from you, newest first (direct chats; groups with `include_groups=True`; optional `since`)
- **get_chat**: Get information about a specific chat
- **get_direct_chat_by_contact**: Find the direct chat with a contact by phone number, LID or JID (exact match; `+`, spaces and dashes are ignored)
- **get_contact_chats**: List all chats involving a specific contact, each once
- **get_last_interaction**: Get the most recent message with a contact
- **get_message_context**: Retrieve context around a specific message (`before` and `after` oldest first)
- **send_message**: Send a WhatsApp message to a person or group
- **send_file**: Send a file (image, video, raw audio, document) to a person or group
- **send_audio_message**: Send an audio file as a WhatsApp voice message (requires the file to be an .ogg opus file or ffmpeg must be installed)
- **download_media**: Download media from a WhatsApp message and get the local file path

`list_messages`, `list_chats` and `list_awaiting_reply` start with a one-line header such as `[bridge up · data as of 2026-09-28T09:12:00Z]` or `[WARNING: bridge down since ~… · data as of …; newer messages are missing]`, so a stopped bridge doesn't look like a quiet inbox. For `list_messages` it is the first line of the text; `list_chats` and `list_awaiting_reply` return `{"status": <header>, "chats": [...]}`. The header's bridge check times out after 0.5 s and is cached for 5 s; `get_status` always checks afresh with a 2 s timeout.

The send tools resolve the recipient against your chats and contacts and pass the full JID to the bridge. A known contact with a direct chat is sent to that chat. A number that is a known LID is sent to its `@lid` JID, and a bare number that is both a known phone number and a known LID is rejected as ambiguous (pass the full JID). Recipients that aren't a known chat or contact are rejected unless `allow_unknown=true` is passed. The result includes `recipient_jid` and `recipient_name`. The tool descriptions tell the model to send only when you explicitly ask, after showing you the exact recipient and content.

Contact names and the phone-number/LID mapping come from the bridge's whatsmeow device store (`whatsapp-bridge/store/whatsapp.db`, next to `messages.db`; override with the `WHATSMEOW_DB_PATH` environment variable). It is opened read-only. Without it, the tools fall back to the chats table. Database errors are returned as tool errors rather than empty results.

#### Breaking changes to tool output

If you have prompts or scripts built on the earlier tool output, note:

- `search_contacts` no longer returns `phone_number` (which held the LID for `@lid` chats). It returns `{contacts, total_matches, truncated}`, and each contact has `jid`, `phone`, `lid`, `name` and `chat_jid`.
- `list_messages`:
  - `include_context` defaults to off when `chat_jid` is set (on otherwise).
  - The line format is now `[time] Chat: <name> (<jid>) | ID: <id> | From: <name or Me>: <text>`, oldest first, with no repeated messages. With context, matches are marked `>>`.
  - `limit`/`page` select the newest matches, which are then printed oldest first.
  - Media shows as `[type: filename] caption`.
  - `%` and `_` in `query` match literally (the same holds for `list_chats`).
- `get_message_context` returns `before` oldest first.
- Times are shown in local time with their UTC offset (`2024-03-31 02:30:00+01:00`). `after`, `before` and `since` accept `Z` or an offset; a time without one is local time.
- A 1:1 chat that used to appear twice (phone JID and `@lid` JID) is one chat, keyed by the `@lid` JID.
- Chat objects:
  - `last_message` is the chat's newest stored message, with media rendered as `[type: filename]`.
  - New fields: `last_message_id`, `last_sender_name` and `last_message_at`.
- Send tools:
  - They reject recipients that aren't a known chat or contact unless `allow_unknown=true`.
  - They reject ambiguous bare numbers.
  - They may send to a contact's existing chat JID instead of the form you gave.
  - Results add `recipient_jid` and `recipient_name`.
- Database errors are returned as tool errors instead of empty results or `null`.
- `list_messages` output starts with a freshness header line. `list_chats` and `list_awaiting_reply` return `{"status": <header>, "chats": [...]}` instead of a bare list.

### Media Handling Features

The MCP server supports both sending and receiving various media types:

#### Media Sending

You can send various media types to your WhatsApp contacts:

Files can only be sent from `whatsapp-bridge/store/outbox/` (or directories listed in `WHATSAPP_SEND_ALLOWED_DIRS`, see [Security](#security)). Copy a file there before asking Claude to send it.

- **Images, Videos, Documents**: Use the `send_file` tool to share any supported media type.
- **Voice Messages**: Use the `send_audio_message` tool to send audio files as playable WhatsApp voice messages.
  - For optimal compatibility, audio files should be in `.ogg` Opus format.
  - With FFmpeg installed, the system will automatically convert other audio formats (MP3, WAV, etc.) to the required format.
  - Without FFmpeg, you can still send raw audio files using the `send_file` tool, but they won't appear as playable voice messages.

#### Media Downloading

By default, just the metadata of the media is stored in the local database. The message will indicate that media was sent. To access this media you need to use the download_media tool which takes the `message_id` and `chat_jid` (which are shown when printing messages containing the meda), this downloads the media and then returns the file path which can be then opened or passed to another tool.

## Security

Incoming WhatsApp messages are untrusted input read by an LLM that can also send messages, so the bridge limits what a confused or prompt-injected client can do:

- **API token.** On start the bridge ensures `whatsapp-bridge/store/bridge_token` exists (random, mode 0600). Every `/api/*` request must send it in an `X-Bridge-Token` header, POST bodies must be `Content-Type: application/json`, and the `Host` header must be `127.0.0.1:8080` or `localhost:8080`. Other requests get 401, 415 or 403. The MCP server reads the token from the directory holding `messages.db` on every call, so nothing needs configuring; if you call the API yourself, send the header.
- **Send allowlist.** `send_file` and `send_audio_message` only send files that, after resolving `..` and symlinks, are inside `whatsapp-bridge/store/outbox/`. To allow more directories set `WHATSAPP_SEND_ALLOWED_DIRS` to absolute paths (separated by `:`, or `;` on Windows; relative entries are ignored with a warning) in the environment of both the bridge and the MCP server (for Claude Desktop, via an `"env"` entry in the server config). The bridge enforces this; the MCP server checks it too for a clearer error. Audio converted by ffmpeg is written to the outbox and removed after sending.
- **Downloads.** Sender-provided document filenames are reduced to a base name, and downloads always stay inside `whatsapp-bridge/store/<chat>/` (dirs 0700, files 0600).
- **Logs and files.** Message text, names and file paths are only logged with `-debug`. On start the bridge makes `store/` 0700 and the databases (`whatsapp.db` holds the session keys), `bridge_token` and other files in it 0600. `scripts/bridge.sh` sets `umask 077` so new files, including the log, are private too; do the same if you start the bridge some other way.
- **Health.** `GET /api/health` is behind the same token and Host checks as the other endpoints.
- **ffmpeg** runs with `-protocol_whitelist file` and an input format taken from the file's contents, so playlist-style inputs are rejected.

## Technical Details

1. The MCP client (e.g. Claude) calls tools on the Python MCP server over stdio.
2. Read tools query the SQLite databases in `whatsapp-bridge/store/` directly.
3. The Go bridge stays connected to WhatsApp and keeps those databases up to date.
4. Sending, media downloads and `get_status` go through the bridge's REST API on `127.0.0.1:8080`.

## Troubleshooting

- **Stale or missing messages**: call `get_status` or run `scripts/bridge.sh health`. If the bridge is down, `scripts/bridge.sh start`; if it's logged out, `scripts/bridge.sh stop && scripts/bridge.sh fg` and scan the QR code. Check `scripts/bridge.sh logs` for errors.
- **Port 8080 in use**: another bridge (or something else) is listening. The bridge exits with status 1 instead of starting a second WhatsApp session; stop the other one first.
- **`uv` not found by the MCP client**: use the full path to `uv` in the client config.

### "Client outdated" (405)

If the bridge log shows `Client outdated (405) connect failure` (or the bridge connects and immediately drops with a 405), WhatsApp has stopped accepting the WhatsApp Web version compiled into your build of whatsmeow. Update whatsmeow and rebuild:

```bash
cd whatsapp-bridge
go get go.mau.fi/whatsmeow@main
go mod tidy
go build -o whatsapp-bridge . && go test ./...
```

Then restart the bridge. Your login session is kept. The weekly "Update whatsmeow" workflow opens a PR with this update, so usually merging that PR and pulling is enough.

### Authentication Issues

- **QR Code Not Displaying**: run the bridge in the foreground (`scripts/bridge.sh fg`); in the background the QR code only goes to the log. If it still doesn't show, check that your terminal can display it.
- **WhatsApp Already Logged In**: If your session is already active, the Go bridge will automatically reconnect without showing a QR code.
- **Device Limit Reached**: WhatsApp limits the number of linked devices. If you reach this limit, you'll need to remove an existing device from WhatsApp on your phone (Settings > Linked Devices).
- **No Messages Loading**: After initial authentication, it can take several minutes for your message history to load, especially if you have many chats.
- **WhatsApp Out of Sync**: If your WhatsApp messages get out of sync with the bridge, delete both database files (`whatsapp-bridge/store/messages.db` and `whatsapp-bridge/store/whatsapp.db`) and restart the bridge to re-authenticate.

For additional Claude Desktop integration troubleshooting, see the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues). The documentation includes helpful tips for checking logs and resolving common issues.
