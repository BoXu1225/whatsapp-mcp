package main

import (
	"context"
	"fmt"
	"reflect"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	// First, check if chat already exists in database with a name
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	if err == nil && existingName != "" && !isPlaceholderName(jid, existingName) {
		// Chat exists with a name, use that
		debugInfof(logger, "Using existing chat name for %s: %s", chatJID, existingName)
		return existingName
	}

	// Need to determine chat name
	var name string

	if jid.Server == "g.us" {
		// This is a group chat
		debugInfof(logger, "Getting name for group: %s", chatJID)

		// Use conversation data if provided (from history sync)
		if conversation != nil {
			// Extract name from conversation if available
			// This uses type assertions to handle different possible types
			var displayName, convName *string
			// Try to extract the fields we care about regardless of the exact type
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			// Use the name we found
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		// If we didn't get a name, try group info
		if name == "" {
			groupInfo, err := client.GetGroupInfo(context.Background(), jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				// Fallback name for groups
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

		debugInfof(logger, "Using group name: %s", name)
	} else {
		// This is an individual contact
		debugInfof(logger, "Getting name for contact: %s", chatJID)

		// Use contact info, falling back to the JID. The sender isn't a safe
		// fallback: for a message we sent, it's our own number.
		name = resolveContactName(client, jid)
		if name == "" {
			name = jid.User
		}

		debugInfof(logger, "Using contact name: %s", name)
	}

	return name
}

// isPlaceholderName reports whether a stored chat name is only the fallback
// built from the JID, so a real name should be looked up again.
func isPlaceholderName(jid types.JID, name string) bool {
	return name == jid.User || name == fmt.Sprintf("Group %s", jid.User)
}

// resolveContactName looks up a contact's name in the device store. Chats with
// @lid JIDs often have no contact entry of their own, so it also tries the
// phone-number JID mapped to the LID. Returns "" if nothing is found.
func resolveContactName(client *whatsmeow.Client, jid types.JID) string {
	ctx := context.Background()
	lookup := func(j types.JID) string {
		contact, err := client.Store.Contacts.GetContact(ctx, j)
		if err != nil || !contact.Found {
			return ""
		}
		for _, n := range []string{contact.FullName, contact.FirstName, contact.BusinessName, contact.PushName} {
			if n != "" {
				return n
			}
		}
		return ""
	}

	if name := lookup(jid); name != "" {
		return name
	}
	if jid.Server == types.HiddenUserServer {
		pn, err := client.Store.LIDs.GetPNForLID(ctx, jid)
		if err == nil && !pn.IsEmpty() {
			return lookup(pn)
		}
	}
	return ""
}

// backfillChatNames replaces placeholder names of individual chats stored
// before their contact name could be resolved.
func backfillChatNames(client *whatsmeow.Client, messageStore *MessageStore, logger waLog.Logger) {
	rows, err := messageStore.db.Query("SELECT jid, name FROM chats")
	if err != nil {
		logger.Warnf("Failed to read chats for name backfill: %v", err)
		return
	}
	updates := map[string]string{}
	for rows.Next() {
		var chatJID, name string
		if err := rows.Scan(&chatJID, &name); err != nil {
			continue
		}
		jid, err := types.ParseJID(chatJID)
		if err != nil || jid.Server == "g.us" || !isPlaceholderName(jid, name) {
			continue
		}
		if resolved := resolveContactName(client, jid); resolved != "" {
			updates[chatJID] = resolved
		}
	}
	rows.Close()

	for chatJID, name := range updates {
		if _, err := messageStore.db.Exec("UPDATE chats SET name = ? WHERE jid = ?", name, chatJID); err != nil {
			logger.Warnf("Failed to update name for %s: %v", chatJID, err)
		}
	}
	logger.Infof("Chat name backfill: updated %d chats", len(updates))
}
