package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

// unwrapMessage returns the message inside FutureProofMessage wrappers
// (ephemeral, view-once, document with caption, group mention, ...).
// events.Message.UnwrapRaw already removes the common ones; this also
// catches nested and less common wrappers. Edits are not unwrapped here.
func unwrapMessage(msg *waE2E.Message) *waE2E.Message {
	for depth := 0; msg != nil && depth < 8; depth++ {
		var inner *waE2E.Message
		for _, w := range []*waE2E.FutureProofMessage{
			msg.GetEphemeralMessage(),
			msg.GetViewOnceMessage(),
			msg.GetViewOnceMessageV2(),
			msg.GetViewOnceMessageV2Extension(),
			msg.GetDocumentWithCaptionMessage(),
			msg.GetLottieStickerMessage(),
			msg.GetGroupMentionedMessage(),
			msg.GetBotInvokeMessage(),
			msg.GetPollCreationMessageV4(),
			msg.GetSpoilerMessage(),
		} {
			if m := w.GetMessage(); m != nil {
				inner = m
				break
			}
		}
		if inner == nil {
			return msg
		}
		msg = inner
	}
	return msg
}

// formatCoord formats a latitude or longitude without trailing zeros.
func formatCoord(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// pollText is the stored text of a poll: "[poll] question: opt1 / opt2".
func pollText(p *waE2E.PollCreationMessage) string {
	var opts []string
	for _, o := range p.GetOptions() {
		opts = append(opts, o.GetOptionName())
	}
	s := "[poll] " + p.GetName()
	if len(opts) > 0 {
		s += ": " + strings.Join(opts, " / ")
	}
	return s
}

// Extract text content from a message: the text, a media caption, or a
// stub for locations, contacts and polls (#15).
func extractTextContent(msg *waE2E.Message) string {
	msg = unwrapMessage(msg)
	if msg == nil {
		return ""
	}

	if text := msg.GetConversation(); text != "" {
		return text
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	switch {
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetCaption()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetCaption()
	case msg.GetPtvMessage() != nil:
		return msg.GetPtvMessage().GetCaption()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetCaption()
	}
	if loc := msg.GetLocationMessage(); loc != nil {
		s := fmt.Sprintf("[location %s,%s", formatCoord(loc.GetDegreesLatitude()), formatCoord(loc.GetDegreesLongitude()))
		label := loc.GetName()
		if label == "" {
			label = loc.GetAddress()
		}
		if label != "" {
			s += " " + label
		}
		return s + "]"
	}
	if live := msg.GetLiveLocationMessage(); live != nil {
		s := fmt.Sprintf("[live location %s,%s]", formatCoord(live.GetDegreesLatitude()), formatCoord(live.GetDegreesLongitude()))
		if c := live.GetCaption(); c != "" {
			s += " " + c
		}
		return s
	}
	if c := msg.GetContactMessage(); c != nil {
		return "[contact " + c.GetDisplayName() + "]"
	}
	if arr := msg.GetContactsArrayMessage(); arr != nil {
		var names []string
		for _, c := range arr.GetContacts() {
			if n := c.GetDisplayName(); n != "" {
				names = append(names, n)
			}
		}
		if len(names) == 0 && arr.GetDisplayName() != "" {
			names = append(names, arr.GetDisplayName())
		}
		return "[contacts " + strings.Join(names, ", ") + "]"
	}
	for _, p := range []*waE2E.PollCreationMessage{
		msg.GetPollCreationMessage(), msg.GetPollCreationMessageV2(), msg.GetPollCreationMessageV3(),
		msg.GetPollCreationMessageV5(), msg.GetPollCreationMessageV6(),
	} {
		if p != nil {
			return pollText(p)
		}
	}
	return ""
}

// contextInfoHolder is any message part that can carry ContextInfo.
type contextInfoHolder interface {
	GetContextInfo() *waE2E.ContextInfo
}

// extractReplyTo returns the ID of the message this one quotes (reply
// context), or "".
func extractReplyTo(msg *waE2E.Message) string {
	msg = unwrapMessage(msg)
	if msg == nil {
		return ""
	}
	for _, part := range []contextInfoHolder{
		msg.GetExtendedTextMessage(), msg.GetImageMessage(), msg.GetVideoMessage(), msg.GetPtvMessage(),
		msg.GetAudioMessage(), msg.GetDocumentMessage(), msg.GetStickerMessage(), msg.GetLocationMessage(),
		msg.GetLiveLocationMessage(), msg.GetContactMessage(), msg.GetContactsArrayMessage(),
		msg.GetPollCreationMessage(), msg.GetPollCreationMessageV2(), msg.GetPollCreationMessageV3(),
		msg.GetPollCreationMessageV5(), msg.GetPollCreationMessageV6(),
	} {
		// part is a typed nil when that field is unset; its getter handles that.
		if id := part.GetContextInfo().GetStanzaID(); id != "" {
			return id
		}
	}
	return ""
}

// Extract media info from a message
func extractMediaInfo(msg *waE2E.Message) (mediaType string, filename string, url string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	msg = unwrapMessage(msg)
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + time.Now().Format("20060102_150405") + ".jpg",
			img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	// Check for video message (a round video note is a video too)
	vid := msg.GetVideoMessage()
	if vid == nil {
		vid = msg.GetPtvMessage()
	}
	if vid != nil {
		return "video", "video_" + time.Now().Format("20060102_150405") + ".mp4",
			vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + time.Now().Format("20060102_150405") + ".ogg",
			aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		// The sender chooses this name: keep only a safe base name.
		filename, ok := safeMediaFilename(doc.GetFileName())
		if !ok {
			filename = "document_" + time.Now().Format("20060102_150405")
		}
		return "document", filename,
			doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	// Check for sticker message
	if st := msg.GetStickerMessage(); st != nil {
		return "sticker", "sticker_" + time.Now().Format("20060102_150405") + ".webp",
			st.GetURL(), st.GetMediaKey(), st.GetFileSHA256(), st.GetFileEncSHA256(), st.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}
