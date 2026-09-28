# WhatsApp MCP Server

This is a Model Context Protocol (MCP) server for WhatsApp.

With this you can search and read your personal Whatsapp messages (including images, videos, documents, and audio messages), search your contacts and send messages to either individuals or groups. You can also send media files including images, videos, documents, and audio messages.

It connects to your **personal WhatsApp account** directly via the Whatsapp web multidevice API (using the [whatsmeow](https://github.com/tulir/whatsmeow) library). All your messages are stored locally in a SQLite database and only sent to an LLM (such as Claude) when the agent accesses them through tools (which you control).

Here's an example of what you can do when it's connected to Claude.

![WhatsApp MCP](./example-use.png)

> To get updates on this and other projects I work on [enter your email here](https://docs.google.com/forms/d/1rTF9wMBTN0vPfzWuQa2BjfGKdKIpTbyeKxhPMcEzgyI/preview)

> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). This means that project injection could lead to private data exfiltration.

## Installation

### Prerequisites

- Go
- Python 3.6+
- Anthropic Claude Desktop app (or Cursor)
- UV (Python package manager), install with `curl -LsSf https://astral.sh/uv/install.sh | sh`
- FFmpeg (_optional_) - Only needed for audio messages. If you want to send audio files as playable WhatsApp voice messages, they must be in `.ogg` Opus format. With FFmpeg installed, the MCP server will automatically convert non-Opus audio files. Without FFmpeg, you can still send raw audio files using the `send_file` tool.

### Steps

1. **Clone this repository**

   ```bash
   git clone https://github.com/lharries/whatsapp-mcp.git
   cd whatsapp-mcp
   ```

2. **Run the WhatsApp bridge**

   Navigate to the whatsapp-bridge directory and run the Go application:

   ```bash
   cd whatsapp-bridge
   go run .
   ```

   The first time you run it, you will be prompted to scan a QR code. Scan the QR code with your WhatsApp mobile app to authenticate.

   On start the bridge creates `store/bridge_token` (the API token, see [Security](#security)) and `store/outbox/` (the only place files can be sent from by default). Add `-debug` (`go run . -debug`) to log message content; by default it is kept out of the logs.

   After approximately 20 days, you will might need to re-authenticate.

3. **Connect to the MCP server**

   Copy the below json with the appropriate {{PATH}} values:

   ```json
   {
     "mcpServers": {
       "whatsapp": {
         "command": "{{PATH_TO_UV}}", // Run `which uv` and place the output here
         "args": [
           "--directory",
           "{{PATH_TO_SRC}}/whatsapp-mcp/whatsapp-mcp-server", // cd into the repo, run `pwd` and enter the output here + "/whatsapp-mcp-server"
           "run",
           "main.py"
         ]
       }
     }
   }
   ```

   For **Claude**, save this as `claude_desktop_config.json` in your Claude Desktop configuration directory at:

   ```
   ~/Library/Application Support/Claude/claude_desktop_config.json
   ```

   For **Cursor**, save this as `mcp.json` in your Cursor configuration directory at:

   ```
   ~/.cursor/mcp.json
   ```

4. **Restart Claude Desktop / Cursor**

   Open Claude Desktop and you should now see WhatsApp as an available integration.

   Or restart Cursor.

### Windows Compatibility

If you're running this project on Windows, be aware that `go-sqlite3` requires **CGO to be enabled** in order to compile and work properly. By default, **CGO is disabled on Windows**, so you need to explicitly enable it and have a C compiler installed.

#### Steps to get it working:

1. **Install a C compiler**  
   We recommend using [MSYS2](https://www.msys2.org/) to install a C compiler for Windows. After installing MSYS2, make sure to add the `ucrt64\bin` folder to your `PATH`.  
   → A step-by-step guide is available [here](https://code.visualstudio.com/docs/cpp/config-mingw).

2. **Enable CGO and run the app**

   ```bash
   cd whatsapp-bridge
   go env -w CGO_ENABLED=1
   go run .
   ```

Without this setup, you'll likely run into errors like:

> `Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work.`

## Architecture Overview

This application consists of two main components:

1. **Go WhatsApp Bridge** (`whatsapp-bridge/`): A Go application that connects to WhatsApp's web API, handles authentication via QR code, and stores message history in SQLite. It serves as the bridge between WhatsApp and the MCP server.

2. **Python MCP Server** (`whatsapp-mcp-server/`): A Python server implementing the Model Context Protocol (MCP), which provides standardized tools for Claude to interact with WhatsApp data and send/receive messages.

### Data Storage

- All message history is stored in a SQLite database within the `whatsapp-bridge/store/` directory
- The database maintains tables for chats and messages
- Messages are indexed for efficient searching and retrieval

## Usage

Once connected, you can interact with your WhatsApp contacts through Claude, leveraging Claude's AI capabilities in your WhatsApp conversations.

### MCP Tools

Claude can access the following tools to interact with WhatsApp:

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
- Chat objects:
  - `last_message` is the chat's newest stored message, with media rendered as `[type: filename]`.
  - New fields: `last_message_id`, `last_sender_name` and `last_message_at`.
- Send tools:
  - They reject recipients that aren't a known chat or contact unless `allow_unknown=true`.
  - They reject ambiguous bare numbers.
  - They may send to a contact's existing chat JID instead of the form you gave.
  - Results add `recipient_jid` and `recipient_name`.
- Database errors are returned as tool errors instead of empty results or `null`.

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
- **Logs and files.** Message text, names and file paths are only logged with `-debug`. On start the bridge makes `store/` 0700 and the databases (`whatsapp.db` holds the session keys), `bridge_token` and other files in it 0600. If you start the bridge from a script, `umask 077` keeps new files private too.
- **ffmpeg** runs with `-protocol_whitelist file` and an input format taken from the file's contents, so playlist-style inputs are rejected.

## Technical Details

1. Claude sends requests to the Python MCP server
2. The MCP server queries the Go bridge for WhatsApp data or directly to the SQLite database
3. The Go accesses the WhatsApp API and keeps the SQLite database up to date
4. Data flows back through the chain to Claude
5. When sending messages, the request flows from Claude through the MCP server to the Go bridge and to WhatsApp

## Troubleshooting

- If you encounter permission issues when running uv, you may need to add it to your PATH or use the full path to the executable.
- Make sure both the Go application and the Python server are running for the integration to work properly.

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

- **QR Code Not Displaying**: If the QR code doesn't appear, try restarting the authentication script. If issues persist, check if your terminal supports displaying QR codes.
- **WhatsApp Already Logged In**: If your session is already active, the Go bridge will automatically reconnect without showing a QR code.
- **Device Limit Reached**: WhatsApp limits the number of linked devices. If you reach this limit, you'll need to remove an existing device from WhatsApp on your phone (Settings > Linked Devices).
- **No Messages Loading**: After initial authentication, it can take several minutes for your message history to load, especially if you have many chats.
- **WhatsApp Out of Sync**: If your WhatsApp messages get out of sync with the bridge, delete both database files (`whatsapp-bridge/store/messages.db` and `whatsapp-bridge/store/whatsapp.db`) and restart the bridge to re-authenticate.

For additional Claude Desktop integration troubleshooting, see the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues). The documentation includes helpful tips for checking logs and resolving common issues.
