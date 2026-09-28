package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// --- #21: lifecycle ------------------------------------------------------------

func readyEvents(t *testing.T, store *MessageStore) *bridgeEvents {
	t.Helper()
	b := newBridgeEvents(nil, store, waLog.Stdout("Test", "ERROR", false))
	b.markReady()
	return b
}

func TestLoggedOutIsFatalWithRepairHint(t *testing.T) {
	store := newTestStore(t)
	b := readyEvents(t, store)
	out := captureStdout(t, func() {
		if !b.handle(&events.LoggedOut{OnConnect: true, Reason: events.ConnectFailureLoggedOut}) {
			t.Error("LoggedOut handler returned false")
		}
	})
	select {
	case err := <-b.fatal:
		if !errors.Is(err, errLoggedOut) {
			t.Errorf("fatal error %v, want errLoggedOut", err)
		}
	case <-time.After(time.Second):
		t.Fatal("LoggedOut did not signal a fatal error")
	}
	if !strings.Contains(out, "scripts/bridge.sh fg") {
		t.Errorf("log should say how to re-pair:\n%s", out)
	}
	// A second logout event doesn't block the event loop.
	done := make(chan struct{})
	go func() { b.handle(&events.LoggedOut{}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second LoggedOut blocked")
	}
}

func TestConnectedSignalsOnce(t *testing.T) {
	store := newTestStore(t)
	b := readyEvents(t, store)
	b.handle(&events.Connected{})
	b.handle(&events.Connected{}) // no panic on a second close
	select {
	case <-b.connected:
	default:
		t.Fatal("connected not signalled")
	}
}

func TestWaitForConnection(t *testing.T) {
	connected := make(chan struct{})
	close(connected)
	if err := waitForConnection(connected, make(chan error), time.Second); err != nil {
		t.Errorf("connected: %v", err)
	}

	fatal := make(chan error, 1)
	fatal <- errLoggedOut
	if err := waitForConnection(make(chan struct{}), fatal, time.Second); !errors.Is(err, errLoggedOut) {
		t.Errorf("logged out while waiting: %v", err)
	}

	start := time.Now()
	err := waitForConnection(make(chan struct{}), make(chan error), 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("timeout: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("waited %v", time.Since(start))
	}
}

// --- status broadcasts -------------------------------------------------------

func TestStatusBroadcastIsSkipped(t *testing.T) {
	setDebug(t, false)
	store := newTestStore(t)
	b := readyEvents(t, store)
	status := types.StatusBroadcastJID
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: status, Sender: types.NewJID("15550000001", types.DefaultUserServer)},
			ID:            "STATUS1",
			Timestamp:     time.Now(),
		},
		Message: &waProto.Message{Conversation: proto.String("my status")},
	}
	if !b.handle(evt) {
		t.Error("status message not acknowledged")
	}

	hist := &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{
		ID: proto.String(status.String()),
		Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{ID: proto.String("STATUS2"), RemoteJID: proto.String(status.String()), Participant: proto.String("15550000001@s.whatsapp.net")},
			Message:          &waProto.Message{Conversation: proto.String("old status")},
			MessageTimestamp: proto.Uint64(1704164645),
		}}},
	}}}}
	captureStdout(t, func() { b.handle(hist) })

	// Direct store calls are no-ops too.
	if err := store.StoreChat(status.String(), "Status", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreMessage("STATUS3", status.String(), "15550000001@s.whatsapp.net", "x", time.Now(), false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if n := len(queryStrings(t, store.db, "SELECT jid FROM chats WHERE jid = ?", status.String())); n != 0 {
		t.Errorf("status@broadcast stored as a chat")
	}
	if n := len(queryStrings(t, store.db, "SELECT id FROM messages WHERE chat_jid = ?", status.String())); n != 0 {
		t.Errorf("%d status messages stored", n)
	}
}

// Status updates stored by older versions are removed by migration 6.
func TestMigration6RemovesStatusBroadcast(t *testing.T) {
	dir := legacyDB(t, `
		INSERT INTO chats VALUES ('status@broadcast', 'status', '2024-01-02 03:04:05+00:00');
		INSERT INTO chats VALUES ('15550000001@s.whatsapp.net', 'Alice Example', '2024-01-02 03:04:05+00:00');
		INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
			VALUES ('s1', 'status@broadcast', '15550000001', 'status', '2024-01-02 03:04:05+00:00', 0);
		INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
			VALUES ('m1', '15550000001@s.whatsapp.net', '15550000001', 'hi', '2024-01-02 03:04:05+00:00', 0);`)
	store, err := NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	if got := queryStrings(t, store.db, "SELECT jid FROM chats ORDER BY jid"); strings.Join(got, ",") != "15550000001@s.whatsapp.net" {
		t.Errorf("chats after migration = %v", got)
	}
	if got := queryStrings(t, store.db, "SELECT id FROM messages ORDER BY id"); strings.Join(got, ",") != "m1" {
		t.Errorf("messages after migration = %v", got)
	}
}

// --- group names ---------------------------------------------------------------

func TestGroupNameLookedUpOncePerGroup(t *testing.T) {
	store := newTestStore(t)
	orig := groupNames
	t.Cleanup(func() { groupNames = orig })
	var mu sync.Mutex
	calls := map[string]int{}
	groupNames = newGroupNameCache()
	groupNames.fetch = func(_ *whatsmeow.Client, jid types.JID) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls[jid.User]++
		if jid.User == "120363000000000002" {
			return "", errors.New("network down")
		}
		return "Hiking Club", nil
	}
	logger := waLog.Noop
	g1 := types.NewJID("120363000000000001", types.GroupServer)
	g2 := types.NewJID("120363000000000002", types.GroupServer)
	for i := 0; i < 3; i++ {
		if name := GetChatName(nil, store, g1, g1.String(), nil, "", logger); name != "Hiking Club" {
			t.Errorf("g1 name %q", name)
		}
		if name := GetChatName(nil, store, g2, g2.String(), nil, "", logger); name != "Group 120363000000000002" {
			t.Errorf("g2 name %q, want the placeholder", name)
		}
		// Placeholder names in the DB don't stop the lookup; the cache does.
		if err := store.StoreChat(g2.String(), "Group 120363000000000002", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if calls["120363000000000001"] != 1 || calls["120363000000000002"] != 1 {
		t.Errorf("GetGroupInfo calls %v, want one per group", calls)
	}
}

func TestGroupNameFailureRetriedLater(t *testing.T) {
	c := newGroupNameCache()
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	n := 0
	c.fetch = func(*whatsmeow.Client, types.JID) (string, error) { n++; return "", errors.New("offline") }
	g := types.NewJID("120363000000000003", types.GroupServer)
	c.lookup(nil, g)
	c.lookup(nil, g)
	now = now.Add(2 * groupNameRetryAfter)
	c.lookup(nil, g)
	if n != 2 {
		t.Errorf("fetched %d times, want 2 (once, then again after the retry interval)", n)
	}
}

// --- shutdown ------------------------------------------------------------------

type orderRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (o *orderRecorder) add(s string) { o.mu.Lock(); o.steps = append(o.steps, s); o.mu.Unlock() }

type fakeHTTP struct{ o *orderRecorder }

func (f fakeHTTP) Shutdown(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		f.o.add("http-without-deadline")
	}
	f.o.add("http")
	return nil
}

type fakeWA struct{ o *orderRecorder }

func (f fakeWA) Disconnect() { f.o.add("whatsapp") }

type fakeDB struct{ o *orderRecorder }

func (f fakeDB) Close() error { f.o.add("db"); return nil }

func TestShutdownOrder(t *testing.T) {
	o := &orderRecorder{}
	shutdown(fakeHTTP{o}, fakeWA{o}, fakeDB{o}, time.Second)
	if got := strings.Join(o.steps, ","); got != "http,whatsapp,db" {
		t.Errorf("shutdown order %s, want http,whatsapp,db", got)
	}
	// Missing parts (e.g. startup failed before the server) are skipped.
	o = &orderRecorder{}
	shutdown(nil, fakeWA{o}, fakeDB{o}, time.Second)
	if got := strings.Join(o.steps, ","); got != "whatsapp,db" {
		t.Errorf("shutdown without server: %s", got)
	}
}

func TestServeRESTReportsServerErrors(t *testing.T) {
	s, _ := newTestAPIServer(t)
	ln, err := listenREST(0)
	if err != nil {
		t.Fatal(err)
	}
	srv, errs := serveREST(s, ln)
	// A clean Shutdown is not an error.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err, ok := <-errs:
		if ok && err != nil {
			t.Errorf("Shutdown reported %v", err)
		}
	case <-time.After(time.Second):
		t.Error("error channel not closed after Shutdown")
	}

	// The listener failing underneath the server is reported.
	ln, err = listenREST(0)
	if err != nil {
		t.Fatal(err)
	}
	_, errs = serveREST(s, ln)
	ln.Close()
	select {
	case err := <-errs:
		if err == nil {
			t.Error("closed listener: nil error")
		}
	case <-time.After(2 * time.Second):
		t.Error("closed listener not reported")
	}
}

func TestExitStatus(t *testing.T) {
	logger := waLog.Noop
	if got := exitStatus(logger, fmt.Errorf("%w (terminated)", errInterrupted)); got != 0 {
		t.Errorf("signal -> %d, want 0", got)
	}
	if got := exitStatus(logger, errLoggedOut); got != 1 {
		t.Errorf("logged out -> %d, want 1", got)
	}
	if got := exitStatus(logger, errors.New("not connected")); got != 1 {
		t.Errorf("connect timeout -> %d, want 1", got)
	}
}
