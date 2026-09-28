"""UTC timestamps: input parsing, local rendering with offset, same-second order (#14). All data is fake."""

import os
import re
import time
from datetime import datetime, timedelta, timezone

import pytest

import whatsapp

LINE = re.compile(r"^(?:>> |   )?\[([^\]]+)\] .*\| ID: (\S+) \|", re.M)

CHAT = "15550000001@s.whatsapp.net"
UTC = timezone.utc


@pytest.fixture
def london():
    """Run with the process's local time zone set to Europe/London (GMT/BST)."""
    old = os.environ.get("TZ")
    os.environ["TZ"] = "Europe/London"
    time.tzset()
    yield
    if old is None:
        del os.environ["TZ"]
    else:
        os.environ["TZ"] = old
    time.tzset()


@pytest.fixture
def dst_db(messages_db):
    """UK clocks go forward at 2024-03-31 01:00 UTC. Stored in UTC, as the bridge now does."""
    db = messages_db
    db.add_chat(CHAT, "Alice Example", datetime(2024, 3, 31, 2, 30, tzinfo=UTC))
    db.add_message("x1", CHAT, CHAT, "before the change", datetime(2024, 3, 31, 0, 30, tzinfo=UTC))
    db.add_message("x2", CHAT, CHAT, "after the change", datetime(2024, 3, 31, 1, 30, tzinfo=UTC))
    db.add_message("x3", CHAT, CHAT, "later", datetime(2024, 3, 31, 2, 30, tzinfo=UTC))
    return db


def lines(output):
    return LINE.findall(output)


def ids(output):
    return [i for _, i in lines(output)]


def test_output_is_local_time_with_offset(london, dst_db):
    out = whatsapp.list_messages(chat_jid=CHAT)
    assert lines(out) == [
        ("2024-03-31 00:30:00+00:00", "x1"),
        ("2024-03-31 02:30:00+01:00", "x2"),
        ("2024-03-31 03:30:00+01:00", "x3"),
    ]


def test_z_input_is_utc(london, dst_db):
    assert ids(whatsapp.list_messages(chat_jid=CHAT, after="2024-03-31T01:00:00Z")) == ["x2", "x3"]
    assert ids(whatsapp.list_messages(chat_jid=CHAT, before="2024-03-31T01:30:00Z")) == ["x1"]


def test_offset_input_is_converted(london, dst_db):
    # 02:00+01:00 is 01:00 UTC.
    assert ids(whatsapp.list_messages(chat_jid=CHAT, after="2024-03-31T02:00:00+01:00")) == ["x2", "x3"]
    # 20:00-05:00 the day before is 01:00 UTC.
    assert ids(whatsapp.list_messages(chat_jid=CHAT, before="2024-03-30T20:00:00-05:00")) == ["x1"]


def test_naive_input_is_local_time(london, dst_db):
    # 03:00 London (BST) is 02:00 UTC.
    assert ids(whatsapp.list_messages(chat_jid=CHAT, after="2024-03-31 03:00")) == ["x3"]
    # 00:45 London (GMT) is 00:45 UTC.
    assert ids(whatsapp.list_messages(chat_jid=CHAT, before="2024-03-31T00:45:00")) == ["x1"]


def test_awaiting_reply_since_is_converted(london, dst_db):
    assert [c.jid for c in whatsapp.list_awaiting_reply(since="2024-03-31T03:30:00+01:00")] == [CHAT]
    assert whatsapp.list_awaiting_reply(since="2024-03-31T02:31:00Z") == []


def test_chat_times_are_local_aware(london, dst_db):
    chat = whatsapp.get_chat(CHAT)
    assert chat.last_message_time == datetime(2024, 3, 31, 2, 30, tzinfo=UTC)
    assert chat.last_message_time.utcoffset() == timedelta(hours=1)
    assert chat.last_message_at.utcoffset() == timedelta(hours=1)


def test_context_messages_are_local_aware(london, dst_db):
    ctx = whatsapp.get_message_context("x2", before=1, after=1)
    assert ctx.message.timestamp.isoformat(" ") == "2024-03-31 02:30:00+01:00"
    assert [m.id for m in ctx.before] == ["x1"] and ctx.before[0].timestamp.utcoffset() == timedelta(0)


def test_rows_stored_before_migration_render_the_same(london, messages_db):
    # Old bridge: local-offset text. Same instant, same output.
    with messages_db_conn(messages_db) as conn:
        conn.execute("INSERT INTO chats VALUES (?, ?, ?)", (CHAT, "Alice Example", "2024-03-31 03:30:00+01:00"))
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, 0)",
            ("old", CHAT, CHAT, "old row", "2024-03-31 03:30:00+01:00"),
        )
    assert lines(whatsapp.list_messages(chat_jid=CHAT)) == [("2024-03-31 03:30:00+01:00", "old")]


def test_same_second_context(messages_db):
    t = datetime(2024, 1, 2, 10, 0, 0, tzinfo=UTC)
    messages_db.add_chat(CHAT, "Alice Example", t)
    for i in range(1, 5):
        messages_db.add_message(f"s{i}", CHAT, CHAT, f"msg {i}", t)
    ctx = whatsapp.get_message_context("s2", before=5, after=5)
    assert [m.id for m in ctx.before] == ["s1"]
    assert [m.id for m in ctx.after] == ["s3", "s4"]


def messages_db_conn(db):
    import sqlite3

    return sqlite3.connect(db.path)
