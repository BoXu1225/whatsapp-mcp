"""Send tools: recipient resolution, unknown-recipient rejection, descriptions (#13).

The whatsapp.send_* functions are replaced with recorders; nothing is sent.
"""

import pytest
from conftest import ALICE, CAROL_LID, CAROL_LID_JID, DAVE_PN, ERIN_LID, ERIN_LID_JID, GROUP

import main


@pytest.fixture
def sent(monkeypatch):
    calls = []

    def recorder(kind):
        def fake(recipient, payload):
            calls.append((kind, recipient, payload))
            return True, f"Sent to {recipient}"

        return fake

    monkeypatch.setattr(main, "whatsapp_send_message", recorder("text"))
    monkeypatch.setattr(main, "whatsapp_send_file", recorder("file"))
    monkeypatch.setattr(main, "whatsapp_audio_voice_message", recorder("audio"))
    return calls


def test_unknown_recipient_is_rejected(contacts_db, sent):
    result = main.send_message("15550009999", "hello")
    assert result["success"] is False
    assert "allow_unknown" in result["message"]
    assert sent == []


def test_unknown_recipient_allowed_explicitly(contacts_db, sent):
    result = main.send_message("15550009999", "hello", allow_unknown=True)
    assert result["success"] is True
    assert sent == [("text", "15550009999@s.whatsapp.net", "hello")]
    assert result["recipient_jid"] == "15550009999@s.whatsapp.net"


def test_known_number_resolves_to_full_jid_and_name(contacts_db, sent):
    result = main.send_message("+1 555 000 0001", "hi")
    assert result["success"] is True
    assert sent == [("text", ALICE, "hi")]
    assert result["recipient_jid"] == ALICE
    assert result["recipient_name"] == "Alice Example"


def test_contact_without_chat_is_known(contacts_db, sent):
    result = main.send_message(DAVE_PN, "hi")
    assert result["success"] is True
    assert sent[0][1] == DAVE_PN + "@s.whatsapp.net"
    assert result["recipient_name"] == "Dave"


def test_lid_number_is_not_treated_as_phone(contacts_db, sent):
    result = main.send_message(CAROL_LID, "hi")
    assert result["success"] is True
    assert sent == [("text", CAROL_LID_JID, "hi")]
    assert result["recipient_name"] == "Carol Example"


def test_lid_number_from_lid_chat_without_device_store(seeded_db, sent):
    from datetime import datetime, timezone

    seeded_db.add_chat(ERIN_LID_JID, "Erin Example", datetime(2024, 1, 2, tzinfo=timezone.utc))
    result = main.send_message(ERIN_LID, "hi")
    assert sent == [("text", ERIN_LID_JID, "hi")]
    assert result["recipient_name"] == "Erin Example"


def test_lid_misused_as_phone_jid_is_corrected(contacts_db, sent):
    main.send_message(CAROL_LID + "@s.whatsapp.net", "hi")
    assert sent == [("text", CAROL_LID_JID, "hi")]


def test_known_group(contacts_db, sent):
    result = main.send_message(GROUP, "hi all")
    assert result["success"] is True
    assert sent == [("text", GROUP, "hi all")]
    assert result["recipient_name"] == "Test Group"


def test_unknown_group_rejected(contacts_db, sent):
    result = main.send_message("120363000000000999@g.us", "hi")
    assert result["success"] is False
    assert sent == []


def test_invalid_recipient_rejected(contacts_db, sent):
    for bad in ("Alice", "", "12"):
        result = main.send_message(bad, "hi", allow_unknown=True)
        assert result["success"] is False, bad
    assert sent == []


def test_send_file_and_audio_resolve_and_reject(contacts_db, sent, tmp_path):
    media = tmp_path / "pic.jpg"
    media.write_bytes(b"fake")
    assert main.send_file("15550009999", str(media))["success"] is False
    assert main.send_audio_message("15550009999", str(media))["success"] is False
    assert sent == []

    assert main.send_file(CAROL_LID, str(media))["recipient_jid"] == CAROL_LID_JID
    assert main.send_audio_message("+1 555 000 0001", str(media))["recipient_name"] == "Alice Example"
    assert [(k, r) for k, r, _ in sent] == [("file", CAROL_LID_JID), ("audio", ALICE)]


@pytest.mark.parametrize("tool", ["send_message", "send_file", "send_audio_message"])
def test_send_descriptions_require_explicit_request_and_preview(tool):
    doc = getattr(main, tool).__doc__.lower()
    assert "explicitly" in doc
    assert "exact" in doc
    assert "allow_unknown" in doc


def test_bare_number_that_is_both_phone_and_lid_is_ambiguous(contacts_db, whatsmeow_db, sent):
    from datetime import datetime, timezone

    # 15550000007 is Gina's phone number and, separately, Hank's LID.
    contacts_db.add_chat("15550000007@s.whatsapp.net", "Gina Example", datetime(2024, 1, 2, tzinfo=timezone.utc))
    whatsmeow_db.add_lid_mapping("15550000007", "15550000008")
    whatsmeow_db.add_contact("15550000008@s.whatsapp.net", full_name="Hank Example")

    result = main.send_message("15550000007", "hi", allow_unknown=True)
    assert result["success"] is False
    assert sent == []
    msg = result["message"]
    assert "ambiguous" in msg
    assert "15550000007@s.whatsapp.net" in msg and "Gina Example" in msg
    assert "15550000007@lid" in msg and "Hank Example" in msg

    # The full JID is unambiguous.
    assert main.send_message("15550000007@lid", "hi")["recipient_name"] == "Hank Example"
    assert main.send_message("15550000007@s.whatsapp.net", "hi")["recipient_name"] == "Gina Example"


def test_phone_of_lid_keyed_contact_goes_to_existing_chat(contacts_db, sent):
    from conftest import CAROL_PN

    for recipient in (CAROL_PN, "+1 555 000 0003", CAROL_PN + "@s.whatsapp.net"):
        result = main.send_message(recipient, "hi")
        assert result["recipient_jid"] == CAROL_LID_JID, recipient
    assert [r for _, r, _ in sent] == [CAROL_LID_JID] * 3
