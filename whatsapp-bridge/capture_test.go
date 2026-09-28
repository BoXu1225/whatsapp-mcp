package main

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Message types beyond plain text (#15). All data is fake.

var danJID = types.JID{User: danPN, Server: types.DefaultUserServer}

// danLive is a live message from Dan (phone only, no LID) in his 1:1 chat.
func danLive(id string, ts time.Time, m *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: danJID, Sender: danJID},
			ID:            id,
			Timestamp:     ts,
		},
		Message: m,
	}
}

// ownLive is a live message from me in Dan's chat.
func ownLive(id string, ts time.Time, m *waE2E.Message) *events.Message {
	evt := danLive(id, ts, m)
	evt.Info.IsFromMe = true
	evt.Info.Sender = testIdentity().OwnPN
	return evt
}

func storeLive(t *testing.T, store *MessageStore, evt *events.Message) liveStored {
	t.Helper()
	res, err := storeLiveMessage(store, testIdentity(), evt, fixedName("Dan Example"))
	if err != nil {
		t.Fatalf("storeLiveMessage %s: %v", evt.Info.ID, err)
	}
	return res
}

var t0 = time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)

func replyCtx(stanza string) *waE2E.ContextInfo {
	return &waE2E.ContextInfo{StanzaID: proto.String(stanza), Participant: proto.String(danChat)}
}

func poll(name string, opts ...string) *waE2E.PollCreationMessage {
	p := &waE2E.PollCreationMessage{Name: proto.String(name)}
	for _, o := range opts {
		p.Options = append(p.Options, &waE2E.PollCreationMessage_Option{OptionName: proto.String(o)})
	}
	return p
}

// captureCases: message -> stored content, media type and reply_to.
var captureCases = []struct {
	name      string
	msg       *waE2E.Message
	content   string
	mediaType string
	replyTo   string
}{
	{"image caption", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("look at this")}}, "look at this", "image", ""},
	{"video caption", &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: proto.String("clip")}}, "clip", "video", ""},
	{"document caption", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("a.pdf"), Caption: proto.String("the doc")}}, "the doc", "document", ""},
	{"document with caption wrapper", &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
		DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("b.pdf"), Caption: proto.String("wrapped doc")}}}}, "wrapped doc", "document", ""},
	{"sticker", &waE2E.Message{StickerMessage: &waE2E.StickerMessage{URL: proto.String("https://mmg.whatsapp.net/s"), Mimetype: proto.String("image/webp")}}, "", "sticker", ""},
	{"location", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
		DegreesLatitude: proto.Float64(51.5074), DegreesLongitude: proto.Float64(-0.1278), Name: proto.String("Fake Cafe")}}, "[location 51.5074,-0.1278 Fake Cafe]", "", ""},
	{"location without name", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
		DegreesLatitude: proto.Float64(1.5), DegreesLongitude: proto.Float64(2)}}, "[location 1.5,2]", "", ""},
	{"live location", &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{
		DegreesLatitude: proto.Float64(48.8566), DegreesLongitude: proto.Float64(2.3522), Caption: proto.String("on my way")}}, "[live location 48.8566,2.3522] on my way", "", ""},
	{"contact", &waE2E.Message{ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String("Erin Example"), Vcard: proto.String("BEGIN:VCARD\nEND:VCARD")}}, "[contact Erin Example]", "", ""},
	{"contacts array", &waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{Contacts: []*waE2E.ContactMessage{
		{DisplayName: proto.String("Erin Example")}, {DisplayName: proto.String("Frank Example")}}}}, "[contacts Erin Example, Frank Example]", "", ""},
	{"poll", &waE2E.Message{PollCreationMessage: poll("Dinner?", "Pizza", "Sushi")}, "[poll] Dinner?: Pizza / Sushi", "", ""},
	{"poll v3", &waE2E.Message{PollCreationMessageV3: poll("Where?", "Here", "There")}, "[poll] Where?: Here / There", "", ""},
	{"reply text", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("agreed"), ContextInfo: replyCtx("ORIG1")}}, "agreed", "", "ORIG1"},
	{"reply with image", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("this one"), ContextInfo: replyCtx("ORIG2")}}, "this one", "image", "ORIG2"},
	{"reply in ephemeral wrapper", &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("wrapped reply"), ContextInfo: replyCtx("ORIG3")}}}}, "wrapped reply", "", "ORIG3"},
}

func TestCaptureMessageTypes(t *testing.T) {
	for i, tt := range captureCases {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			evt := danLive("C"+string(rune('A'+i)), t0, tt.msg)
			// As whatsmeow delivers it: wrappers already unwrapped.
			evt.RawMessage = tt.msg
			evt.UnwrapRaw()
			storeLive(t, store, evt)

			var content, mediaType, replyTo string
			err := store.db.QueryRow(
				"SELECT COALESCE(content, ''), COALESCE(media_type, ''), COALESCE(reply_to, '') FROM messages WHERE id = ?",
				evt.Info.ID).Scan(&content, &mediaType, &replyTo)
			if err != nil {
				t.Fatalf("row not stored: %v", err)
			}
			if content != tt.content || mediaType != tt.mediaType || replyTo != tt.replyTo {
				t.Errorf("stored (%q, %q, reply_to %q), want (%q, %q, reply_to %q)",
					content, mediaType, replyTo, tt.content, tt.mediaType, tt.replyTo)
			}
		})
	}
}

// The same message types are captured from history sync.
func TestCaptureMessageTypesFromHistory(t *testing.T) {
	store := newTestStore(t)
	var msgs []*waHistorySync.HistorySyncMsg
	for i, tt := range captureCases {
		msgs = append(msgs, histMsg(danChat, "H"+string(rune('A'+i)), false, uint64(1704200000+i), tt.msg))
	}
	storeHistorySync(store, testIdentity(), histConv(danChat, 0, msgs...), fixedName("Dan Example"), quietLogger())
	for i, tt := range captureCases {
		var content, mediaType, replyTo string
		err := store.db.QueryRow(
			"SELECT COALESCE(content, ''), COALESCE(media_type, ''), COALESCE(reply_to, '') FROM messages WHERE id = ?",
			"H"+string(rune('A'+i))).Scan(&content, &mediaType, &replyTo)
		if err != nil {
			t.Errorf("%s: not stored: %v", tt.name, err)
			continue
		}
		if content != tt.content || mediaType != tt.mediaType || replyTo != tt.replyTo {
			t.Errorf("%s: stored (%q, %q, %q), want (%q, %q, %q)", tt.name, content, mediaType, replyTo, tt.content, tt.mediaType, tt.replyTo)
		}
	}
}

type reactionRow struct{ sender, emoji string }

func reactionsFor(t *testing.T, store *MessageStore, msgID string) []reactionRow {
	t.Helper()
	rows, err := store.db.Query("SELECT sender, emoji FROM reactions WHERE message_id = ? ORDER BY sender", msgID)
	if err != nil {
		t.Fatalf("query reactions: %v", err)
	}
	defer rows.Close()
	var out []reactionRow
	for rows.Next() {
		var r reactionRow
		if err := rows.Scan(&r.sender, &r.emoji); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func reaction(target string, emoji string, tsMS int64) *waE2E.Message {
	return &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key:               &waCommon.MessageKey{ID: proto.String(target), RemoteJID: proto.String(danChat)},
		Text:              proto.String(emoji),
		SenderTimestampMS: proto.Int64(tsMS),
	}}
}

// Reactions go to the reactions table, one per sender and message; a newer
// reaction replaces it and an empty one removes it. They are not messages.
func TestReactionsAreStoredPerSender(t *testing.T) {
	store := newTestStore(t)
	storeLive(t, store, danLive("M1", t0, text("fake plan")))
	ms := t0.UnixMilli()

	storeLive(t, store, danLive("R1", t0.Add(time.Minute), reaction("M1", "👍", ms+60000)))
	storeLive(t, store, ownLive("R2", t0.Add(2*time.Minute), reaction("M1", "❤️", ms+120000)))
	got := reactionsFor(t, store, "M1")
	want := []reactionRow{{ownPN + "@s.whatsapp.net", "❤️"}, {danChat, "👍"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("reactions = %v, want %v", got, want)
	}
	if _, ok := contentOf(t, store, "R1"); ok {
		t.Error("a reaction was stored as a message")
	}

	// Dan changes his reaction, then an older (out-of-order) one arrives.
	storeLive(t, store, danLive("R3", t0.Add(3*time.Minute), reaction("M1", "😂", ms+180000)))
	storeLive(t, store, danLive("R0", t0.Add(30*time.Second), reaction("M1", "😮", ms+30000)))
	if got := reactionsFor(t, store, "M1"); len(got) != 2 || got[1].emoji != "😂" {
		t.Errorf("after change = %v, want Dan's 😂", got)
	}

	// Removal.
	storeLive(t, store, danLive("R4", t0.Add(4*time.Minute), reaction("M1", "", ms+240000)))
	if got := reactionsFor(t, store, "M1"); len(got) != 1 || got[0].emoji != "❤️" {
		t.Errorf("after removal = %v, want only my ❤️", got)
	}
}

func TestReactionsFromHistory(t *testing.T) {
	store := newTestStore(t)
	storeHistorySync(store, testIdentity(), histConv(danChat, 0,
		histMsg(danChat, "HR1", false, 1704200100, reaction("HM1", "🎉", 1704200100000)),
		histMsg(danChat, "HM1", true, 1704200000, text("fake news")),
	), fixedName("Dan Example"), quietLogger())
	if got := reactionsFor(t, store, "HM1"); len(got) != 1 || got[0] != (reactionRow{danChat, "🎉"}) {
		t.Errorf("reactions = %v, want Dan's 🎉", got)
	}
}

// --- migration 5 ---------------------------------------------------------

func columnNames(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	for _, c := range queryStrings(t, db, "SELECT name FROM pragma_table_info('"+table+"')") {
		got[c] = true
	}
	return got
}

func assertCaptureSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	cols := columnNames(t, db, "messages")
	for _, c := range []string{"reply_to", "edited_at", "is_deleted", "deleted_at"} {
		if !cols[c] {
			t.Errorf("messages.%s missing", c)
		}
	}
	rcols := columnNames(t, db, "reactions")
	for _, c := range []string{"message_id", "chat_jid", "sender", "emoji", "timestamp"} {
		if !rcols[c] {
			t.Errorf("reactions.%s missing", c)
		}
	}
}

func TestMigration5OnFreshStore(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	assertCaptureSchema(t, store.db)
	if got := appliedVersions(t, store); !strings.Contains(strings.Join(got, ","), "5") {
		t.Errorf("schema_version = %v, want 5 applied", got)
	}
}

// A database at version 4 with data gets migration 5 at open (it needs no
// identity), with a backup, keeping its rows; reopening changes nothing.
func TestMigration5FromVersion4(t *testing.T) {
	dir := legacyDB(t, `
		INSERT INTO chats VALUES ('`+danChat+`', 'Dan Example', '2024-01-02 10:00:00+00:00');
		INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
			VALUES ('v4', '`+danChat+`', '`+danChat+`', 'old row', '2024-01-02 10:00:00+00:00', 0);`)
	orig := migrations
	migrations = orig[:4]
	store, err := NewMessageStoreAt(dir)
	if err == nil {
		_, err = store.MigrateIdentity(testIdentity())
		store.Close()
	}
	migrations = orig
	if err != nil {
		t.Fatal(err)
	}
	before := len(backups(t, dir))

	store, err = NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	assertCaptureSchema(t, store.db)
	if got := appliedVersions(t, store); strings.Join(got, ",") != "1,2,3,4,5" {
		t.Errorf("schema_version = %v, want 1..5", got)
	}
	b := backups(t, dir)
	if len(b) != before+1 {
		t.Fatalf("backups = %v, want one more for migration 5", b)
	}
	if c, _ := contentOf(t, store, "v4"); c != "old row" {
		t.Errorf("row content after migration = %q", c)
	}
	var deleted int
	store.db.QueryRow("SELECT is_deleted FROM messages WHERE id = 'v4'").Scan(&deleted)
	if deleted != 0 {
		t.Errorf("is_deleted default = %d, want 0", deleted)
	}
	store.Close()

	// Idempotent: reopening applies nothing and makes no backup.
	store, err = NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	if len(backups(t, dir)) != before+1 {
		t.Errorf("reopen made another backup")
	}
	// The migration itself is idempotent too (columns already there).
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, m := range migrations {
		if m.version == 5 {
			if err := m.run(tx, nil, &migrationReport{}); err != nil {
				t.Errorf("migration 5 run twice: %v", err)
			}
		}
	}
}

// Before login, the identity migrations (3, 4) can't run, so migration 5
// waits behind them; the new columns are still added at open so live
// messages can be stored.
func TestCaptureColumnsExistBeforeIdentityMigrations(t *testing.T) {
	dir := legacyDB(t, "")
	store, err := NewMessageStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertCaptureSchema(t, store.db)
	storeLive(t, store, danLive("P1", t0, &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String("before login"), ContextInfo: replyCtx("P0")}}))
	if _, err := store.MigrateIdentity(testIdentity()); err != nil {
		t.Fatal(err)
	}
	if got := appliedVersions(t, store); strings.Join(got, ",") != "1,2,3,4,5" {
		t.Errorf("schema_version = %v, want 1..5", got)
	}
}
