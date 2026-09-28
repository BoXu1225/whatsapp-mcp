package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"google.golang.org/protobuf/proto"
)

func TestSafeMediaFilename(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"report.pdf", "report.pdf", true},
		{"my report (final).pdf", "my report (final).pdf", true},
		{".hidden", ".hidden", true},
		{"../../etc/passwd", "passwd", true},
		{"/etc/passwd", "passwd", true},
		{"sub/dir/file.txt", "file.txt", true},
		{`..\..\evil.exe`, "evil.exe", true},
		{`C:\Windows\system32\x.dll`, "x.dll", true},
		{"dir/", "dir", true},
		{"", "", false},
		{".", "", false},
		{"..", "", false},
		{"../", "", false},
		{"/", "", false},
		{`\`, "", false},
		{"a/..", "", false},
		{"bad\x00name.pdf", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := safeMediaFilename(tt.in)
			if ok != tt.wantOK || (ok && got != tt.want) {
				t.Errorf("safeMediaFilename(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.wantOK)
			}
			if ok && (strings.ContainsAny(got, `/\`) || got == "." || got == "..") {
				t.Errorf("safeMediaFilename(%q) = %q is not a plain base name", tt.in, got)
			}
		})
	}
}

// storeUnderTemp returns a store dir three levels below a temp root, so even a
// path that escapes it by a few ".." stays inside the test's temp dir.
func storeUnderTemp(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "a", "b", "store")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMediaLocalPathStaysInStore(t *testing.T) {
	tests := []struct {
		name     string
		chatJID  string
		filename string
		wantBase string // "" means rejected
	}{
		{"plain", "15550000001@s.whatsapp.net", "report.pdf", "report.pdf"},
		{"device jid colon", "15550000001:12@s.whatsapp.net", "image_1.jpg", "image_1.jpg"},
		{"group", "120363000000000001@g.us", "doc.pdf", "doc.pdf"},
		{"filename traversal", "15550000001@s.whatsapp.net", "../../evil.sh", "evil.sh"},
		{"filename deep traversal", "15550000001@s.whatsapp.net", "../../../../../../tmp/evil.sh", "evil.sh"},
		{"filename absolute", "15550000001@s.whatsapp.net", "/etc/cron.d/evil", "evil"},
		{"filename backslashes", "15550000001@s.whatsapp.net", `..\..\evil.bat`, "evil.bat"},
		{"filename empty", "15550000001@s.whatsapp.net", "", ""},
		{"filename dotdot", "15550000001@s.whatsapp.net", "..", ""},
		{"filename dot", "15550000001@s.whatsapp.net", ".", ""},
		{"chat traversal", "../..", "x.pdf", ""},
		{"chat with slash", "../escape@s.whatsapp.net", "x.pdf", ""},
		{"chat absolute", "/tmp", "x.pdf", ""},
		{"chat empty", "", "x.pdf", ""},
		{"chat dot", ".", "x.pdf", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storeDir := storeUnderTemp(t)
			got, err := mediaLocalPath(storeDir, tt.chatJID, tt.filename)
			if tt.wantBase == "" {
				if err == nil {
					t.Errorf("mediaLocalPath(%q, %q) = %q, want rejection", tt.chatJID, tt.filename, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("mediaLocalPath(%q, %q): %v", tt.chatJID, tt.filename, err)
			}
			realStore, _ := filepath.EvalSymlinks(storeDir)
			if !filepath.IsAbs(got) {
				t.Errorf("path %q is not absolute", got)
			}
			rel, err := filepath.Rel(realStore, got)
			if err != nil || strings.HasPrefix(rel, "..") || strings.Count(rel, string(filepath.Separator)) != 1 {
				t.Errorf("path %q is not <store>/<chat>/<file> under %q", got, realStore)
			}
			if filepath.Base(got) != tt.wantBase {
				t.Errorf("file name = %q, want %q", filepath.Base(got), tt.wantBase)
			}
			info, err := os.Stat(filepath.Dir(got))
			if err != nil {
				t.Fatalf("chat dir not created: %v", err)
			}
			if perm := info.Mode().Perm(); perm != 0o700 {
				t.Errorf("chat dir mode = %o, want 700", perm)
			}
		})
	}
}

func TestMediaLocalPathRejectsSymlinkedChatDir(t *testing.T) {
	storeDir := storeUnderTemp(t)
	outside := t.TempDir()
	chat := "15550000001@s.whatsapp.net"
	if err := os.Symlink(outside, filepath.Join(storeDir, chat)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got, err := mediaLocalPath(storeDir, chat, "x.pdf"); err == nil {
		t.Errorf("mediaLocalPath through a symlinked chat dir = %q, want rejection", got)
	}
}

func TestSaveMediaFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.jpg")
	if err := saveMediaFile(path, []byte("fake")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("media file mode = %o, want 600", perm)
	}
}

func TestExtractMediaInfoSanitisesDocumentFilename(t *testing.T) {
	tests := []struct {
		in         string
		want       string
		wantPrefix string
	}{
		{"../../.ssh/authorized_keys", "authorized_keys", ""},
		{"/etc/passwd", "passwd", ""},
		{`..\..\evil.exe`, "evil.exe", ""},
		{"..", "", "document_"},
		{"/", "", "document_"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			msg := &waProto.Message{DocumentMessage: &waProto.DocumentMessage{FileName: proto.String(tt.in)}}
			_, filename, _, _, _, _, _ := extractMediaInfo(msg)
			if tt.want != "" && filename != tt.want {
				t.Errorf("filename = %q, want %q", filename, tt.want)
			}
			if tt.wantPrefix != "" && !strings.HasPrefix(filename, tt.wantPrefix) {
				t.Errorf("filename = %q, want prefix %q", filename, tt.wantPrefix)
			}
		})
	}
}

func TestDownloadMediaUsesSafePathForStoredFilename(t *testing.T) {
	store := newTestStore(t)
	chat := "15550000001@s.whatsapp.net"
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := store.StoreChat(chat, "Test Contact", ts); err != nil {
		t.Fatal(err)
	}
	// A row stored before filenames were sanitised.
	if err := store.StoreMessage("m1", chat, "15550000001", "", ts, false, "document", "../../evil.txt", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	// Cached copy at the safe location, so downloadMedia never needs a client.
	chatDir := filepath.Join(store.dir, chat)
	if err := os.MkdirAll(chatDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chatDir, "evil.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	ok, _, filename, got, err := downloadMedia(nil, store, "m1", chat)
	if !ok || err != nil {
		t.Fatalf("downloadMedia: ok=%v err=%v", ok, err)
	}
	realStore, _ := filepath.EvalSymlinks(store.dir)
	if filename != "evil.txt" || got != filepath.Join(realStore, chat, "evil.txt") {
		t.Errorf("got filename %q path %q, want evil.txt under the chat dir", filename, got)
	}
}

func TestDownloadMediaRejectsTraversalChatJID(t *testing.T) {
	store := newTestStore(t)
	chat := "../outside"
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := store.StoreChat(chat, "x", ts); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreMessage("m1", chat, "15550000001", "", ts, false, "image", "image_1.jpg", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if ok, _, _, _, err := downloadMedia(nil, store, "m1", chat); ok || err == nil {
		t.Fatalf("downloadMedia with chat %q succeeded", chat)
	}
	if _, err := os.Stat(filepath.Join(store.dir, "..", "outside")); !os.IsNotExist(err) {
		t.Errorf("directory created outside the store (stat err %v)", err)
	}
}

func TestSafeMediaFilenameReplacesControlChars(t *testing.T) {
	tests := []struct{ in, want string }{
		{"a\nb.pdf", "a_b.pdf"},
		{"report\r\n[12:00:00] fake log line.pdf", "report__[12:00:00] fake log line.pdf"},
		{"tab\there.txt", "tab_here.txt"},
		{"\x1b[31mred.txt", "_[31mred.txt"},
		{"del\x7f.txt", "del_.txt"},
		{"nel\u0085.txt", "nel_.txt"},
		{"ünïcödé ok.txt", "ünïcödé ok.txt"},
	}
	for _, tt := range tests {
		got, ok := safeMediaFilename(tt.in)
		if !ok || got != tt.want {
			t.Errorf("safeMediaFilename(%q) = %q, %v; want %q", tt.in, got, ok, tt.want)
		}
	}
}

func TestSafeMediaFilenameTruncatesKeepingExtension(t *testing.T) {
	tests := []struct {
		name, in, wantSuffix string
	}{
		{"long ascii", strings.Repeat("a", 300) + ".pdf", ".pdf"},
		{"long multibyte", strings.Repeat("é", 150) + ".txt", ".txt"},
		{"long no extension", strings.Repeat("b", 500), "b"},
		{"long extension", "x." + strings.Repeat("z", 300), "z"},
		{"long after traversal", "../../" + strings.Repeat("c", 250) + ".jpeg", ".jpeg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := safeMediaFilename(tt.in)
			if !ok {
				t.Fatalf("rejected %q", tt.in)
			}
			if len(got) > maxMediaFilenameBytes {
				t.Errorf("len = %d, want <= %d", len(got), maxMediaFilenameBytes)
			}
			if len(got) < maxMediaFilenameBytes-4 {
				t.Errorf("len = %d, truncated more than needed", len(got))
			}
			if !strings.HasSuffix(got, tt.wantSuffix) {
				t.Errorf("%q lost its suffix %q", got, tt.wantSuffix)
			}
			if !utf8.ValidString(got) {
				t.Errorf("%q is not valid UTF-8", got)
			}
		})
	}
	short := "short.pdf"
	if got, _ := safeMediaFilename(short); got != short {
		t.Errorf("short name changed to %q", got)
	}
}

func TestMediaLocalPathRejectsReservedChatNames(t *testing.T) {
	for _, chat := range []string{outboxDirName, "bridge_token", "messages.db", "whatsapp.db", "bridge.log", "not-a-jid"} {
		t.Run(chat, func(t *testing.T) {
			storeDir := storeUnderTemp(t)
			if got, err := mediaLocalPath(storeDir, chat, "x.jpg"); err == nil {
				t.Errorf("mediaLocalPath(chat %q) = %q, want rejection (downloads must never land in the outbox or on store files)", chat, got)
			}
		})
	}
}
