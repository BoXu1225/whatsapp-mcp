"""Shared fixtures. Every database here is a throwaway file under tmp_path.

Fixtures:
    messages_db   -- empty bridge messages.db (schema from whatsapp-bridge/store.go);
                     whatsapp.MESSAGES_DB_PATH points at it for the test.
    seeded_db     -- messages_db with a few fake chats and messages (see SEED below).
    whatsmeow_db  -- fake whatsmeow device store (whatsapp.db next to messages.db)
                     with whatsmeow_contacts and whatsmeow_lid_map only. The
                     contact-resolution layer finds it via MESSAGES_DB_PATH.
    contacts_db   -- seeded_db plus contacts/LID fixtures (see contacts_db below).

All data is fake: +1 555 numbers, made-up names.
"""

import sqlite3
from datetime import datetime, timezone

import pytest
import requests

import whatsapp

# The schema NewMessageStoreAt in whatsapp-bridge/store.go created before
# schema versioning (migration 0). LEGACY_BRIDGE_SCHEMA keeps it; BRIDGE_SCHEMA
# adds what the migrations in whatsapp-bridge/migrate.go add.
LEGACY_BRIDGE_SCHEMA = """
    CREATE TABLE IF NOT EXISTS chats (
        jid TEXT PRIMARY KEY,
        name TEXT,
        last_message_time TIMESTAMP
    );

    CREATE TABLE IF NOT EXISTS messages (
        id TEXT,
        chat_jid TEXT,
        sender TEXT,
        content TEXT,
        timestamp TIMESTAMP,
        is_from_me BOOLEAN,
        media_type TEXT,
        filename TEXT,
        url TEXT,
        media_key BLOB,
        file_sha256 BLOB,
        file_enc_sha256 BLOB,
        file_length INTEGER,
        PRIMARY KEY (id, chat_jid),
        FOREIGN KEY (chat_jid) REFERENCES chats(jid)
    );
"""

PRE_CAPTURE_BRIDGE_SCHEMA = LEGACY_BRIDGE_SCHEMA + """
    ALTER TABLE messages ADD COLUMN sender_alt TEXT;
"""

# Migration 5 (message capture, #15/#16): reply context, edits, deletes, reactions.
CAPTURE_MIGRATION = """
    ALTER TABLE messages ADD COLUMN reply_to TEXT;
    ALTER TABLE messages ADD COLUMN edited_at TIMESTAMP;
    ALTER TABLE messages ADD COLUMN is_deleted INTEGER NOT NULL DEFAULT 0;
    ALTER TABLE messages ADD COLUMN deleted_at TIMESTAMP;
    ALTER TABLE messages ADD COLUMN deleted_by TEXT;
    CREATE TABLE reactions (
        message_id TEXT NOT NULL,
        chat_jid TEXT NOT NULL,
        sender TEXT NOT NULL,
        emoji TEXT NOT NULL,
        timestamp TIMESTAMP,
        PRIMARY KEY (message_id, chat_jid, sender)
    );
"""
BRIDGE_SCHEMA = PRE_CAPTURE_BRIDGE_SCHEMA + CAPTURE_MIGRATION

# From go.mau.fi/whatsmeow store/sqlstore/upgrades/00-latest-schema.sql. The
# contacts foreign key to whatsmeow_device is left out; that table is not needed.
WHATSMEOW_SCHEMA = """
    CREATE TABLE whatsmeow_contacts (
        our_jid        TEXT,
        their_jid      TEXT,
        first_name     TEXT,
        full_name      TEXT,
        push_name      TEXT,
        business_name  TEXT,
        redacted_phone TEXT,

        PRIMARY KEY (our_jid, their_jid)
    );

    CREATE TABLE whatsmeow_lid_map (
        lid TEXT PRIMARY KEY,
        pn  TEXT UNIQUE NOT NULL
    );
"""

OWN_JID = "15550000000@s.whatsapp.net"


def bridge_timestamp(dt: datetime) -> str:
    """Format a datetime the way go-sqlite3 stores a Go time.Time.

    go-sqlite3 uses the layout "2006-01-02 15:04:05.999999999-07:00": fractional
    seconds with trailing zeros trimmed, omitted entirely when zero.
    """
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    frac = f".{dt.microsecond:06d}".rstrip("0") if dt.microsecond else ""
    return dt.strftime("%Y-%m-%d %H:%M:%S") + frac + dt.isoformat()[-6:]


class FakeMessagesDB:
    def __init__(self, path, legacy=False, pre_capture=False):
        """legacy=True: the schema before migrations (no sender_alt column).
        pre_capture=True: the schema before migration 5 (no reply_to, edited_at,
        is_deleted, deleted_at, reactions)."""
        self.path = str(path)
        self.legacy = legacy
        schema = LEGACY_BRIDGE_SCHEMA if legacy else PRE_CAPTURE_BRIDGE_SCHEMA if pre_capture else BRIDGE_SCHEMA
        with sqlite3.connect(self.path) as conn:
            conn.executescript(schema)

    def add_chat(self, jid, name, last_message_time):
        with sqlite3.connect(self.path) as conn:
            conn.execute(
                "INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
                (jid, name, bridge_timestamp(last_message_time)),
            )

    def add_message(
        self, id, chat_jid, sender, content, timestamp, is_from_me=False, media_type="", filename="", sender_alt=None,
        reply_to=None, edited_at=None, deleted_at=None,
    ):
        columns = ["id", "chat_jid", "sender", "content", "timestamp", "is_from_me", "media_type", "filename"]
        values = [id, chat_jid, sender, content, bridge_timestamp(timestamp), is_from_me, media_type, filename]
        if sender_alt is not None:
            columns.append("sender_alt")
            values.append(sender_alt)
        if reply_to is not None:
            columns.append("reply_to")
            values.append(reply_to)
        if edited_at is not None:
            columns.append("edited_at")
            values.append(bridge_timestamp(edited_at))
        if deleted_at is not None:
            columns += ["is_deleted", "deleted_at"]
            values += [1, bridge_timestamp(deleted_at)]
        with sqlite3.connect(self.path) as conn:
            conn.execute(
                f"INSERT OR REPLACE INTO messages ({', '.join(columns)}) VALUES ({', '.join('?' * len(values))})",
                values,
            )


    def add_reaction(self, message_id, chat_jid, sender, emoji, timestamp):
        with sqlite3.connect(self.path) as conn:
            conn.execute(
                "INSERT OR REPLACE INTO reactions (message_id, chat_jid, sender, emoji, timestamp) VALUES (?, ?, ?, ?, ?)",
                (message_id, chat_jid, sender, emoji, bridge_timestamp(timestamp)),
            )


class FakeWhatsmeowDB:
    def __init__(self, path):
        self.path = str(path)
        with sqlite3.connect(self.path) as conn:
            conn.executescript(WHATSMEOW_SCHEMA)

    def add_contact(self, their_jid, first_name="", full_name="", push_name="", business_name="", our_jid=OWN_JID):
        with sqlite3.connect(self.path) as conn:
            conn.execute(
                "INSERT OR REPLACE INTO whatsmeow_contacts"
                " (our_jid, their_jid, first_name, full_name, push_name, business_name)"
                " VALUES (?, ?, ?, ?, ?, ?)",
                (our_jid, their_jid, first_name, full_name, push_name, business_name),
            )

    def add_lid_mapping(self, lid_user, pn_user):
        """Map a LID to a phone number. whatsmeow stores bare user parts, no server."""
        with sqlite3.connect(self.path) as conn:
            conn.execute("INSERT OR REPLACE INTO whatsmeow_lid_map (lid, pn) VALUES (?, ?)", (lid_user, pn_user))


@pytest.fixture(autouse=True)
def isolate_from_real_bridge(monkeypatch, tmp_path):
    """Keep every test away from the real bridge.

    MESSAGES_DB_PATH points at a path that doesn't exist (messages_db overrides
    it), so a test that forgets the fixture can never read the real store, and
    HTTP calls fail instead of reaching the bridge REST API.
    """
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(tmp_path / "missing" / "messages.db"))
    monkeypatch.delenv("WHATSMEOW_DB_PATH", raising=False)

    def blocked(*args, **kwargs):
        raise AssertionError("tests must not make HTTP requests to the bridge")

    for name in ("get", "post", "put", "delete", "request"):
        monkeypatch.setattr(requests, name, blocked)
    monkeypatch.setattr(requests.sessions.Session, "request", blocked)


@pytest.fixture
def messages_db(tmp_path, monkeypatch):
    db = FakeMessagesDB(tmp_path / "messages.db")
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", db.path)
    return db


ALICE = "15550000001@s.whatsapp.net"
BOB = "15550000002@s.whatsapp.net"
GROUP = "120363000000000001@g.us"


def _t(minute):
    return datetime(2024, 1, 2, 10, minute, 0, tzinfo=timezone.utc)


@pytest.fixture
def seeded_db(messages_db):
    """Two direct chats and one group.

    ALICE: a1 (10:00, from Alice), a2 (10:01, from me), a3 (10:02, from Alice)
    BOB:   b1 (10:05, from Bob)                       -- most recent chat
    GROUP: g1 (10:03, from Alice)
    """
    db = messages_db
    db.add_chat(ALICE, "Alice Example", _t(2))
    db.add_chat(BOB, "Bob Example", _t(5))
    db.add_chat(GROUP, "Test Group", _t(3))

    db.add_message("a1", ALICE, "15550000001", "hi there", _t(0))
    db.add_message("a2", ALICE, "15550000000", "hello back", _t(1), is_from_me=True)
    db.add_message("a3", ALICE, "15550000001", "see you soon", _t(2))
    db.add_message("b1", BOB, "15550000002", "photo", _t(5), media_type="image", filename="image_1.jpg")
    db.add_message("g1", GROUP, "15550000001", "group hello", _t(3))
    return db


@pytest.fixture
def whatsmeow_db(tmp_path):
    return FakeWhatsmeowDB(tmp_path / "whatsapp.db")


# Contact-resolution fixtures (#12). All fake.
CAROL_PN = "15550000003"
CAROL_LID = "100000000000003"
CAROL_LID_JID = CAROL_LID + "@lid"
DAVE_PN = "15550000004"
ERIN_LID = "100000000000005"
ERIN_LID_JID = ERIN_LID + "@lid"
# Its digits contain Alice's number, so a substring match on "15550000001" hits it.
ALICE_LOOKALIKE = "115550000001@s.whatsapp.net"


@pytest.fixture
def contacts_db(seeded_db, whatsmeow_db):
    """seeded_db plus:

    Carol  -- phone 15550000003, LID 100000000000003. Her direct chat is the
              @lid JID with a placeholder name; her name is only in the device
              store (under her phone JID). She writes from her LID in both
              sender formats: bare user in her chat, full JID in the group.
    Dave   -- device-store contact only (phone 15550000004), no chat.
    Erin   -- @lid chat named "Erin Example", no LID mapping.
    Lookalike -- chat 115550000001@s.whatsapp.net, inserted before Alice's.
    """
    db = seeded_db
    with sqlite3.connect(db.path) as conn:
        # Recreate Alice's chat after the lookalike so a table scan meets the lookalike first.
        conn.execute("DELETE FROM chats WHERE jid = ?", (ALICE,))
    db.add_chat(ALICE_LOOKALIKE, "Alicia Lookalike", _t(1))
    db.add_chat(ALICE, "Alice Example", _t(2))

    db.add_chat(CAROL_LID_JID, CAROL_LID, _t(7))
    db.add_message("c1", CAROL_LID_JID, CAROL_LID, "carol here", _t(6))
    db.add_message("c2", CAROL_LID_JID, OWN_JID.split("@")[0], "hi carol", _t(7), is_from_me=True)
    db.add_message("c3", GROUP, CAROL_LID_JID, "carol in group", _t(8))
    db.add_chat(GROUP, "Test Group", _t(8))

    db.add_chat(ERIN_LID_JID, "Erin Example", _t(4))
    db.add_message("e1", ERIN_LID_JID, ERIN_LID, "erin here", _t(4))

    whatsmeow_db.add_contact(ALICE, full_name="Alice Example", push_name="ally")
    whatsmeow_db.add_contact(CAROL_PN + "@s.whatsapp.net", full_name="Carol Example", push_name="caz")
    whatsmeow_db.add_lid_mapping(CAROL_LID, CAROL_PN)
    whatsmeow_db.add_contact(DAVE_PN + "@s.whatsapp.net", first_name="Dave", push_name="davey")
    whatsmeow_db.add_contact("15550000006@s.whatsapp.net", business_name="Fake Bakery", push_name="bakery")
    return db
