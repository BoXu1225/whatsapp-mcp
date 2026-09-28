package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	waProto "go.mau.fi/whatsmeow/binary/proto"
)

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
// The bridge logs with fmt.Printf and waLog.Stdout, both of which write to
// os.Stdout at call time.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan []byte)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		done <- buf.Bytes()
	}()
	defer func() { os.Stdout = orig }()
	fn()
	w.Close()
	os.Stdout = orig
	return string(<-done)
}

func setDebug(t *testing.T, on bool) {
	t.Helper()
	old := debugLogging
	debugLogging = on
	t.Cleanup(func() { debugLogging = old })
}

const (
	secretText     = "SECRET-TEXT-4242"
	secretFilename = "secret-plan-4242.pdf"
	secretName     = "Alice Secretname"
)

// liveMessageEvent is an incoming document message with a caption-less text
// body, in a chat whose name is already stored (so GetChatName never needs
// the nil client).
func liveMessageEvent(t *testing.T, store *MessageStore) *events.Message {
	t.Helper()
	chat := types.JID{User: "15550000001", Server: types.DefaultUserServer}
	if err := store.StoreChat(chat.String(), secretName, time.Now()); err != nil {
		t.Fatal(err)
	}
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: chat},
			ID:            "MSGID0001",
			Timestamp:     time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		Message: &waProto.Message{Conversation: proto.String(secretText)},
	}
	return evt
}

func TestHandleMessageDoesNotLogContentAtInfo(t *testing.T) {
	setDebug(t, false)
	store := newTestStore(t)
	logger := waLog.Stdout("Test", "INFO", false)

	text := liveMessageEvent(t, store)
	doc := liveMessageEvent(t, store)
	doc.Info.ID = "MSGID0002"
	doc.Message = &waProto.Message{DocumentMessage: &waProto.DocumentMessage{FileName: proto.String(secretFilename), URL: proto.String("https://mmg.whatsapp.net/d")}}

	out := captureStdout(t, func() {
		handleMessage(nil, store, text, logger)
		handleMessage(nil, store, doc, logger)
	})
	for _, secret := range []string{secretText, secretFilename, secretName} {
		if strings.Contains(out, secret) {
			t.Errorf("INFO output contains %q:\n%s", secret, out)
		}
	}
	for _, want := range []string{"MSGID0001", "MSGID0002"} {
		if !strings.Contains(out, want) {
			t.Errorf("INFO output should still identify message %s:\n%s", want, out)
		}
	}
}

func TestHandleMessageLogsContentWithDebug(t *testing.T) {
	setDebug(t, true)
	store := newTestStore(t)
	logger := waLog.Stdout("Test", "INFO", false)
	evt := liveMessageEvent(t, store)
	out := captureStdout(t, func() { handleMessage(nil, store, evt, logger) })
	if !strings.Contains(out, secretText) {
		t.Errorf("-debug output should contain the message text:\n%s", out)
	}
}

func historyEvent(t *testing.T, store *MessageStore) *events.HistorySync {
	t.Helper()
	chat := "15550000002@s.whatsapp.net"
	if err := store.StoreChat(chat, secretName, time.Now()); err != nil {
		t.Fatal(err)
	}
	msg := func(id string, m *waProto.Message) *waHistorySync.HistorySyncMsg {
		return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(false), RemoteJID: proto.String(chat)},
			Message:          m,
			MessageTimestamp: proto.Uint64(1704164645),
		}}
	}
	return &events.HistorySync{Data: &waHistorySync.HistorySync{
		Conversations: []*waHistorySync.Conversation{{
			ID: proto.String(chat),
			Messages: []*waHistorySync.HistorySyncMsg{
				msg("HIST0001", &waProto.Message{Conversation: proto.String(secretText)}),
				msg("HIST0002", &waProto.Message{DocumentMessage: &waProto.DocumentMessage{FileName: proto.String(secretFilename)}}),
			},
		}},
	}}
}

func TestHandleHistorySyncDoesNotLogContentAtInfo(t *testing.T) {
	setDebug(t, false)
	store := newTestStore(t)
	logger := waLog.Stdout("Test", "INFO", false)
	evt := historyEvent(t, store)
	out := captureStdout(t, func() { handleHistorySync(nil, store, evt, logger) })
	for _, secret := range []string{secretText, secretFilename, secretName} {
		if strings.Contains(out, secret) {
			t.Errorf("INFO output contains %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "Stored 2 messages") {
		t.Errorf("summary missing:\n%s", out)
	}
}

func TestAPISendDoesNotLogContentAtInfo(t *testing.T) {
	setDebug(t, false)
	_, outbox := allowlistFixture(t)
	secretPath := filepath.Join(outbox, "ok.jpg")

	s, _ := newTestAPIServer(t)
	s.allowedDirs = []string{outbox}
	s.send = func(recipient, message, mediaPath string, mediaData []byte) (bool, string) {
		return true, "Message sent to " + recipient
	}
	out := captureStdout(t, func() {
		for _, body := range []string{
			fmt.Sprintf(`{"recipient":"15550000001","message":%q}`, secretText),
			fmt.Sprintf(`{"recipient":"15550000001","media_path":%q}`, secretPath),
		} {
			rec := httptest.NewRecorder()
			s.handler().ServeHTTP(rec, validRequest(http.MethodPost, "/api/send", body))
			if rec.Code != http.StatusOK {
				t.Errorf("-> %d (%s)", rec.Code, rec.Body.String())
			}
		}
	})
	for _, secret := range []string{secretText, "ok.jpg", outbox} {
		if strings.Contains(out, secret) {
			t.Errorf("INFO output contains %q:\n%s", secret, out)
		}
	}
}

func TestParseFlagsDebug(t *testing.T) {
	f, err := parseFlags([]string{"-debug"})
	if err != nil || !f.debug {
		t.Errorf("parseFlags(-debug) = %+v, %v; want debug on", f, err)
	}
	f, err = parseFlags(nil)
	if err != nil || f.debug {
		t.Errorf("parseFlags() = %+v, %v; want debug off", f, err)
	}
	if _, err := parseFlags([]string{"-nope"}); err == nil {
		t.Error("parseFlags(-nope) should fail")
	}
}

func TestHardenStorePermissions(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(filepath.Join(store, "15550000001@s.whatsapp.net"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store, 0o755); err != nil {
		t.Fatal(err)
	}
	private := []string{"messages.db", "whatsapp.db", "whatsapp.db-wal", "whatsapp.db-shm", "messages.db-journal", bridgeTokenFile, "bridge.log"}
	for _, name := range append(private, "notes.txt") {
		if err := os.WriteFile(filepath.Join(store, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := hardenStorePermissions(store); err != nil {
		t.Fatalf("hardenStorePermissions: %v", err)
	}

	if info, _ := os.Stat(store); info.Mode().Perm() != 0o700 {
		t.Errorf("store mode = %o, want 700", info.Mode().Perm())
	}
	for _, name := range private {
		info, err := os.Stat(filepath.Join(store, name))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, perm)
		}
	}
}

func TestHardenStorePermissionsMissingDirErrors(t *testing.T) {
	if err := hardenStorePermissions(filepath.Join(t.TempDir(), "absent")); err == nil {
		// Creating the store is main's job; a missing dir should be reported.
		t.Error("want an error for a missing store dir")
	}
}
