package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// On-demand history backfill (#20).
//
// POST /api/history {"chat_jid": ..., "count": 1-50 (default 50)} asks our
// phone for up to count messages older than the oldest message stored for
// the chat. The request goes to the phone as a peer message; the messages
// arrive later as an ON_DEMAND history sync and are stored like any other.

const (
	defaultHistoryCount = 50 // whatsmeow's recommended batch size
	maxHistoryCount     = 50
)

type HistoryRequest struct {
	ChatJID string `json:"chat_jid"`
	Count   int    `json:"count,omitempty"`
}

type HistoryResponse struct {
	Success         bool   `json:"success"`
	Message         string `json:"message"`
	RequestID       string `json:"request_id,omitempty"`
	OldestMessageID string `json:"oldest_message_id,omitempty"`
	Count           int    `json:"count,omitempty"`
}

// clientPeerSender sends peer messages (to our own phone) with client.
func clientPeerSender(s *apiServer) func(ctx context.Context, msg *waE2E.Message) (string, error) {
	return func(ctx context.Context, msg *waE2E.Message) (string, error) {
		if s.client == nil || !s.client.IsLoggedIn() {
			return "", errors.New("not connected to WhatsApp")
		}
		resp, err := s.client.SendPeerMessage(ctx, msg)
		return resp.ID, err
	}
}

// oldestMessage returns the anchor for a history request: the oldest stored
// message in chat.
func (store *MessageStore) oldestMessage(chatJID string) (types.MessageInfo, error) {
	var id, sender string
	var fromMe bool
	var ts time.Time
	err := store.db.QueryRow(`SELECT id, COALESCE(sender, ''), COALESCE(is_from_me, 0), timestamp
		FROM messages WHERE chat_jid = ? ORDER BY timestamp ASC, id ASC LIMIT 1`, chatJID).Scan(&id, &sender, &fromMe, &ts)
	if err != nil {
		return types.MessageInfo{}, err
	}
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return types.MessageInfo{}, err
	}
	info := types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, IsFromMe: fromMe, IsGroup: chat.Server == types.GroupServer},
		ID:            id,
		Timestamp:     ts,
	}
	if s, err := types.ParseJID(sender); err == nil {
		info.Sender = s
	}
	return info, nil
}

func writeHistory(w http.ResponseWriter, code int, resp HistoryResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(resp)
}

func (s *apiServer) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req HistoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHistory(w, http.StatusBadRequest, HistoryResponse{Message: "Invalid request format"})
		return
	}
	if req.ChatJID == "" || !strings.Contains(req.ChatJID, "@") {
		writeHistory(w, http.StatusBadRequest, HistoryResponse{Message: "chat_jid must be a chat JID such as 123@s.whatsapp.net, 123@lid or 123-456@g.us"})
		return
	}
	if _, err := types.ParseJID(req.ChatJID); err != nil {
		writeHistory(w, http.StatusBadRequest, HistoryResponse{Message: fmt.Sprintf("invalid chat_jid: %v", err)})
		return
	}
	if req.Count == 0 {
		req.Count = defaultHistoryCount
	}
	if req.Count < 1 || req.Count > maxHistoryCount {
		writeHistory(w, http.StatusBadRequest, HistoryResponse{Message: fmt.Sprintf("count must be between 1 and %d", maxHistoryCount)})
		return
	}

	anchor, err := s.store.oldestMessage(req.ChatJID)
	if errors.Is(err, sql.ErrNoRows) {
		writeHistory(w, http.StatusNotFound, HistoryResponse{Message: "no stored messages in this chat: a history request needs the oldest known message as its starting point"})
		return
	}
	if err != nil {
		writeHistory(w, http.StatusInternalServerError, HistoryResponse{Message: fmt.Sprintf("failed to read the chat's oldest message: %v", err)})
		return
	}

	// BuildHistorySyncRequest only reads its arguments, so it works on a nil
	// client too (tests).
	msg := (*whatsmeow.Client).BuildHistorySyncRequest(s.client, &anchor, req.Count)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	id, err := s.sendPeer(ctx, msg)
	if err != nil {
		writeHistory(w, http.StatusBadGateway, HistoryResponse{Message: fmt.Sprintf("failed to send the history request to your phone: %v", err)})
		return
	}
	fmt.Printf("History request %s: up to %d messages before %s in %s\n", id, req.Count, anchor.ID, req.ChatJID)
	writeHistory(w, http.StatusAccepted, HistoryResponse{
		Success:         true,
		Message:         fmt.Sprintf("Requested up to %d messages older than %s from your phone. They arrive asynchronously (usually within a minute, if the phone is online) and are stored like other history.", req.Count, anchor.Timestamp.UTC().Format(time.RFC3339)),
		RequestID:       id,
		OldestMessageID: anchor.ID,
		Count:           req.Count,
	})
}
