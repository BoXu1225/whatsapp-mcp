package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Media downloads (#19).
//
// Files are saved as <store>/<chat>/<message ID>.<ext>, so two messages never
// share a path; the sender's file name stays in messages.filename and is
// returned as original_filename. Files saved by older versions under their
// time-based or original name are still found (and, when the message has a
// SHA-256, only used if the content matches).
//
// Downloads use messages.direct_path when stored and fall back to the path in
// the URL. When WhatsApp's servers answer 404 or 410 (expired media), the
// bridge sends a media retry receipt asking the sender's phone to upload it
// again and reports "retry requested"; the phone's answer arrives as an
// events.MediaRetry, whose new direct path is stored and downloaded.

// mediaFetcher is the part of *whatsmeow.Client that downloads use.
type mediaFetcher interface {
	Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error)
	SendMediaRetryReceipt(ctx context.Context, message *types.MessageInfo, mediaKey []byte) error
}

// clientFetcher returns client as a mediaFetcher, or nil (no client).
func clientFetcher(client *whatsmeow.Client) mediaFetcher {
	if client == nil {
		return nil
	}
	return client
}

// errMediaRetryRequested is returned while a re-upload of expired media has
// been requested from the sender's phone.
var errMediaRetryRequested = errors.New("the media has expired on WhatsApp's servers; asked the sender's phone to upload it again, try again shortly")

const (
	mediaDownloadTimeout = 2 * time.Minute
	// A retry without an answer after this long is sent again.
	mediaRetryWait = 10 * time.Minute
)

type mediaResult struct {
	MediaType        string
	Filename         string // local file name
	OriginalFilename string // as stored for the message (sender's name for documents)
	Path             string
}

type pendingRetry struct {
	chatJID string
	at      time.Time
	err     error // the phone's negative answer, reported once
}

// mediaService downloads media and handles media retries. One instance is
// shared by the REST API and the event handler.
type mediaService struct {
	store   *MessageStore
	fetcher mediaFetcher // nil without a client
	decrypt func(*events.MediaRetry, []byte) (*waMmsRetry.MediaRetryNotification, error)
	now     func() time.Time

	mu      sync.Mutex
	pending map[string]*pendingRetry // by message ID
}

func newMediaService(fetcher mediaFetcher, store *MessageStore) *mediaService {
	return &mediaService{
		store:   store,
		fetcher: fetcher,
		decrypt: whatsmeow.DecryptMediaRetryNotification,
		now:     time.Now,
		pending: map[string]*pendingRetry{},
	}
}

// mediaRecord is what messages.db holds about a media message.
type mediaRecord struct {
	mediaType, filename, url, directPath, sender string
	isFromMe                                     bool
	mediaKey, fileSHA256, fileEncSHA256          []byte
	fileLength                                   uint64
}

func (store *MessageStore) mediaRecord(id, chatJID string) (mediaRecord, error) {
	// direct_path comes with migration 6, which can still be pending on a
	// database that waits for its identity migrations (not logged in).
	directPath := "''"
	if ok, err := store.hasColumn("messages", "direct_path"); err != nil {
		return mediaRecord{}, err
	} else if ok {
		directPath = "COALESCE(direct_path, '')"
	}
	var r mediaRecord
	err := store.db.QueryRow(`SELECT COALESCE(media_type, ''), COALESCE(filename, ''), COALESCE(url, ''),
		`+directPath+`, COALESCE(sender, ''), COALESCE(is_from_me, 0),
		media_key, file_sha256, file_enc_sha256, COALESCE(file_length, 0)
		FROM messages WHERE id = ? AND chat_jid = ?`, id, chatJID).Scan(
		&r.mediaType, &r.filename, &r.url, &r.directPath, &r.sender, &r.isFromMe,
		&r.mediaKey, &r.fileSHA256, &r.fileEncSHA256, &r.fileLength)
	return r, err
}

func (store *MessageStore) hasColumn(table, column string) (bool, error) {
	var n int
	err := store.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", table, column).Scan(&n)
	return n > 0, err
}

// directPathRef identifies a stored media message by ID and URL.
type directPathRef struct{ id, url, directPath string }

// StoreDirectPaths records media direct paths. Rows are matched by message ID
// and URL (as stored by extractMediaInfo); refs without a path are skipped.
func (store *MessageStore) StoreDirectPaths(refs []directPathRef) error {
	var todo []directPathRef
	for _, r := range refs {
		if r.id != "" && r.directPath != "" {
			todo = append(todo, r)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range todo {
		if _, err := tx.Exec("UPDATE messages SET direct_path = ? WHERE id = ? AND COALESCE(url, '') = ? AND COALESCE(media_type, '') != ''",
			r.directPath, r.id, r.url); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (store *MessageStore) setDirectPath(id, chatJID, directPath string) error {
	_, err := store.db.Exec("UPDATE messages SET direct_path = ? WHERE id = ? AND chat_jid = ?", directPath, id, chatJID)
	return err
}

// mediaDirectPath returns the URL and direct path of the media in msg (the
// media kinds extractMediaInfo stores), or "" for both.
func mediaDirectPath(msg *waProto.Message) (url, directPath string) {
	if msg == nil {
		return "", ""
	}
	if m := msg.GetImageMessage(); m != nil {
		return m.GetURL(), m.GetDirectPath()
	}
	if m := msg.GetVideoMessage(); m != nil {
		return m.GetURL(), m.GetDirectPath()
	}
	if m := msg.GetAudioMessage(); m != nil {
		return m.GetURL(), m.GetDirectPath()
	}
	if m := msg.GetDocumentMessage(); m != nil {
		return m.GetURL(), m.GetDirectPath()
	}
	return "", ""
}

var defaultMediaExt = map[string]string{"image": ".jpg", "video": ".mp4", "audio": ".ogg"}

// mediaFileName is the local file name for a message's media:
// <message ID>.<ext>. Characters other than letters, digits, - and _ in the
// ID become "_". The extension comes from the stored file name when it is
// short and alphanumeric, else from the media type (none for documents).
func mediaFileName(messageID, originalName, mediaType string) (string, error) {
	switch messageID {
	case "", ".", "..":
		return "", fmt.Errorf("invalid message ID %q", messageID)
	}
	id := strings.Map(func(r rune) rune {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return r
		}
		return '_'
	}, messageID)
	if len(id) > 128 {
		id = id[:128]
	}
	ext := ""
	if safe, ok := safeMediaFilename(originalName); ok {
		ext = path.Ext(safe)
		if len(ext) < 2 || len(ext) > 17 || strings.IndexFunc(ext[1:], func(r rune) bool {
			return !(r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'))
		}) >= 0 {
			ext = ""
		}
	}
	if ext == "" {
		ext = defaultMediaExt[mediaType]
	}
	return id + ext, nil
}

// fileMatches reports whether the regular file at p has the given SHA-256
// (true when no hash is known).
func fileMatches(p string, want []byte) bool {
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if len(want) == 0 {
		return true
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return bytes.Equal(h.Sum(nil), want)
}

// existingFile looks for media saved earlier: under the old name in the chat
// dir, then in a merged chat's old phone-JID folder (new name, then old name).
func (m *mediaService) existingFile(chatJID, name string, rec mediaRecord) (string, bool) {
	legacy, legacyOK := safeMediaFilename(rec.filename)
	legacyOK = legacyOK && legacy != name
	if legacyOK {
		if p, err := mediaLocalPath(m.store.dir, chatJID, legacy); err == nil && fileMatches(p, rec.fileSHA256) {
			return p, true
		}
	}
	if p, ok := mergedChatMediaPath(m.store, chatJID, name); ok {
		return p, true
	}
	if legacyOK {
		if p, ok := mergedChatMediaPath(m.store, chatJID, legacy); ok && fileMatches(p, rec.fileSHA256) {
			return p, true
		}
	}
	return "", false
}

// download returns the local file for a message's media, downloading it if
// needed. errMediaRetryRequested means a re-upload was requested.
func (m *mediaService) download(messageID, chatJID string) (mediaResult, error) {
	rec, err := m.store.mediaRecord(messageID, chatJID)
	if err != nil {
		return mediaResult{}, fmt.Errorf("failed to find message: %v", err)
	}
	if rec.mediaType == "" {
		return mediaResult{}, fmt.Errorf("not a media message")
	}
	name, err := mediaFileName(messageID, rec.filename, rec.mediaType)
	if err != nil {
		return mediaResult{}, err
	}
	// <store>/<chat>/<name>, checked to stay inside the store.
	localPath, err := mediaLocalPath(m.store.dir, chatJID, name)
	if err != nil {
		return mediaResult{}, fmt.Errorf("unsafe media path: %v", err)
	}
	res := mediaResult{MediaType: rec.mediaType, Filename: name, OriginalFilename: rec.filename, Path: localPath}
	if info, err := os.Lstat(localPath); err == nil && info.Mode().IsRegular() {
		return res, nil
	}
	if p, ok := m.existingFile(chatJID, name, rec); ok {
		res.Path, res.Filename = p, filepath.Base(p)
		return res, nil
	}

	if err := m.retryStatus(messageID); err != nil {
		return mediaResult{}, err
	}
	if m.fetcher == nil {
		return mediaResult{}, fmt.Errorf("not connected to WhatsApp, can't download media")
	}
	directPath := rec.directPath
	if directPath == "" && rec.url != "" {
		directPath = extractDirectPathFromURL(rec.url)
	}
	if directPath == "" || len(rec.mediaKey) == 0 || len(rec.fileSHA256) == 0 || len(rec.fileEncSHA256) == 0 || rec.fileLength == 0 {
		return mediaResult{}, fmt.Errorf("incomplete media information for download")
	}
	var waMediaType whatsmeow.MediaType
	switch rec.mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return mediaResult{}, fmt.Errorf("unsupported media type: %s", rec.mediaType)
	}

	fmt.Printf("Downloading media for message %s in chat %s...\n", messageID, chatJID)
	ctx, cancel := context.WithTimeout(context.Background(), mediaDownloadTimeout)
	defer cancel()
	data, err := m.fetcher.Download(ctx, &MediaDownloader{
		URL:           rec.url,
		DirectPath:    directPath,
		MediaKey:      rec.mediaKey,
		FileLength:    rec.fileLength,
		FileSHA256:    rec.fileSHA256,
		FileEncSHA256: rec.fileEncSHA256,
		MediaType:     waMediaType,
	})
	if errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) || errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410) {
		return mediaResult{}, m.requestRetry(messageID, chatJID, rec)
	}
	if err != nil {
		return mediaResult{}, fmt.Errorf("failed to download media: %v", err)
	}
	if err := saveMediaFile(localPath, data); err != nil {
		return mediaResult{}, fmt.Errorf("failed to save media file: %v", err)
	}
	fmt.Printf("Downloaded %s media for message %s in %s (%d bytes)\n", rec.mediaType, messageID, chatJID, len(data))
	debugPrintf("Saved media to %s\n", localPath)
	return res, nil
}

// retryStatus reports a pending retry (errMediaRetryRequested) or, once, the
// phone's negative answer to one. nil means no retry is in progress.
func (m *mediaService) retryStatus(messageID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.pending[messageID]
	if !ok {
		return nil
	}
	if st.err != nil {
		delete(m.pending, messageID)
		return st.err
	}
	if m.now().Sub(st.at) < mediaRetryWait {
		return errMediaRetryRequested
	}
	delete(m.pending, messageID) // no answer: ask again
	return nil
}

// requestRetry asks the sender's phone to re-upload expired media.
func (m *mediaService) requestRetry(messageID, chatJID string, rec mediaRecord) error {
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return fmt.Errorf("media expired and chat JID %q is invalid: %v", chatJID, err)
	}
	var sender types.JID
	if rec.sender != "" {
		sender, _ = types.ParseJID(rec.sender)
	}
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chat,
			Sender:   sender,
			IsFromMe: rec.isFromMe,
			IsGroup:  chat.Server == types.GroupServer,
		},
		ID: messageID,
	}
	m.mu.Lock()
	m.pending[messageID] = &pendingRetry{chatJID: chatJID, at: m.now()}
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.fetcher.SendMediaRetryReceipt(ctx, info, rec.mediaKey); err != nil {
		m.mu.Lock()
		delete(m.pending, messageID)
		m.mu.Unlock()
		return fmt.Errorf("the media has expired and asking the sender's phone to upload it again failed: %v", err)
	}
	fmt.Printf("Media for message %s in %s has expired; requested a re-upload from the phone\n", messageID, chatJID)
	return errMediaRetryRequested
}

// handleRetry processes the phone's answer to a media retry receipt: on
// success the new direct path is stored and the media downloaded. Answers
// for messages we didn't ask about are ignored. It may block on the download;
// call it off the event loop.
func (m *mediaService) handleRetry(evt *events.MediaRetry) {
	m.mu.Lock()
	st, ok := m.pending[evt.MessageID]
	m.mu.Unlock()
	if !ok || st.err != nil {
		return
	}
	fail := func(err error) {
		fmt.Printf("Media retry for message %s failed: %v\n", evt.MessageID, err)
		m.mu.Lock()
		st.err = err
		m.mu.Unlock()
	}
	rec, err := m.store.mediaRecord(evt.MessageID, st.chatJID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = fmt.Errorf("message no longer stored")
		}
		fail(fmt.Errorf("media retry: %v", err))
		return
	}
	notif, err := m.decrypt(evt, rec.mediaKey)
	if err != nil {
		fail(fmt.Errorf("media retry failed: %v", err))
		return
	}
	if notif.GetResult() != waMmsRetry.MediaRetryNotification_SUCCESS || notif.GetDirectPath() == "" {
		fail(fmt.Errorf("the sender's phone could not upload the media again (%s)", notif.GetResult()))
		return
	}
	if err := m.store.setDirectPath(evt.MessageID, st.chatJID, notif.GetDirectPath()); err != nil {
		fail(fmt.Errorf("media retry: storing the new path failed: %v", err))
		return
	}
	m.mu.Lock()
	delete(m.pending, evt.MessageID)
	m.mu.Unlock()
	if _, err := m.download(evt.MessageID, st.chatJID); err != nil {
		fmt.Printf("Download after media retry for message %s: %v\n", evt.MessageID, err)
	}
}
