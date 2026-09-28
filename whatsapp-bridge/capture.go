package main

import (
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waWeb"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Live and history-sync messages go through the same path (#17): history
// messages are turned into *events.Message by ParseWebMessage (which unwraps
// ephemeral, view-once and document-with-caption wrappers, like live
// messages), then both are stored by processMessage.

// captureTarget is where processMessage stores a message: the canonical chat
// (see identity.go), a lazily computed chat name, and the canonical sender.
type captureTarget struct {
	chat      types.JID
	chatName  func() string
	sender    string
	senderAlt string
}

// processed describes what processMessage did, for logging.
type processed struct {
	kind                         string // "message", or "" if nothing was stored
	stored                       bool
	content, mediaType, filename string
}

// processMessage stores one message (live or from history sync) under t.
func processMessage(store *MessageStore, evt *events.Message, t captureTarget) (processed, error) {
	var out processed
	msg := evt.Message
	info := evt.Info
	chatJID := t.chat.String()

	out.content = extractTextContent(msg)
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg)
	out.mediaType, out.filename = mediaType, filename
	if out.content == "" && mediaType == "" {
		return out, nil
	}
	out.kind = "message"
	name := ""
	if t.chatName != nil {
		name = t.chatName()
	}
	if err := store.StoreChat(chatJID, name, info.Timestamp); err != nil {
		return out, err
	}
	err := store.StoreMessageWithAlt(
		info.ID, chatJID, t.sender, t.senderAlt, out.content, info.Timestamp, info.IsFromMe,
		mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
	out.stored = err == nil
	return out, err
}

// webParser turns a history-sync message into a message event.
// (*whatsmeow.Client).ParseWebMessage is one.
type webParser func(chat types.JID, msg *waWeb.WebMessageInfo) (*events.Message, error)

// identityWebParser is ParseWebMessage on a client that knows only our own
// JIDs, which is all ParseWebMessage uses. For callers without a client
// (tests). If our JIDs are unknown, own messages parse with the chat as the
// sender; the history path stores its own sender anyway (HistorySender).
func identityWebParser(id Identity) webParser {
	return func(chat types.JID, msg *waWeb.WebMessageInfo) (*events.Message, error) {
		dev := &wastore.Device{LID: id.OwnLID}
		if !id.OwnPN.IsEmpty() {
			pn := id.OwnPN
			dev.ID = &pn
		} else if id.OwnLID.IsEmpty() {
			c := chat.ToNonAD()
			dev.ID = &c
		}
		return (&whatsmeow.Client{Store: dev}).ParseWebMessage(chat, msg)
	}
}
