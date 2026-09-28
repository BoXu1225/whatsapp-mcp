"""Chat listings: last message, include_last_message=False, surfaced errors (#11)."""

import sqlite3
from datetime import datetime, timezone

import pytest
from conftest import ALICE, BOB, GROUP

import whatsapp

T = datetime(2024, 1, 2, 12, 0, 0, tzinfo=timezone.utc)


def test_list_chats_without_last_message(seeded_db):
    chats = whatsapp.list_chats(include_last_message=False)
    assert [c.jid for c in chats] == [BOB, GROUP, ALICE]
    assert all(c.last_message is None for c in chats)


def test_get_chat_without_last_message(seeded_db):
    chat = whatsapp.get_chat(ALICE, include_last_message=False)
    assert chat is not None
    assert chat.name == "Alice Example"
    assert chat.last_message is None


def test_last_message_when_last_message_time_has_no_matching_message(seeded_db):
    # The bridge bumps last_message_time for events it doesn't store (reactions).
    seeded_db.add_chat(ALICE, "Alice Example", datetime(2024, 1, 2, 10, 30, tzinfo=timezone.utc))
    chat = whatsapp.get_chat(ALICE)
    assert chat.last_message == "see you soon"
    assert chat.last_message_id == "a3"
    [listed] = whatsapp.list_chats(query="alice")
    assert listed.last_message == "see you soon"


def test_same_second_messages_give_one_row_and_latest_insert(messages_db):
    messages_db.add_chat(ALICE, "Alice Example", T)
    messages_db.add_message("x1", ALICE, "15550000001", "first", T)
    messages_db.add_message("x2", ALICE, "15550000001", "second", T)
    chats = whatsapp.list_chats()
    assert len(chats) == 1
    assert chats[0].last_message == "second"
    assert whatsapp.get_chat(ALICE).last_message == "second"


def test_last_message_sender_name(seeded_db):
    alice = whatsapp.get_chat(ALICE)
    assert alice.last_sender_name == "Alice Example"
    assert alice.last_is_from_me is False
    seeded_db.add_message("a4", ALICE, "15550000000", "bye", datetime(2024, 1, 2, 10, 9, tzinfo=timezone.utc), is_from_me=True)
    alice = whatsapp.get_chat(ALICE)
    assert (alice.last_message, alice.last_sender_name, alice.last_is_from_me) == ("bye", "Me", True)


def test_list_chats_sort_by_name(seeded_db):
    assert [c.jid for c in whatsapp.list_chats(sort_by="name")] == [ALICE, BOB, GROUP]


def test_missing_database_raises_clear_error():
    with pytest.raises(whatsapp.WhatsAppDBError, match="not found"):
        whatsapp.list_chats()
    with pytest.raises(whatsapp.WhatsAppDBError):
        whatsapp.get_chat(ALICE)


def test_sql_error_is_raised_not_swallowed(seeded_db):
    with sqlite3.connect(seeded_db.path) as conn:
        conn.execute("DROP TABLE messages")
    with pytest.raises(whatsapp.WhatsAppDBError, match="messages"):
        whatsapp.list_chats()
    with pytest.raises(whatsapp.WhatsAppDBError):
        whatsapp.get_chat(ALICE)


def test_get_chat_unknown_is_none(seeded_db):
    assert whatsapp.get_chat("15550009999@s.whatsapp.net") is None


def test_tool_surfaces_error():
    import main

    with pytest.raises(whatsapp.WhatsAppDBError):
        main.list_chats()
