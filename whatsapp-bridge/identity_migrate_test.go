package main

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// Senders from the old bridge: bare users (live), full JIDs with devices, own
// messages as our bare PN (history) or bare LID (live in LID chats).
const legacyIdentity = `
	INSERT INTO chats VALUES ('100000000000003@lid', '100000000000003', '2024-01-02 10:05:00+00:00');
	INSERT INTO chats VALUES ('15550000003@s.whatsapp.net', 'Carol Example', '2024-01-02 10:09:00+00:00');
	INSERT INTO chats VALUES ('15550000004@s.whatsapp.net', 'Dan Example', '2024-01-02 10:00:00+00:00');
	INSERT INTO chats VALUES ('120363000000000001@g.us', 'Test Group', '2024-01-02 10:00:00+00:00');

	-- Carol's LID chat (live): bare LID sender, own bare LID.
	INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES
		('c1', '100000000000003@lid', '100000000000003', 'fake 1', '2024-01-02 10:04:00+00:00', 0),
		('c2', '100000000000003@lid', '100000000000000', 'fake 2', '2024-01-02 10:05:00+00:00', 1),
		('dup', '100000000000003@lid', '100000000000003', 'fake dup (lid copy)', '2024-01-02 10:03:00+00:00', 0);
	-- Carol's PN chat (history): full PN sender, own bare PN, one ID also in the LID chat.
	INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES
		('p1', '15550000003@s.whatsapp.net', '15550000003@s.whatsapp.net', 'fake 3', '2024-01-02 10:01:00+00:00', 0),
		('p2', '15550000003@s.whatsapp.net', '15550000000', 'fake 4', '2024-01-02 10:09:00+00:00', 1),
		('dup', '15550000003@s.whatsapp.net', '15550000003', 'fake dup (pn copy)', '2024-01-02 10:03:00+00:00', 0);
	-- Dan: PN only, no mapping. Bare sender, own bare PN.
	INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES
		('d1', '15550000004@s.whatsapp.net', '15550000004', 'fake 5', '2024-01-02 09:59:00+00:00', 0),
		('d2', '15550000004@s.whatsapp.net', '15550000000', 'fake 6', '2024-01-02 10:00:00+00:00', 1);
	-- Group: device-suffixed PN, bare mapped LID, bare unknown, own bare LID.
	INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES
		('g1', '120363000000000001@g.us', '15550000003:7@s.whatsapp.net', 'fake 7', '2024-01-02 10:00:00+00:00', 0),
		('g2', '120363000000000001@g.us', '100000000000003', 'fake 8', '2024-01-02 10:00:01+00:00', 0),
		('g3', '120363000000000001@g.us', '19990000009', 'fake 9', '2024-01-02 10:00:02+00:00', 0),
		('g4', '120363000000000001@g.us', '100000000000000', 'fake 10', '2024-01-02 10:00:03+00:00', 1);`

type msgIdentity struct{ chat, sender, alt string }

func messageIdentities(t *testing.T, db *sql.DB) map[string]msgIdentity {
	t.Helper()
	rows, err := db.Query("SELECT id, chat_jid, sender, COALESCE(sender_alt, '') FROM messages")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]msgIdentity{}
	for rows.Next() {
		var id string
		var m msgIdentity
		if err := rows.Scan(&id, &m.chat, &m.sender, &m.alt); err != nil {
			t.Fatal(err)
		}
		if _, dup := got[id]; dup {
			t.Errorf("message %s stored more than once", id)
		}
		got[id] = m
	}
	return got
}

func TestIdentityMigrationMergesChatsAndNormalisesSenders(t *testing.T) {
	dir := legacyDB(t, legacyIdentity)
	store, err := NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	report, err := store.MigrateIdentity(testIdentity())
	if err != nil {
		t.Fatal(err)
	}

	const (
		carol    = "100000000000003@lid"
		carolAlt = "15550000003@s.whatsapp.net"
		meLID    = "100000000000000@lid"
		mePN     = "15550000000@s.whatsapp.net"
		dan      = "15550000004@s.whatsapp.net"
		group    = "120363000000000001@g.us"
	)
	want := map[string]msgIdentity{
		"c1":  {carol, carol, carolAlt},
		"c2":  {carol, meLID, mePN},
		"dup": {carol, carol, carolAlt},
		"p1":  {carol, carol, carolAlt}, // the other person in a 1:1 chat: the chat's form
		"p2":  {carol, meLID, mePN}, // own message now in a LID chat
		"d1":  {dan, dan, ""},
		"d2":  {dan, mePN, meLID},
		"g1":  {group, carolAlt, carol},
		"g2":  {group, carol, carolAlt},
		"g3":  {group, "19990000009", ""}, // unknowable: left as is
		"g4":  {group, mePN, meLID},
	}
	got := messageIdentities(t, store.db)
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s = %+v, want %+v", id, got[id], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d messages, want %d", len(got), len(want))
	}

	chats := queryStrings(t, store.db, "SELECT jid || '|' || name || '|' || CAST(last_message_time AS TEXT) FROM chats ORDER BY jid")
	wantChats := []string{
		carol + "|Carol Example|2024-01-02 10:09:00+00:00", // best name, latest time
		group + "|Test Group|2024-01-02 10:00:00+00:00",
		dan + "|Dan Example|2024-01-02 10:00:00+00:00",
	}
	if strings.Join(chats, "\n") != strings.Join(wantChats, "\n") {
		t.Errorf("chats =\n%s\nwant\n%s", strings.Join(chats, "\n"), strings.Join(wantChats, "\n"))
	}

	if report.ChatsMerged != 1 || report.MessageCollisions != 1 || report.SendersUnresolved != 1 {
		t.Errorf("report = %+v, want 1 merged chat, 1 collision, 1 unresolved sender", report)
	}
	if fk := queryStrings(t, store.db, "SELECT \"table\" FROM pragma_foreign_key_check"); len(fk) != 0 {
		t.Errorf("foreign key violations: %v", fk)
	}
}

// A PN chat with a mapping but no LID chat yet is re-keyed to the LID; its
// placeholder name becomes the LID placeholder so name backfill still works.
func TestIdentityMigrationRekeysPNOnlyChat(t *testing.T) {
	dir := legacyDB(t, `
		INSERT INTO chats VALUES ('15550000003@s.whatsapp.net', '15550000003', '2024-01-02 10:00:00+00:00');
		INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES
			('p1', '15550000003@s.whatsapp.net', '15550000003', 'fake', '2024-01-02 10:00:00+00:00', 0);`)
	store, err := NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	chats := queryStrings(t, store.db, "SELECT jid || '|' || name FROM chats")
	if len(chats) != 1 || chats[0] != "100000000000003@lid|100000000000003" {
		t.Errorf("chats = %v, want the LID chat with the LID placeholder name", chats)
	}
	if got := messageIdentities(t, store.db)["p1"]; got.chat != "100000000000003@lid" || got.sender != "100000000000003@lid" || got.alt != "15550000003@s.whatsapp.net" {
		t.Errorf("p1 = %+v", got)
	}
}

// A failure in the identity phase rolls back and reports the backup.
func TestIdentityMigrationFailureRollsBack(t *testing.T) {
	dir := legacyDB(t, legacyIdentity)
	store, err := NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	orig := migrations
	t.Cleanup(func() { migrations = orig })
	migrations = append(append([]migration{}, orig...), migration{
		version: len(orig) + 1, name: "boom", needsIdentity: true,
		run: func(tx *sql.Tx, _ *Identity, _ *migrationReport) error {
			if _, err := tx.Exec("DELETE FROM messages"); err != nil {
				return err
			}
			return errors.New("simulated failure")
		},
	})
	_, err = store.MigrateIdentity(testIdentity())
	if err == nil || !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "bak-0-") {
		t.Fatalf("MigrateIdentity error = %v, want one naming the migration and the backup", err)
	}
	if n := queryStrings(t, store.db, "SELECT COUNT(*) FROM messages"); n[0] == "0" {
		t.Error("failed migration deleted messages")
	}
}
