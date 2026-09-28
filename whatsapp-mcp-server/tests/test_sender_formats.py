"""Both sender formats resolve to one person (#8). All data is fake."""

import re
from datetime import datetime, timedelta, timezone

import pytest
from conftest import FakeMessagesDB

import whatsapp

LINE_ID = re.compile(r"\| ID: (\S+) \|")
UTC = timezone.utc


def ids(output):
    return LINE_ID.findall(output)


CAROL_PN = "15550000003"
CAROL_LID = "100000000000003"
GROUP = "120363000000000001@g.us"


@pytest.fixture
def mixed_senders(messages_db):
    """No device store. Carol's direct chat is her LID JID, named. In the group
    she writes in every format the bridge has stored: bare LID (old live),
    full PN with sender_alt (new, PN-addressed group), full LID with sender_alt."""
    db = messages_db
    t = datetime(2024, 1, 2, 10, 0, tzinfo=UTC)
    db.add_chat(CAROL_LID + "@lid", "Carol Example", t)
    db.add_chat(GROUP, "Test Group", t + timedelta(minutes=5))
    db.add_message("k1", GROUP, CAROL_LID, "old bare", t + timedelta(minutes=1))
    db.add_message(
        "k2", GROUP, CAROL_PN + "@s.whatsapp.net", "pn with alt", t + timedelta(minutes=2), sender_alt=CAROL_LID + "@lid"
    )
    db.add_message(
        "k3", GROUP, CAROL_LID + "@lid", "lid with alt", t + timedelta(minutes=3), sender_alt=CAROL_PN + "@s.whatsapp.net"
    )
    return db


def test_every_sender_format_resolves_to_the_same_name(mixed_senders):
    out = whatsapp.list_messages(chat_jid=GROUP)
    for text in ("old bare", "pn with alt", "lid with alt"):
        assert f"From: Carol Example: {text}" in out


@pytest.mark.parametrize("who", [CAROL_PN, "+1 555 000 0003", CAROL_LID + "@lid", CAROL_PN + "@s.whatsapp.net"])
def test_sender_filter_matches_every_format(mixed_senders, who):
    out = whatsapp.list_messages(sender_phone_number=who, include_context=False)
    assert sorted(ids(out)) == ["k1", "k2", "k3"]


def test_phone_number_finds_lid_chat_via_sender_alt(mixed_senders):
    chat = whatsapp.get_direct_chat_by_contact("+1 555 000 0003")
    assert chat is not None and chat.jid == CAROL_LID + "@lid"


def test_legacy_schema_without_sender_alt(tmp_path, monkeypatch):
    db = FakeMessagesDB(tmp_path / "legacy.db", legacy=True)
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", db.path)
    t = datetime(2024, 1, 2, 10, 0, tzinfo=UTC)
    db.add_chat(CAROL_LID + "@lid", "Carol Example", t)
    db.add_message("k1", CAROL_LID + "@lid", CAROL_LID, "old bare", t)
    out = whatsapp.list_messages(sender_phone_number=CAROL_LID, include_context=False)
    assert ids(out) == ["k1"] and "From: Carol Example: old bare" in out
