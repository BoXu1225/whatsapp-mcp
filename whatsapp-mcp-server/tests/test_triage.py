"""Triage: list_awaiting_reply, media filters, media rendering (#22)."""

from datetime import datetime, timezone

import pytest
from conftest import ALICE, BOB, CAROL_LID_JID, GROUP

import main
import whatsapp


def at(hour, minute=0):
    return datetime(2024, 1, 2, hour, minute, tzinfo=timezone.utc)


# --- list_awaiting_reply ---------------------------------------------------


def test_awaiting_reply_direct_chats_newest_first(seeded_db):
    chats = whatsapp.list_awaiting_reply()
    assert [c.jid for c in chats] == [BOB, ALICE]
    bob, alice = chats
    assert bob.last_message_id == "b1"
    assert bob.last_sender_name == "Bob Example"
    assert bob.last_message == "[image: image_1.jpg] photo"
    assert bob.last_message_at == at(10, 5)
    assert alice.last_message == "see you soon"
    assert alice.last_is_from_me is False


def test_awaiting_reply_include_groups(seeded_db):
    assert [c.jid for c in whatsapp.list_awaiting_reply(include_groups=True)] == [BOB, GROUP, ALICE]


def test_awaiting_reply_skips_chats_where_i_spoke_last(seeded_db):
    seeded_db.add_message("a4", ALICE, "15550000000", "on my way", at(10, 9), is_from_me=True)
    assert [c.jid for c in whatsapp.list_awaiting_reply()] == [BOB]


def test_awaiting_reply_same_second_uses_insert_order(seeded_db):
    seeded_db.add_message("b2", BOB, "15550000002", "you there?", at(10, 30))
    seeded_db.add_message("b3", BOB, "15550000000", "yes", at(10, 30), is_from_me=True)
    assert BOB not in [c.jid for c in whatsapp.list_awaiting_reply()]


def test_awaiting_reply_since(seeded_db):
    assert [c.jid for c in whatsapp.list_awaiting_reply(since="2024-01-02T10:03:00+00:00")] == [BOB]
    assert [c.jid for c in whatsapp.list_awaiting_reply(since="2024-01-02T10:03:00+00:00", include_groups=True)] == [BOB, GROUP]
    with pytest.raises(ValueError):
        whatsapp.list_awaiting_reply(since="yesterday")


def test_awaiting_reply_limit(seeded_db):
    assert [c.jid for c in whatsapp.list_awaiting_reply(limit=1)] == [BOB]


def test_awaiting_reply_ignores_status_and_uses_contact_names(contacts_db):
    contacts_db.add_chat("status@broadcast", "status", at(11))
    contacts_db.add_message("s1", "status@broadcast", "15550000002", "my status", at(11))
    jids = [c.jid for c in whatsapp.list_awaiting_reply(include_groups=True)]
    assert "status@broadcast" not in jids
    # Carol: I replied last in her chat; she wrote last in the group.
    assert CAROL_LID_JID not in jids
    group = next(c for c in whatsapp.list_awaiting_reply(include_groups=True) if c.jid == GROUP)
    assert group.last_sender_name == "Carol Example"


def test_awaiting_reply_tool(seeded_db):
    result = main.list_awaiting_reply()
    assert [c.jid for c in result] == [BOB, ALICE]


def test_awaiting_reply_surfaces_errors():
    with pytest.raises(whatsapp.WhatsAppDBError):
        whatsapp.list_awaiting_reply()


# --- media filter and rendering --------------------------------------------


@pytest.fixture
def media_db(seeded_db):
    seeded_db.add_message("b2", BOB, "15550000002", "", at(10, 6), media_type="document", filename="report.pdf")
    seeded_db.add_message("b3", BOB, "15550000002", "", at(10, 7), media_type="audio")
    seeded_db.add_message("b4", BOB, "15550000002", "text after", at(10, 8))
    return seeded_db


def test_media_only_filter(media_db):
    out = whatsapp.list_messages(media_only=True, include_context=False)
    assert "ID: b1" in out and "ID: b2" in out and "ID: b3" in out
    assert "ID: b4" not in out and "ID: a1" not in out


def test_media_type_filter(media_db):
    out = whatsapp.list_messages(chat_jid=BOB, media_type="document")
    assert "ID: b2" in out
    assert "ID: b1" not in out and "ID: b4" not in out


def test_media_only_rendering(media_db):
    lines = whatsapp.list_messages(chat_jid=BOB).splitlines()
    by_id = {line.split("ID: ")[1].split(" ")[0]: line for line in lines}
    assert by_id["b1"].endswith("From: Bob Example: [image: image_1.jpg] photo")
    assert by_id["b2"].endswith("From: Bob Example: [document: report.pdf]")
    assert by_id["b3"].endswith("From: Bob Example: [audio]")


def test_chat_last_message_renders_media(media_db):
    media_db.add_message("b5", BOB, "15550000002", "", at(10, 9), media_type="image", filename="cat.jpg")
    assert whatsapp.get_chat(BOB).last_message == "[image: cat.jpg]"


def test_list_messages_tool_media_filter(media_db):
    out = main.list_messages(chat_jid=BOB, media_only=True)
    assert "ID: b4" not in out and "ID: b2" in out
