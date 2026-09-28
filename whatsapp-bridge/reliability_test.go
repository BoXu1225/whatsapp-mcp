package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// --- #18: WAL, busy timeout, no ack on a failed store -----------------------

func TestMessageStoreOpensWithWALAndBusyTimeout(t *testing.T) {
	store := newTestStore(t)
	// Every pooled connection gets the DSN pragmas; check a few at once.
	store.db.SetMaxOpenConns(3)
	for i := 0; i < 3; i++ {
		conn, err := store.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var mode string
		var timeout, fk int
		if err := conn.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&timeout); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if mode != "wal" || timeout != 5000 || fk != 1 {
			t.Errorf("conn %d: journal_mode=%q busy_timeout=%d foreign_keys=%d, want wal/5000/1", i, mode, timeout, fk)
		}
	}
}

// failInserts makes every insert into messages fail, like a locked or full DB.
func failInserts(t *testing.T, store *MessageStore) {
	t.Helper()
	if _, err := store.db.Exec(`CREATE TRIGGER fail_inserts BEFORE INSERT ON messages
		BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`); err != nil {
		t.Fatal(err)
	}
}

func TestHandleMessageReportsStoreResult(t *testing.T) {
	setDebug(t, false)
	logger := waLog.Stdout("Test", "ERROR", false)

	store := newTestStore(t)
	evt := liveMessageEvent(t, store)
	if ok := handleMessage(nil, store, evt, logger); !ok {
		t.Errorf("stored message: handleMessage = false, want true")
	}

	// Nothing to store (no text, no media) is not a failure.
	empty := liveMessageEvent(t, store)
	empty.Info.ID = "EMPTY1"
	empty.Message.Conversation = nil
	if ok := handleMessage(nil, store, empty, logger); !ok {
		t.Errorf("empty message: handleMessage = false, want true")
	}

	failing := newTestStore(t)
	evt = liveMessageEvent(t, failing)
	failInserts(t, failing)
	if ok := handleMessage(nil, failing, evt, logger); ok {
		t.Errorf("failed store: handleMessage = true, want false")
	}
}

// The registered handler must return false for a message it failed to store:
// whatsmeow then skips the delivery receipt, and with the decrypted event
// buffer on, replays the message when the server redelivers it.
func TestEventHandlerDoesNotAckFailedStore(t *testing.T) {
	setDebug(t, false)
	logger := waLog.Stdout("Test", "ERROR", false)

	store := newTestStore(t)
	b := newBridgeEvents(nil, store, logger)
	if ok := b.handle(liveMessageEvent(t, store)); !ok {
		t.Errorf("stored message: handler returned false")
	}

	failing := newTestStore(t)
	evt := liveMessageEvent(t, failing)
	failInserts(t, failing)
	b = newBridgeEvents(nil, failing, logger)
	if ok := b.handle(evt); ok {
		t.Errorf("failed store: handler returned true, want false so WhatsApp isn't acked")
	}
	// Other events are always acknowledged.
	if ok := b.handle(&events.Receipt{}); !ok {
		t.Errorf("receipt: handler returned false")
	}
}

func testClient(t *testing.T) *whatsmeow.Client {
	t.Helper()
	container, err := sqlstore.New(context.Background(), "sqlite3",
		"file:"+filepath.Join(t.TempDir(), "whatsapp.db")+"?_foreign_keys=on", waLog.Noop)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { container.Close() })
	return whatsmeow.NewClient(container.NewDevice(), waLog.Noop)
}

func TestConfigureClient(t *testing.T) {
	client := testClient(t)
	configureClient(client)
	if !client.EnableDecryptedEventBuffer {
		t.Error("EnableDecryptedEventBuffer is off: a message whose store failed could not be replayed on redelivery")
	}
	if !client.InitialAutoReconnect || !client.EnableAutoReconnect {
		t.Error("a transient network error on the first connect should be retried in the background")
	}
}

var _ = time.Second
