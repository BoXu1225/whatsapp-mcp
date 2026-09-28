import json
import os.path
import sqlite3
import sys
from contextlib import contextmanager
from dataclasses import dataclass
from datetime import datetime
from typing import List, Optional, Tuple

import requests

import audio
import contacts

MESSAGES_DB_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'whatsapp-bridge', 'store', 'messages.db')
WHATSAPP_API_BASE_URL = "http://localhost:8080/api"

# The bridge writes a random token to <store>/bridge_token on start and rejects
# /api/* requests that don't carry it (see whatsapp-bridge/security.go).
BRIDGE_TOKEN_FILE = "bridge_token"
BRIDGE_TOKEN_HEADER = "X-Bridge-Token"
BRIDGE_TIMEOUT = (5, 120)  # (connect, read) seconds; sends upload media before replying


class BridgeTokenError(RuntimeError):
    """The bridge API token could not be read."""


def _bridge_store_dir() -> str:
    """The bridge's store directory: the one holding messages.db."""
    return os.path.dirname(os.path.abspath(MESSAGES_DB_PATH))


def _bridge_headers() -> dict:
    """Headers for a bridge API call. Reads the token now, so a bridge restart
    that rotates it is picked up without restarting the MCP server."""
    path = os.path.join(_bridge_store_dir(), BRIDGE_TOKEN_FILE)
    try:
        with open(path, encoding="utf-8") as fh:
            token = fh.read().strip()
    except FileNotFoundError:
        raise BridgeTokenError(
            f"Bridge API token not found at {path}. Start (or rebuild and restart) the WhatsApp bridge; it creates this file on start."
        ) from None
    except OSError as e:
        raise BridgeTokenError(f"Could not read bridge API token at {path}: {e}") from None
    if not token:
        raise BridgeTokenError(f"Bridge API token file {path} is empty. Restart the WhatsApp bridge to regenerate it.")
    return {BRIDGE_TOKEN_HEADER: token}


# Files can only be sent from the bridge's outbox (<store>/outbox) and any extra
# directories in this env var. The bridge enforces the same rule; checking here
# too gives a clear error before any request.
SEND_ALLOWED_DIRS_ENV = "WHATSAPP_SEND_ALLOWED_DIRS"


def _outbox_dir() -> str:
    return os.path.join(_bridge_store_dir(), "outbox")


def _send_allowed_dirs() -> List[str]:
    """Outbox plus WHATSAPP_SEND_ALLOWED_DIRS entries, with symlinks resolved.
    Relative entries would depend on the working directory, so they are
    skipped with a warning on stderr (stdout is the MCP transport)."""
    dirs = [_outbox_dir()]
    for d in os.environ.get(SEND_ALLOWED_DIRS_ENV, "").split(os.pathsep):
        if not d.strip():
            continue
        if not os.path.isabs(d):
            print(f"Warning: ignoring {SEND_ALLOWED_DIRS_ENV} entry {d!r}: must be an absolute path", file=sys.stderr)
            continue
        dirs.append(d)
    return [os.path.realpath(d) for d in dirs]


def _resolve_send_path(media_path: str) -> Tuple[Optional[str], Optional[str]]:
    """Return (resolved_path, None) if media_path is a file inside an allowed
    directory after resolving '..' and symlinks, else (None, error message)."""
    real = os.path.realpath(media_path)
    if not os.path.isfile(real):
        return None, f"Media file not found: {media_path}"
    for d in _send_allowed_dirs():
        if real.startswith(d.rstrip(os.sep) + os.sep):
            return real, None
    return None, (
        f"Refusing to send {media_path}: files can only be sent from the outbox ({_outbox_dir()})"
        f" or directories listed in {SEND_ALLOWED_DIRS_ENV}. Copy the file into the outbox first."
    )


def _bridge_post(endpoint: str, payload: dict) -> requests.Response:
    """POST JSON to the bridge API with the token header and a timeout."""
    return requests.post(f"{WHATSAPP_API_BASE_URL}/{endpoint}", json=payload, headers=_bridge_headers(), timeout=BRIDGE_TIMEOUT)


# The whatsmeow device store (contacts, LID<->phone map). Defaults to whatsapp.db
# next to messages.db; set WHATSMEOW_DB_PATH to override.
WHATSMEOW_DB_PATH_ENV = "WHATSMEOW_DB_PATH"


def whatsmeow_db_path() -> str:
    """Path of the whatsmeow device store, resolved at call time."""
    return os.environ.get(WHATSMEOW_DB_PATH_ENV) or os.path.join(os.path.dirname(MESSAGES_DB_PATH), "whatsapp.db")


class WhatsAppDBError(RuntimeError):
    """A query against the bridge's messages database failed."""


def _open_messages_db() -> sqlite3.Connection:
    """Connect to messages.db without creating it if it's missing."""
    if not os.path.isfile(MESSAGES_DB_PATH):
        raise WhatsAppDBError(
            f"WhatsApp messages database not found at {MESSAGES_DB_PATH}. Is the bridge set up and has it synced?"
        )
    return sqlite3.connect(MESSAGES_DB_PATH)


@contextmanager
def _connect():
    """Open messages.db; turn sqlite errors into WhatsAppDBError with a clear message."""
    conn = _open_messages_db()
    try:
        yield conn
    except sqlite3.Error as e:
        raise WhatsAppDBError(f"WhatsApp database query failed: {e}") from e
    finally:
        conn.close()


def load_directory(conn: Optional[sqlite3.Connection] = None) -> contacts.Directory:
    """Contact directory from messages.db and the device store (optional)."""
    if conn is not None:
        return contacts.Directory.load(conn, whatsmeow_db_path())
    with _connect() as own:
        return contacts.Directory.load(own, whatsmeow_db_path())


@dataclass
class Message:
    timestamp: datetime
    sender: str
    content: str
    is_from_me: bool
    chat_jid: str
    id: str
    chat_name: Optional[str] = None
    media_type: Optional[str] = None
    filename: Optional[str] = None
    sender_name: Optional[str] = None

@dataclass
class Chat:
    jid: str
    name: Optional[str]
    last_message_time: Optional[datetime]
    last_message: Optional[str] = None
    last_sender: Optional[str] = None
    last_is_from_me: Optional[bool] = None
    last_message_id: Optional[str] = None
    last_sender_name: Optional[str] = None
    last_message_at: Optional[datetime] = None  # timestamp of last_message

    @property
    def is_group(self) -> bool:
        """Determine if chat is a group based on JID pattern."""
        return self.jid.endswith("@g.us")

@dataclass
class Contact:
    """A person. `phone` and `lid` are bare user parts; either may be None.

    `jid` is the JID to use for this person: their existing direct chat if any,
    otherwise the phone JID, otherwise the LID JID. `chat_jid` is the existing
    direct chat (None if there's no chat yet).
    """
    jid: str
    name: Optional[str]
    phone: Optional[str]
    lid: Optional[str]
    chat_jid: Optional[str] = None

@dataclass
class MessageContext:
    message: Message
    before: List[Message]
    after: List[Message]

def get_sender_name(sender_jid: str, directory: Optional[contacts.Directory] = None) -> str:
    """Display name for a sender (bare user or JID, PN or LID); the input itself if unknown.

    Matches exactly via the phone<->LID map; never by substring.
    """
    try:
        if directory is None:
            directory = load_directory()
    except WhatsAppDBError:
        return sender_jid
    return directory.name_for(sender_jid) or sender_jid


def _content_text(content: Optional[str], media_type: Optional[str], filename: Optional[str] = None) -> str:
    """Message text; media messages start with a [type] or [type: filename] tag."""
    content = content or ""
    if not media_type:
        return content
    tag = f"[{media_type}: {filename}]" if filename else f"[{media_type}]"
    return f"{tag} {content}" if content else tag


def format_message(message: Message, show_chat_info: bool = True, directory: Optional[contacts.Directory] = None, marker: str = "") -> str:
    """Format a single message as one line: time, chat, message ID, sender, content.

    `show_chat_info` is kept for compatibility; the chat is always shown so every
    line carries what download_media and get_message_context need.
    """
    if message.is_from_me:
        sender_name = "Me"
    else:
        sender_name = message.sender_name or get_sender_name(message.sender, directory)
    chat = f"{message.chat_name} ({message.chat_jid})" if message.chat_name else message.chat_jid
    content = _content_text(message.content, message.media_type, message.filename)
    return (
        f"{marker}[{message.timestamp:%Y-%m-%d %H:%M:%S}] Chat: {chat} | ID: {message.id} | "
        f"From: {sender_name}: {content}\n"
    )


def format_messages_list(
    messages: List[Message],
    show_chat_info: bool = True,
    directory: Optional[contacts.Directory] = None,
    matched_ids: Optional[set] = None,
) -> str:
    """Format messages, one per line, in the order given.

    With `matched_ids`, lines of matching messages start with '>> ' and context
    lines with three spaces, after a one-line legend.
    """
    if not messages:
        return "No messages to display."

    if directory is None:
        directory = load_directory()
    output = ""
    if matched_ids is not None:
        output += "Oldest first. Lines starting with '>>' match the filters; the others are context.\n"
    for message in messages:
        marker = ""
        if matched_ids is not None:
            marker = ">> " if (message.chat_jid, message.id) in matched_ids else "   "
        output += format_message(message, show_chat_info, directory, marker)
    return output


_MESSAGE_COLUMNS = (
    "m.timestamp, m.sender, c.name, m.content, m.is_from_me, m.chat_jid, m.id, m.media_type, m.filename, m.rowid"
)
_MESSAGE_FROM = "FROM messages m JOIN chats c ON m.chat_jid = c.jid"


def _message_from_row(row: tuple, directory: contacts.Directory) -> Message:
    timestamp, sender, chat_name, content, is_from_me, chat_jid, msg_id, media_type, filename, _rowid = row
    return Message(
        timestamp=datetime.fromisoformat(timestamp),
        sender=sender,
        content=content,
        is_from_me=bool(is_from_me),
        chat_jid=chat_jid,
        id=msg_id,
        chat_name=directory.chat_display_name(chat_jid, chat_name),
        media_type=media_type or None,
        filename=filename or None,
        sender_name="Me" if is_from_me else (directory.name_for(sender) or sender),
    )


def _neighbours(conn: sqlite3.Connection, row: tuple, count: int, direction: str) -> List[tuple]:
    """Up to `count` messages of the same chat before/after `row`, oldest first.

    Order is (timestamp, rowid), so messages in the same second are not lost.
    """
    if count <= 0:
        return []
    timestamp, chat_jid, rowid = row[0], row[5], row[9]
    if direction == "before":
        cond, order = "(m.timestamp < ? OR (m.timestamp = ? AND m.rowid < ?))", "DESC"
    else:
        cond, order = "(m.timestamp > ? OR (m.timestamp = ? AND m.rowid > ?))", "ASC"
    rows = conn.execute(
        f"SELECT {_MESSAGE_COLUMNS} {_MESSAGE_FROM} WHERE m.chat_jid = ? AND {cond}"
        f" ORDER BY m.timestamp {order}, m.rowid {order} LIMIT ?",
        (chat_jid, timestamp, timestamp, rowid, count),
    ).fetchall()
    return rows[::-1] if direction == "before" else rows


def _iso_param(name: str, value: str) -> str:
    """Validate an ISO-8601 filter and format it like the stored timestamps compare."""
    try:
        return datetime.fromisoformat(value).isoformat(" ")
    except ValueError:
        raise ValueError(f"Invalid date format for '{name}': {value}. Please use ISO-8601 format.")


def list_messages(
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_context: Optional[bool] = None,
    context_before: int = 1,
    context_after: int = 1,
    media_only: bool = False,
    media_type: Optional[str] = None,
) -> str:
    """Get messages matching the criteria, formatted one per line, oldest first.

    `limit`/`page` select matches newest-first (page 0 is the most recent
    `limit` matches); the page is then printed oldest to newest. Each message
    appears once. include_context defaults to False when chat_jid is set and
    True otherwise; with context, matches are marked '>>'. media_only keeps
    only media messages; media_type (e.g. "image", "document") one media type.
    """
    if include_context is None:
        include_context = chat_jid is None

    where_clauses = []
    params: list = []
    if after:
        where_clauses.append("m.timestamp > ?")
        params.append(_iso_param("after", after))
    if before:
        where_clauses.append("m.timestamp < ?")
        params.append(_iso_param("before", before))

    with _connect() as conn:
        directory = load_directory(conn)

        if sender_phone_number:
            # Match every sender format for this person: bare user and full JID, PN and LID.
            sender_ids = directory.sender_ids(sender_phone_number)
            where_clauses.append(f"m.sender IN ({', '.join('?' * len(sender_ids))})")
            params.extend(sender_ids)
        if chat_jid:
            where_clauses.append("m.chat_jid = ?")
            params.append(chat_jid)
        if query:
            where_clauses.append("LOWER(m.content) LIKE LOWER(?) ESCAPE '\\'")
            params.append(f"%{_escape_like(query)}%")
        if media_type:
            where_clauses.append("LOWER(m.media_type) = LOWER(?)")
            params.append(media_type)
        elif media_only:
            where_clauses.append("COALESCE(m.media_type, '') != ''")

        where = ("WHERE " + " AND ".join(where_clauses)) if where_clauses else ""
        matches = conn.execute(
            f"SELECT {_MESSAGE_COLUMNS} {_MESSAGE_FROM} {where}"
            " ORDER BY m.timestamp DESC, m.rowid DESC LIMIT ? OFFSET ?",
            (*params, limit, page * limit),
        ).fetchall()

        rows = {(r[5], r[6]): r for r in matches}
        if include_context:
            for match in matches:
                for r in _neighbours(conn, match, context_before, "before") + _neighbours(conn, match, context_after, "after"):
                    rows.setdefault((r[5], r[6]), r)

        ordered = sorted(rows.values(), key=lambda r: (r[0], r[9]))
        messages = [_message_from_row(r, directory) for r in ordered]
        matched = {(r[5], r[6]) for r in matches} if include_context else None
        return format_messages_list(messages, directory=directory, matched_ids=matched)


def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5,
    chat_jid: Optional[str] = None,
) -> MessageContext:
    """Get a message and the messages around it in its chat; before/after are oldest first.

    Message IDs are unique per chat; pass chat_jid when the same ID could occur in several chats.
    """
    with _connect() as conn:
        directory = load_directory(conn)
        sql = f"SELECT {_MESSAGE_COLUMNS} {_MESSAGE_FROM} WHERE m.id = ?"
        params: tuple = (message_id,)
        if chat_jid:
            sql += " AND m.chat_jid = ?"
            params += (chat_jid,)
        target = conn.execute(sql + " ORDER BY m.timestamp DESC LIMIT 1", params).fetchone()
        if not target:
            raise ValueError(f"Message with ID {message_id} not found")

        return MessageContext(
            message=_message_from_row(target, directory),
            before=[_message_from_row(r, directory) for r in _neighbours(conn, target, before, "before")],
            after=[_message_from_row(r, directory) for r in _neighbours(conn, target, after, "after")],
        )


def _escape_like(text: str) -> str:
    """Escape LIKE wildcards so user input matches literally (use with ESCAPE '\\')."""
    return text.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")


def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Chat]:
    """Get chats matching the specified criteria.

    The last message is the newest stored message in each chat. Raises
    WhatsAppDBError if the database can't be queried.
    """
    with _connect() as conn:
        directory = load_directory(conn)
        where, params = "", ()
        if query:
            pattern = f"%{_escape_like(query)}%"
            where = "WHERE (LOWER(name) LIKE LOWER(?) ESCAPE '\\' OR jid LIKE ? ESCAPE '\\')"
            params = (pattern, pattern)
        order = "last_message_time DESC" if sort_by == "last_active" else "name"
        return _fetch_chats(
            conn, directory, where, params, order=order, limit=limit, offset=page * limit,
            include_last_message=include_last_message,
        )


def search_contacts(query: str, limit: int = 50) -> List[Contact]:
    """Search people by name (contact names, push names, chat names) or number.

    Uses the device store's contacts and LID map when available, so contacts
    without a chat are found and LID chats report the real phone number.
    Returns at most `limit` people, sorted by name; see search_contacts_counted
    for the total number of matches.
    """
    return search_contacts_counted(query, limit)[0]


def search_contacts_counted(query: str, limit: int = 50) -> Tuple[List[Contact], int]:
    """Like search_contacts, but also returns the total number of matches."""
    found = load_directory().search(query)
    contacts_page = [
        Contact(jid=p.jid, name=p.name, phone=p.phone, lid=p.lid, chat_jid=p.chat_jid)
        for p in found[:max(limit, 0)]
    ]
    return contacts_page, len(found)


def _fetch_chats(
    conn: sqlite3.Connection,
    directory: contacts.Directory,
    where: str = "",
    params: tuple = (),
    order: str = "last_message_time DESC",
    limit: int = -1,
    offset: int = 0,
    include_last_message: bool = True,
) -> List[Chat]:
    """Select chats (filtered, ordered, paginated), then attach each one's last message.

    The last message is the chat's newest stored message, by (timestamp, rowid).
    `order` must use unqualified chats columns (jid, name, last_message_time).
    """
    page = f"SELECT jid, name, last_message_time FROM chats {where} ORDER BY {order} LIMIT ? OFFSET ?"
    if include_last_message:
        sql = f"""
            SELECT p.jid, p.name, p.last_message_time,
                   lm.content, lm.sender, lm.is_from_me, lm.id, lm.timestamp, lm.media_type, lm.filename
            FROM ({page}) p
            LEFT JOIN messages lm ON lm.rowid = (
                SELECT m.rowid FROM messages m
                WHERE m.chat_jid = p.jid
                ORDER BY m.timestamp DESC, m.rowid DESC
                LIMIT 1
            )
            ORDER BY {order}
        """
    else:
        sql = f"""
            SELECT jid, name, last_message_time,
                   NULL, NULL, NULL, NULL, NULL, NULL, NULL
            FROM ({page})
            ORDER BY {order}
        """
    rows = conn.execute(sql, (*params, limit, offset)).fetchall()
    return [_chat_from_row(row, directory) for row in rows]


def _chat_from_row(row: tuple, directory: contacts.Directory) -> Chat:
    jid, name, last_time, content, sender, is_from_me, msg_id, msg_time, media_type, filename = row
    last_sender_name = None
    if msg_id is not None:
        last_sender_name = "Me" if is_from_me else (directory.name_for(sender) or sender)
    return Chat(
        jid=jid,
        name=directory.chat_display_name(jid, name),
        last_message_time=datetime.fromisoformat(last_time) if last_time else None,
        last_message=_content_text(content, media_type, filename) if msg_id is not None else None,
        last_sender=sender,
        last_is_from_me=bool(is_from_me) if is_from_me is not None else None,
        last_message_id=msg_id,
        last_sender_name=last_sender_name,
        last_message_at=datetime.fromisoformat(msg_time) if msg_time else None,
    )


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Chat]:
    """Get all chats involving the contact, each chat once, most recent first.

    Args:
        jid: The contact's JID or phone number (PN or LID form)
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    with _connect() as conn:
        directory = load_directory(conn)
        chat_jids = directory.direct_chat_jids(jid)
        sender_ids = directory.sender_ids(jid)
        where = (
            f"WHERE jid IN ({', '.join('?' * len(chat_jids))}) OR EXISTS ("
            f"SELECT 1 FROM messages m WHERE m.chat_jid = chats.jid AND m.sender IN ({', '.join('?' * len(sender_ids))}))"
        )
        return _fetch_chats(conn, directory, where, (*chat_jids, *sender_ids), limit=limit, offset=page * limit)


def get_last_interaction(jid: str) -> Optional[str]:
    """Get the most recent message involving the contact (in their chat, or sent by them anywhere)."""
    with _connect() as conn:
        directory = load_directory(conn)
        chat_jids = directory.direct_chat_jids(jid)
        sender_ids = directory.sender_ids(jid)
        row = conn.execute(
            f"""
            SELECT {_MESSAGE_COLUMNS} {_MESSAGE_FROM}
            WHERE m.chat_jid IN ({', '.join('?' * len(chat_jids))})
               OR m.sender IN ({', '.join('?' * len(sender_ids))})
            ORDER BY m.timestamp DESC, m.rowid DESC
            LIMIT 1
            """,
            (*chat_jids, *sender_ids),
        ).fetchone()
        if not row:
            return None
        return format_message(_message_from_row(row, directory), directory=directory)


def list_awaiting_reply(since: Optional[str] = None, include_groups: bool = False, limit: int = 20) -> List[Chat]:
    """Chats whose newest stored message is not from me, newest first.

    Direct chats (phone and LID JIDs) only, plus groups with include_groups.
    Broadcasts and newsletters are never included. `since` (ISO-8601) keeps
    chats whose last message is at or after that time. Each chat's
    last_message is a preview (media shown as a [type: filename] tag).
    """
    servers = ["%@s.whatsapp.net", "%@lid"] + (["%@g.us"] if include_groups else [])
    where = ["r.rn = 1", "COALESCE(lm.is_from_me, 0) = 0", "(" + " OR ".join("c.jid LIKE ?" for _ in servers) + ")"]
    params: list = list(servers)
    if since:
        where.append("lm.timestamp >= ?")
        params.append(_iso_param("since", since))

    with _connect() as conn:
        directory = load_directory(conn)
        rows = conn.execute(
            f"""
            WITH ranked AS (
                SELECT rowid AS rid,
                       ROW_NUMBER() OVER (PARTITION BY chat_jid ORDER BY timestamp DESC, rowid DESC) AS rn
                FROM messages
            )
            SELECT c.jid, c.name, c.last_message_time,
                   lm.content, lm.sender, lm.is_from_me, lm.id, lm.timestamp, lm.media_type, lm.filename
            FROM ranked r
            JOIN messages lm ON lm.rowid = r.rid
            JOIN chats c ON c.jid = lm.chat_jid
            WHERE {" AND ".join(where)}
            ORDER BY lm.timestamp DESC, lm.rowid DESC
            LIMIT ?
            """,
            (*params, limit),
        ).fetchall()
        chats = [_chat_from_row(row, directory) for row in rows]
    for chat in chats:
        if chat.last_message and len(chat.last_message) > PREVIEW_LENGTH:
            chat.last_message = chat.last_message[:PREVIEW_LENGTH] + "..."
    return chats


PREVIEW_LENGTH = 200


def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Chat]:
    """Get chat metadata by JID; None if there is no such chat."""
    with _connect() as conn:
        directory = load_directory(conn)
        chats = _fetch_chats(conn, directory, "WHERE jid = ?", (chat_jid,), limit=1, include_last_message=include_last_message)
        return chats[0] if chats else None


def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Chat]:
    """Get the direct chat with a person, by phone number, LID or JID.

    Matches exactly (after stripping '+', spaces and dashes) via the phone<->LID
    map, so a phone number finds an @lid chat and vice versa. If both exist,
    the most recently active chat is returned.
    """
    with _connect() as conn:
        directory = load_directory(conn)
        if directory.identify(sender_phone_number) is None:
            return None
        chat_jids = directory.direct_chat_jids(sender_phone_number)
        where = f"WHERE jid IN ({', '.join('?' * len(chat_jids))})"
        chats = _fetch_chats(conn, directory, where, tuple(chat_jids), limit=1)
        return chats[0] if chats else None


def send_message(recipient: str, message: str) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"
        
        payload = {
            "recipient": recipient,
            "message": message,
        }

        response = _bridge_post("send", payload)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except BridgeTokenError as e:
        return False, str(e)
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def send_file(recipient: str, media_path: str) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"
        
        if not media_path:
            return False, "Media path must be provided"
        
        media_path, error = _resolve_send_path(media_path)
        if error:
            return False, error

        payload = {
            "recipient": recipient,
            "media_path": media_path
        }

        response = _bridge_post("send", payload)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except BridgeTokenError as e:
        return False, str(e)
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def send_audio_message(recipient: str, media_path: str) -> Tuple[bool, str]:
    converted = None
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"

        if not media_path:
            return False, "Media path must be provided"

        media_path, error = _resolve_send_path(media_path)
        if error:
            return False, error

        if not media_path.lower().endswith(".ogg"):
            # Write the converted file into the outbox so the bridge will send it.
            outbox = _outbox_dir()
            os.makedirs(outbox, mode=0o700, exist_ok=True)
            try:
                converted = audio.convert_to_opus_ogg_temp(media_path, output_dir=os.path.realpath(outbox))
            except Exception as e:
                return False, f"Error converting file to opus ogg. You likely need to install ffmpeg: {str(e)}"
            media_path, error = _resolve_send_path(converted)
            if error:
                return False, error

        payload = {
            "recipient": recipient,
            "media_path": media_path
        }

        response = _bridge_post("send", payload)

        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"

    except BridgeTokenError as e:
        return False, str(e)
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"
    finally:
        # The bridge has read the file by the time it replies.
        if converted and os.path.exists(converted):
            os.unlink(converted)

def download_media(message_id: str, chat_jid: str) -> Optional[str]:
    """Download media from a message and return the local file path.
    
    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message
    
    Returns:
        The local file path if download was successful, None otherwise
    """
    try:
        payload = {
            "message_id": message_id,
            "chat_jid": chat_jid
        }

        response = _bridge_post("download", payload)
        
        if response.status_code == 200:
            result = response.json()
            if result.get("success", False):
                path = result.get("path")
                print(f"Media downloaded successfully: {path}", file=sys.stderr)
                return path
            else:
                print(f"Download failed: {result.get('message', 'Unknown error')}", file=sys.stderr)
                return None
        else:
            print(f"Error: HTTP {response.status_code} - {response.text}", file=sys.stderr)
            return None
            
    except BridgeTokenError as e:
        print(str(e), file=sys.stderr)  # stdout is the MCP stdio transport
        return None
    except requests.RequestException as e:
        print(f"Request error: {str(e)}", file=sys.stderr)
        return None
    except json.JSONDecodeError:
        print(f"Error parsing response: {response.text}", file=sys.stderr)
        return None
    except Exception as e:
        print(f"Unexpected error: {str(e)}", file=sys.stderr)
        return None
