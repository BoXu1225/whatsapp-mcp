"""Contact resolution: phone numbers (PN), LIDs and display names.

WhatsApp identifies a person either by phone number (``<pn>@s.whatsapp.net``)
or by an opaque LID (``<lid>@lid``). The bridge stores chats and senders in
whichever form WhatsApp delivered, and senders sometimes as a bare user part
with no server. A LID is not a phone number: sending to ``<lid>@s.whatsapp.net``
reaches nobody (or the wrong person).

``Directory`` joins three sources, read once per call:

- whatsmeow_lid_map (lid, pn) from the whatsmeow device store (whatsapp.db);
- whatsmeow_contacts (their_jid, full_name, first_name, business_name, push_name);
- the bridge's chats table.

The device store is optional. Without it, only the chats table is used and a
bare number is treated as a LID only when a ``<number>@lid`` chat exists.
"""

import os
import re
import sqlite3
from dataclasses import dataclass, field
from pathlib import Path
from typing import Dict, Iterable, List, Optional, Set, Tuple

PN_SERVER = "s.whatsapp.net"
LID_SERVER = "lid"
GROUP_SERVER = "g.us"
DIRECT_SERVERS = (PN_SERVER, LID_SERVER)

# Name fields in order of preference.
NAME_FIELDS = ("full_name", "first_name", "business_name", "push_name")

_NUMBER_NOISE = re.compile(r"[\s+\-().]")


def normalise_number(value: str) -> str:
    """Strip '+', spaces, dashes, dots and parentheses: '+1 (555) 000-0001' -> '15550000001'."""
    return _NUMBER_NOISE.sub("", value or "")


def split_jid(value: str) -> Tuple[str, Optional[str]]:
    """Split a JID or number into (user, server).

    '15550000001:3@s.whatsapp.net' -> ('15550000001', 's.whatsapp.net')
    '+1 555 000 0001'             -> ('15550000001', None)
    """
    value = (value or "").strip()
    if "@" in value:
        user, server = value.split("@", 1)
        return user.split(":", 1)[0].strip(), server.strip().lower()
    return normalise_number(value), None


@dataclass(frozen=True)
class Identity:
    """Who an identifier refers to. `kind` is the form the identifier itself used."""

    kind: str  # "pn" or "lid"
    phone: Optional[str]
    lid: Optional[str]

    @property
    def jid(self) -> str:
        """The JID in the identifier's own form. Never '<lid>@s.whatsapp.net'."""
        if self.kind == "lid":
            return f"{self.lid}@{LID_SERVER}"
        return f"{self.phone}@{PN_SERVER}"


@dataclass
class Person:
    phone: Optional[str] = None
    lid: Optional[str] = None
    contact_entries: List[dict] = field(default_factory=list)  # PN entry first
    chats: List[Tuple[str, Optional[str]]] = field(default_factory=list)  # (jid, name), most recent first

    @property
    def chat_jid(self) -> Optional[str]:
        return self.chats[0][0] if self.chats else None

    @property
    def jid(self) -> str:
        """Existing direct chat if any, else the phone JID, else the LID JID."""
        if self.chats:
            return self.chats[0][0]
        if self.phone:
            return f"{self.phone}@{PN_SERVER}"
        return f"{self.lid}@{LID_SERVER}"

    @property
    def name(self) -> Optional[str]:
        """full_name > first_name > business_name > push_name > chat name."""
        for name_field in NAME_FIELDS:
            for entry in self.contact_entries:
                if entry.get(name_field):
                    return entry[name_field]
        for _, chat_name in self.chats:
            if chat_name and chat_name not in (self.phone, self.lid):
                return chat_name
        return None

    def searchable_names(self) -> Iterable[str]:
        for entry in self.contact_entries:
            for name_field in NAME_FIELDS:
                if entry.get(name_field):
                    yield entry[name_field]
        for _, chat_name in self.chats:
            if chat_name:
                yield chat_name


@dataclass
class Recipient:
    jid: str
    name: Optional[str]
    known: bool
    is_group: bool = False


class Directory:
    def __init__(
        self,
        lid_map: Iterable[Tuple[str, str]] = (),
        contacts: Iterable[dict] = (),
        chats: Iterable[Tuple[str, Optional[str]]] = (),
        device_store_available: bool = False,
    ):
        """chats: (jid, name) rows, most recently active first."""
        self.device_store_available = device_store_available
        self.lid_to_pn: Dict[str, str] = {}
        self.pn_to_lid: Dict[str, str] = {}
        for lid, pn in lid_map:
            if lid and pn:
                self.lid_to_pn[lid] = pn
                self.pn_to_lid[pn] = lid

        self.chats: Dict[str, Optional[str]] = {}
        chats = list(chats)
        for jid, name in chats:
            self.chats.setdefault(jid, name)

        # Users that only appear as a LID chat count as LIDs even without a mapping.
        self._lid_chat_users: Set[str] = set()
        self._known_phones: Set[str] = set(self.pn_to_lid)
        for jid in self.chats:
            user, server = split_jid(jid)
            if server == LID_SERVER:
                self._lid_chat_users.add(user)
            elif server == PN_SERVER:
                self._known_phones.add(user)
        contacts = list(contacts)
        for row in contacts:
            user, server = split_jid(row.get("their_jid") or "")
            if server == PN_SERVER:
                self._known_phones.add(user)
            elif server == LID_SERVER:
                self._lid_chat_users.add(user)

        self._people: Dict[Tuple[str, str], Person] = {}
        for row in contacts:
            person = self._person_for(row.get("their_jid") or "", create=True)
            if person is None:
                continue
            _, server = split_jid(row["their_jid"])
            if server == PN_SERVER:
                person.contact_entries.insert(0, row)
            else:
                person.contact_entries.append(row)
        for jid, name in chats:
            person = self._person_for(jid, create=True)
            if person is not None and jid not in [j for j, _ in person.chats]:
                person.chats.append((jid, name))

    # --- loading -----------------------------------------------------------

    @classmethod
    def load(cls, messages_conn: sqlite3.Connection, whatsmeow_db_path: Optional[str]) -> "Directory":
        """Build from an open messages.db connection and the device store path.

        A missing or unreadable device store is not an error: the directory then
        falls back to the chats table. The device store is opened read-only and
        never created.
        """
        chats = messages_conn.execute(
            "SELECT jid, name FROM chats ORDER BY last_message_time DESC"
        ).fetchall()

        lid_map: List[Tuple[str, str]] = []
        contacts: List[dict] = []
        available = False
        if whatsmeow_db_path and os.path.isfile(whatsmeow_db_path):
            try:
                uri = Path(whatsmeow_db_path).resolve().as_uri() + "?mode=ro"
                conn = sqlite3.connect(uri, uri=True)
                try:
                    lid_map = conn.execute("SELECT lid, pn FROM whatsmeow_lid_map").fetchall()
                    conn.row_factory = sqlite3.Row
                    contacts = [
                        dict(row)
                        for row in conn.execute(
                            "SELECT their_jid, first_name, full_name, push_name, business_name FROM whatsmeow_contacts"
                        )
                    ]
                    available = True
                finally:
                    conn.close()
            except sqlite3.Error:
                lid_map, contacts = [], []
        return cls(lid_map, contacts, chats, device_store_available=available)

    # --- identity ----------------------------------------------------------

    def identify(self, value: str) -> Optional[Identity]:
        """Resolve a number or JID to a person identity, exactly (no substring matching).

        A bare number is a LID when it is a known LID (device store mapping or
        an existing @lid chat); otherwise it is a phone number. A
        '<n>@s.whatsapp.net' whose <n> is a known LID and not a known phone is
        read as a LID too: that's the malformed JID older versions produced.
        Returns None for groups, broadcasts, newsletters and non-numeric input.
        """
        user, server = split_jid(value)
        if not user or not user.isdigit():
            return None
        if server == LID_SERVER:
            return Identity("lid", self.lid_to_pn.get(user), user)
        if server not in (None, PN_SERVER):
            return None
        if self._is_lid(user) and (server is None or user not in self._known_phones):
            return Identity("lid", self.lid_to_pn.get(user), user)
        return Identity("pn", user, self.pn_to_lid.get(user))

    def _is_lid(self, user: str) -> bool:
        return user in self.lid_to_pn or user in self._lid_chat_users

    def _person_for(self, value: str, create: bool = False) -> Optional[Person]:
        ident = self.identify(value)
        if ident is None:
            return None
        key = ("pn", ident.phone) if ident.phone else ("lid", ident.lid)
        person = self._people.get(key)
        if person is None and create:
            person = self._people[key] = Person(phone=ident.phone, lid=ident.lid)
        return person

    def person(self, value: str) -> Optional[Person]:
        """The known person (has a chat or a device-store contact) for a number or JID."""
        return self._person_for(value)

    def sender_ids(self, value: str) -> List[str]:
        """Every string messages.sender may hold for this person: bare user and full JID, PN and LID."""
        ident = self.identify(value)
        if ident is None:
            return [value]
        ids = []
        if ident.phone:
            ids += [ident.phone, f"{ident.phone}@{PN_SERVER}"]
        if ident.lid:
            ids += [ident.lid, f"{ident.lid}@{LID_SERVER}"]
        return ids

    def direct_chat_jids(self, value: str) -> List[str]:
        """Possible direct-chat JIDs for this person (PN and LID form)."""
        ident = self.identify(value)
        if ident is None:
            return [value]
        jids = []
        if ident.phone:
            jids.append(f"{ident.phone}@{PN_SERVER}")
        if ident.lid:
            jids.append(f"{ident.lid}@{LID_SERVER}")
        return jids

    # --- names -------------------------------------------------------------

    def name_for(self, value: str) -> Optional[str]:
        """Display name for a sender or chat identifier, or None if unknown."""
        person = self.person(value)
        if person is not None:
            return person.name
        return self.chats.get(value) or None

    def chat_display_name(self, jid: str, stored_name: Optional[str]) -> Optional[str]:
        """Contact name for direct chats (replaces placeholder names), stored name otherwise."""
        person = self.person(jid)
        if person is not None and person.name:
            return person.name
        return stored_name

    # --- search ------------------------------------------------------------

    def search(self, query: str, limit: int = 50) -> List[Person]:
        """People whose name contains the query, or whose phone/LID contains its digits."""
        needle = (query or "").strip().lower()
        digits = normalise_number(query)
        if not digits.isdigit():
            digits = ""
        found = []
        for person in self._people.values():
            if needle and any(needle in n.lower() for n in person.searchable_names()):
                found.append(person)
            elif digits and ((person.phone and digits in person.phone) or (person.lid and digits in person.lid)):
                found.append(person)
        found.sort(key=lambda p: ((p.name or "").lower(), p.jid))
        return found[:limit]

    # --- sending -----------------------------------------------------------

    def resolve_recipient(self, value: str) -> Recipient:
        """Resolve a send recipient to a full JID.

        Raises ValueError for input that isn't a phone number or JID.
        """
        user, server = split_jid(value)
        if server is not None and server not in DIRECT_SERVERS:
            if not user or not re.fullmatch(r"[0-9-]+", user):
                raise ValueError(f"'{value}' is not a valid JID")
            jid = f"{user}@{server}"
            return Recipient(jid=jid, name=self.chats.get(jid), known=jid in self.chats, is_group=server == GROUP_SERVER)

        ident = self.identify(value)
        if ident is None or not 5 <= len(user) <= 20:
            raise ValueError(
                f"'{value}' is not a phone number or JID. Use search_contacts to find the contact's JID."
            )
        if server is None and self._is_lid(user) and user in self._known_phones:
            # The same digits are someone's phone number and (maybe someone else's) LID.
            candidates = []
            for jid in (f"{user}@{PN_SERVER}", f"{user}@{LID_SERVER}"):
                candidate = self.person(jid)
                candidates.append(f"{jid} ({(candidate.name if candidate else None) or 'unknown name'})")
            raise ValueError(
                f"'{value}' is ambiguous: it is both a phone number and a LID. Candidates: "
                f"{' and '.join(candidates)}. Pass the full JID of the intended recipient."
            )
        person = self.person(value)
        return Recipient(jid=ident.jid, name=person.name if person else None, known=person is not None)
