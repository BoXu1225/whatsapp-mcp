package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"go.mau.fi/whatsmeow"
)

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// apiServer holds what the REST handlers need. send defaults to
// sendWhatsAppMessage on client; tests replace it with a stub.
type apiServer struct {
	client *whatsmeow.Client
	store  *MessageStore
	send   func(recipient, message, mediaPath string) (bool, string)

	token string // required X-Bridge-Token value
	port  int    // port the listener is bound to; the Host header must match
}

func newAPIServer(client *whatsmeow.Client, messageStore *MessageStore) *apiServer {
	s := &apiServer{client: client, store: messageStore}
	s.send = func(recipient, message, mediaPath string) (bool, string) {
		return sendWhatsAppMessage(s.client, recipient, message, mediaPath)
	}
	return s
}

// handler returns the REST API on its own mux.
func (s *apiServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/send", s.handleSend)
	mux.HandleFunc("/api/download", s.handleDownload)
	return s.guard(mux)
}

// Handler for sending messages
func (s *apiServer) handleSend(w http.ResponseWriter, r *http.Request) {
	// Only allow POST requests
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse the request body
	var req SendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request format", http.StatusBadRequest)
		return
	}

	// Validate request
	if req.Recipient == "" {
		http.Error(w, "Recipient is required", http.StatusBadRequest)
		return
	}

	if req.Message == "" && req.MediaPath == "" {
		http.Error(w, "Message or media path is required", http.StatusBadRequest)
		return
	}

	fmt.Println("Received request to send message", req.Message, req.MediaPath)

	// Send the message
	success, message := s.send(req.Recipient, req.Message, req.MediaPath)
	fmt.Println("Message sent", success, message)
	// Set response headers
	w.Header().Set("Content-Type", "application/json")

	// Set appropriate status code
	if !success {
		w.WriteHeader(http.StatusInternalServerError)
	}

	// Send response
	json.NewEncoder(w).Encode(SendMessageResponse{
		Success: success,
		Message: message,
	})
}

// Handler for downloading media
func (s *apiServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	// Only allow POST requests
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse the request body
	var req DownloadMediaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request format", http.StatusBadRequest)
		return
	}

	// Validate request
	if req.MessageID == "" || req.ChatJID == "" {
		http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
		return
	}

	// Download the media
	success, mediaType, filename, path, err := downloadMedia(s.client, s.store, req.MessageID, req.ChatJID)

	// Set response headers
	w.Header().Set("Content-Type", "application/json")

	// Handle download result
	if !success || err != nil {
		errMsg := "Unknown error"
		if err != nil {
			errMsg = err.Error()
		}

		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to download media: %s", errMsg),
		})
		return
	}

	// Send successful response
	json.NewEncoder(w).Encode(DownloadMediaResponse{
		Success:  true,
		Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
		Filename: filename,
		Path:     path,
	})
}

// Start a REST API server to expose the WhatsApp client functionality
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, port int) {
	s := newAPIServer(client, messageStore)

	// Start the server
	serverAddr := fmt.Sprintf("127.0.0.1:%d", port)
	fmt.Printf("Starting REST API server on %s...\n", serverAddr)

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := http.ListenAndServe(serverAddr, s.handler()); err != nil {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()
}
