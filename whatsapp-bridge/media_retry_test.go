package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// --- fakes --------------------------------------------------------------------

// fakeFetcher stands in for the whatsmeow client: downloads are answered from
// data (by direct path) or errs, retry receipts are recorded.
type fakeFetcher struct {
	mu        sync.Mutex
	data      map[string][]byte
	errs      map[string]error
	downloads []string
	receipts  []types.MessageInfo
	keys      [][]byte
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{data: map[string][]byte{}, errs: map[string]error{}}
}

func (f *fakeFetcher) Download(_ context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := msg.GetDirectPath()
	f.downloads = append(f.downloads, p)
	if err, ok := f.errs[p]; ok {
		return nil, err
	}
	if d, ok := f.data[p]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("fake: nothing at %s", p)
}

func (f *fakeFetcher) SendMediaRetryReceipt(_ context.Context, info *types.MessageInfo, mediaKey []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receipts = append(f.receipts, *info)
	f.keys = append(f.keys, mediaKey)
	return nil
}

func (f *fakeFetcher) downloadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.downloads)
}

func sha(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

// seedMedia stores a media message with complete download info.
func seedMedia(t *testing.T, store *MessageStore, id, chat, sender, mediaType, filename, url string, content []byte) {
	t.Helper()
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := store.StoreChat(chat, "Test Chat", ts); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreMessage(id, chat, sender, "", ts, false, mediaType, filename, url,
		[]byte("media-key-"+id), sha(content), sha(append([]byte("enc"), content...)), uint64(len(content))); err != nil {
		t.Fatal(err)
	}
}

func setDirectPath(t *testing.T, store *MessageStore, id, chat, directPath string) {
	t.Helper()
	if _, err := store.db.Exec("UPDATE messages SET direct_path = ? WHERE id = ? AND chat_jid = ?", directPath, id, chat); err != nil {
		t.Fatal(err)
	}
}

func realStoreDir(t *testing.T, store *MessageStore) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// --- file names ---------------------------------------------------------------

func TestMediaFileNameByMessageID(t *testing.T) {
	tests := []struct {
		id, orig, mediaType, want string
	}{
		{"3EB0A1B2C3", "image_20240101_120000.jpg", "image", "3EB0A1B2C3.jpg"},
		{"3EB0A1B2C3", "video_20240101_120000.mp4", "video", "3EB0A1B2C3.mp4"},
		{"3EB0A1B2C3", "audio_20240101_120000.ogg", "audio", "3EB0A1B2C3.ogg"},
		{"3EB0A1B2C3", "Quarterly Report.PDF", "document", "3EB0A1B2C3.PDF"},
		{"3EB0A1B2C3", "README", "document", "3EB0A1B2C3"},
		{"3EB0A1B2C3", "", "image", "3EB0A1B2C3.jpg"},
		{"3EB0A1B2C3", "evil.p$f", "document", "3EB0A1B2C3"},
		{"3EB0A1B2C3", "x." + strings.Repeat("z", 40), "document", "3EB0A1B2C3"},
		{"../../etc/passwd", "a.jpg", "image", "______etc_passwd.jpg"},
		{"id with space", "a.jpg", "image", "id_with_space.jpg"},
	}
	for _, tt := range tests {
		got, err := mediaFileName(tt.id, tt.orig, tt.mediaType)
		if err != nil || got != tt.want {
			t.Errorf("mediaFileName(%q, %q, %q) = %q, %v; want %q", tt.id, tt.orig, tt.mediaType, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", ".", ".."} {
		if got, err := mediaFileName(bad, "a.jpg", "image"); err == nil {
			t.Errorf("mediaFileName(%q) = %q, want an error", bad, got)
		}
	}
}

// Two images stored in the same second used to share image_<time>.jpg, and
// the second download returned the first file.
func TestDownloadMediaSameSecondNoCollision(t *testing.T) {
	store := newTestStore(t)
	chat := "15550000001@s.whatsapp.net"
	same := "image_20240102_030405.jpg"
	a, b := []byte("first image"), []byte("second image")
	seedMedia(t, store, "MSGA", chat, chat, "image", same, "https://mmg.whatsapp.net/v/t62/a.enc?x=1", a)
	seedMedia(t, store, "MSGB", chat, chat, "image", same, "https://mmg.whatsapp.net/v/t62/b.enc?x=1", b)
	f := newFakeFetcher()
	f.data["/v/t62/a.enc"] = a
	f.data["/v/t62/b.enc"] = b
	m := newMediaService(f, store)

	ra, err := m.download("MSGA", chat)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := m.download("MSGB", chat)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Path == rb.Path {
		t.Fatalf("both messages saved to %s", ra.Path)
	}
	for _, c := range []struct {
		r    mediaResult
		want []byte
		name string
	}{{ra, a, "MSGA.jpg"}, {rb, b, "MSGB.jpg"}} {
		got, err := os.ReadFile(c.r.Path)
		if err != nil || string(got) != string(c.want) {
			t.Errorf("%s holds %q (%v), want %q", c.r.Path, got, err, c.want)
		}
		if c.r.Filename != c.name || c.r.Path != filepath.Join(realStoreDir(t, store), chat, c.name) {
			t.Errorf("result %+v, want %s in the chat dir", c.r, c.name)
		}
		if c.r.OriginalFilename != same {
			t.Errorf("original filename %q, want %q", c.r.OriginalFilename, same)
		}
	}
	// A second request is served from disk.
	n := f.downloadCount()
	if _, err := m.download("MSGA", chat); err != nil {
		t.Fatal(err)
	}
	if f.downloadCount() != n {
		t.Errorf("cached media downloaded again")
	}
}

func TestDownloadMediaPrefersStoredDirectPath(t *testing.T) {
	store := newTestStore(t)
	chat := "15550000001@s.whatsapp.net"
	content := []byte("img")
	seedMedia(t, store, "M1", chat, chat, "image", "image_1.jpg", "https://mmg.whatsapp.net/v/t62/from-url.enc?ccb=1", content)
	seedMedia(t, store, "M2", chat, chat, "image", "image_2.jpg", "https://mmg.whatsapp.net/v/t62/only-url.enc?ccb=1", content)
	setDirectPath(t, store, "M1", chat, "/v/t62/stored-direct?ccb=11-4&oh=abc")
	f := newFakeFetcher()
	f.data["/v/t62/stored-direct?ccb=11-4&oh=abc"] = content
	f.data["/v/t62/only-url.enc"] = content
	m := newMediaService(f, store)

	if _, err := m.download("M1", chat); err != nil {
		t.Fatal(err)
	}
	if _, err := m.download("M2", chat); err != nil {
		t.Fatal(err)
	}
	want := []string{"/v/t62/stored-direct?ccb=11-4&oh=abc", "/v/t62/only-url.enc"}
	if strings.Join(f.downloads, " ") != strings.Join(want, " ") {
		t.Errorf("downloaded %v, want %v (stored direct path first, URL-derived only as fallback)", f.downloads, want)
	}
}

// A file saved under the old time-based name is only trusted when its
// SHA-256 matches the message (the name may belong to another message).
func TestDownloadMediaLegacyFileCheckedBySHA(t *testing.T) {
	store := newTestStore(t)
	chat := "15550000001@s.whatsapp.net"
	legacy := "image_20240102_030405.jpg"
	mine, other := []byte("mine"), []byte("someone else's")
	seedMedia(t, store, "OK1", chat, chat, "image", legacy, "https://mmg.whatsapp.net/v/t62/ok1.enc", mine)
	chatDir := filepath.Join(store.dir, chat)
	if err := os.MkdirAll(chatDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chatDir, legacy), mine, 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFakeFetcher()
	m := newMediaService(f, store)
	r, err := m.download("OK1", chat)
	if err != nil || r.Filename != legacy {
		t.Fatalf("matching legacy file: %+v %v, want it reused", r, err)
	}

	seedMedia(t, store, "BAD1", chat, chat, "image", "image_20240102_030406.jpg", "https://mmg.whatsapp.net/v/t62/bad1.enc", mine)
	if err := os.WriteFile(filepath.Join(chatDir, "image_20240102_030406.jpg"), other, 0o600); err != nil {
		t.Fatal(err)
	}
	f.data["/v/t62/bad1.enc"] = mine
	r, err = m.download("BAD1", chat)
	if err != nil {
		t.Fatal(err)
	}
	if r.Filename != "BAD1.jpg" {
		t.Errorf("mismatching legacy file was used: %+v", r)
	}
	if got, _ := os.ReadFile(r.Path); string(got) != string(mine) {
		t.Errorf("downloaded content %q, want %q", got, mine)
	}
}

// Media downloaded under the new name before a chat merge is found in the old
// phone-JID folder too.
func TestDownloadMediaMergedFolderByMessageID(t *testing.T) {
	store := newTestStore(t)
	chat := "100000000000003@lid"
	pnChat := "15550000003@s.whatsapp.net"
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := store.StoreChat(chat, "Carol Example", ts); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreMessageWithAlt("m9", chat, chat, pnChat, "", ts, false, "image", "image_1.jpg", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(store.dir, pnChat)
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "m9.jpg"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := newMediaService(nil, store).download("m9", chat)
	if err != nil || r.Path != filepath.Join(realStoreDir(t, store), pnChat, "m9.jpg") {
		t.Errorf("got %+v %v, want the ID-named file in the old folder", r, err)
	}
}

// --- direct paths are stored ------------------------------------------------

func TestMediaDirectPath(t *testing.T) {
	msg := &waProto.Message{ImageMessage: &waProto.ImageMessage{
		URL: proto.String("https://mmg.whatsapp.net/u"), DirectPath: proto.String("/v/t62/dp"),
	}}
	if url, dp := mediaDirectPath(msg); url != "https://mmg.whatsapp.net/u" || dp != "/v/t62/dp" {
		t.Errorf("image: %q %q", url, dp)
	}
	doc := &waProto.Message{DocumentMessage: &waProto.DocumentMessage{DirectPath: proto.String("/d")}}
	if _, dp := mediaDirectPath(doc); dp != "/d" {
		t.Errorf("document direct path %q", dp)
	}
	if url, dp := mediaDirectPath(&waProto.Message{Conversation: proto.String("hi")}); url != "" || dp != "" {
		t.Errorf("text message: %q %q", url, dp)
	}
	if _, dp := mediaDirectPath(nil); dp != "" {
		t.Errorf("nil message: %q", dp)
	}
}

func directPathOf(t *testing.T, store *MessageStore, id string) string {
	t.Helper()
	var dp sql.NullString
	if err := store.db.QueryRow("SELECT direct_path FROM messages WHERE id = ?", id).Scan(&dp); err != nil {
		t.Fatal(err)
	}
	return dp.String
}

func TestLiveAndHistoryMediaStoreDirectPath(t *testing.T) {
	setDebug(t, false)
	store := newTestStore(t)
	b := newBridgeEvents(nil, store, waLog.Stdout("Test", "ERROR", false))
	b.markReady()

	live := liveMessageEvent(t, store)
	live.Info.ID = "LIVEIMG1"
	live.Message = &waProto.Message{ImageMessage: &waProto.ImageMessage{
		URL: proto.String("https://mmg.whatsapp.net/v/t62/live.enc?x"), DirectPath: proto.String("/v/t62/live-dp?ccb=1"),
		MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), FileLength: proto.Uint64(3),
	}}
	if !b.handle(live) {
		t.Fatal("live media message not stored")
	}
	if got := directPathOf(t, store, "LIVEIMG1"); got != "/v/t62/live-dp?ccb=1" {
		t.Errorf("live direct_path = %q", got)
	}

	chat := "15550000002@s.whatsapp.net"
	if err := store.StoreChat(chat, "Bob Example", time.Now()); err != nil {
		t.Fatal(err)
	}
	hist := &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{
		ID: proto.String(chat),
		Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
			Key: &waCommon.MessageKey{ID: proto.String("HISTIMG1"), FromMe: proto.Bool(false), RemoteJID: proto.String(chat)},
			Message: &waProto.Message{VideoMessage: &waProto.VideoMessage{
				URL: proto.String("https://mmg.whatsapp.net/v/t62/h.enc"), DirectPath: proto.String("/v/t62/hist-dp"),
			}},
			MessageTimestamp: proto.Uint64(1704164645),
		}}},
	}}}}
	captureStdout(t, func() { b.handle(hist) })
	if got := directPathOf(t, store, "HISTIMG1"); got != "/v/t62/hist-dp" {
		t.Errorf("history direct_path = %q", got)
	}
}

// Until the identity migrations have run (right after pairing), message
// events wait: the handler must not write to a schema that isn't current.
func TestBridgeEventsWaitUntilReady(t *testing.T) {
	setDebug(t, false)
	store := newTestStore(t)
	b := newBridgeEvents(nil, store, waLog.Stdout("Test", "ERROR", false))
	evt := liveMessageEvent(t, store)
	done := make(chan bool, 1)
	go func() { done <- b.handle(evt) }()
	select {
	case <-done:
		t.Fatal("message handled before the store was ready")
	case <-time.After(100 * time.Millisecond):
	}
	b.markReady()
	b.markReady() // idempotent
	select {
	case ok := <-done:
		if !ok {
			t.Error("message not stored after ready")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler still blocked after markReady")
	}
}

// --- media retry ------------------------------------------------------------

func TestDownloadMediaExpiredRequestsRetry(t *testing.T) {
	for _, dlErr := range []error{whatsmeow.ErrMediaDownloadFailedWith404, whatsmeow.ErrMediaDownloadFailedWith410} {
		t.Run(dlErr.Error(), func(t *testing.T) {
			store := newTestStore(t)
			chat := "120363000000000001@g.us"
			sender := "100000000000009@lid"
			content := []byte("old photo")
			seedMedia(t, store, "OLD1", chat, sender, "image", "image_1.jpg", "https://mmg.whatsapp.net/v/t62/old.enc", content)
			f := newFakeFetcher()
			f.errs["/v/t62/old.enc"] = fmt.Errorf("wrapped: %w", dlErr)
			m := newMediaService(f, store)

			_, err := m.download("OLD1", chat)
			if !errors.Is(err, errMediaRetryRequested) {
				t.Fatalf("err = %v, want errMediaRetryRequested", err)
			}
			if !strings.Contains(err.Error(), "try again") {
				t.Errorf("error %q should tell the caller to try again shortly", err)
			}
			if len(f.receipts) != 1 {
				t.Fatalf("sent %d retry receipts, want 1", len(f.receipts))
			}
			info := f.receipts[0]
			if info.ID != "OLD1" || info.Chat.String() != chat || info.Sender.String() != sender || !info.IsGroup || info.IsFromMe {
				t.Errorf("retry receipt for %+v", info)
			}
			if string(f.keys[0]) != "media-key-OLD1" {
				t.Errorf("retry receipt media key %q", f.keys[0])
			}

			// Asking again while the retry is pending doesn't send another receipt.
			if _, err := m.download("OLD1", chat); !errors.Is(err, errMediaRetryRequested) {
				t.Errorf("second call err = %v", err)
			}
			if len(f.receipts) != 1 {
				t.Errorf("sent %d receipts, want still 1", len(f.receipts))
			}

			// The phone re-uploads; the retry event carries the new path.
			f.data["/v/t62/new-path?ccb=9"] = content
			m.decrypt = func(evt *events.MediaRetry, key []byte) (*waMmsRetry.MediaRetryNotification, error) {
				if evt.MessageID != "OLD1" || string(key) != "media-key-OLD1" {
					return nil, fmt.Errorf("unexpected decrypt %s %q", evt.MessageID, key)
				}
				return &waMmsRetry.MediaRetryNotification{
					StanzaID:   proto.String("OLD1"),
					DirectPath: proto.String("/v/t62/new-path?ccb=9"),
					Result:     waMmsRetry.MediaRetryNotification_SUCCESS.Enum(),
				}, nil
			}
			m.handleRetry(&events.MediaRetry{MessageID: "OLD1", ChatID: types.NewJID("120363000000000001", types.GroupServer)})

			if got := directPathOf(t, store, "OLD1"); got != "/v/t62/new-path?ccb=9" {
				t.Errorf("direct_path after retry = %q", got)
			}
			want := filepath.Join(realStoreDir(t, store), chat, "OLD1.jpg")
			if got, err := os.ReadFile(want); err != nil || string(got) != string(content) {
				t.Errorf("retry did not complete the download to %s: %q %v", want, got, err)
			}
			n := f.downloadCount()
			r, err := m.download("OLD1", chat)
			if err != nil || r.Path != want || f.downloadCount() != n {
				t.Errorf("after retry: %+v %v (downloads %d -> %d)", r, err, n, f.downloadCount())
			}
		})
	}
}

func TestMediaRetryFailureIsReported(t *testing.T) {
	store := newTestStore(t)
	chat := "15550000001@s.whatsapp.net"
	seedMedia(t, store, "GONE1", chat, chat, "image", "image_1.jpg", "https://mmg.whatsapp.net/v/t62/gone.enc", []byte("x"))
	f := newFakeFetcher()
	f.errs["/v/t62/gone.enc"] = whatsmeow.ErrMediaDownloadFailedWith404
	m := newMediaService(f, store)
	if _, err := m.download("GONE1", chat); !errors.Is(err, errMediaRetryRequested) {
		t.Fatalf("err = %v", err)
	}
	m.decrypt = func(*events.MediaRetry, []byte) (*waMmsRetry.MediaRetryNotification, error) {
		return nil, whatsmeow.ErrMediaNotAvailableOnPhone
	}
	m.handleRetry(&events.MediaRetry{MessageID: "GONE1"})

	_, err := m.download("GONE1", chat)
	if err == nil || errors.Is(err, errMediaRetryRequested) || !strings.Contains(err.Error(), "no longer available") {
		t.Errorf("after a failed retry err = %v, want the phone's answer", err)
	}
	if len(f.receipts) != 1 {
		t.Errorf("reported failure should not send a new receipt (sent %d)", len(f.receipts))
	}
	// The next request tries again from scratch.
	if _, err := m.download("GONE1", chat); !errors.Is(err, errMediaRetryRequested) || len(f.receipts) != 2 {
		t.Errorf("new attempt: err %v, receipts %d", err, len(f.receipts))
	}
}

func TestMediaRetryForUnknownMessageIgnored(t *testing.T) {
	store := newTestStore(t)
	f := newFakeFetcher()
	m := newMediaService(f, store)
	called := false
	m.decrypt = func(*events.MediaRetry, []byte) (*waMmsRetry.MediaRetryNotification, error) {
		called = true
		return nil, errors.New("unexpected")
	}
	m.handleRetry(&events.MediaRetry{MessageID: "NOT-PENDING"})
	if called || f.downloadCount() != 0 {
		t.Errorf("retry for a message we never asked about was processed")
	}
}

func TestDownloadMediaWithoutClient(t *testing.T) {
	store := newTestStore(t)
	chat := "15550000001@s.whatsapp.net"
	seedMedia(t, store, "NC1", chat, chat, "image", "image_1.jpg", "https://mmg.whatsapp.net/v/t62/nc.enc", []byte("x"))
	if _, err := newMediaService(nil, store).download("NC1", chat); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("err = %v, want a not-connected error", err)
	}
}

// --- API ----------------------------------------------------------------------

func TestAPIDownloadReportsRetryAndOriginalName(t *testing.T) {
	chat := "15550000077@s.whatsapp.net"
	content := []byte("doc body")
	seedMedia(t, apiStore, "APIDOC1", chat, chat, "document", "Holiday Plan.pdf", "https://mmg.whatsapp.net/v/t62/apidoc.enc", content)
	seedMedia(t, apiStore, "APIOLD1", chat, chat, "image", "image_1.jpg", "https://mmg.whatsapp.net/v/t62/apiold.enc", content)
	f := newFakeFetcher()
	f.data["/v/t62/apidoc.enc"] = content
	f.errs["/v/t62/apiold.enc"] = whatsmeow.ErrMediaDownloadFailedWith410
	s, _ := newTestAPIServer(t)
	s.media = newMediaService(f, apiStore)
	h := s.handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, validRequest(http.MethodPost, "/api/download", `{"message_id":"APIDOC1","chat_jid":"`+chat+`"}`))
	var ok DownloadMediaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("-> %d %s", rec.Code, rec.Body)
	}
	if !ok.Success || ok.Filename != "APIDOC1.pdf" || ok.OriginalFilename != "Holiday Plan.pdf" {
		t.Errorf("response %+v", ok)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, validRequest(http.MethodPost, "/api/download", `{"message_id":"APIOLD1","chat_jid":"`+chat+`"}`))
	var retry DownloadMediaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &retry); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusAccepted || retry.Success || !retry.RetryRequested || !strings.Contains(retry.Message, "try again") {
		t.Errorf("expired media -> %d %+v, want 202 with retry_requested", rec.Code, retry)
	}
}

// --- migration 6 ----------------------------------------------------------------

func TestMigration6AddsDirectPath(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	if got := appliedVersions(t, store); !contains(got, "6") {
		t.Errorf("schema_version = %v, want 6 applied", got)
	}
	if _, err := store.db.Exec("SELECT direct_path FROM messages"); err != nil {
		t.Errorf("direct_path column: %v", err)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func migrationsUpTo(v int) []migration {
	var out []migration
	for _, m := range migrations {
		if m.version <= v {
			out = append(out, m)
		}
	}
	return out
}

// A database at version 4 (the last release before this change) upgrades to 6
// even though this branch has no migration 5, keeps its data, and backs up.
func TestMigration4To6(t *testing.T) {
	dir := legacyDB(t, legacyTimestamps)
	orig := migrations
	t.Cleanup(func() { migrations = orig })

	migrations = migrationsUpTo(4)
	store, err := NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	store.Close()
	before := len(backups(t, dir))

	migrations = orig
	store, err = NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	got := appliedVersions(t, store)
	if !contains(got, "4") || !contains(got, "6") {
		t.Errorf("schema_version = %v, want 4 and 6", got)
	}
	if _, err := store.db.Exec("SELECT direct_path FROM messages"); err != nil {
		t.Errorf("direct_path column: %v", err)
	}
	if n := len(queryStrings(t, store.db, "SELECT id FROM messages")); n != 3 {
		t.Errorf("%d messages after upgrade, want 3", n)
	}
	if after := len(backups(t, dir)); after != before+1 {
		t.Errorf("backups %d -> %d, want one more before migrating 4 -> 6", before, after)
	}

	// Idempotent: reopening applies nothing, and the migration tolerates an
	// existing column.
	store.Close()
	store, err = NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	if b := len(backups(t, dir)); b != before+1 {
		t.Errorf("reopen made a backup (%d)", b)
	}
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var rep migrationReport
	if err := migrateDirectPath(tx, nil, &rep); err != nil {
		t.Errorf("migration 6 on a database that has the column: %v", err)
	}
}

// Migrations are applied by version, not by the highest version seen: a
// migration merged later with a lower number (5) still runs on a database
// already at 6.
func TestMigrationGapIsFilledLater(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	store.Close()

	orig := migrations
	t.Cleanup(func() { migrations = orig })
	ran := false
	five := migration{version: 5, name: "late_five", needsIdentity: true, run: func(tx *sql.Tx, _ *Identity, _ *migrationReport) error {
		ran = true
		return nil
	}}
	var withFive []migration
	for _, m := range orig {
		if m.version == 6 {
			withFive = append(withFive, five)
		}
		withFive = append(withFive, m)
	}
	migrations = withFive

	store, err = NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if pending, err := store.PendingIdentityMigrations(); err != nil || !pending {
		t.Errorf("PendingIdentityMigrations = %v, %v; want true (5 not applied)", pending, err)
	}
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	if !ran || !contains(appliedVersions(t, store), "5") {
		t.Errorf("migration 5 not applied on a database at 6 (ran=%v, versions %v)", ran, appliedVersions(t, store))
	}
}
