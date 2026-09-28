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
	db         *sql.DB
	dir        string     // store directory; downloaded media is saved under it
	backupPath string     // backup made in this process (migration or first live merge), if any
	backupMu   sync.Mutex // guards backupPath for live merges
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
	db, err := sql.Open("sqlite3", "file:"+dir+"/messages.db?_foreign_keys=on")
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
	return store, nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// Store a chat in the database. Times are stored in UTC.
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, lastMessageTime.UTC(),
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
	// Only store if there's actual content or media
	if content == "" && mediaType == "" {
		return nil
	}

	_, err := store.db.Exec(
		`INSERT OR REPLACE INTO messages
		(id, chat_jid, sender, sender_alt, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length)
		VALUES (?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, senderAlt, content, timestamp.UTC(), isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
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
