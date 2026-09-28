package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// v0Schema is the messages.db schema before schema versioning.
const v0Schema = `
	CREATE TABLE chats (
		jid TEXT PRIMARY KEY,
		name TEXT,
		last_message_time TIMESTAMP
	);
	CREATE TABLE messages (
		id TEXT,
		chat_jid TEXT,
		sender TEXT,
		content TEXT,
		timestamp TIMESTAMP,
		is_from_me BOOLEAN,
		media_type TEXT,
		filename TEXT,
		url TEXT,
		media_key BLOB,
		file_sha256 BLOB,
		file_enc_sha256 BLOB,
		file_length INTEGER,
		PRIMARY KEY (id, chat_jid),
		FOREIGN KEY (chat_jid) REFERENCES chats(jid)
	);`

// legacyDB writes an unversioned messages.db into a new temp dir, runs seed
// SQL against it, and returns the dir.
func legacyDB(t *testing.T, seed string) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(v0Schema); err != nil {
		t.Fatal(err)
	}
	if seed != "" {
		if _, err := db.Exec(seed); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func queryStrings(t *testing.T, db *sql.DB, query string, args ...interface{}) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s.String)
	}
	return out
}

func backups(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "messages.db.bak-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func appliedVersions(t *testing.T, store *MessageStore) []string {
	return queryStrings(t, store.db, "SELECT version FROM schema_version ORDER BY version")
}

func TestFreshStoreIsFullyVersionedWithoutBackup(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	got := appliedVersions(t, store)
	if len(got) != len(migrations) {
		t.Errorf("schema_version = %v, want all %d migrations", got, len(migrations))
	}
	if b := backups(t, dir); len(b) != 0 {
		t.Errorf("backups for an empty DB: %v", b)
	}
	// sender_alt exists.
	if _, err := store.db.Exec("SELECT sender_alt FROM messages"); err != nil {
		t.Errorf("sender_alt column: %v", err)
	}
}

const legacyTimestamps = `
	INSERT INTO chats VALUES ('15550000001@s.whatsapp.net', 'Alice Example', '2024-07-01 11:00:00+01:00');
	INSERT INTO chats VALUES ('15550000002@s.whatsapp.net', 'Bob Example', '2024-01-02 03:04:05.5+00:00');
	INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('m1', '15550000001@s.whatsapp.net', '15550000001', 'summer', '2024-07-01 11:00:00+01:00', 0);
	INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('m2', '15550000001@s.whatsapp.net', '15550000001', 'winter', '2024-01-02 10:00:00-05:00', 0);
	INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
		VALUES ('m3', '15550000002@s.whatsapp.net', '15550000002', 'utc', '2024-01-02 03:04:05.5+00:00', 0);`

func TestMigrationConvertsTimestampsToUTCAndBacksUp(t *testing.T) {
	dir := legacyDB(t, legacyTimestamps)
	store, err := NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := queryStrings(t, store.db, "SELECT CAST(timestamp AS TEXT) FROM messages ORDER BY id")
	want := []string{"2024-07-01 10:00:00+00:00", "2024-01-02 15:00:00+00:00", "2024-01-02 03:04:05.5+00:00"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("message timestamps = %v, want %v", got, want)
	}
	got = queryStrings(t, store.db, "SELECT CAST(last_message_time AS TEXT) FROM chats ORDER BY jid")
	want = []string{"2024-07-01 10:00:00+00:00", "2024-01-02 03:04:05.5+00:00"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("chat times = %v, want %v", got, want)
	}

	b := backups(t, dir)
	if len(b) != 1 || !strings.Contains(filepath.Base(b[0]), "bak-0-") {
		t.Fatalf("backups = %v, want one messages.db.bak-0-<timestamp>", b)
	}
	info, err := os.Stat(b[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %o, want 600", info.Mode().Perm())
	}
	bak, err := sql.Open("sqlite3", "file:"+b[0]+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer bak.Close()
	if orig := queryStrings(t, bak, "SELECT CAST(timestamp AS TEXT) FROM messages WHERE id = 'm1'"); len(orig) != 1 || orig[0] != "2024-07-01 11:00:00+01:00" {
		t.Errorf("backup m1 timestamp = %v, want the original local-offset text", orig)
	}

	// The identity phase in the same process does not make a second backup.
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if b := backups(t, dir); len(b) != 1 {
		t.Errorf("backups after identity migrations = %v, want still one", b)
	}

	// Reopening: nothing pending, no new backup, data unchanged.
	store, err = NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	if b := backups(t, dir); len(b) != 1 {
		t.Errorf("backups after reopen = %v, want still one", b)
	}
	if got := queryStrings(t, store.db, "SELECT CAST(timestamp AS TEXT) FROM messages WHERE id = 'm2'"); got[0] != "2024-01-02 15:00:00+00:00" {
		t.Errorf("m2 after reopen = %v", got)
	}
}

func TestFailedMigrationLeavesDBUntouched(t *testing.T) {
	dir := legacyDB(t, legacyTimestamps)
	orig := migrations
	t.Cleanup(func() { migrations = orig })
	migrations = append([]migration{}, orig[0], migration{
		version: 2, name: "boom",
		run: func(tx *sql.Tx, _ *Identity, _ *migrationReport) error {
			if _, err := tx.Exec("UPDATE messages SET content = 'clobbered'"); err != nil {
				return err
			}
			return errors.New("simulated failure")
		},
	})

	_, err := NewMessageStoreAt(dir)
	if err == nil {
		t.Fatal("NewMessageStoreAt succeeded, want the migration error")
	}
	b := backups(t, dir)
	if len(b) != 1 {
		t.Fatalf("backups = %v, want one", b)
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), b[0]) {
		t.Errorf("error %q should name the migration and the backup %s", err, b[0])
	}

	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := queryStrings(t, db, "SELECT content FROM messages WHERE content = 'clobbered'"); len(got) != 0 {
		t.Errorf("failed migration's changes were kept: %v", got)
	}
	// Migration 1 committed before the failure; the failed one is not recorded.
	if got := queryStrings(t, db, "SELECT version FROM schema_version"); strings.Join(got, ",") != "1" {
		t.Errorf("schema_version = %v, want only 1", got)
	}
}

func TestStoreConvertsTimestampsToUTC(t *testing.T) {
	store := newTestStore(t)
	paris := time.FixedZone("CET", 3600)
	ts := time.Date(2024, 1, 2, 11, 0, 0, 0, paris)
	if err := store.StoreChat(testChatJID, "x", ts); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreMessage("m1", testChatJID, "15550000001@s.whatsapp.net", "hi", ts, false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	var msgTS, chatTS string
	if err := store.db.QueryRow("SELECT CAST(timestamp AS TEXT) FROM messages").Scan(&msgTS); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT CAST(last_message_time AS TEXT) FROM chats").Scan(&chatTS); err != nil {
		t.Fatal(err)
	}
	for _, got := range []string{msgTS, chatTS} {
		if got != "2024-01-02 10:00:00+00:00" {
			t.Errorf("stored timestamp %q, want UTC 2024-01-02 10:00:00+00:00", got)
		}
	}
}
