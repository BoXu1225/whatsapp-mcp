package main

import (
	"context"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	storeHistorySync(messageStore, clientIdentity(client), historySync, clientChatNamer(client, messageStore, logger), logger)
}

// storeHistorySync stores history-sync conversations under their canonical
// chat and sender JIDs (identity.go), merging a phone-number chat for the
// same person into the canonical LID chat.
func storeHistorySync(messageStore *MessageStore, id Identity, historySync *events.HistorySync, name chatNamer, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		// Try to parse the JID
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
		chatJID := jid.String()
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
				if err := messageStore.MergeChat(pn.String(), chatJID, id); err != nil {
					logger.Warnf("Failed to merge chat %s into %s: %v", pn, chatJID, err)
				}
			}
		}

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			// Get appropriate chat name by passing the history sync conversation directly
			if err := messageStore.StoreChat(chatJID, name(jid, conversation), timestamp); err != nil {
				logger.Warnf("Failed to store chat: %v", err)
			}

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Extract text content
				var content string
				if msg.Message.Message != nil {
					if conv := msg.Message.Message.GetConversation(); conv != "" {
						content = conv
					} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
						content = ext.GetText()
					}
				}

				// Extract media info
				var mediaType, filename, url string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				if msg.Message.Message != nil {
					mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
				}

				// Skip messages with no content and no media
				if content == "" && mediaType == "" {
					continue
				}

				// Determine sender
				isFromMe := msg.Message.GetKey().GetFromMe()
				sender, senderAlt := id.HistorySender(jid, isFromMe, msg.Message.GetKey().GetParticipant())

				// Store message
				msgID := msg.Message.GetKey().GetID()

				// Get message timestamp
				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				err = messageStore.StoreMessageWithAlt(
					msgID,
					chatJID,
					sender,
					senderAlt,
					content,
					timestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
					// Per-message logging (with content) only with -debug
					if mediaType != "" {
						debugInfof(logger, "Stored message: [%s] %s -> %s: [%s: %s] %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename, content)
					} else {
						debugInfof(logger, "Stored message: [%s] %s -> %s: %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
					}
				}
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}

// Request history sync from the server
func requestHistorySync(client *whatsmeow.Client) {
	if client == nil {
		fmt.Println("Client is not initialized. Cannot request history sync.")
		return
	}

	if !client.IsConnected() {
		fmt.Println("Client is not connected. Please ensure you are connected to WhatsApp first.")
		return
	}

	if client.Store.ID == nil {
		fmt.Println("Client is not logged in. Please scan the QR code first.")
		return
	}

	// Build and send a history sync request
	historyMsg := client.BuildHistorySyncRequest(nil, 100)
	if historyMsg == nil {
		fmt.Println("Failed to build history sync request.")
		return
	}

	_, err := client.SendMessage(context.Background(), types.JID{
		Server: "s.whatsapp.net",
		User:   "status",
	}, historyMsg)

	if err != nil {
		fmt.Printf("Failed to request history sync: %v\n", err)
	} else {
		fmt.Println("History sync requested. Waiting for server response...")
	}
}
