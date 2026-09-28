package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
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

	// OriginalFilename is the stored name (the sender's, for documents);
	// Filename is the local <message ID>.<ext> name.
	OriginalFilename string `json:"original_filename,omitempty"`
	// RetryRequested: the media had expired and a re-upload was requested
	// from the sender's phone (HTTP 202); ask again shortly.
	RetryRequested bool `json:"retry_requested,omitempty"`
}

// apiServer holds what the REST handlers need. send defaults to
// sendWhatsAppMessage on client; tests replace it with a stub.
type apiServer struct {
	client *whatsmeow.Client
	store  *MessageStore
	send   func(recipient, message, mediaPath string, mediaData []byte) (bool, string)

	token string // required X-Bridge-Token value
	port  int    // port the listener is bound to; the Host header must match

	allowedDirs []string // media_path must resolve inside one of these

	// /api/health state. isConnected and isLoggedIn default to asking client
	// (false while it is nil); tests replace them.
	health      *bridgeHealth
	isConnected func() bool
	isLoggedIn  func() bool

	media *mediaService // downloads and media retries

	// sendPeer sends a message to our own phone (history requests); defaults
	// to client.SendPeerMessage.
	sendPeer func(ctx context.Context, msg *waE2E.Message) (string, error)
}

func newAPIServer(client *whatsmeow.Client, messageStore *MessageStore) *apiServer {
	s := &apiServer{client: client, store: messageStore, health: newBridgeHealth(time.Now())}
	s.media = newMediaService(clientFetcher(client), messageStore)
	s.sendPeer = clientPeerSender(s)
	s.send = func(recipient, message, mediaPath string, mediaData []byte) (bool, string) {
		return sendWhatsAppMessage(s.client, recipient, message, mediaPath, mediaData)
	}
	s.isConnected = func() bool { return s.client != nil && s.client.IsConnected() }
	s.isLoggedIn = func() bool { return s.client != nil && s.client.IsLoggedIn() }
	return s
}

// handler returns the REST API on its own mux.
func (s *apiServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/send", s.handleSend)
	mux.HandleFunc("/api/download", s.handleDownload)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/history", s.handleHistory)
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

	// Media is read from the handle openSendFile checked, not re-opened by
	// path, so the file can't be swapped after the allowlist check.
	var mediaData []byte
	if req.MediaPath != "" {
		f, resolved, err := openSendFile(req.MediaPath, s.allowedDirs)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(SendMessageResponse{Success: false, Message: err.Error()})
			return
		}
		mediaData, err = io.ReadAll(f)
		f.Close()
		if err != nil {
			http.Error(w, "Failed to read media file", http.StatusInternalServerError)
			return
		}
		req.MediaPath = resolved
	}

	fmt.Printf("Send request: recipient=%s text=%d chars media=%v\n", req.Recipient, len(req.Message), req.MediaPath != "")
	debugPrintf("Send request content: %q media_path=%q\n", req.Message, req.MediaPath)

	// Send the message
	success, message := s.send(req.Recipient, req.Message, req.MediaPath, mediaData)
	fmt.Printf("Send result: recipient=%s success=%v\n", req.Recipient, success)
	debugPrintf("Send result message: %s\n", message)
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

	res, err := s.media.download(req.MessageID, req.ChatJID)
	w.Header().Set("Content-Type", "application/json")
	if errors.Is(err, errMediaRetryRequested) {
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:          false,
			Message:          err.Error(),
			OriginalFilename: res.OriginalFilename,
			RetryRequested:   true,
		})
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to download media: %s", err),
		})
		return
	}
	json.NewEncoder(w).Encode(DownloadMediaResponse{
		Success:          true,
		Message:          fmt.Sprintf("Successfully downloaded %s media", res.MediaType),
		Filename:         res.Filename,
		OriginalFilename: res.OriginalFilename,
		Path:             res.Path,
	})
}

// listenREST binds the REST API to 127.0.0.1:port (0 picks a free port).
// main calls it before connecting to WhatsApp, so a second bridge instance
// fails here instead of taking over the first one's WhatsApp session.
func listenREST(port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("REST API listen on 127.0.0.1:%d: %w", port, err)
	}
	return ln, nil
}

// serveREST serves the API on ln in the background. It records the bound
// port on s, since the Host check needs it. The channel receives the error
// if the server stops for any reason other than Shutdown, and is closed
// when it stops.
func serveREST(s *apiServer, ln net.Listener) (*http.Server, <-chan error) {
	s.port = ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("Starting REST API server on %s...\n", ln.Addr())

	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 1)
	go func() {
		defer close(errs)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("REST API server stopped: %w", err)
		}
	}()
	return srv, errs
}
