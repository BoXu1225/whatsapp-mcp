package main

import (
	"database/sql"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Edits and revokes (#16). All data is fake.

func editProto(target string, newMsg *waE2E.Message, at time.Time) *waE2E.ProtocolMessage {
	return &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           &waCommon.MessageKey{ID: proto.String(target)},
		EditedMessage: newMsg,
		TimestampMS:   proto.Int64(at.UnixMilli()),
	}
}

// wrappedEdit is an edit as whatsmeow delivers it: an EditedMessage wrapper
// around the protocol message, unwrapped by UnwrapRaw.
func wrappedEdit(evt *events.Message, target string, newMsg *waE2E.Message) *events.Message {
	evt.RawMessage = &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
		ProtocolMessage: editProto(target, newMsg, evt.Info.Timestamp),
	}}}
	return evt.UnwrapRaw()
}

func revokeMsg(target string) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key:  &waCommon.MessageKey{ID: proto.String(target)},
	}}
}

type msgState struct {
	content          string
	edited, deleted  bool
	editedAt         string
	deletedAtPresent bool
}

func stateOf(t *testing.T, store *MessageStore, id string) msgState {
	t.Helper()
	var s msgState
	var editedAt, deletedAt sql.NullString
	var isDeleted int
	err := store.db.QueryRow(
		"SELECT COALESCE(content, ''), CAST(edited_at AS TEXT), is_deleted, CAST(deleted_at AS TEXT) FROM messages WHERE id = ?", id,
	).Scan(&s.content, &editedAt, &isDeleted, &deletedAt)
	if err != nil {
		t.Fatalf("message %s: %v", id, err)
	}
	s.edited, s.editedAt = editedAt.Valid, editedAt.String
	s.deleted, s.deletedAtPresent = isDeleted != 0, deletedAt.Valid
	return s
}

func messageCount(t *testing.T, store *MessageStore) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLiveEditUpdatesMessage(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(evt *events.Message) *events.Message
	}{
		{"EditedMessage wrapper", func(evt *events.Message) *events.Message { return wrappedEdit(evt, "M1", text("fixed text")) }},
		{"bare protocol message", func(evt *events.Message) *events.Message {
			evt.Message = &waE2E.Message{ProtocolMessage: editProto("M1", text("fixed text"), evt.Info.Timestamp)}
			return evt
		}},
		{"extended text edit", func(evt *events.Message) *events.Message {
			return wrappedEdit(evt, "M1", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("fixed text")}})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			storeLive(t, store, danLive("M1", t0, text("typo txet")))
			storeLive(t, store, tt.edit(danLive("E1", t0.Add(time.Minute), nil)))

			s := stateOf(t, store, "M1")
			if s.content != "fixed text" || !s.edited || s.deleted {
				t.Errorf("after edit: %+v, want the new text, edited", s)
			}
			if n := messageCount(t, store); n != 1 {
				t.Errorf("messages = %d, want 1 (the edit is not a message of its own)", n)
			}
		})
	}
}

func TestEditOfCaption(t *testing.T) {
	store := newTestStore(t)
	storeLive(t, store, danLive("M1", t0, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("old caption")}}))
	storeLive(t, store, wrappedEdit(danLive("E1", t0.Add(time.Minute), nil), "M1",
		&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("new caption")}}))
	if s := stateOf(t, store, "M1"); s.content != "new caption" || !s.edited {
		t.Errorf("after caption edit: %+v", s)
	}
	var mediaType string
	store.db.QueryRow("SELECT media_type FROM messages WHERE id = 'M1'").Scan(&mediaType)
	if mediaType != "image" {
		t.Errorf("media_type = %q, want image kept", mediaType)
	}
}

// Only the author can edit: an edit from someone else is ignored.
func TestEditFromAnotherSenderIgnored(t *testing.T) {
	store := newTestStore(t)
	storeLive(t, store, danLive("M1", t0, text("dan's words")))
	storeLive(t, store, wrappedEdit(ownLive("E1", t0.Add(time.Minute), nil), "M1", text("my words")))
	if s := stateOf(t, store, "M1"); s.content != "dan's words" || s.edited {
		t.Errorf("edit by another sender applied: %+v", s)
	}
}

// Edits out of order: an older edit arriving later doesn't win.
func TestOlderEditIgnored(t *testing.T) {
	store := newTestStore(t)
	storeLive(t, store, danLive("M1", t0, text("v1")))
	storeLive(t, store, wrappedEdit(danLive("E2", t0.Add(2*time.Minute), nil), "M1", text("v3")))
	storeLive(t, store, wrappedEdit(danLive("E1", t0.Add(time.Minute), nil), "M1", text("v2")))
	if s := stateOf(t, store, "M1"); s.content != "v3" {
		t.Errorf("content = %q, want the newest edit v3", s.content)
	}
}

func TestEditFromHistory(t *testing.T) {
	store := newTestStore(t)
	edit := &waE2E.Message{ProtocolMessage: editProto("HM1", text("edited in history"), time.Unix(1704200100, 0))}
	storeHistorySync(store, testIdentity(), histConv(danChat, 0,
		histMsg(danChat, "HE1", false, 1704200100, edit),
		histMsg(danChat, "HM1", false, 1704200000, text("original")),
	), fixedName("Dan Example"), quietLogger())
	if s := stateOf(t, store, "HM1"); s.content != "edited in history" || !s.edited {
		t.Errorf("after history edit: %+v", s)
	}
	if n := messageCount(t, store); n != 1 {
		t.Errorf("messages = %d, want 1", n)
	}
}

// The original delivered again (a later history sync) keeps the edit.
func TestRedeliveredOriginalKeepsEdit(t *testing.T) {
	store := newTestStore(t)
	storeLive(t, store, danLive("M1", t0, text("original")))
	storeLive(t, store, wrappedEdit(danLive("E1", t0.Add(time.Minute), nil), "M1", text("edited")))
	storeLive(t, store, danLive("M1", t0, text("original")))
	if s := stateOf(t, store, "M1"); s.content != "edited" || !s.edited {
		t.Errorf("after re-delivery: %+v, want the edit kept", s)
	}
}

func TestRevokeMarksDeletedKeepingContent(t *testing.T) {
	store := newTestStore(t)
	storeLive(t, store, danLive("M1", t0, text("regrettable")))
	storeLive(t, store, danLive("D1", t0.Add(time.Minute), revokeMsg("M1")))

	s := stateOf(t, store, "M1")
	if !s.deleted || !s.deletedAtPresent || s.content != "regrettable" {
		t.Errorf("after revoke: %+v, want deleted with content kept", s)
	}
	if n := messageCount(t, store); n != 1 {
		t.Errorf("messages = %d, want 1", n)
	}
	// Re-delivery doesn't undelete.
	storeLive(t, store, danLive("M1", t0, text("regrettable")))
	if s := stateOf(t, store, "M1"); !s.deleted {
		t.Errorf("re-delivered message undeleted: %+v", s)
	}
	// An edit after the delete doesn't apply.
	storeLive(t, store, wrappedEdit(danLive("E1", t0.Add(2*time.Minute), nil), "M1", text("sneaky")))
	if s := stateOf(t, store, "M1"); s.content != "regrettable" {
		t.Errorf("edit applied to a deleted message: %+v", s)
	}
}

func TestOwnRevoke(t *testing.T) {
	store := newTestStore(t)
	storeLive(t, store, ownLive("M1", t0, text("oops")))
	storeLive(t, store, ownLive("D1", t0.Add(time.Minute), revokeMsg("M1")))
	if s := stateOf(t, store, "M1"); !s.deleted {
		t.Errorf("own revoke not applied: %+v", s)
	}
}

// In a 1:1 chat only the author can revoke.
func TestRevokeFromAnotherSenderIgnoredInDirectChat(t *testing.T) {
	store := newTestStore(t)
	storeLive(t, store, ownLive("M1", t0, text("mine")))
	storeLive(t, store, danLive("D1", t0.Add(time.Minute), revokeMsg("M1")))
	if s := stateOf(t, store, "M1"); s.deleted {
		t.Errorf("revoke by the other person applied: %+v", s)
	}
}

func groupMsg(id string, sender types.JID, m *waE2E.Message, ts time.Time) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: testGroup, Sender: sender, IsGroup: true},
			ID:            id, Timestamp: ts,
		},
		Message: m,
	}
}

func deletedBy(t *testing.T, store *MessageStore, id string) string {
	t.Helper()
	var by sql.NullString
	if err := store.db.QueryRow("SELECT deleted_by FROM messages WHERE id = ?", id).Scan(&by); err != nil {
		t.Fatalf("deleted_by of %s: %v", id, err)
	}
	return by.String
}

// In a group someone else (an admin, or anyone claiming to be) can delete a
// message: it is marked deleted, the revoker is recorded in deleted_by, and
// the text is never purged, even with -purge-deleted.
func TestNonAuthorRevokeInGroup(t *testing.T) {
	for _, purge := range []bool{false, true} {
		store := newTestStore(t)
		store.purgeDeleted = purge
		storeLive(t, store, groupMsg("G1", carolLIDJID, text("spam"), t0))
		storeLive(t, store, groupMsg("GD1", danJID, revokeMsg("G1"), t0.Add(time.Minute)))
		if s := stateOf(t, store, "G1"); !s.deleted || s.content != "spam" {
			t.Errorf("purge=%v: non-author revoke: %+v, want deleted with text kept", purge, s)
		}
		if by := deletedBy(t, store, "G1"); by != danChat {
			t.Errorf("purge=%v: deleted_by = %q, want the revoker %s", purge, by, danChat)
		}
	}
}

// The author's own revoke in a group: deleted, purged with -purge-deleted,
// no deleted_by.
func TestAuthorRevokeInGroup(t *testing.T) {
	store := newTestStore(t)
	store.purgeDeleted = true
	storeLive(t, store, groupMsg("G1", carolLIDJID, text("oops"), t0))
	storeLive(t, store, groupMsg("GD1", carolLIDJID, revokeMsg("G1"), t0.Add(time.Minute)))
	if s := stateOf(t, store, "G1"); !s.deleted || s.content != "" {
		t.Errorf("author revoke with purge: %+v, want deleted and purged", s)
	}
	if by := deletedBy(t, store, "G1"); by != "" {
		t.Errorf("deleted_by = %q, want empty for the author's own revoke", by)
	}
}

// A message ID delivered again with another sender keeps the original
// sender, so it can't be used to pass the edit/revoke author checks.
func TestRedeliveryKeepsOriginalSender(t *testing.T) {
	store := newTestStore(t)
	storeLive(t, store, groupMsg("G1", carolLIDJID, text("carol's"), t0))
	storeLive(t, store, groupMsg("G1", danJID, text("carol's"), t0))
	var sender string
	store.db.QueryRow("SELECT sender FROM messages WHERE id = 'G1'").Scan(&sender)
	if sender != carolLID+"@lid" {
		t.Fatalf("sender after re-delivery = %q, want Carol's", sender)
	}
	store.purgeDeleted = true
	storeLive(t, store, groupMsg("GE1", danJID, &waE2E.Message{ProtocolMessage: editProto("G1", text("dan's now"), t0.Add(time.Minute))}, t0.Add(time.Minute)))
	storeLive(t, store, groupMsg("GD1", danJID, revokeMsg("G1"), t0.Add(2*time.Minute)))
	if s := stateOf(t, store, "G1"); s.content != "carol's" || s.edited {
		t.Errorf("after edit and revoke by Dan: %+v, want Carol's text kept, not edited", s)
	}
}

// A protocol message without an explicit type is not a revoke (REVOKE is
// the enum's zero value).
func TestUntypedProtocolMessageIsNotARevoke(t *testing.T) {
	store := newTestStore(t)
	storeLive(t, store, danLive("M1", t0, text("still here")))
	storeLive(t, store, danLive("P1", t0.Add(time.Minute), &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Key: &waCommon.MessageKey{ID: proto.String("M1")},
	}}))
	if s := stateOf(t, store, "M1"); s.deleted {
		t.Errorf("untyped protocol message treated as revoke: %+v", s)
	}
}

func TestRevokeFromHistory(t *testing.T) {
	store := newTestStore(t)
	storeHistorySync(store, testIdentity(), histConv(danChat, 0,
		histMsg(danChat, "HD1", false, 1704200100, revokeMsg("HM1")),
		histMsg(danChat, "HM1", false, 1704200000, text("gone")),
	), fixedName("Dan Example"), quietLogger())
	if s := stateOf(t, store, "HM1"); !s.deleted {
		t.Errorf("history revoke not applied: %+v", s)
	}
}

// With -purge-deleted, a revoke also clears the stored text, live and in a
// history batch (which goes through a transaction view of the store).
func TestPurgeDeletedClearsContent(t *testing.T) {
	store := newTestStore(t)
	store.purgeDeleted = true
	storeLive(t, store, danLive("M1", t0, text("secret")))
	storeLive(t, store, danLive("D1", t0.Add(time.Minute), revokeMsg("M1")))
	if s := stateOf(t, store, "M1"); !s.deleted || s.content != "" {
		t.Errorf("after purge revoke: %+v, want deleted with no content", s)
	}
	storeHistorySync(store, testIdentity(), histConv(danChat, 0,
		histMsg(danChat, "HD1", false, 1704200100, revokeMsg("HM1")),
		histMsg(danChat, "HM1", false, 1704200000, text("history secret")),
	), fixedName("Dan Example"), quietLogger())
	if s := stateOf(t, store, "HM1"); !s.deleted || s.content != "" {
		t.Errorf("after history purge revoke: %+v", s)
	}
	// Re-delivery doesn't bring the text back.
	storeLive(t, store, danLive("M1", t0, text("secret")))
	if s := stateOf(t, store, "M1"); s.content != "" {
		t.Errorf("re-delivery restored purged content: %+v", s)
	}
}

func TestPurgeDeletedFlag(t *testing.T) {
	f, err := parseFlags([]string{"-purge-deleted"})
	if err != nil || !f.purgeDeleted {
		t.Errorf("parseFlags(-purge-deleted) = %+v, %v", f, err)
	}
	if f, _ := parseFlags(nil); f.purgeDeleted {
		t.Error("purge-deleted should be off by default")
	}
}
