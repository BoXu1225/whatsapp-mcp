package main

import (
	"fmt"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// History sync (#17). All data is fake.

const danChat = danPN + "@s.whatsapp.net"

// histMsg is a history-sync message in chat from Dan (or from me).
func histMsg(chat, id string, fromMe bool, ts uint64, m *waE2E.Message) *waHistorySync.HistorySyncMsg {
	return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(fromMe), RemoteJID: proto.String(chat)},
		Message:          m,
		MessageTimestamp: proto.Uint64(ts),
	}}
}

func histConv(chat string, convTS uint64, msgs ...*waHistorySync.HistorySyncMsg) *events.HistorySync {
	conv := &waHistorySync.Conversation{ID: proto.String(chat), Messages: msgs}
	if convTS != 0 {
		conv.ConversationTimestamp = proto.Uint64(convTS)
	}
	return &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{conv}}}
}

func text(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }

func quietLogger() waLog.Logger { return waLog.Stdout("Test", "ERROR", false) }

func storedTime(t *testing.T, store *MessageStore, query string, args ...interface{}) time.Time {
	t.Helper()
	var s string
	if err := store.db.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	ts, ok := parseStoredTime(s)
	if !ok {
		t.Fatalf("unparsable stored time %q", s)
	}
	return ts
}

func contentOf(t *testing.T, store *MessageStore, id string) (string, bool) {
	t.Helper()
	var c string
	err := store.db.QueryRow("SELECT COALESCE(content, '') FROM messages WHERE id = ?", id).Scan(&c)
	if err != nil {
		return "", false
	}
	return c, true
}

// StoreChat never moves last_message_time backwards (an older history chunk
// arriving after newer messages).
func TestStoreChatKeepsLatestTime(t *testing.T) {
	store := newTestStore(t)
	newer := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	older := time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := store.StoreChat(danChat, "Dan Example", newer); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreChat(danChat, "Dan Renamed", older); err != nil {
		t.Fatal(err)
	}
	if got := storedTime(t, store, "SELECT last_message_time FROM chats WHERE jid = ?", danChat); !got.Equal(newer) {
		t.Errorf("last_message_time = %v, want the newer %v", got, newer)
	}
	var name string
	store.db.QueryRow("SELECT name FROM chats WHERE jid = ?", danChat).Scan(&name)
	if name != "Dan Renamed" {
		t.Errorf("name = %q, want the latest name", name)
	}
	later := newer.Add(time.Hour)
	if err := store.StoreChat(danChat, "Dan Renamed", later); err != nil {
		t.Fatal(err)
	}
	if got := storedTime(t, store, "SELECT last_message_time FROM chats WHERE jid = ?", danChat); !got.Equal(later) {
		t.Errorf("last_message_time = %v, want %v", got, later)
	}
}

// An older history batch after a newer one leaves the chat's time alone.
func TestHistoryOlderBatchDoesNotMoveChatTimeBack(t *testing.T) {
	store := newTestStore(t)
	storeHistorySync(store, Identity{}, histConv(danChat, 1704200000,
		histMsg(danChat, "N1", false, 1704200000, text("newer"))), fixedName("Dan Example"), quietLogger())
	storeHistorySync(store, Identity{}, histConv(danChat, 1600000000,
		histMsg(danChat, "O1", false, 1600000000, text("older"))), fixedName("Dan Example"), quietLogger())

	if got := storedTime(t, store, "SELECT last_message_time FROM chats WHERE jid = ?", danChat); !got.Equal(time.Unix(1704200000, 0)) {
		t.Errorf("last_message_time = %v, want the newer batch's time", got)
	}
	if got := chatRows(t, store); got[danChat] != 2 {
		t.Errorf("chats = %v, want both messages", got)
	}
}

// A conversation whose first (newest) message is empty or has no timestamp
// is not skipped; the chat time comes from the conversation timestamp.
func TestHistoryConversationWithEmptyFirstMessage(t *testing.T) {
	for _, tt := range []struct {
		name  string
		first *waHistorySync.HistorySyncMsg
	}{
		{"nil message", &waHistorySync.HistorySyncMsg{}},
		{"no content", histMsg(danChat, "E0", false, 1704300000, nil)},
		{"no timestamp", &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key:     &waCommon.MessageKey{ID: proto.String("E0"), RemoteJID: proto.String(danChat)},
			Message: text("no time"),
		}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			storeHistorySync(store, Identity{}, histConv(danChat, 1704300000,
				tt.first,
				histMsg(danChat, "E2", false, 1704200100, text("second")),
				histMsg(danChat, "E1", false, 1704200000, text("first")),
			), fixedName("Dan Example"), quietLogger())

			for _, id := range []string{"E1", "E2"} {
				if _, ok := contentOf(t, store, id); !ok {
					t.Errorf("message %s not stored", id)
				}
			}
			if got := storedTime(t, store, "SELECT last_message_time FROM chats WHERE jid = ?", danChat); !got.Equal(time.Unix(1704300000, 0)) {
				t.Errorf("last_message_time = %v, want the conversation timestamp", got)
			}
		})
	}
}

// Without a conversation timestamp, the newest stored message's time is used.
func TestHistoryChatTimeFallsBackToNewestMessage(t *testing.T) {
	store := newTestStore(t)
	storeHistorySync(store, Identity{}, histConv(danChat, 0,
		&waHistorySync.HistorySyncMsg{},
		histMsg(danChat, "F2", false, 1704200100, text("second")),
		histMsg(danChat, "F1", false, 1704200000, text("first")),
	), fixedName("Dan Example"), quietLogger())
	if got := storedTime(t, store, "SELECT last_message_time FROM chats WHERE jid = ?", danChat); !got.Equal(time.Unix(1704200100, 0)) {
		t.Errorf("last_message_time = %v, want the newest message's time", got)
	}
}

// Wrapped messages (ephemeral, view-once, document with caption) are
// unwrapped like live ones instead of being dropped.
func TestHistoryUnwrapsWrappedMessages(t *testing.T) {
	store := newTestStore(t)
	ephemeral := &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: text("disappearing hello")}}
	viewOnce := &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{URL: proto.String("https://mmg.whatsapp.net/v1"), FileLength: proto.Uint64(5)},
	}}}
	docCaption := &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
		DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("plan.pdf"), Caption: proto.String("the plan")},
	}}}
	ephemeralExt := &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("see https://example.com")},
	}}}
	storeHistorySync(store, Identity{}, histConv(danChat, 1704200300,
		histMsg(danChat, "W4", false, 1704200300, ephemeralExt),
		histMsg(danChat, "W3", false, 1704200200, docCaption),
		histMsg(danChat, "W2", false, 1704200100, viewOnce),
		histMsg(danChat, "W1", false, 1704200000, ephemeral),
	), fixedName("Dan Example"), quietLogger())

	want := map[string]struct{ content, mediaType string }{
		"W1": {"disappearing hello", ""},
		"W2": {"", "image"},
		"W3": {"", "document"}, // the caption: see TestCaptureMessageTypes (#15)
		"W4": {"see https://example.com", ""},
	}
	for id, w := range want {
		var content, mediaType string
		err := store.db.QueryRow("SELECT COALESCE(content, ''), COALESCE(media_type, '') FROM messages WHERE id = ?", id).Scan(&content, &mediaType)
		if err != nil {
			t.Errorf("%s: not stored (%v)", id, err)
			continue
		}
		if content != w.content || mediaType != w.mediaType {
			t.Errorf("%s = (%q, %q), want (%q, %q)", id, content, mediaType, w.content, w.mediaType)
		}
	}
}

// A history batch is written in one transaction: InTx commits everything or
// nothing.
func TestInTxCommitsOrRollsBack(t *testing.T) {
	store := newTestStore(t)
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	boom := fmt.Errorf("boom")
	err := store.InTx(func(tx *MessageStore) error {
		if err := tx.StoreChat(danChat, "Dan Example", ts); err != nil {
			return err
		}
		return boom
	})
	if err != boom {
		t.Fatalf("InTx error = %v, want boom", err)
	}
	if got := chatRows(t, store); len(got) != 0 {
		t.Errorf("rolled-back transaction left chats %v", got)
	}
	if err := store.InTx(func(tx *MessageStore) error { return tx.StoreChat(danChat, "Dan Example", ts) }); err != nil {
		t.Fatal(err)
	}
	if got := chatRows(t, store); len(got) != 1 {
		t.Errorf("committed transaction: chats %v", got)
	}
	if err := store.InTx(func(tx *MessageStore) error { return tx.MergeChat(danChat, "100000000000004@lid", Identity{}) }); err == nil {
		t.Error("MergeChat inside InTx should be refused")
	}
}
