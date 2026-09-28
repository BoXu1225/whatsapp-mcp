# Changelog

All notable changes to this fork are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Versions before 0.2.0 are [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp),
which this fork started from.

## [Unreleased]

### Added

- Schema versioning for `messages.db`: a `schema_version` table and ordered
  migrations, each in its own transaction. Before the first pending migration
  on a database with data, the bridge saves `store/messages.db.bak-<version>-<UTC
  timestamp>` (mode 0600). A failed migration is rolled back and the bridge
  exits 1, naming the backup.
- `messages.sender_alt`: the sender's other address (phone JID for a LID
  sender and vice versa) when known (#8).

### Changed

- Timestamps are stored in UTC. The MCP server reads `Z`, offsets and local
  (no offset) times in `after`/`before`/`since`, and shows times in local
  time with their offset, e.g. `2024-03-31 02:30:00+01:00` (#14).
- A 1:1 chat is keyed by the person's `@lid` JID when the LID is known, else
  by their phone JID. Phone-number and LID copies of a chat are merged, on
  upgrade and when a message reveals the mapping. The first such live merge
  in a run backs up `messages.db` first (#9).
- Senders are stored as full JIDs without device part. In a 1:1 chat the
  other person uses the chat's JID. Your own messages use your LID in LID
  chats and your phone JID elsewhere. The MCP server reads old bare-number
  senders too (#8).

### Migrations

1. `utc_timestamps`: converts `messages.timestamp` and `chats.last_message_time`.
2. `sender_alt_column`: adds `messages.sender_alt`.
3. `canonical_chats`: merges PN/LID duplicate chats (a message ID in both keeps
   the LID chat's copy, the better name and later time win) and re-keys
   phone-number chats with a known LID.
4. `canonical_senders`: bare numbers become full JIDs, device parts are
   dropped, own messages follow the rule above and `sender_alt` is filled.
   Bare numbers whose server can't be determined are left and counted.

Migrations 3 and 4 need the device store and run only when logged in; they
wait for a start after login otherwise.

### Upgrade notes

- Rebuild and restart the bridge, then restart the MCP server. The first start
  writes `store/messages.db.bak-0-<timestamp>`; delete it when you're satisfied.
- Until the bridge has migrated, time filters on old rows may be off by the
  UTC offset.
- A re-keyed chat's JID changes from `<phone>@s.whatsapp.net` to `<lid>@lid`.
  Media already downloaded stays under the old `store/<phone JID>/` folder,
  and `download_media` with the new JID finds it there.
- A chat merged while the bridge runs (a message revealed the phone/LID
  mapping) gets the same treatment as the migration, including canonical
  senders for the moved messages. Each run that merges a chat this way writes
  one more `store/messages.db.bak-<version>-<timestamp>` before its first
  merge, so old backups can pile up; delete the ones you don't need.
- Downgrading is not supported: an older bridge doesn't know the new schema
  or the UTC timestamps. To go back, stop the bridge and restore the `.bak`
  file from before the upgrade as `store/messages.db`.

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
