"""Smoke tests for the read-only query functions in whatsapp.py."""

import sqlite3

from conftest import ALICE, BOB, GROUP

import whatsapp


def test_fixture_points_module_at_temp_db(messages_db, tmp_path):
    assert whatsapp.MESSAGES_DB_PATH == str(tmp_path / "messages.db")


def test_list_chats_empty(messages_db):
    assert whatsapp.list_chats() == []


def test_list_chats_orders_by_last_active(seeded_db):
    chats = whatsapp.list_chats()
    assert [c.jid for c in chats] == [BOB, GROUP, ALICE]
    alice = chats[2]
    assert alice.name == "Alice Example"
    assert alice.last_message == "see you soon"
    assert not alice.is_group
    assert chats[1].is_group


def test_list_chats_query_and_paging(seeded_db):
    assert [c.jid for c in whatsapp.list_chats(query="alice")] == [ALICE]
    assert [c.jid for c in whatsapp.list_chats(limit=1, page=1)] == [GROUP]


def test_search_contacts_skips_groups(seeded_db):
    contacts = whatsapp.search_contacts("Example")
    assert [(c.jid, c.phone_number) for c in contacts] == [
        (ALICE, "15550000001"),
        (BOB, "15550000002"),
    ]
    assert whatsapp.search_contacts("Test Group") == []


def test_get_chat(seeded_db):
    chat = whatsapp.get_chat(BOB)
    assert chat.name == "Bob Example"
    assert chat.last_message == "photo"
    assert whatsapp.get_chat("15550009999@s.whatsapp.net") is None


def test_get_direct_chat_by_contact(seeded_db):
    assert whatsapp.get_direct_chat_by_contact("15550000001").jid == ALICE


def test_get_message_context(seeded_db):
    ctx = whatsapp.get_message_context("a2", before=5, after=5)
    assert ctx.message.content == "hello back"
    assert [m.id for m in ctx.before] == ["a1"]
    assert [m.id for m in ctx.after] == ["a3"]


def test_list_messages_formats_output(seeded_db):
    out = whatsapp.list_messages(chat_jid=ALICE, include_context=False)
    assert "see you soon" in out
    assert "From: Me: hello back" in out
    assert "hi there" in out


def test_list_messages_shows_media_marker(seeded_db):
    out = whatsapp.list_messages(chat_jid=BOB, include_context=False)
    assert "[image - Message ID: b1" in out


def test_get_sender_name(seeded_db):
    assert whatsapp.get_sender_name(ALICE) == "Alice Example"
    assert whatsapp.get_sender_name("15550009999") == "15550009999"


def test_whatsmeow_fixture(whatsmeow_db):
    whatsmeow_db.add_contact(BOB, full_name="Bob Example")
    whatsmeow_db.add_lid_mapping("100000000000002", "15550000002")
    with sqlite3.connect(whatsmeow_db.path) as conn:
        assert conn.execute("SELECT full_name FROM whatsmeow_contacts WHERE their_jid = ?", (BOB,)).fetchone() == (
            "Bob Example",
        )
        assert conn.execute("SELECT pn FROM whatsmeow_lid_map WHERE lid = ?", ("100000000000002",)).fetchone() == (
            "15550000002",
        )
