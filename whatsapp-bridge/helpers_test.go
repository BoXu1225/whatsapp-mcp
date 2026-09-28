package main

import (
	"strings"
	"testing"

	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestExtractTextContent(t *testing.T) {
	tests := []struct {
		name string
		msg  *waProto.Message
		want string
	}{
		{"nil message", nil, ""},
		{"empty message", &waProto.Message{}, ""},
		{"conversation", &waProto.Message{Conversation: proto.String("hello")}, "hello"},
		{
			"extended text",
			&waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{Text: proto.String("see https://example.com")}},
			"see https://example.com",
		},
		{
			"conversation wins over extended text",
			&waProto.Message{
				Conversation:        proto.String("plain"),
				ExtendedTextMessage: &waProto.ExtendedTextMessage{Text: proto.String("extended")},
			},
			"plain",
		},
		{
			"image caption is the content",
			&waProto.Message{ImageMessage: &waProto.ImageMessage{Caption: proto.String("a caption")}},
			"a caption",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractTextContent(tt.msg); got != tt.want {
				t.Errorf("extractTextContent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractMediaInfo(t *testing.T) {
	tests := []struct {
		name           string
		msg            *waProto.Message
		wantType       string
		wantFilePrefix string
		wantURL        string
		wantLength     uint64
	}{
		{"nil message", nil, "", "", "", 0},
		{"text only", &waProto.Message{Conversation: proto.String("hi")}, "", "", "", 0},
		{
			"image",
			&waProto.Message{ImageMessage: &waProto.ImageMessage{URL: proto.String("https://mmg.whatsapp.net/img"), FileLength: proto.Uint64(10)}},
			"image", "image_", "https://mmg.whatsapp.net/img", 10,
		},
		{
			"video",
			&waProto.Message{VideoMessage: &waProto.VideoMessage{URL: proto.String("https://mmg.whatsapp.net/vid"), FileLength: proto.Uint64(20)}},
			"video", "video_", "https://mmg.whatsapp.net/vid", 20,
		},
		{
			"audio",
			&waProto.Message{AudioMessage: &waProto.AudioMessage{URL: proto.String("https://mmg.whatsapp.net/aud"), FileLength: proto.Uint64(30)}},
			"audio", "audio_", "https://mmg.whatsapp.net/aud", 30,
		},
		{
			"document with filename",
			&waProto.Message{DocumentMessage: &waProto.DocumentMessage{FileName: proto.String("report.pdf"), URL: proto.String("https://mmg.whatsapp.net/doc")}},
			"document", "report.pdf", "https://mmg.whatsapp.net/doc", 0,
		},
		{
			"sticker",
			&waProto.Message{StickerMessage: &waProto.StickerMessage{URL: proto.String("https://mmg.whatsapp.net/st"), FileLength: proto.Uint64(7)}},
			"sticker", "sticker_", "https://mmg.whatsapp.net/st", 7,
		},
		{
			"document without filename",
			&waProto.Message{DocumentMessage: &waProto.DocumentMessage{}},
			"document", "document_", "", 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mediaType, filename, url, _, _, _, length := extractMediaInfo(tt.msg)
			if mediaType != tt.wantType {
				t.Errorf("mediaType = %q, want %q", mediaType, tt.wantType)
			}
			if !strings.HasPrefix(filename, tt.wantFilePrefix) || (tt.wantFilePrefix == "" && filename != "") {
				t.Errorf("filename = %q, want prefix %q", filename, tt.wantFilePrefix)
			}
			if url != tt.wantURL {
				t.Errorf("url = %q, want %q", url, tt.wantURL)
			}
			if length != tt.wantLength {
				t.Errorf("fileLength = %d, want %d", length, tt.wantLength)
			}
		})
	}
}

func TestIsPlaceholderName(t *testing.T) {
	user := types.JID{User: "15550000001", Server: types.DefaultUserServer}
	lid := types.JID{User: "100000000000001", Server: types.HiddenUserServer}
	group := types.JID{User: "120363000000000001", Server: types.GroupServer}

	tests := []struct {
		name string
		jid  types.JID
		in   string
		want bool
	}{
		{"bare number", user, "15550000001", true},
		{"real contact name", user, "Test Contact", false},
		{"empty name", user, "", false},
		{"lid user part", lid, "100000000000001", true},
		{"group fallback", group, "Group 120363000000000001", true},
		{"real group name", group, "Test Group", false},
		{"other user's number", user, "15550000002", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPlaceholderName(tt.jid, tt.in); got != tt.want {
				t.Errorf("isPlaceholderName(%v, %q) = %v, want %v", tt.jid, tt.in, got, tt.want)
			}
		})
	}
}

func TestExtractDirectPathFromURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			"typical media url",
			"https://mmg.whatsapp.net/v/t62.7118-24/111_222_n.enc?ccb=11-4&oh=abc",
			"/v/t62.7118-24/111_222_n.enc",
		},
		{"no query", "https://mmg.whatsapp.net/d/f/abc.enc", "/d/f/abc.enc"},
		{"no .net domain returns input", "https://example.com/x", "https://example.com/x"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractDirectPathFromURL(tt.url); got != tt.want {
				t.Errorf("extractDirectPathFromURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestAnalyzeOggOpusRejectsNonOgg(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("Og"), []byte("RIFF....WAVE")} {
		if _, _, err := analyzeOggOpus(data); err == nil {
			t.Errorf("analyzeOggOpus(%q) returned nil error", data)
		}
	}
}

func TestPlaceholderWaveform(t *testing.T) {
	for _, d := range []uint32{1, 30, 300} {
		w := placeholderWaveform(d)
		if len(w) != 64 {
			t.Fatalf("placeholderWaveform(%d) length = %d, want 64", d, len(w))
		}
		for i, v := range w {
			if v > 100 {
				t.Errorf("placeholderWaveform(%d)[%d] = %d, want <= 100", d, i, v)
			}
		}
	}
}
