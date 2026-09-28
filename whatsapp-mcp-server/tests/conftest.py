"""Shared fixtures. Every database here is a throwaway file under tmp_path.

Fixtures:
    messages_db   -- empty bridge messages.db (schema from whatsapp-bridge/store.go);
                     whatsapp.MESSAGES_DB_PATH points at it for the test.
    seeded_db     -- messages_db with a few fake chats and messages (see SEED below).
    whatsmeow_db  -- fake whatsmeow device store with whatsmeow_contacts and
                     whatsmeow_lid_map only. Nothing reads it yet; it is here for
                     tests of code that resolves names from the device store.

All data is fake: +1 555 numbers, made-up names.
"""

import sqlite3
from datetime import datetime, timezone

import pytest
import requests

import whatsapp

# Copied verbatim from NewMessageStoreAt in whatsapp-bridge/store.go.
BRIDGE_SCHEMA = """
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
    """Format a datetime the way go-sqlite3 stores a Go time.Time."""
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.isoformat(sep=" ")


class FakeMessagesDB:
    def __init__(self, path):
        self.path = str(path)
        with sqlite3.connect(self.path) as conn:
            conn.executescript(BRIDGE_SCHEMA)

    def add_chat(self, jid, name, last_message_time):
        with sqlite3.connect(self.path) as conn:
            conn.execute(
                "INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
                (jid, name, bridge_timestamp(last_message_time)),
            )

    def add_message(self, id, chat_jid, sender, content, timestamp, is_from_me=False, media_type="", filename=""):
        with sqlite3.connect(self.path) as conn:
            conn.execute(
                "INSERT OR REPLACE INTO messages"
                " (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename)"
                " VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                (id, chat_jid, sender, content, bridge_timestamp(timestamp), is_from_me, media_type, filename),
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
def no_bridge_http(monkeypatch):
    """Fail any test that tries to talk to the bridge REST API."""

    def blocked(*args, **kwargs):
        raise AssertionError("tests must not make HTTP requests to the bridge")

    for name in ("get", "post", "put", "delete", "request"):
        monkeypatch.setattr(requests, name, blocked)


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
