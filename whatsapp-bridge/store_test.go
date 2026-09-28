package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testChatJID = "15550000001@s.whatsapp.net"

func TestNewMessageStoreAtCreatesDB(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	store, err := NewMessageStoreAt(dir)
	if err != nil {
		t.Fatalf("NewMessageStoreAt: %v", err)
	}
	defer store.Close()
	if _, err := os.Stat(filepath.Join(dir, "messages.db")); err != nil {
		t.Fatalf("messages.db not created: %v", err)
	}
}

func TestStoreChatAndMessages(t *testing.T) {
	store := newTestStore(t)
	t0 := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

	if err := store.StoreChat(testChatJID, "Test Contact", t0); err != nil {
		t.Fatalf("StoreChat: %v", err)
	}
	if err := store.StoreMessage("m1", testChatJID, "15550000001", "first", t0, false,
		"", "", "", nil, nil, nil, 0); err != nil {
		t.Fatalf("StoreMessage m1: %v", err)
	}
	if err := store.StoreMessage("m2", testChatJID, "15550000002", "", t0.Add(time.Minute), true,
		"image", "image_1.jpg", "https://mmg.whatsapp.net/x", []byte{1}, []byte{2}, []byte{3}, 42); err != nil {
		t.Fatalf("StoreMessage m2: %v", err)
	}
	// No content and no media: silently skipped.
	if err := store.StoreMessage("m3", testChatJID, "15550000001", "", t0, false,
		"", "", "", nil, nil, nil, 0); err != nil {
		t.Fatalf("StoreMessage m3: %v", err)
	}

	msgs, err := store.GetMessages(testChatJID, 10)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].MediaType != "image" || !msgs[0].IsFromMe {
		t.Errorf("newest message = %+v, want the image from me", msgs[0])
	}
	if msgs[1].Content != "first" {
		t.Errorf("oldest message content = %q, want %q", msgs[1].Content, "first")
	}

	chats, err := store.GetChats()
	if err != nil {
		t.Fatalf("GetChats: %v", err)
	}
	if _, ok := chats[testChatJID]; !ok || len(chats) != 1 {
		t.Errorf("GetChats = %v, want one entry for %s", chats, testChatJID)
	}

	mediaType, filename, url, key, _, _, length, err := store.GetMediaInfo("m2", testChatJID)
	if err != nil {
		t.Fatalf("GetMediaInfo: %v", err)
	}
	if mediaType != "image" || filename != "image_1.jpg" || url != "https://mmg.whatsapp.net/x" || len(key) != 1 || length != 42 {
		t.Errorf("GetMediaInfo = %q %q %q %v %d", mediaType, filename, url, key, length)
	}

	if err := store.StoreMediaInfo("m2", testChatJID, "https://mmg.whatsapp.net/y", []byte{9}, []byte{8}, []byte{7}, 7); err != nil {
		t.Fatalf("StoreMediaInfo: %v", err)
	}
	_, _, url, _, _, _, length, err = store.GetMediaInfo("m2", testChatJID)
	if err != nil || url != "https://mmg.whatsapp.net/y" || length != 7 {
		t.Errorf("after StoreMediaInfo: url=%q length=%d err=%v", url, length, err)
	}
}

func TestStoreMessageRequiresChat(t *testing.T) {
	store := newTestStore(t)
	// messages.chat_jid has a foreign key to chats(jid), enforced via _foreign_keys=on.
	err := store.StoreMessage("m1", "15550009999@s.whatsapp.net", "15550009999", "orphan", time.Now(), false,
		"", "", "", nil, nil, nil, 0)
	if err == nil {
		t.Fatal("StoreMessage for unknown chat succeeded, want foreign key error")
	}
}
