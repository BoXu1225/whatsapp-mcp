"""Replies, edits, deletes and reactions in tool output (#15, #16). All data is fake."""

import sqlite3
from datetime import datetime, timezone

import pytest
from conftest import ALICE, BOB, CAPTURE_MIGRATION, FakeMessagesDB

import whatsapp


def at(minute):
    return datetime(2024, 1, 2, 11, minute, 0, tzinfo=timezone.utc)


ALICE_USER = ALICE.split("@")[0]
ME = "15550000000@s.whatsapp.net"


@pytest.fixture
def capture_db(messages_db):
    db = messages_db
    db.add_chat(ALICE, "Alice Example", at(5))
    db.add_message("q1", ALICE, ALICE, "dinner at 8?", at(0))
    db.add_message("q2", ALICE, ME, "yes!", at(1), is_from_me=True, reply_to="q1")
    db.add_message("q3", ALICE, ALICE, "fixed typo", at(2), edited_at=at(3))
    db.add_message("q4", ALICE, ALICE, "oops wrong chat", at(4), deleted_at=at(5))
    db.add_message("q5", ALICE, ALICE, "", at(5), deleted_at=at(6))  # deleted with -purge-deleted
    db.add_reaction("q1", ALICE, ME, "👍", at(1))
    db.add_reaction("q1", ALICE, ALICE, "👍", at(2))
    db.add_reaction("q1", ALICE, "15550000009@s.whatsapp.net", "❤️", at(2))
    return db


def line_for(out, msg_id):
    for line in out.splitlines():
        if f"ID: {msg_id} " in line:
            return line
    raise AssertionError(f"no line for {msg_id} in:\n{out}")


def test_reply_shows_quoted_message_id(capture_db):
    out = whatsapp.list_messages(chat_jid=ALICE)
    assert "↪ reply to q1" in line_for(out, "q2")
    assert "yes!" in line_for(out, "q2")
    assert "reply to" not in line_for(out, "q1")


def test_edited_message_is_marked(capture_db):
    out = whatsapp.list_messages(chat_jid=ALICE)
    assert line_for(out, "q3").rstrip().endswith("fixed typo (edited)")
    assert "(edited)" not in line_for(out, "q1")


def test_deleted_message_is_marked_and_keeps_text(capture_db):
    out = whatsapp.list_messages(chat_jid=ALICE)
    assert "[deleted] oops wrong chat" in line_for(out, "q4")
    assert line_for(out, "q5").rstrip().endswith("[deleted]")
    assert "[deleted]" not in line_for(out, "q1")


def test_reactions_are_summarised(capture_db):
    line = line_for(whatsapp.list_messages(chat_jid=ALICE), "q1")
    assert "👍×2" in line
    assert "❤️×1" in line
    assert line.index("👍×2") < line.index("❤️×1")  # most frequent first


def test_message_context_carries_capture_fields(capture_db):
    ctx = whatsapp.get_message_context("q2", before=1, after=2)
    assert ctx.message.reply_to == "q1"
    assert ctx.before[0].reactions == {"👍": 2, "❤️": 1}
    edited, deleted = ctx.after
    assert edited.edited is True and edited.deleted is False
    assert deleted.deleted is True


def test_formatted_context_line(capture_db):
    ctx = whatsapp.get_message_context("q1", before=0, after=0)
    line = whatsapp.format_message(ctx.message)
    assert "dinner at 8?" in line and "👍×2" in line


def test_awaiting_reply_ignores_deleted_last_message(messages_db):
    """Bob's last message was deleted; before it, I answered: not awaiting."""
    db = messages_db
    db.add_chat(BOB, "Bob Example", at(3))
    db.add_message("b1", BOB, BOB, "question", at(0))
    db.add_message("b2", BOB, ME, "answer", at(1), is_from_me=True)
    db.add_message("b3", BOB, BOB, "never mind", at(2), deleted_at=at(3))
    assert [c.jid for c in whatsapp.list_awaiting_reply()] == []


def test_database_before_migration_5_still_works(tmp_path, monkeypatch):
    db = FakeMessagesDB(tmp_path / "messages.db", pre_capture=True)
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", db.path)
    db.add_chat(ALICE, "Alice Example", at(0))
    db.add_message("o1", ALICE, ALICE, "old bridge", at(0))
    out = whatsapp.list_messages(chat_jid=ALICE)
    assert "old bridge" in line_for(out, "o1")
    ctx = whatsapp.get_message_context("o1")
    assert ctx.message.reply_to is None and ctx.message.reactions == {}
    assert [c.jid for c in whatsapp.list_awaiting_reply()] == [ALICE]


def test_capture_schema_check_is_cached(capture_db, monkeypatch):
    calls = []
    probe = whatsapp._probe_capture_schema

    def counting(conn):
        calls.append(1)
        return probe(conn)

    monkeypatch.setattr(whatsapp, "_probe_capture_schema", counting)
    whatsapp._capture_schema_cache.clear()
    whatsapp.list_messages(chat_jid=ALICE)
    whatsapp.list_messages()  # with context: several neighbour queries
    whatsapp.get_message_context("q2")
    assert len(calls) == 1


def test_capture_schema_found_after_bridge_migrates(tmp_path, monkeypatch):
    """A missing schema isn't cached: once the bridge migrates, markers appear."""
    db = FakeMessagesDB(tmp_path / "messages.db", pre_capture=True)
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", db.path)
    db.add_chat(ALICE, "Alice Example", at(0))
    db.add_message("o1", ALICE, ALICE, "before", at(0))
    assert "[deleted]" not in whatsapp.list_messages(chat_jid=ALICE)
    with sqlite3.connect(db.path) as conn:
        conn.executescript(CAPTURE_MIGRATION)
        conn.execute("UPDATE messages SET is_deleted = 1 WHERE id = 'o1'")
    assert "[deleted] before" in whatsapp.list_messages(chat_jid=ALICE)
