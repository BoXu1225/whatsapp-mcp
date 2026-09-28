# Changelog

All notable changes to this fork are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Versions before 0.2.0 are [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp),
which this fork started from.

## [Unreleased]

## [0.2.0] - 2026-09-28

First release of the fork. Issue numbers refer to
[BoXu1225/whatsapp-mcp](https://github.com/BoXu1225/whatsapp-mcp/issues).
Several tool outputs changed; see "Breaking changes to tool output" in the README.

### Added

- `get_status` tool and bridge `GET /api/health` endpoint (`connected`,
  `logged_in`, `last_event`, `started_at`, `version`). When the bridge is down,
  `get_status` reports the newest stored message and the database's last write
  instead (#7).
- A one-line header saying whether the bridge is up and how fresh the data is:
  the first line of `list_messages`, and `status` in `list_chats` and
  `list_awaiting_reply`, which now return `{"status", "chats"}` (#7).
- `list_awaiting_reply` tool: chats whose newest message isn't from you (#22).
- `media_only` and `media_type` filters for `list_messages` (#22).
- `search_contacts` takes a `limit` and reports `total_matches` and `truncated`.
- Send tools resolve the recipient against known chats and contacts, reject
  unknown recipients unless `allow_unknown=true`, reject ambiguous bare numbers,
  and return `recipient_jid` and `recipient_name` (#13).
- API token: the bridge writes `store/bridge_token` and requires it in an
  `X-Bridge-Token` header on every `/api/*` request (#1).
- `-debug` flag for logging message content, names and paths (#4).
- `scripts/bridge.sh` to build, start, stop, check and follow the bridge
  (`start|stop|restart|status|health|logs|fg`), with `umask 077` and log
  rotation over 10 MB (#25).
- Go and Python test suites and a CI workflow (#23, partial).
- Weekly workflow that opens a PR moving whatsmeow to `main` after vet, build
  and tests pass; Dependabot for GitHub Actions, uv and Go modules (#24).
- `CHANGELOG.md`.

### Changed

- The REST API only accepts a loopback `Host` and `application/json` POST bodies (#1).
- Files can only be sent from `whatsapp-bridge/store/outbox/` and directories
  in `WHATSAPP_SEND_ALLOWED_DIRS`; the file is read from the handle that was
  checked (#2).
- `list_messages` prints each message once, oldest first, with message IDs;
  context is off by default when `chat_jid` is set, and matches are marked
  `>>` (#10).
- Chats report their newest stored message (`last_message`, `last_message_id`,
  `last_sender_name`, `last_message_at`) (#11).
- Contacts are resolved through the whatsmeow device store's LID map and
  contacts; a LID is no longer reported as a phone number, and lookups are
  exact (#12).
- `%` and `_` in `list_chats` and `list_messages` queries match literally.
- Database errors are returned as tool errors instead of empty results (#11).
- The bridge binds its port before connecting to WhatsApp and exits with
  status 1 if it is taken, so a second instance can't take over the session (#1).
- Go module path is `github.com/BoXu1225/whatsapp-mcp/whatsapp-bridge`; the
  binary is still `whatsapp-bridge` (#25).
- README rewritten for the fork; LICENSE adds the fork's copyright (#25).

### Removed

- Unused `httpx` dependency and the custom `min()` helper (Go has a builtin) (#25).

### Security

- Downloaded media filenames are sanitised (control characters, length) and
  downloads stay inside the store; chat JIDs can't map onto the outbox or other
  store entries (#3).
- Message content is only logged with `-debug`; `store/` is made 0700 and its
  files 0600 (#4).
- `.gitignore` covers downloaded media, SQLite side files and logs (#5).
- ffmpeg runs with `-protocol_whitelist file` and an explicit input format (#6).
- `download_media` logs errors to stderr, not stdout (the MCP transport).

[Unreleased]: https://github.com/BoXu1225/whatsapp-mcp/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/BoXu1225/whatsapp-mcp/releases/tag/v0.2.0
