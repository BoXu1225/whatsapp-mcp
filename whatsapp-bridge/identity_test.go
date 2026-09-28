package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Fake identities. All numbers are made up.
const (
	ownPN     = "15550000000"
	ownLID    = "100000000000000"
	carolPN   = "15550000003"
	carolLID  = "100000000000003"
	danPN     = "15550000004" // phone only, no LID mapping
	groupUser = "120363000000000001"
)

var (
	carolPNJID  = types.JID{User: carolPN, Server: types.DefaultUserServer}
	carolLIDJID = types.JID{User: carolLID, Server: types.HiddenUserServer}
	testGroup   = types.JID{User: groupUser, Server: types.GroupServer}
)

// fakeLIDs is an in-memory LID map (lid user -> pn user), standing in for
// client.Store.LIDs.
type fakeLIDs map[string]string

func (f fakeLIDs) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	if pn, ok := f[lid.User]; ok {
		return types.JID{User: pn, Server: types.DefaultUserServer}, nil
	}
	return types.JID{}, nil
}

func (f fakeLIDs) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	for lid, p := range f {
		if p == pn.User {
			return types.JID{User: lid, Server: types.HiddenUserServer}, nil
		}
	}
	return types.JID{}, nil
}

func testIdentity() Identity {
	return Identity{
		OwnPN:  types.JID{User: ownPN, Server: types.DefaultUserServer},
		OwnLID: types.JID{User: ownLID, Server: types.HiddenUserServer},
		LIDs:   fakeLIDs{ownLID: ownPN, carolLID: carolPN},
	}
}

func TestCanonicalChatPrefersLID(t *testing.T) {
	id := testIdentity()
	tests := []struct {
		name  string
		chat  types.JID
		hints []types.JID
		want  string
	}{
		{"PN with mapping", carolPNJID, nil, carolLID + "@lid"},
		{"PN with device part", types.JID{User: carolPN, Device: 3, Server: types.DefaultUserServer}, nil, carolLID + "@lid"},
		{"LID stays", carolLIDJID, nil, carolLID + "@lid"},
		{"PN without mapping stays", types.JID{User: danPN, Server: types.DefaultUserServer}, nil, danPN + "@s.whatsapp.net"},
		{"PN with LID hint", types.JID{User: danPN, Server: types.DefaultUserServer},
			[]types.JID{{User: "100000000000004", Server: types.HiddenUserServer}}, "100000000000004@lid"},
		{"group unchanged", testGroup, nil, groupUser + "@g.us"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := id.CanonicalChat(tt.chat, tt.hints...).String(); got != tt.want {
				t.Errorf("CanonicalChat = %s, want %s", got, tt.want)
			}
		})
	}
	// No LID map (e.g. no client): the chat is kept as is.
	if got := (Identity{}).CanonicalChat(carolPNJID).String(); got != carolPN+"@s.whatsapp.net" {
		t.Errorf("CanonicalChat without map = %s", got)
	}
}

func TestLiveSenderIsFullNonADJIDWithAlt(t *testing.T) {
	id := testIdentity()
	info := func(chat, sender, alt types.JID, fromMe bool) types.MessageInfo {
		return types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: sender, SenderAlt: alt, IsFromMe: fromMe}}
	}
	carolDevice := types.JID{User: carolLID, Device: 5, Server: types.HiddenUserServer}
	tests := []struct {
		name            string
		info            types.MessageInfo
		wantSender, alt string
	}{
		{"LID sender with SenderAlt", info(testGroup, carolDevice, types.JID{User: carolPN, Device: 5, Server: types.DefaultUserServer}, false),
			carolLID + "@lid", carolPN + "@s.whatsapp.net"},
		{"PN sender in 1:1 uses the chat's LID form", info(carolPNJID, types.JID{User: carolPN, Device: 2, Server: types.DefaultUserServer}, types.EmptyJID, false),
			carolLID + "@lid", carolPN + "@s.whatsapp.net"},
		{"PN sender in group, alt from LID map", info(testGroup, types.JID{User: carolPN, Device: 2, Server: types.DefaultUserServer}, types.EmptyJID, false),
			carolPN + "@s.whatsapp.net", carolLID + "@lid"},
		{"unknown sender, no alt", info(types.JID{User: danPN, Server: types.DefaultUserServer}, types.JID{User: danPN, Server: types.DefaultUserServer}, types.EmptyJID, false),
			danPN + "@s.whatsapp.net", ""},
		{"own message in LID chat", info(carolLIDJID, types.JID{User: ownPN, Device: 1, Server: types.DefaultUserServer}, types.EmptyJID, true),
			ownLID + "@lid", ownPN + "@s.whatsapp.net"},
		{"own message in PN chat", info(types.JID{User: danPN, Server: types.DefaultUserServer}, types.JID{User: ownLID, Server: types.HiddenUserServer}, types.EmptyJID, true),
			ownPN + "@s.whatsapp.net", ownLID + "@lid"},
		{"own message in group", info(testGroup, types.JID{User: ownLID, Server: types.HiddenUserServer}, types.EmptyJID, true),
			ownPN + "@s.whatsapp.net", ownLID + "@lid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chat := id.CanonicalChat(tt.info.Chat)
			sender, alt := id.LiveSender(tt.info, chat)
			if sender != tt.wantSender || alt != tt.alt {
				t.Errorf("LiveSender = (%q, %q), want (%q, %q)", sender, alt, tt.wantSender, tt.alt)
			}
		})
	}
}

func TestHistorySender(t *testing.T) {
	id := testIdentity()
	carolChat := id.CanonicalChat(carolPNJID)
	tests := []struct {
		name            string
		chat            types.JID
		fromMe          bool
		participant     string
		wantSender, alt string
	}{
		{"1:1 from them is the chat", carolChat, false, "", carolLID + "@lid", carolPN + "@s.whatsapp.net"},
		{"1:1 from me", carolChat, true, "", ownLID + "@lid", ownPN + "@s.whatsapp.net"},
		{"group participant with device", testGroup, false, carolPN + ":7@s.whatsapp.net", carolPN + "@s.whatsapp.net", carolLID + "@lid"},
		{"group participant LID", testGroup, false, carolLID + "@lid", carolLID + "@lid", carolPN + "@s.whatsapp.net"},
		{"group from me", testGroup, true, "", ownPN + "@s.whatsapp.net", ownLID + "@lid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender, alt := id.HistorySender(tt.chat, tt.fromMe, tt.participant)
			if sender != tt.wantSender || alt != tt.alt {
				t.Errorf("HistorySender = (%q, %q), want (%q, %q)", sender, alt, tt.wantSender, tt.alt)
			}
		})
	}
}

func fixedName(name string) chatNamer {
	return func(types.JID, interface{}) string { return name }
}

func carolHistorySync(convID string, lidJID string) *events.HistorySync {
	msg := func(id string, fromMe bool, ts uint64, text string) *waHistorySync.HistorySyncMsg {
		return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(fromMe), RemoteJID: proto.String(convID)},
			Message:          &waProto.Message{Conversation: proto.String(text)},
			MessageTimestamp: proto.Uint64(ts),
		}}
	}
	conv := &waHistorySync.Conversation{
		ID: proto.String(convID),
		Messages: []*waHistorySync.HistorySyncMsg{
			msg("H2", true, 1704164700, "fake reply"),
			msg("H1", false, 1704164645, "fake hello"),
		},
	}
	if lidJID != "" {
		conv.LidJID = proto.String(lidJID)
	}
	return &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{conv}}}
}

func carolLiveMessage() *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat: carolLIDJID, Sender: types.JID{User: carolLID, Device: 3, Server: types.HiddenUserServer},
				SenderAlt: types.JID{User: carolPN, Device: 3, Server: types.DefaultUserServer},
			},
			ID:        "L1",
			Timestamp: time.Date(2024, 1, 3, 9, 0, 0, 0, time.UTC),
		},
		Message: &waProto.Message{Conversation: proto.String("fake live")},
	}
}

func chatRows(t *testing.T, store *MessageStore) map[string]int {
	t.Helper()
	rows, err := store.db.Query("SELECT c.jid, COUNT(m.id) FROM chats c LEFT JOIN messages m ON m.chat_jid = c.jid GROUP BY c.jid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var jid string
		var n int
		if err := rows.Scan(&jid, &n); err != nil {
			t.Fatal(err)
		}
		got[jid] = n
	}
	return got
}

// A PN-keyed history conversation and a LID-keyed live message from the same
// contact end up in one chat (#9), via the LID map.
func TestHistoryPNAndLiveLIDShareOneChat(t *testing.T) {
	store := newTestStore(t)
	logger := waLog.Stdout("Test", "ERROR", false)
	id := testIdentity()

	storeHistorySync(store, id, carolHistorySync(carolPN+"@s.whatsapp.net", ""), fixedName("Carol Example"), logger)
	if _, err := storeLiveMessage(store, id, carolLiveMessage(), fixedName("Carol Example")); err != nil {
		t.Fatal(err)
	}

	got := chatRows(t, store)
	if len(got) != 1 || got[carolLID+"@lid"] != 3 {
		t.Fatalf("chats = %v, want one chat %s@lid with 3 messages", got, carolLID)
	}
	var sender, alt string
	if err := store.db.QueryRow("SELECT sender, sender_alt FROM messages WHERE id = 'H2'").Scan(&sender, &alt); err != nil {
		t.Fatal(err)
	}
	if sender != ownLID+"@lid" || alt != ownPN+"@s.whatsapp.net" {
		t.Errorf("own history message sender = (%q, %q), want our LID in a LID chat", sender, alt)
	}
}

// Without a LID map entry, the conversation's own lidJID field is used.
func TestHistoryUsesConversationLIDJID(t *testing.T) {
	store := newTestStore(t)
	logger := waLog.Stdout("Test", "ERROR", false)
	id := Identity{OwnPN: types.JID{User: ownPN, Server: types.DefaultUserServer}}

	storeHistorySync(store, id, carolHistorySync(carolPN+"@s.whatsapp.net", carolLID+"@lid"), fixedName("Carol Example"), logger)
	if _, err := storeLiveMessage(store, id, carolLiveMessage(), fixedName("Carol Example")); err != nil {
		t.Fatal(err)
	}
	if got := chatRows(t, store); len(got) != 1 || got[carolLID+"@lid"] != 3 {
		t.Fatalf("chats = %v, want one chat %s@lid with 3 messages", got, carolLID)
	}
}

// A live message whose chat maps to a LID merges an existing PN-keyed chat
// for the same person (e.g. stored before the mapping was known).
func TestLiveMessageMergesExistingPNChat(t *testing.T) {
	store := newTestStore(t)
	logger := waLog.Stdout("Test", "ERROR", false)
	// Stored with no LID knowledge at all.
	storeHistorySync(store, Identity{}, carolHistorySync(carolPN+"@s.whatsapp.net", ""), fixedName("Carol Example"), logger)
	if got := chatRows(t, store); got[carolPN+"@s.whatsapp.net"] != 2 {
		t.Fatalf("setup: chats = %v", got)
	}

	if _, err := storeLiveMessage(store, testIdentity(), carolLiveMessage(), fixedName("Carol Example")); err != nil {
		t.Fatal(err)
	}
	if got := chatRows(t, store); len(got) != 1 || got[carolLID+"@lid"] != 3 {
		t.Fatalf("chats = %v, want one chat %s@lid with 3 messages", got, carolLID)
	}
}

// The first live merge of a run backs the database up first (like the startup
// migrations); later merges in the same run don't make another copy.
func TestLiveMergeBacksUpFirst(t *testing.T) {
	dir := t.TempDir()
	store, err := NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := waLog.Stdout("Test", "ERROR", false)
	storeHistorySync(store, Identity{}, carolHistorySync(carolPN+"@s.whatsapp.net", ""), fixedName("Carol Example"), logger)
	if b := backups(t, dir); len(b) != 0 {
		t.Fatalf("backup before any merge: %v", b)
	}

	if _, err := storeLiveMessage(store, testIdentity(), carolLiveMessage(), fixedName("Carol Example")); err != nil {
		t.Fatal(err)
	}
	b := backups(t, dir)
	if len(b) != 1 {
		t.Fatalf("backups after live merge = %v, want one", b)
	}
	bak, err := sql.Open("sqlite3", "file:"+b[0]+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer bak.Close()
	if got := queryStrings(t, bak, "SELECT jid FROM chats"); strings.Join(got, ",") != carolPN+"@s.whatsapp.net" {
		t.Errorf("backup chats = %v, want the pre-merge PN chat", got)
	}

	// Another PN chat merged later in the run: no second backup.
	dan := types.JID{User: danPN, Server: types.DefaultUserServer}
	if err := store.StoreChat(dan.String(), "Dan Example", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.MergeChat(dan.String(), "100000000000004@lid"); err != nil {
		t.Fatal(err)
	}
	if b := backups(t, dir); len(b) != 1 {
		t.Errorf("backups after second merge = %v, want still one", b)
	}
}
