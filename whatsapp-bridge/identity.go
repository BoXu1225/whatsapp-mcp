package main

import (
	"context"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// How chats and senders are stored (#8, #9):
//
//   - Chat JID: for a 1:1 chat, the person's LID JID (<lid>@lid) when the LID
//     is known, else their phone JID (<pn>@s.whatsapp.net). Groups, broadcasts
//     and newsletters as delivered.
//   - Sender: the full JID without device part (user@server). In a 1:1 chat,
//     the other person's messages use the chat JID. In groups, the form
//     WhatsApp delivered (a PN-addressed group has PN senders).
//   - Own messages: our LID in LID chats, our phone JID everywhere else
//     (PN chats and groups), so a chat's own messages all use one form.
//   - sender_alt: the sender's other form (PN for a LID sender, LID for a PN
//     sender) when known, else NULL.

// LIDLookup maps between phone-number and LID JIDs. whatsmeow's
// client.Store.LIDs implements it; tests use an in-memory map. Lookups that
// find nothing return an empty JID and no error.
type LIDLookup interface {
	GetPNForLID(ctx context.Context, lid types.JID) (types.JID, error)
	GetLIDForPN(ctx context.Context, pn types.JID) (types.JID, error)
}

// Identity is what the bridge needs to store canonical chat and sender JIDs:
// our own phone and LID JIDs and the LID map. Any part may be empty.
type Identity struct {
	OwnPN  types.JID
	OwnLID types.JID
	LIDs   LIDLookup
}

// clientIdentity reads our JIDs and the LID map from the device store. It
// works before the client connects. A nil or logged-out client gives a
// partial identity.
func clientIdentity(client *whatsmeow.Client) Identity {
	var id Identity
	if client == nil || client.Store == nil {
		return id
	}
	if client.Store.ID != nil {
		id.OwnPN = client.Store.ID.ToNonAD()
	}
	if !client.Store.LID.IsEmpty() {
		id.OwnLID = client.Store.LID.ToNonAD()
	}
	if client.Store.LIDs != nil {
		id.LIDs = client.Store.LIDs
	}
	return id
}

func isPersonJID(j types.JID) bool {
	return j.Server == types.DefaultUserServer || j.Server == types.HiddenUserServer
}

// Alt returns the other form of a person's JID (PN for a LID, LID for a PN),
// without device part, or an empty JID if unknown.
func (id Identity) Alt(j types.JID) types.JID {
	j = j.ToNonAD()
	if !isPersonJID(j) {
		return types.EmptyJID
	}
	if !id.OwnPN.IsEmpty() && !id.OwnLID.IsEmpty() {
		if j == id.OwnPN {
			return id.OwnLID
		}
		if j == id.OwnLID {
			return id.OwnPN
		}
	}
	if id.LIDs == nil {
		return types.EmptyJID
	}
	var alt types.JID
	var err error
	if j.Server == types.HiddenUserServer {
		alt, err = id.LIDs.GetPNForLID(context.Background(), j)
	} else {
		alt, err = id.LIDs.GetLIDForPN(context.Background(), j)
	}
	if err != nil || alt.IsEmpty() {
		return types.EmptyJID
	}
	return alt.ToNonAD()
}

// CanonicalChat returns the JID to store a chat under: the LID JID for a
// phone-number chat whose LID is known (from the LID map or a hint such as
// the history conversation's lidJID), otherwise the chat without device part.
func (id Identity) CanonicalChat(chat types.JID, hints ...types.JID) types.JID {
	chat = chat.ToNonAD()
	if chat.Server != types.DefaultUserServer {
		return chat
	}
	if lid := id.Alt(chat); !lid.IsEmpty() {
		return lid
	}
	for _, h := range hints {
		if h.Server == types.HiddenUserServer && h.User != "" {
			return h.ToNonAD()
		}
	}
	return chat
}

// OwnSender returns our own sender JID for a message in chat, and its alt:
// our LID in a LID chat, our phone JID otherwise. Falls back to whichever of
// the two is known.
func (id Identity) OwnSender(chat types.JID) (string, string) {
	primary, other := id.OwnPN, id.OwnLID
	if chat.Server == types.HiddenUserServer {
		primary, other = id.OwnLID, id.OwnPN
	}
	if primary.IsEmpty() {
		primary, other = other, types.EmptyJID
	}
	return jidString(primary), jidString(other)
}

func jidString(j types.JID) string {
	if j.IsEmpty() {
		return ""
	}
	return j.ToNonAD().String()
}

// senderWithAlt is the canonical sender string for a delivered JID, and its
// alt (the given alt if set, else from the LID map).
func (id Identity) senderWithAlt(sender, alt types.JID) (string, string) {
	sender = sender.ToNonAD()
	alt = alt.ToNonAD()
	if alt.IsEmpty() || alt.Server == sender.Server {
		alt = id.Alt(sender)
	}
	return jidString(sender), jidString(alt)
}

// LiveSender returns the sender and sender_alt to store for a live message in
// chat (the canonical chat JID).
func (id Identity) LiveSender(info types.MessageInfo, chat types.JID) (string, string) {
	if info.IsFromMe {
		if s, alt := id.OwnSender(chat); s != "" {
			return s, alt
		}
	}
	if isPersonJID(chat) && !info.IsFromMe {
		// In a 1:1 chat, the other person: use the chat's form.
		alt := types.EmptyJID
		for _, j := range []types.JID{info.Sender, info.SenderAlt} {
			if isPersonJID(j) && j.Server != chat.Server {
				alt = j
			}
		}
		return id.senderWithAlt(chat, alt)
	}
	return id.senderWithAlt(info.Sender, info.SenderAlt)
}

// HistorySender returns the sender and sender_alt for a history-sync message
// in chat (the canonical chat JID). participant is the message key's
// participant (set in groups).
func (id Identity) HistorySender(chat types.JID, fromMe bool, participant string) (string, string) {
	if fromMe {
		if s, alt := id.OwnSender(chat); s != "" {
			return s, alt
		}
	}
	if participant != "" && !fromMe {
		if p, err := types.ParseJID(participant); err == nil {
			return id.senderWithAlt(p, types.EmptyJID)
		}
		return participant, ""
	}
	if isPersonJID(chat) {
		return id.senderWithAlt(chat, types.EmptyJID)
	}
	return chat.String(), ""
}

// resolveBareUser turns a bare user part (as older bridges stored senders)
// into a full JID, or returns ok=false if its server can't be determined.
// chat is the message's chat; chatUsers maps users with a direct chat to that
// chat's server.
func (id Identity) resolveBareUser(user string, chat types.JID, chatUsers map[string]string) (types.JID, bool) {
	if isPersonJID(chat) && chat.User == user {
		return chat, true
	}
	lid := types.JID{User: user, Server: types.HiddenUserServer}
	pn := types.JID{User: user, Server: types.DefaultUserServer}
	isLID := !id.Alt(lid).IsEmpty()
	isPN := !id.Alt(pn).IsEmpty()
	switch {
	case isLID && !isPN:
		return lid, true
	case isPN && !isLID:
		return pn, true
	case isLID && isPN:
		return types.EmptyJID, false
	}
	switch chatUsers[user] {
	case types.HiddenUserServer:
		return lid, true
	case types.DefaultUserServer:
		return pn, true
	}
	return types.EmptyJID, false
}

// parseStoredSender parses a stored sender: a full JID (possibly with a
// device part) or a bare user. bare is true for the latter.
func parseStoredSender(s string) (j types.JID, bare bool, ok bool) {
	if s == "" {
		return types.EmptyJID, false, false
	}
	if !strings.Contains(s, "@") {
		return types.EmptyJID, true, true
	}
	j, err := types.ParseJID(s)
	if err != nil {
		return types.EmptyJID, false, false
	}
	return j, false, true
}
