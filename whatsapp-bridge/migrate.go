package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow/types"
)

// Schema versioning for messages.db.
//
// schema_version holds one row per applied migration. Migrations run in
// version order, each in its own transaction together with its
// schema_version row, so a failed migration leaves the database as it was
// before that migration. Before the first pending migration runs on a
// database that holds data, the whole file is copied to
// messages.db.bak-<version>-<UTC timestamp> (mode 0600).
//
// Migrations that need our own JIDs and the LID map (needsIdentity) run in a
// second phase, MigrateIdentity, once the device store is loaded. The first
// phase, at NewMessageStoreAt, stops before the first of them.

type migrationReport struct {
	TimestampsConverted int // messages.timestamp / chats.last_message_time rewritten as UTC
	TimestampsUnparsed  int // left as is: not in a format go-sqlite3 writes
	ChatsMerged         int // PN chats merged into an existing LID chat
	ChatsRekeyed        int // PN chats moved to their LID JID (no LID chat yet)
	MessageCollisions   int // messages in both chats of a merge; the LID chat's copy was kept
	SendersNormalised   int // sender rewritten (bare user, device part, own-JID form, 1:1 form)
	SenderAltFilled     int // sender_alt set where it was empty
	SendersUnresolved   int // bare users whose server can't be determined; left as is
}

func (r migrationReport) String() string {
	return fmt.Sprintf("timestamps converted %d (unparsed %d), chats merged %d, chats re-keyed %d, "+
		"message ID collisions %d, senders normalised %d, sender_alt filled %d, senders unresolved %d",
		r.TimestampsConverted, r.TimestampsUnparsed, r.ChatsMerged, r.ChatsRekeyed,
		r.MessageCollisions, r.SendersNormalised, r.SenderAltFilled, r.SendersUnresolved)
}

type migration struct {
	version       int
	name          string
	needsIdentity bool
	run           func(tx *sql.Tx, id *Identity, rep *migrationReport) error
}

// migrations, in order. Never renumber or edit an applied migration; add a new one.
var migrations = []migration{
	{version: 1, name: "utc_timestamps", run: migrateUTCTimestamps},
	{version: 2, name: "sender_alt_column", run: migrateSenderAltColumn},
	{version: 3, name: "canonical_chats", needsIdentity: true, run: migrateCanonicalChats},
	{version: 4, name: "canonical_senders", needsIdentity: true, run: migrateCanonicalSenders},
	{version: 5, name: "message_capture", run: migrateMessageCapture},
}

func (store *MessageStore) schemaVersion() (int, error) {
	if _, err := store.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return 0, err
	}
	var v int
	err := store.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&v)
	return v, err
}

// migrate runs pending migrations. With id == nil it stops before the first
// migration that needs the identity.
func (store *MessageStore) migrate(id *Identity) (migrationReport, error) {
	var rep migrationReport
	current, err := store.schemaVersion()
	if err != nil {
		return rep, fmt.Errorf("failed to read schema version: %v", err)
	}
	var pending []migration
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if m.needsIdentity && id == nil {
			break
		}
		pending = append(pending, m)
	}
	if len(pending) == 0 {
		return rep, nil
	}

	hasData, err := store.hasData()
	if err != nil {
		return rep, fmt.Errorf("failed to inspect database before migrating: %v", err)
	}
	if store.backupPath == "" {
		if hasData {
			path, err := store.backup(current)
			if err != nil {
				return rep, fmt.Errorf("failed to back up messages.db before migrating (nothing was changed): %v", err)
			}
			store.backupPath = path
			fmt.Printf("Upgrading messages.db from schema version %d; backup saved to %s\n", current, path)
		}
	}

	for _, m := range pending {
		if err := store.runMigration(m, id, &rep); err != nil {
			hint := ""
			if store.backupPath != "" {
				hint = "; a copy of the database from before the upgrade is at " + store.backupPath
			}
			return rep, fmt.Errorf("migration %d (%s) failed and was rolled back: %v%s", m.version, m.name, err, hint)
		}
	}
	if hasData {
		fmt.Printf("messages.db migrated to schema version %d: %s\n", pending[len(pending)-1].version, rep)
	}
	return rep, nil
}

func (store *MessageStore) runMigration(m migration, id *Identity, rep *migrationReport) error {
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := m.run(tx, id, rep); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO schema_version (version, name, applied_at) VALUES (?, ?, ?)",
		m.version, m.name, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return tx.Commit()
}

// MigrateIdentity runs the migrations that need our JIDs and the LID map
// (canonical chat and sender JIDs). Call it once the device store is loaded,
// before handling events.
func (store *MessageStore) MigrateIdentity(id Identity) (migrationReport, error) {
	return store.migrate(&id)
}

// PendingIdentityMigrations reports whether MigrateIdentity has work to do.
func (store *MessageStore) PendingIdentityMigrations() (bool, error) {
	current, err := store.schemaVersion()
	if err != nil {
		return false, err
	}
	return current < migrations[len(migrations)-1].version, nil
}

func (store *MessageStore) hasData() (bool, error) {
	var n int
	err := store.db.QueryRow("SELECT (SELECT COUNT(*) FROM messages) + (SELECT COUNT(*) FROM chats)").Scan(&n)
	return n > 0, err
}

// backup copies the database with VACUUM INTO to a new 0600 file next to it.
func (store *MessageStore) backup(version int) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("messages.db.bak-%d-%s", version, stamp)
		if i > 0 {
			name += fmt.Sprintf("-%d", i)
		}
		path := filepath.Join(store.dir, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		f.Close()
		if _, err := store.db.Exec("VACUUM INTO ?", path); err != nil {
			os.Remove(path)
			return "", err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return "", err
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return path, nil
		}
		return abs, nil
	}
	return "", errors.New("could not pick a backup file name")
}

// --- 1: UTC timestamps (#14) ---------------------------------------------

// sqliteTimeFormat is how go-sqlite3 writes a time.Time.
var sqliteTimeFormat = sqlite3.SQLiteTimestampFormats[0]

// parseStoredTime parses a timestamp as go-sqlite3 would read it back.
func parseStoredTime(s string) (time.Time, bool) {
	s = strings.TrimSuffix(s, "Z")
	for _, f := range sqlite3.SQLiteTimestampFormats {
		if t, err := time.ParseInLocation(f, s, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func migrateUTCTimestamps(tx *sql.Tx, _ *Identity, rep *migrationReport) error {
	convert := func(table, key, column string) error {
		rows, err := tx.Query(fmt.Sprintf("SELECT %s, CAST(%s AS TEXT) FROM %s WHERE %s IS NOT NULL", key, column, table, column))
		if err != nil {
			return err
		}
		type update struct {
			key interface{}
			val string
		}
		var updates []update
		for rows.Next() {
			var k interface{}
			var s string
			if err := rows.Scan(&k, &s); err != nil {
				rows.Close()
				return err
			}
			t, ok := parseStoredTime(s)
			if !ok {
				rep.TimestampsUnparsed++
				continue
			}
			if utc := t.UTC().Format(sqliteTimeFormat); utc != s {
				updates = append(updates, update{k, utc})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		stmt, err := tx.Prepare(fmt.Sprintf("UPDATE %s SET %s = ? WHERE %s = ?", table, column, key))
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, u := range updates {
			if _, err := stmt.Exec(u.val, u.key); err != nil {
				return err
			}
		}
		rep.TimestampsConverted += len(updates)
		return nil
	}
	if err := convert("messages", "rowid", "timestamp"); err != nil {
		return err
	}
	return convert("chats", "jid", "last_message_time")
}

// --- 2: sender_alt column (#8) ---------------------------------------------

func migrateSenderAltColumn(tx *sql.Tx, _ *Identity, _ *migrationReport) error {
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'sender_alt'").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := tx.Exec("ALTER TABLE messages ADD COLUMN sender_alt TEXT")
	return err
}

// --- 3: canonical chats (#9) -----------------------------------------------

func migrateCanonicalChats(tx *sql.Tx, id *Identity, rep *migrationReport) error {
	rows, err := tx.Query("SELECT jid FROM chats WHERE jid LIKE '%@" + types.DefaultUserServer + "'")
	if err != nil {
		return err
	}
	var jids []string
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			rows.Close()
			return err
		}
		jids = append(jids, jid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, jid := range jids {
		j, err := types.ParseJID(jid)
		if err != nil {
			continue
		}
		target := id.CanonicalChat(j)
		if target.String() == jid {
			continue
		}
		existed, collisions, err := mergeChatTx(tx, jid, target.String())
		if err != nil {
			return err
		}
		if existed {
			rep.ChatsMerged++
		} else {
			rep.ChatsRekeyed++
		}
		rep.MessageCollisions += collisions
	}
	return nil
}

// isPlaceholderFor reports whether name is empty or only the user part of
// one of the given JIDs (the fallback chat name).
func isPlaceholderFor(name string, jids ...string) bool {
	if name == "" {
		return true
	}
	for _, j := range jids {
		if user, _, _ := strings.Cut(j, "@"); name == user {
			return true
		}
	}
	return false
}

// mergeChatTx moves chat `from` into chat `to`: messages move over (when a
// message ID is in both, the copy already in `to` is kept), `to` keeps the
// better name and the later last_message_time, and `from` is deleted. If `to`
// doesn't exist it is created (re-keying `from`). Returns whether `to`
// existed and the number of colliding messages dropped from `from`.
func mergeChatTx(tx *sql.Tx, from, to string) (bool, int, error) {
	var fromName, fromTime sql.NullString
	err := tx.QueryRow("SELECT name, CAST(last_message_time AS TEXT) FROM chats WHERE jid = ?", from).Scan(&fromName, &fromTime)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	var toName, toTime sql.NullString
	err = tx.QueryRow("SELECT name, CAST(last_message_time AS TEXT) FROM chats WHERE jid = ?", to).Scan(&toName, &toTime)
	existed := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, 0, err
	}

	name := toName.String
	switch {
	case !isPlaceholderFor(toName.String, from, to):
	case !isPlaceholderFor(fromName.String, from, to):
		name = fromName.String
	default:
		name, _, _ = strings.Cut(to, "@")
	}
	last := toTime
	if later(fromTime, toTime) {
		last = fromTime
	}

	if existed {
		_, err = tx.Exec("UPDATE chats SET name = ?, last_message_time = ? WHERE jid = ?", name, last, to)
	} else {
		_, err = tx.Exec("INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)", to, name, last)
	}
	if err != nil {
		return false, 0, err
	}
	res, err := tx.Exec("DELETE FROM messages WHERE chat_jid = ? AND id IN (SELECT id FROM messages WHERE chat_jid = ?)", from, to)
	if err != nil {
		return false, 0, err
	}
	collisions, _ := res.RowsAffected()
	if _, err := tx.Exec("UPDATE messages SET chat_jid = ? WHERE chat_jid = ?", to, from); err != nil {
		return false, 0, err
	}
	if err := moveReactionsTx(tx, from, to); err != nil {
		return false, 0, err
	}
	if _, err := tx.Exec("DELETE FROM chats WHERE jid = ?", from); err != nil {
		return false, 0, err
	}
	return existed, int(collisions), nil
}

// moveReactionsTx moves reactions from chat `from` to `to` (a chat merge),
// keeping the copy already in `to` when a sender reacted in both. No-op when
// the reactions table doesn't exist yet (migrations 3 and 4 on an old DB).
func moveReactionsTx(tx *sql.Tx, from, to string) error {
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'reactions'").Scan(&n); err != nil || n == 0 {
		return err
	}
	if _, err := tx.Exec("UPDATE OR IGNORE reactions SET chat_jid = ? WHERE chat_jid = ?", to, from); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM reactions WHERE chat_jid = ?", from)
	return err
}

// later reports whether stored time a is after b (NULL/unparsable counts as earliest).
func later(a, b sql.NullString) bool {
	ta, okA := parseStoredTime(a.String)
	tb, okB := parseStoredTime(b.String)
	if !a.Valid || !okA {
		return false
	}
	if !b.Valid || !okB {
		return true
	}
	return ta.After(tb)
}

// --- 4: canonical senders (#8) ---------------------------------------------

func migrateCanonicalSenders(tx *sql.Tx, id *Identity, rep *migrationReport) error {
	return canonicaliseSendersTx(tx, id, "", rep)
}

// canonicaliseSendersTx rewrites sender and sender_alt to the canonical form
// (see identity.go) for all messages, or only those in chatJID if it is set.
// Used by migration 4 and after a runtime chat merge.
func canonicaliseSendersTx(tx *sql.Tx, id *Identity, chatJID string, rep *migrationReport) error {
	// Users with a direct chat, by server; "" when they have both kinds.
	chatUsers := map[string]string{}
	crows, err := tx.Query("SELECT jid FROM chats")
	if err != nil {
		return err
	}
	for crows.Next() {
		var jid string
		if err := crows.Scan(&jid); err != nil {
			crows.Close()
			return err
		}
		j, err := types.ParseJID(jid)
		if err != nil || !isPersonJID(j) {
			continue
		}
		if prev, seen := chatUsers[j.User]; seen && prev != j.Server {
			chatUsers[j.User] = ""
		} else {
			chatUsers[j.User] = j.Server
		}
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return err
	}

	query := "SELECT rowid, chat_jid, COALESCE(sender, ''), COALESCE(sender_alt, ''), COALESCE(is_from_me, 0) FROM messages"
	var args []interface{}
	if chatJID != "" {
		query += " WHERE chat_jid = ?"
		args = append(args, chatJID)
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return err
	}
	type update struct {
		rowid       int64
		sender, alt string
	}
	var updates []update
	for rows.Next() {
		var rowid int64
		var chatJID, sender, oldAlt string
		var fromMe bool
		if err := rows.Scan(&rowid, &chatJID, &sender, &oldAlt, &fromMe); err != nil {
			rows.Close()
			return err
		}
		chat, err := types.ParseJID(chatJID)
		if err != nil {
			rep.SendersUnresolved++
			continue
		}
		stored, bare, ok := parseStoredSender(sender)
		var s, alt string
		switch {
		case fromMe:
			s, alt = id.OwnSender(chat)
		case isPersonJID(chat):
			// The other person in a 1:1 chat: the chat's form. A stored
			// full JID of the other form is a known alt.
			hint := types.EmptyJID
			if ok && !bare && isPersonJID(stored) && stored.Server != chat.Server {
				hint = stored
			}
			s, alt = id.senderWithAlt(chat, hint)
		case !ok:
		case bare:
			if j, found := id.resolveBareUser(sender, chat, chatUsers); found {
				s, alt = id.senderWithAlt(j, types.EmptyJID)
			}
		default:
			s, alt = id.senderWithAlt(stored, types.EmptyJID)
		}
		if s == "" {
			rep.SendersUnresolved++
			continue
		}
		if alt == "" {
			alt = oldAlt
		}
		if s == sender && alt == oldAlt {
			continue
		}
		if s != sender {
			rep.SendersNormalised++
		}
		if oldAlt == "" && alt != "" {
			rep.SenderAltFilled++
		}
		updates = append(updates, update{rowid, s, alt})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	stmt, err := tx.Prepare("UPDATE messages SET sender = ?, sender_alt = NULLIF(?, '') WHERE rowid = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, u := range updates {
		if _, err := stmt.Exec(u.sender, u.alt, u.rowid); err != nil {
			return err
		}
	}
	return nil
}

// --- 5: message capture (#15, #16) -----------------------------------------

// captureColumns are the messages columns migration 5 adds.
var captureColumns = []struct{ name, def string }{
	{"reply_to", "TEXT"},                         // ID of the quoted message (ContextInfo.StanzaID)
	{"edited_at", "TIMESTAMP"},                   // time of the last applied edit, NULL if never edited
	{"is_deleted", "INTEGER NOT NULL DEFAULT 0"}, // 1 once revoked ("deleted for everyone")
	{"deleted_at", "TIMESTAMP"},                  // time of the revoke
	{"deleted_by", "TEXT"},                       // revoker, when not the author (group admin); NULL otherwise
}

// captureSchemaSQL creates the reactions table: one reaction per sender and
// message; the message may not be stored (yet).
const captureSchemaSQL = `CREATE TABLE IF NOT EXISTS reactions (
	message_id TEXT NOT NULL,
	chat_jid TEXT NOT NULL,
	sender TEXT NOT NULL,
	emoji TEXT NOT NULL,
	timestamp TIMESTAMP,
	PRIMARY KEY (message_id, chat_jid, sender)
)`

// migrateMessageCapture adds reply_to, edited_at, is_deleted, deleted_at,
// deleted_by and
// the reactions table. Idempotent: existing columns are skipped.
func migrateMessageCapture(tx *sql.Tx, _ *Identity, _ *migrationReport) error {
	return addCaptureSchema(tx)
}

func addCaptureSchema(tx *sql.Tx) error {
	for _, c := range captureColumns {
		var n int
		if err := tx.QueryRow("SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = ?", c.name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if _, err := tx.Exec("ALTER TABLE messages ADD COLUMN " + c.name + " " + c.def); err != nil {
			return err
		}
	}
	_, err := tx.Exec(captureSchemaSQL)
	return err
}

// ensureCaptureSchemaEarly adds migration 5's columns and table without
// recording the migration, when migration 5 is still pending (it waits
// behind the identity migrations until after login). The changes only add
// nullable/defaulted columns and a new table, so no backup is made; the
// migration itself later finds them in place.
func (store *MessageStore) ensureCaptureSchemaEarly() error {
	current, err := store.schemaVersion()
	if err != nil {
		return err
	}
	if current >= 5 {
		return nil
	}
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := addCaptureSchema(tx); err != nil {
		return err
	}
	return tx.Commit()
}
