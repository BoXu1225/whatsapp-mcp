package main

import (
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"
)

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// Database handler for storing message history
type MessageStore struct {
	db *sql.DB
	tx *sql.Tx // set on the view InTx passes to its callback
	// purgeDeleted clears the content of revoked messages (-purge-deleted);
	// by default it is kept and the row only marked deleted (#16).
	purgeDeleted bool
	dir          string     // store directory; downloaded media is saved under it
	backupPath   string     // backup made in this process (migration or first live merge), if any
	backupMu     sync.Mutex // guards backupPath for live merges
}

// Initialize message store
func NewMessageStore() (*MessageStore, error) {
	return NewMessageStoreAt("store")
}

// NewMessageStoreAt opens (creating if needed) messages.db inside dir and
// runs the pending migrations that don't need the device identity (see
// migrate.go). On a migration error the store is closed and the error names
// the backup.
func NewMessageStoreAt(dir string) (*MessageStore, error) {
	// Create directory for database if it doesn't exist
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	// Open SQLite database for messages
	db, err := sql.Open("sqlite3", messagesDSN(dir))
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);
		
		CREATE TABLE IF NOT EXISTS messages (
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
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	store := &MessageStore{db: db, dir: dir}
	if _, err := store.migrate(nil); err != nil {
		db.Close()
		return nil, err
	}
	// Before login the identity migrations (3, 4) can't run, so migration 5
	// waits behind them; add its columns now so messages can be stored.
	if err := store.ensureCaptureSchemaEarly(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to add message capture columns: %v", err)
	}
	return store, nil
}

// messagesDSN is the go-sqlite3 DSN for dir/messages.db. The pragmas apply to
// every pooled connection: WAL lets the MCP server read while the bridge
// writes, and a writer waits up to 5 s for a lock instead of failing at once.
func messagesDSN(dir string) string {
	return "file:" + dir + "/messages.db?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000"
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// sqlConn is what *sql.DB and *sql.Tx have in common.
type sqlConn interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// conn is the transaction when inside InTx, else the database.
func (store *MessageStore) conn() sqlConn {
	if store.tx != nil {
		return store.tx
	}
	return store.db
}

// InTx runs fn with a view of the store whose writes all go through one
// transaction, committed if fn returns nil and rolled back otherwise. Don't
// call MergeChat on the view (it has its own transaction and backup).
func (store *MessageStore) InTx(fn func(tx *MessageStore) error) error {
	if store.tx != nil {
		return fn(store)
	}
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	view := &MessageStore{db: store.db, tx: tx, dir: store.dir, purgeDeleted: store.purgeDeleted}
	if err := fn(view); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Store a chat in the database. Times are stored in UTC. last_message_time
// only moves forward (an older history chunk can arrive after newer
// messages), and an empty name keeps the stored one.
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	var last interface{}
	if !lastMessageTime.IsZero() {
		last = lastMessageTime.UTC()
	}
	_, err := store.conn().Exec(
		`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			name = COALESCE(NULLIF(excluded.name, ''), chats.name),
			last_message_time = CASE
				WHEN chats.last_message_time IS NULL OR excluded.last_message_time > chats.last_message_time
				THEN excluded.last_message_time ELSE chats.last_message_time END`,
		jid, name, last,
	)
	return err
}

// Store a message in the database, with no sender_alt.
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	return store.StoreMessageWithAlt(id, chatJID, sender, "", content, timestamp, isFromMe,
		mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength)
}

// StoreMessageWithAlt stores a message. sender is the canonical sender JID and
// senderAlt its other form ("" if unknown); see identity.go. The timestamp is
// stored in UTC.
func (store *MessageStore) StoreMessageWithAlt(id, chatJID, sender, senderAlt, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	return store.storeMessageRow(messageRow{
		id: id, chatJID: chatJID, sender: sender, senderAlt: senderAlt, content: content,
		timestamp: timestamp, isFromMe: isFromMe, mediaType: mediaType, filename: filename, url: url,
		mediaKey: mediaKey, fileSHA256: fileSHA256, fileEncSHA256: fileEncSHA256, fileLength: fileLength,
	})
}

// messageRow is a row of the messages table, as stored by storeMessageRow.
type messageRow struct {
	id, chatJID, sender, senderAlt, content string
	timestamp                               time.Time
	isFromMe                                bool
	mediaType, filename, url                string
	mediaKey, fileSHA256, fileEncSHA256     []byte
	fileLength                              uint64
	replyTo                                 string // ID of the quoted message, "" if none
}

// storeMessageRow inserts or updates a message. A message delivered again
// (history re-sync) is updated, but an edited or deleted message keeps its
// stored content, edited_at and deletion mark (#16), and an unknown
// sender_alt or reply_to doesn't erase a known one.
func (store *MessageStore) storeMessageRow(m messageRow) error {
	// Only store if there's actual content or media
	if m.content == "" && m.mediaType == "" {
		return nil
	}

	_, err := store.conn().Exec(
		`INSERT INTO messages
		(id, chat_jid, sender, sender_alt, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length, reply_to)
		VALUES (?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''))
		ON CONFLICT(id, chat_jid) DO UPDATE SET
			sender = CASE WHEN COALESCE(messages.sender, '') = '' THEN excluded.sender ELSE messages.sender END,
			sender_alt = COALESCE(excluded.sender_alt, messages.sender_alt),
			content = CASE WHEN messages.edited_at IS NOT NULL OR COALESCE(messages.is_deleted, 0) != 0
				THEN messages.content ELSE excluded.content END,
			timestamp = excluded.timestamp,
			is_from_me = excluded.is_from_me,
			media_type = excluded.media_type,
			filename = excluded.filename,
			url = excluded.url,
			media_key = excluded.media_key,
			file_sha256 = excluded.file_sha256,
			file_enc_sha256 = excluded.file_enc_sha256,
			file_length = excluded.file_length,
			reply_to = COALESCE(excluded.reply_to, messages.reply_to)`,
		m.id, m.chatJID, m.sender, m.senderAlt, m.content, m.timestamp.UTC(), m.isFromMe, m.mediaType, m.filename, m.url,
		m.mediaKey, m.fileSHA256, m.fileEncSHA256, m.fileLength, m.replyTo,
	)
	return err
}

// senderMatchSQL matches a stored message's sender against a sender and its
// alt (bind: sender, alt, sender).
const senderMatchSQL = "(sender = ? OR sender = NULLIF(?, '') OR sender_alt = ?)"

// ApplyEdit replaces the content of message targetID in chatJID with an
// edit made at `at` by sender (#16). Only the author's edits apply, never to
// a deleted message, and an edit older than the last applied one is
// ignored. Returns whether the message was updated.
func (store *MessageStore) ApplyEdit(chatJID, targetID, sender, senderAlt, content string, at time.Time) (bool, error) {
	at = at.UTC()
	res, err := store.conn().Exec(
		`UPDATE messages SET content = ?, edited_at = ?
		WHERE id = ? AND chat_jid = ? AND COALESCE(is_deleted, 0) = 0
			AND (edited_at IS NULL OR edited_at <= ?)
			AND `+senderMatchSQL,
		content, at, targetID, chatJID, at, sender, senderAlt, sender)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MarkDeleted marks message targetID in chatJID as deleted for everyone
// (revoked) at `at` (#16). A revoke by the author (sender or sender_alt)
// keeps the content unless the store was opened with purgeDeleted. With
// anySender (a group, where admins can delete others' messages, but the
// bridge can't check who is an admin) a revoke by someone else also marks
// the message deleted and records the revoker in deleted_by, but never
// clears the content. Returns whether a message was marked.
func (store *MessageStore) MarkDeleted(chatJID, targetID, sender, senderAlt string, anySender bool, at time.Time) (bool, error) {
	res, err := store.conn().Exec(
		`UPDATE messages SET is_deleted = 1, deleted_at = COALESCE(deleted_at, ?), deleted_by = NULL,
			content = CASE WHEN ? THEN '' ELSE content END
		WHERE id = ? AND chat_jid = ? AND `+senderMatchSQL,
		at.UTC(), store.purgeDeleted, targetID, chatJID, sender, senderAlt, sender)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n > 0 || !anySender {
		return n > 0, err
	}
	res, err = store.conn().Exec(
		`UPDATE messages SET is_deleted = 1, deleted_at = ?, deleted_by = ?
		WHERE id = ? AND chat_jid = ? AND COALESCE(is_deleted, 0) = 0`,
		at.UTC(), sender, targetID, chatJID)
	if err != nil {
		return false, err
	}
	n, err = res.RowsAffected()
	return n > 0, err
}

// StoreReaction records sender's reaction to message targetID in chatJID
// (#15): one per sender and message, the newest wins. An empty emoji removes
// the reaction. A reaction older than the stored one is ignored.
func (store *MessageStore) StoreReaction(chatJID, targetID, sender, emoji string, at time.Time) error {
	at = at.UTC()
	if emoji == "" {
		_, err := store.conn().Exec(
			"DELETE FROM reactions WHERE message_id = ? AND chat_jid = ? AND sender = ? AND (timestamp IS NULL OR timestamp <= ?)",
			targetID, chatJID, sender, at)
		return err
	}
	_, err := store.conn().Exec(
		`INSERT INTO reactions (message_id, chat_jid, sender, emoji, timestamp) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(message_id, chat_jid, sender) DO UPDATE SET emoji = excluded.emoji, timestamp = excluded.timestamp
		WHERE reactions.timestamp IS NULL OR excluded.timestamp >= reactions.timestamp`,
		targetID, chatJID, sender, emoji, at)
	return err
}

// MergeChat moves chat `from` into chat `to` (see mergeChatTx) if `from`
// exists. Used when a phone-number chat turns out to belong to a LID chat.
// Like the startup migrations, the first merge of a run backs the database up
// first (unless this run already made a backup); if that fails, nothing is merged.
// The moved rows then get canonical senders for their new chat (as in
// migration 4), using id.
func (store *MessageStore) MergeChat(from, to string, id Identity) error {
	if from == to || from == "" {
		return nil
	}
	if store.tx != nil {
		return fmt.Errorf("MergeChat inside a transaction")
	}
	var one int
	if err := store.db.QueryRow("SELECT 1 FROM chats WHERE jid = ?", from).Scan(&one); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if err := store.ensureBackup(); err != nil {
		return fmt.Errorf("not merged: failed to back up messages.db first: %v", err)
	}
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, _, err := mergeChatTx(tx, from, to); err != nil {
		return err
	}
	var rep migrationReport
	if err := canonicaliseSendersTx(tx, &id, to, &rep); err != nil {
		return err
	}
	return tx.Commit()
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

// Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	return mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}

// ensureBackup makes a backup (see backup in migrate.go) unless this run
// already has one.
func (store *MessageStore) ensureBackup() error {
	store.backupMu.Lock()
	defer store.backupMu.Unlock()
	if store.backupPath != "" {
		return nil
	}
	version, err := store.schemaVersion()
	if err != nil {
		return err
	}
	path, err := store.backup(version)
	if err != nil {
		return err
	}
	store.backupPath = path
	fmt.Printf("Merging a phone-number chat into its LID chat; backup of messages.db saved to %s\n", path)
	return nil
}
