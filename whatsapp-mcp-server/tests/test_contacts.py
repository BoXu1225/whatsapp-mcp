"""Contact resolution: PN <-> LID, exact matching, names from the device store (#12)."""

import os
import re

from conftest import (
    ALICE,
    ALICE_LOOKALIKE,
    BOB,
    CAROL_LID,
    CAROL_LID_JID,
    CAROL_PN,
    DAVE_PN,
    ERIN_LID,
    ERIN_LID_JID,
    GROUP,
    FakeWhatsmeowDB,
)

import whatsapp


def _by_name(contacts, name):
    matches = [c for c in contacts if c.name == name]
    assert len(matches) == 1, contacts
    return matches[0]


# --- search_contacts -------------------------------------------------------


def test_search_contacts_lid_chat_returns_real_phone_not_lid(contacts_db):
    carol = _by_name(whatsapp.search_contacts("Carol"), "Carol Example")
    assert carol.phone == CAROL_PN
    assert carol.lid == CAROL_LID
    assert carol.jid == CAROL_LID_JID  # the existing chat
    assert carol.chat_jid == CAROL_LID_JID


def test_search_contacts_by_lid_digits_reports_phone_separately(contacts_db):
    [carol] = whatsapp.search_contacts(CAROL_LID)
    assert carol.phone == CAROL_PN
    assert carol.lid == CAROL_LID


def test_search_contacts_unmapped_lid_has_no_phone(contacts_db):
    erin = _by_name(whatsapp.search_contacts("Erin"), "Erin Example")
    assert erin.phone is None
    assert erin.lid == ERIN_LID
    assert erin.jid == ERIN_LID_JID


def test_search_contacts_finds_contacts_without_chat(contacts_db):
    dave = _by_name(whatsapp.search_contacts("dave"), "Dave")
    assert dave.phone == DAVE_PN
    assert dave.jid == DAVE_PN + "@s.whatsapp.net"
    assert dave.chat_jid is None
    # push name is searchable too, but first_name wins as display name
    assert [c.name for c in whatsapp.search_contacts("davey")] == ["Dave"]


def test_search_contacts_name_priority(contacts_db):
    # business_name is used when there is no full/first name
    bakery = _by_name(whatsapp.search_contacts("bakery"), "Fake Bakery")
    assert bakery.phone == "15550000006"
    # full_name beats push_name
    assert [c.name for c in whatsapp.search_contacts("ally")] == ["Alice Example"]


def test_search_contacts_one_row_per_person(contacts_db):
    results = whatsapp.search_contacts("Example")
    jids = [c.jid for c in results]
    assert len(jids) == len(set(jids))
    assert CAROL_PN + "@s.whatsapp.net" not in jids  # Carol appears once, as her chat
    assert {ALICE, BOB, CAROL_LID_JID, ERIN_LID_JID} <= set(jids)
    assert GROUP not in jids


def test_search_contacts_without_device_store_uses_chats(seeded_db):
    [alice] = whatsapp.search_contacts("Alice")
    assert (alice.jid, alice.phone, alice.lid, alice.name) == (ALICE, "15550000001", None, "Alice Example")


# --- get_direct_chat_by_contact --------------------------------------------


def test_direct_chat_exact_match_not_substring(contacts_db):
    assert whatsapp.get_direct_chat_by_contact("15550000001").jid == ALICE
    assert whatsapp.get_direct_chat_by_contact("5550000001") is None


def test_direct_chat_accepts_formatted_number(contacts_db):
    assert whatsapp.get_direct_chat_by_contact("+1 555-000-0001").jid == ALICE


def test_direct_chat_by_phone_finds_lid_chat(contacts_db):
    chat = whatsapp.get_direct_chat_by_contact(CAROL_PN)
    assert chat.jid == CAROL_LID_JID
    assert chat.name == "Carol Example"  # placeholder chat name replaced by contact name


def test_direct_chat_by_lid(contacts_db):
    assert whatsapp.get_direct_chat_by_contact(CAROL_LID).jid == CAROL_LID_JID
    assert whatsapp.get_direct_chat_by_contact(ERIN_LID_JID).jid == ERIN_LID_JID


def test_direct_chat_lid_without_device_store(seeded_db):
    seeded_db.add_chat(ERIN_LID_JID, "Erin Example", seeded_db_time())
    assert whatsapp.get_direct_chat_by_contact(ERIN_LID).jid == ERIN_LID_JID


def seeded_db_time():
    from datetime import datetime, timezone

    return datetime(2024, 1, 2, 11, 0, tzinfo=timezone.utc)


# --- get_contact_chats -----------------------------------------------------


def test_contact_chats_each_chat_once(contacts_db):
    chats = whatsapp.get_contact_chats(ALICE)
    assert [c.jid for c in chats] == [GROUP, ALICE]


def test_contact_chats_across_pn_and_lid(contacts_db):
    chats = whatsapp.get_contact_chats(CAROL_PN + "@s.whatsapp.net")
    assert [c.jid for c in chats] == [GROUP, CAROL_LID_JID]
    assert [c.jid for c in whatsapp.get_contact_chats(CAROL_PN)] == [GROUP, CAROL_LID_JID]


# --- get_last_interaction / sender filter ----------------------------------


def test_last_interaction_matches_both_sender_formats(contacts_db):
    out = whatsapp.get_last_interaction(CAROL_PN + "@s.whatsapp.net")
    assert out is not None
    assert "carol in group" in out


def test_last_interaction_by_bare_lid(contacts_db):
    assert "carol in group" in whatsapp.get_last_interaction(CAROL_LID)


def test_list_messages_sender_filter_matches_bare_and_full_jid(contacts_db):
    out = whatsapp.list_messages(sender_phone_number=CAROL_PN, include_context=False)
    assert "carol here" in out
    assert "carol in group" in out
    assert "hi carol" not in out  # sent by me, not Carol


def test_list_messages_sender_filter_accepts_plus_format(contacts_db):
    out = whatsapp.list_messages(sender_phone_number="+1 555 000 0001", include_context=False)
    assert "hi there" in out
    assert "group hello" in out


# --- get_sender_name -------------------------------------------------------


def test_sender_name_resolves_lid_via_device_store(contacts_db):
    assert whatsapp.get_sender_name(CAROL_LID) == "Carol Example"
    assert whatsapp.get_sender_name(CAROL_LID_JID) == "Carol Example"


def test_sender_name_no_substring_false_positive(contacts_db):
    assert whatsapp.get_sender_name("5550000001") == "5550000001"
    assert whatsapp.get_sender_name(ALICE_LOOKALIKE.split("@")[0]) == "Alicia Lookalike"


def test_list_messages_shows_resolved_sender_name(contacts_db):
    out = whatsapp.list_messages(chat_jid=GROUP, include_context=False)
    assert re.search(r"From: Carol Example\b.*carol in group", out)


# --- device store path -----------------------------------------------------


def test_whatsmeow_db_path_follows_messages_db(messages_db, tmp_path):
    assert whatsapp.whatsmeow_db_path() == str(tmp_path / "whatsapp.db")


def test_whatsmeow_db_path_env_override(contacts_db, tmp_path, monkeypatch):
    other = FakeWhatsmeowDB(tmp_path / "elsewhere.db")
    other.add_contact(ALICE, full_name="Alice From Env")
    monkeypatch.setenv("WHATSMEOW_DB_PATH", other.path)
    assert whatsapp.whatsmeow_db_path() == other.path
    assert whatsapp.get_sender_name(ALICE) == "Alice From Env"


def test_missing_device_store_is_not_created(seeded_db, tmp_path):
    whatsapp.search_contacts("Alice")
    assert not os.path.exists(tmp_path / "whatsapp.db")


# --- search limit / truncation ----------------------------------------------


def test_search_contacts_limit_parameter(contacts_db):
    assert len(whatsapp.search_contacts("Example")) == 4
    assert [c.name for c in whatsapp.search_contacts("Example", limit=2)] == ["Alice Example", "Bob Example"]


def test_search_contacts_tool_reports_truncation(contacts_db):
    import main

    result = main.search_contacts("Example", limit=2)
    assert [c.name for c in result["contacts"]] == ["Alice Example", "Bob Example"]
    assert result["total_matches"] == 4
    assert result["truncated"] is True
    assert "2 of 4" in result["note"]

    full = main.search_contacts("Example")
    assert len(full["contacts"]) == 4
    assert full["truncated"] is False
