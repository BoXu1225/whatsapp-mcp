"""list_messages output: ordered, unique, with IDs, chat names and match markers (#10)."""

import re
from datetime import datetime, timezone

from conftest import ALICE, GROUP

import main
import whatsapp

LINE_ID = re.compile(r"ID: (\S+)")


def ids(output):
    return LINE_ID.findall(output)


def matched_ids(output):
    return [LINE_ID.search(line).group(1) for line in output.splitlines() if line.startswith(">>")]


def at(minute, second=0):
    return datetime(2024, 1, 2, 11, minute, second, tzinfo=timezone.utc)


def test_chat_jid_defaults_to_no_context_oldest_first(seeded_db):
    out = whatsapp.list_messages(chat_jid=ALICE)
    assert ids(out) == ["a1", "a2", "a3"]


def test_tool_default_with_chat_jid_has_no_duplicates(seeded_db):
    out = main.list_messages(chat_jid=ALICE)
    assert ids(out) == ["a1", "a2", "a3"]


def test_every_line_has_id_chat_name_and_sender(seeded_db):
    out = whatsapp.list_messages(chat_jid=ALICE)
    lines = [line for line in out.splitlines() if LINE_ID.search(line)]
    assert len(lines) == 3
    assert all("Chat: Alice Example" in line for line in lines)
    assert "From: Alice Example: hi there" in lines[0]
    assert "From: Me: hello back" in lines[1]


def test_limit_returns_newest_page_in_chronological_order(messages_db):
    messages_db.add_chat(ALICE, "Alice Example", at(4))
    for i in range(5):
        messages_db.add_message(f"m{i}", ALICE, "15550000001", f"msg {i}", at(i))
    assert ids(whatsapp.list_messages(chat_jid=ALICE, limit=3)) == ["m2", "m3", "m4"]
    assert ids(whatsapp.list_messages(chat_jid=ALICE, limit=3, page=1)) == ["m0", "m1"]


def test_same_second_messages_keep_insert_order(messages_db):
    messages_db.add_chat(ALICE, "Alice Example", at(0))
    messages_db.add_message("z-first", ALICE, "15550000001", "one", at(0))
    messages_db.add_message("a-second", ALICE, "15550000001", "two", at(0))
    assert ids(whatsapp.list_messages(chat_jid=ALICE)) == ["z-first", "a-second"]


def test_context_is_deduplicated_ordered_and_matches_marked(seeded_db):
    out = whatsapp.list_messages(chat_jid=ALICE, query="o", include_context=True)
    assert ids(out) == ["a1", "a2", "a3"]
    assert matched_ids(out) == ["a2", "a3"]


def test_cross_chat_search_with_context(seeded_db):
    out = whatsapp.list_messages(query="hello")  # include_context defaults to True without chat_jid
    got = ids(out)
    assert got == ["a1", "a2", "a3", "g1"]
    assert len(got) == len(set(got))
    assert matched_ids(out) == ["a2", "g1"]
    assert "Chat: Test Group" in out


def test_no_match_markers_without_context(seeded_db):
    out = whatsapp.list_messages(chat_jid=ALICE, include_context=False)
    assert matched_ids(out) == []


def test_message_context_before_is_chronological(seeded_db):
    ctx = whatsapp.get_message_context("a3", before=5, after=5)
    assert [m.id for m in ctx.before] == ["a1", "a2"]


def test_message_context_includes_same_second_neighbours(messages_db):
    messages_db.add_chat(GROUP, "Test Group", at(0))
    messages_db.add_message("s1", GROUP, "15550000001", "one", at(0))
    messages_db.add_message("s2", GROUP, "15550000002", "two", at(0))
    messages_db.add_message("s3", GROUP, "15550000001", "three", at(0))
    ctx = whatsapp.get_message_context("s2", before=5, after=5)
    assert [m.id for m in ctx.before] == ["s1"]
    assert [m.id for m in ctx.after] == ["s3"]


def test_query_treats_like_wildcards_literally(seeded_db):
    seeded_db.add_message("p1", ALICE, "15550000001", "50% off", at(0))
    assert ids(whatsapp.list_messages(query="0%", include_context=False)) == ["p1"]
