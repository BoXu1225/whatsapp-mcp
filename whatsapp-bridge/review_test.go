package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// Review items on PR #34, after #33 (message capture) was merged.

// --- stickers download (#33 + #19) -----------------------------------------

// typeRecorder wraps fakeFetcher and records the whatsmeow media type asked for.
type typeRecorder struct {
	*fakeFetcher
	types []whatsmeow.MediaType
}

func (r *typeRecorder) Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error) {
	if mt, ok := msg.(whatsmeow.MediaTypeable); ok {
		r.types = append(r.types, mt.GetMediaType())
	}
	return r.fakeFetcher.Download(ctx, msg)
}

func TestStickerDownload(t *testing.T) {
	if got, _ := mediaFileName("STK1", "sticker_20240102_030405.webp", "sticker"); got != "STK1.webp" {
		t.Errorf("sticker file name %q, want STK1.webp", got)
	}
	if got, _ := mediaFileName("STK1", "", "sticker"); got != "STK1.webp" {
		t.Errorf("sticker without stored name: %q, want STK1.webp", got)
	}
	msg := &waE2E.Message{StickerMessage: &waE2E.StickerMessage{
		URL: proto.String("https://mmg.whatsapp.net/v/t62/s.enc"), DirectPath: proto.String("/v/t62/sticker-dp"),
	}}
	if url, dp := mediaDirectPath(msg); url != "https://mmg.whatsapp.net/v/t62/s.enc" || dp != "/v/t62/sticker-dp" {
		t.Errorf("sticker direct path: %q %q", url, dp)
	}

	store := newMigratedTestStore(t)
	chat := "15550000001@s.whatsapp.net"
	content := []byte("RIFF....WEBP")
	seedMedia(t, store, "STK1", chat, chat, "sticker", "sticker_20240102_030405.webp", "https://mmg.whatsapp.net/v/t62/s.enc", content)
	f := &typeRecorder{fakeFetcher: newFakeFetcher()}
	f.data["/v/t62/s.enc"] = content
	r, err := newMediaService(f, store).download("STK1", chat)
	if err != nil {
		t.Fatalf("sticker download: %v", err)
	}
	if r.Filename != "STK1.webp" || len(f.types) != 1 || f.types[0] != whatsmeow.MediaImage {
		t.Errorf("result %+v, media types %v; want STK1.webp downloaded as %q", r, f.types, whatsmeow.MediaImage)
	}
}

// --- sanitised message IDs don't collide -------------------------------------

func TestMediaFileNameDistinguishesSanitisedIDs(t *testing.T) {
	plain, _ := mediaFileName("A_", "a.jpg", "image")
	bang, _ := mediaFileName("A!", "a.jpg", "image")
	if plain != "A_.jpg" {
		t.Errorf("clean ID changed: %q", plain)
	}
	if bang == plain || !strings.HasPrefix(bang, "A_-") || !strings.HasSuffix(bang, ".jpg") {
		t.Errorf("mediaFileName(A!) = %q, want A_-<hash>.jpg distinct from %q", bang, plain)
	}
	again, _ := mediaFileName("A!", "b.jpg", "image")
	if again != bang {
		t.Errorf("not stable: %q vs %q", again, bang)
	}
	if _, ok := safeMediaFilename(bang); !ok {
		t.Errorf("%q is not a safe file name", bang)
	}
}

// --- direct_path and status@broadcast in #33's store path ---------------------

func TestProcessMessageStoresDirectPathLiveAndHistory(t *testing.T) {
	// Not logged in yet: migration 6 waits, but its column is added at open.
	store := newTestStore(t)
	img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		URL: proto.String("https://mmg.whatsapp.net/v/t62/l.enc"), DirectPath: proto.String("/v/t62/live-dp"),
		MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), FileLength: proto.Uint64(3),
	}}
	storeLive(t, store, danLive("DPLIVE", t0, img))
	if got := directPathOf(t, store, "DPLIVE"); got != "/v/t62/live-dp" {
		t.Errorf("live direct_path = %q", got)
	}

	// History goes through storeHistorySync (one transaction per batch)
	// without the event dispatcher.
	vid := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
		URL: proto.String("https://mmg.whatsapp.net/v/t62/h.enc"), DirectPath: proto.String("/v/t62/hist-dp"),
	}}
	captureStdout(t, func() {
		storeHistorySync(store, Identity{}, histConv(danChat, 1704200000, histMsg(danChat, "DPHIST", false, 1704200000, vid)), fixedName("Dan"), quietLogger())
	})
	if got := directPathOf(t, store, "DPHIST"); got != "/v/t62/hist-dp" {
		t.Errorf("history direct_path = %q", got)
	}
}

func TestStatusBroadcastSkippedInStorePath(t *testing.T) {
	store := newTestStore(t)
	status := types.StatusBroadcastJID.String()
	evt := danLive("STATUS9", t0, text("my status"))
	evt.Info.Chat = types.StatusBroadcastJID
	if _, err := processMessage(store, evt, captureTarget{chat: types.StatusBroadcastJID, sender: danChat}); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		storeHistorySync(store, Identity{}, histConv(status, 1704200000, histMsg(status, "STATUS10", false, 1704200000, text("old status"))), fixedName("Status"), quietLogger())
	})
	if n := len(queryStrings(t, store.db, "SELECT id FROM messages WHERE chat_jid = ?", status)); n != 0 {
		t.Errorf("%d status messages stored", n)
	}
	if n := len(queryStrings(t, store.db, "SELECT jid FROM chats WHERE jid = ?", status)); n != 0 {
		t.Errorf("status chat stored")
	}
}

// --- migration 5 early schema checks "5 recorded" -----------------------------

// A database that recorded 6 before migration 5 existed, still waiting for
// the identity migrations: 5's columns must be added at open anyway.
func TestCaptureSchemaEarlyWhen6RecordedBut5Not(t *testing.T) {
	dir := legacyDB(t, "")
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL);
		INSERT INTO schema_version VALUES (1, 'utc_timestamps', 'x'), (2, 'sender_alt_column', 'x'), (6, 'direct_path_drop_status', 'x');
		ALTER TABLE messages ADD COLUMN sender_alt TEXT;
		ALTER TABLE messages ADD COLUMN direct_path TEXT;`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertCaptureSchema(t, store.db)
}

// --- backups name the version the database was at before the migration run ---

func TestGapFillBackupNamedAfterMigrationRun(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreChat(testChatJID, "x", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("DELETE FROM schema_version WHERE version = 5"); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b := backups(t, dir)
	if len(b) != 1 || !strings.Contains(filepath.Base(b[0]), "bak-4-") {
		t.Errorf("backups %v, want one named bak-4-* (before migration 5), not after the highest applied (6)", b)
	}
	if pending, err := store.PendingMigrations(); err != nil || pending {
		t.Errorf("PendingMigrations = %v, %v after all ran", pending, err)
	}
}

// --- group name lookups don't serialise on one lock -------------------------------

func TestGroupNameLookupsRunConcurrently(t *testing.T) {
	c := newGroupNameCache()
	g1 := types.NewJID("120363000000000011", types.GroupServer)
	g2 := types.NewJID("120363000000000012", types.GroupServer)
	started2 := make(chan struct{})
	var mu sync.Mutex
	calls := map[types.JID]int{}
	c.fetch = func(_ *whatsmeow.Client, jid types.JID) (string, error) {
		mu.Lock()
		calls[jid]++
		mu.Unlock()
		if jid == g1 {
			// g1's lookup is slow: it waits for g2's to start.
			select {
			case <-started2:
			case <-time.After(2 * time.Second):
			}
			return "One", nil
		}
		close(started2)
		return "Two", nil
	}
	var wg sync.WaitGroup
	results := make([]string, 3)
	wg.Add(1)
	go func() { defer wg.Done(); results[0], _ = c.lookup(nil, g1) }()
	wg.Add(1)
	go func() { defer wg.Done(); results[2], _ = c.lookup(nil, g1) }() // same group: shares the call
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	results[1], _ = c.lookup(nil, g2)
	if time.Since(start) > time.Second {
		t.Errorf("lookup of another group waited %v for a slow one", time.Since(start))
	}
	wg.Wait()
	if results[0] != "One" || results[1] != "Two" || results[2] != "One" {
		t.Errorf("results %v", results)
	}
	if calls[g1] != 1 || calls[g2] != 1 {
		t.Errorf("fetch calls %v, want one per group", calls)
	}
}
