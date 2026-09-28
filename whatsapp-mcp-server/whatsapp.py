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


def format_message(message: Message, show_chat_info: bool = True, directory: Optional[contacts.Directory] = None) -> str:
    """Format a single message as one line."""
    output = ""
    
    if show_chat_info and message.chat_name:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] Chat: {message.chat_name} "
    else:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] "
        
    content_prefix = ""
    if hasattr(message, 'media_type') and message.media_type:
        content_prefix = f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "
    
    sender_name = get_sender_name(message.sender, directory) if not message.is_from_me else "Me"
    output += f"From: {sender_name}: {content_prefix}{message.content}\n"
    return output

def format_messages_list(messages: List[Message], show_chat_info: bool = True, directory: Optional[contacts.Directory] = None) -> str:
    output = ""
    if not messages:
        output += "No messages to display."
        return output

    if directory is None:
        directory = load_directory()
    for message in messages:
        output += format_message(message, show_chat_info, directory)
    return output

def list_messages(
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1
) -> List[Message]:
    """Get messages matching the specified criteria with optional context."""
    try:
        conn = _open_messages_db()
        cursor = conn.cursor()
        
        # Build base query
        query_parts = ["SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type FROM messages"]
        query_parts.append("JOIN chats ON messages.chat_jid = chats.jid")
        where_clauses = []
        params = []
        
        # Add filters
        if after:
            try:
                after = datetime.fromisoformat(after)
            except ValueError:
                raise ValueError(f"Invalid date format for 'after': {after}. Please use ISO-8601 format.")
            
            where_clauses.append("messages.timestamp > ?")
            params.append(after)

        if before:
            try:
                before = datetime.fromisoformat(before)
            except ValueError:
                raise ValueError(f"Invalid date format for 'before': {before}. Please use ISO-8601 format.")
            
            where_clauses.append("messages.timestamp < ?")
            params.append(before)

        if sender_phone_number:
            # Match every sender format for this person: bare user and full JID, PN and LID.
            sender_ids = load_directory(conn).sender_ids(sender_phone_number)
            where_clauses.append(f"messages.sender IN ({', '.join('?' * len(sender_ids))})")
            params.extend(sender_ids)
            
        if chat_jid:
            where_clauses.append("messages.chat_jid = ?")
            params.append(chat_jid)
            
        if query:
            where_clauses.append("LOWER(messages.content) LIKE LOWER(?)")
            params.append(f"%{query}%")
            
        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))
            
        # Add pagination
        offset = page * limit
        query_parts.append("ORDER BY messages.timestamp DESC")
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit, offset])
        
        cursor.execute(" ".join(query_parts), tuple(params))
        messages = cursor.fetchall()
        
        result = []
        for msg in messages:
            message = Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            )
            result.append(message)
            
        if include_context and result:
            # Add context for each message
            messages_with_context = []
            for msg in result:
                context = get_message_context(msg.id, context_before, context_after)
                messages_with_context.extend(context.before)
                messages_with_context.append(context.message)
                messages_with_context.extend(context.after)
            
            return format_messages_list(messages_with_context, show_chat_info=True)
            
        # Format and display messages without context
        return format_messages_list(result, show_chat_info=True)    
        
    except sqlite3.Error as e:
        raise WhatsAppDBError(f"WhatsApp database query failed: {e}") from e
    finally:
        if 'conn' in locals():
            conn.close()


def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5
) -> MessageContext:
    """Get context around a specific message."""
    try:
        conn = _open_messages_db()
        cursor = conn.cursor()
        
        # Get the target message first
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.chat_jid, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.id = ?
        """, (message_id,))
        msg_data = cursor.fetchone()
        
        if not msg_data:
            raise ValueError(f"Message with ID {message_id} not found")
            
        target_message = Message(
            timestamp=datetime.fromisoformat(msg_data[0]),
            sender=msg_data[1],
            chat_name=msg_data[2],
            content=msg_data[3],
            is_from_me=msg_data[4],
            chat_jid=msg_data[5],
            id=msg_data[6],
            media_type=msg_data[8]
        )
        
        # Get messages before
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp < ?
            ORDER BY messages.timestamp DESC
            LIMIT ?
        """, (msg_data[7], msg_data[0], before))
        
        before_messages = []
        for msg in cursor.fetchall():
            before_messages.append(Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            ))
        
        # Get messages after
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp > ?
            ORDER BY messages.timestamp ASC
            LIMIT ?
        """, (msg_data[7], msg_data[0], after))
        
        after_messages = []
        for msg in cursor.fetchall():
            after_messages.append(Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            ))
        
        return MessageContext(
            message=target_message,
            before=before_messages,
            after=after_messages
        )
        
    except sqlite3.Error as e:
        raise WhatsAppDBError(f"WhatsApp database query failed: {e}") from e
    finally:
        if 'conn' in locals():
            conn.close()


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
            where = "WHERE (LOWER(name) LIKE LOWER(?) OR jid LIKE ?)"
            params = (f"%{query}%", f"%{query}%")
        order = "last_message_time DESC" if sort_by == "last_active" else "name"
        return _fetch_chats(
            conn, directory, where, params, order=order, limit=limit, offset=page * limit,
            include_last_message=include_last_message,
        )


def search_contacts(query: str) -> List[Contact]:
    """Search people by name (contact names, push names, chat names) or number.

    Uses the device store's contacts and LID map when available, so contacts
    without a chat are found and LID chats report the real phone number.
    """
    directory = load_directory()
    return [
        Contact(jid=p.jid, name=p.name, phone=p.phone, lid=p.lid, chat_jid=p.chat_jid)
        for p in directory.search(query)
    ]


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
        last_message=content,
        last_sender=sender,
        last_is_from_me=bool(is_from_me) if is_from_me is not None else None,
        last_message_id=msg_id,
        last_sender_name=last_sender_name,
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
        msg_data = conn.execute(
            f"""
            SELECT m.timestamp, m.sender, c.name, m.content, m.is_from_me, c.jid, m.id, m.media_type
            FROM messages m
            JOIN chats c ON m.chat_jid = c.jid
            WHERE m.chat_jid IN ({', '.join('?' * len(chat_jids))})
               OR m.sender IN ({', '.join('?' * len(sender_ids))})
            ORDER BY m.timestamp DESC, m.rowid DESC
            LIMIT 1
            """,
            (*chat_jids, *sender_ids),
        ).fetchone()

        if not msg_data:
            return None

        message = Message(
            timestamp=datetime.fromisoformat(msg_data[0]),
            sender=msg_data[1],
            chat_name=directory.chat_display_name(msg_data[5], msg_data[2]),
            content=msg_data[3],
            is_from_me=msg_data[4],
            chat_jid=msg_data[5],
            id=msg_data[6],
            media_type=msg_data[7]
        )
        return format_message(message, directory=directory)


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
                print(f"Media downloaded successfully: {path}")
                return path
            else:
                print(f"Download failed: {result.get('message', 'Unknown error')}")
                return None
        else:
            print(f"Error: HTTP {response.status_code} - {response.text}")
            return None
            
    except BridgeTokenError as e:
        print(str(e), file=sys.stderr)  # stdout is the MCP stdio transport
        return None
    except requests.RequestException as e:
        print(f"Request error: {str(e)}")
        return None
    except json.JSONDecodeError:
        print(f"Error parsing response: {response.text}")
        return None
    except Exception as e:
        print(f"Unexpected error: {str(e)}")
        return None
