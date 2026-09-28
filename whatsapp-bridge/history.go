package main

import (
	"fmt"
	"sort"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	id := clientIdentity(client)
	parse := identityWebParser(id)
	if client != nil && client.Store != nil && client.Store.ID != nil {
		parse = client.ParseWebMessage
	}
	storeHistorySyncWith(messageStore, id, parse, historySync, clientChatNamer(client, messageStore, logger), logger)
}

// storeHistorySync stores a history-sync batch, parsing messages with only
// our own JIDs (see identityWebParser).
func storeHistorySync(messageStore *MessageStore, id Identity, historySync *events.HistorySync, name chatNamer, logger waLog.Logger) {
	storeHistorySyncWith(messageStore, id, identityWebParser(id), historySync, name, logger)
}

// historyConversation is one conversation of a batch, ready to store.
type historyConversation struct {
	chat     types.JID
	name     string
	convTime time.Time // conversationTimestamp; zero if unset
	msgs     []*events.Message
}

// storeHistorySyncWith stores history-sync conversations under their
// canonical chat and sender JIDs (identity.go), merging a phone-number chat
// for the same person into the canonical LID chat. Each message is parsed
// with parse (unwrapping it like a live message) and stored by
// processMessage, oldest first, so an edit or reaction in the batch finds
// its target. Chat merges and names are resolved first; then the whole
// batch is written in one transaction.
func storeHistorySyncWith(messageStore *MessageStore, id Identity, parse webParser, historySync *events.HistorySync, name chatNamer, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	var convs []historyConversation
	for _, conversation := range historySync.Data.Conversations {
		if conversation.ID == nil || len(conversation.Messages) == 0 {
			continue
		}
		convJID, err := types.ParseJID(*conversation.ID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", *conversation.ID, err)
			continue
		}

		var lidHint types.JID
		if l, err := types.ParseJID(conversation.GetLidJID()); err == nil {
			lidHint = l
		}
		jid := id.CanonicalChat(convJID, lidHint)
		if jid.Server == types.HiddenUserServer {
			pn := convJID.ToNonAD()
			if pn.Server != types.DefaultUserServer {
				pn = id.Alt(jid)
				if pn.IsEmpty() {
					if p, err := types.ParseJID(conversation.GetPnJID()); err == nil {
						pn = p.ToNonAD()
					}
				}
			}
			if pn.Server == types.DefaultUserServer && pn.User != "" {
				if err := messageStore.MergeChat(pn.String(), jid.String(), id); err != nil {
					logger.Warnf("Failed to merge chat %s into %s: %v", pn, jid, err)
				}
			}
		}

		conv := historyConversation{chat: jid}
		if ts := conversation.GetConversationTimestamp(); ts != 0 {
			conv.convTime = time.Unix(int64(ts), 0)
		}
		// History lists messages newest first: walk it backwards, then sort
		// (stably) by time, so the batch is stored oldest first.
		for i := len(conversation.Messages) - 1; i >= 0; i-- {
			webMsg := conversation.Messages[i].GetMessage()
			if webMsg == nil || webMsg.GetMessageTimestamp() == 0 {
				continue
			}
			evt, err := parse(convJID, webMsg)
			if err != nil {
				logger.Warnf("Failed to parse history message %s: %v", webMsg.GetKey().GetID(), err)
				continue
			}
			conv.msgs = append(conv.msgs, evt)
		}
		sort.SliceStable(conv.msgs, func(i, j int) bool {
			return conv.msgs[i].Info.Timestamp.Before(conv.msgs[j].Info.Timestamp)
		})
		// Names may need the network (group info): resolve outside the transaction.
		conv.name = name(jid, conversation)
		convs = append(convs, conv)
	}

	syncedCount := 0
	err := messageStore.InTx(func(tx *MessageStore) error {
		for _, conv := range convs {
			chatJID := conv.chat.String()
			chatName := conv.name
			last := conv.convTime
			for _, evt := range conv.msgs {
				webMsg := evt.SourceWebMsg
				participant := webMsg.GetParticipant()
				if participant == "" {
					participant = webMsg.GetKey().GetParticipant()
				}
				sender, senderAlt := id.HistorySender(conv.chat, evt.Info.IsFromMe, participant)
				res, err := processMessage(tx, evt, captureTarget{
					chat:      conv.chat,
					chatName:  func() string { return chatName },
					sender:    sender,
					senderAlt: senderAlt,
				})
				if err != nil {
					logger.Warnf("Failed to store history message %s: %v", evt.Info.ID, err)
					continue
				}
				// Reactions, edits and revokes don't count as chat activity.
				if !res.stored || res.kind != "message" {
					continue
				}
				if evt.Info.Timestamp.After(last) {
					last = evt.Info.Timestamp
				}
				syncedCount++
				// Per-message logging (with content) only with -debug
				ts := evt.Info.Timestamp.Format("2006-01-02 15:04:05")
				if res.mediaType != "" {
					debugInfof(logger, "Stored message: [%s] %s -> %s: [%s: %s] %s",
						ts, sender, chatJID, res.mediaType, res.filename, res.content)
				} else {
					debugInfof(logger, "Stored message: [%s] %s -> %s: %s", ts, sender, chatJID, res.content)
				}
			}
			if last.IsZero() {
				continue
			}
			if err := tx.StoreChat(chatJID, chatName, last); err != nil {
				logger.Warnf("Failed to store chat: %v", err)
			}
		}
		return nil
	})
	if err != nil {
		logger.Warnf("Failed to store history sync batch: %v", err)
		fmt.Println("History sync failed; nothing from this batch was stored.")
		return
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}
