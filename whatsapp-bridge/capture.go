package main

import (
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
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
	kind                         string // "message", "reaction", "edit", "revoke", or "" if nothing to store
	stored                       bool   // a row was stored or updated
	targetID                     string // for a reaction, edit or revoke: the message it applies to
	content, mediaType, filename string
}

// processMessage stores one message (live or from history sync) under t:
// a regular message as a messages row (with its reply context), a reaction
// in the reactions table (#15), an edit or revoke as an update of the
// message it targets (#16). Other protocol messages are skipped.
func processMessage(store *MessageStore, evt *events.Message, t captureTarget) (processed, error) {
	var out processed
	msg := unwrapMessage(evt.Message)
	info := evt.Info
	chatJID := t.chat.String()

	if pm := msg.GetProtocolMessage(); pm != nil {
		// REVOKE is the enum's zero value: require the type to be set.
		if pm.Type == nil {
			return out, nil
		}
		out.targetID = pm.GetKey().GetID()
		if out.targetID == "" {
			return out, nil
		}
		var err error
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			out.kind = "revoke"
			out.stored, err = store.MarkDeleted(chatJID, out.targetID, t.sender, t.senderAlt,
				t.chat.Server == types.GroupServer, info.Timestamp)
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			at := info.Timestamp
			if ms := pm.GetTimestampMS(); ms > 0 {
				at = time.UnixMilli(ms)
			}
			out, err = applyEdit(store, chatJID, out.targetID, pm.GetEditedMessage(), at, t)
		}
		return out, err
	}
	// ParseWebMessage turns a history-sync edit into the new content under
	// the original message's ID.
	if src := evt.SourceWebMsg; src != nil && src.GetKey().GetID() != "" && src.GetKey().GetID() != info.ID {
		return applyEdit(store, chatJID, info.ID, msg, info.Timestamp, t)
	}

	if r := msg.GetReactionMessage(); r != nil {
		out.kind, out.targetID = "reaction", r.GetKey().GetID()
		if out.targetID == "" {
			return processed{}, nil
		}
		at := info.Timestamp
		if ms := r.GetSenderTimestampMS(); ms > 0 {
			at = time.UnixMilli(ms)
		}
		err := store.StoreReaction(chatJID, out.targetID, t.sender, r.GetText(), at)
		out.stored = err == nil
		return out, err
	}

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
	err := store.storeMessageRow(messageRow{
		id: info.ID, chatJID: chatJID, sender: t.sender, senderAlt: t.senderAlt, content: out.content,
		timestamp: info.Timestamp, isFromMe: info.IsFromMe, mediaType: mediaType, filename: filename, url: url,
		mediaKey: mediaKey, fileSHA256: fileSHA256, fileEncSHA256: fileEncSHA256, fileLength: fileLength,
		replyTo: extractReplyTo(msg),
	})
	out.stored = err == nil
	return out, err
}

// applyEdit applies an edit of message targetID to newMsg's text or caption.
func applyEdit(store *MessageStore, chatJID, targetID string, newMsg *waE2E.Message, at time.Time, t captureTarget) (processed, error) {
	out := processed{kind: "edit", targetID: targetID, content: extractTextContent(newMsg)}
	if out.content == "" {
		return out, nil
	}
	var err error
	out.stored, err = store.ApplyEdit(chatJID, targetID, t.sender, t.senderAlt, out.content, at)
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
