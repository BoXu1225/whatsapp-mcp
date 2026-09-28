package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// chatNamer returns the display name to store for a chat. conversation is the
// history-sync conversation, or nil for live messages.
type chatNamer func(jid types.JID, conversation interface{}) string

func clientChatNamer(client *whatsmeow.Client, messageStore *MessageStore, logger waLog.Logger) chatNamer {
	return func(jid types.JID, conversation interface{}) string {
		return GetChatName(client, messageStore, jid, jid.String(), conversation, "", logger)
	}
}

// liveStored describes a stored live message, for logging.
type liveStored struct {
	kind                       string // see processed
	target                     string // message a reaction, edit or revoke applies to
	stored                     bool
	chatJID, sender            string
	content, mediaType, fileNm string
}

// storeLiveMessage stores a live message under its canonical chat and sender
// JIDs (identity.go). A phone-number chat for the same person is merged into
// the canonical LID chat first.
func storeLiveMessage(messageStore *MessageStore, id Identity, msg *events.Message, name chatNamer) (liveStored, error) {
	info := msg.Info
	// Hints for the chat's LID: in a 1:1 chat, the other side's alt address.
	var hints []types.JID
	if info.Chat.Server == types.DefaultUserServer {
		if info.IsFromMe {
			hints = append(hints, info.RecipientAlt)
		} else {
			hints = append(hints, info.SenderAlt)
		}
	}
	chat := id.CanonicalChat(info.Chat, hints...)
	chatJID := chat.String()
	if chat.Server == types.HiddenUserServer {
		pn := info.Chat.ToNonAD()
		if pn.Server != types.DefaultUserServer {
			pn = id.Alt(chat)
		}
		if !pn.IsEmpty() {
			if err := messageStore.MergeChat(pn.String(), chatJID, id); err != nil {
				return liveStored{}, fmt.Errorf("failed to merge chat %s into %s: %v", pn, chatJID, err)
			}
		}
	}
	sender, senderAlt := id.LiveSender(info, chat)
	out := liveStored{chatJID: chatJID, sender: sender}

	res, err := processMessage(messageStore, msg, captureTarget{
		chat:      chat,
		chatName:  func() string { return name(chat, nil) },
		sender:    sender,
		senderAlt: senderAlt,
	})
	out.kind, out.stored, out.target = res.kind, res.stored, res.targetID
	out.content, out.mediaType, out.fileNm = res.content, res.mediaType, res.filename
	return out, err
}

// Handle regular incoming messages with media support
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) bool {
	res, err := storeLiveMessage(messageStore, clientIdentity(client), msg, clientChatNamer(client, messageStore, logger))
	chatJID, sender := res.chatJID, res.sender
	content, mediaType, filename := res.content, res.mediaType, res.fileNm
	if err == nil && !res.stored {
		return true
	}

	if err != nil {
		// Not acknowledged: whatsmeow redelivers it later (see bridgeEvents).
		logger.Errorf("Failed to store message %s, not acknowledging it: %v", msg.Info.ID, err)
		return false
	} else {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Content, filenames and names only with -debug
		if res.kind != "message" {
			// Reactions, edits, deletes: IDs only.
			fmt.Printf("[%s] %s %s in %s: id=%s %s of %s\n", timestamp, direction, sender, chatJID, msg.Info.ID, res.kind, res.target)
		} else if debugLogging {
			if mediaType != "" {
				fmt.Printf("[%s] %s %s: [%s: %s] %s\n", timestamp, direction, sender, mediaType, filename, content)
			} else if content != "" {
				fmt.Printf("[%s] %s %s: %s\n", timestamp, direction, sender, content)
			}
		} else {
			kind := "text"
			if mediaType != "" {
				kind = mediaType
			}
			fmt.Printf("[%s] %s %s in %s: id=%s %s, %d chars\n", timestamp, direction, sender, chatJID, msg.Info.ID, kind, len(content))
		}
	}
	return true
}

// bridgeEvents is the bridge's whatsmeow event handler. It is registered with
// AddEventHandlerWithSuccessStatus: returning false for a message whose store
// failed makes whatsmeow skip the delivery receipt, so the server redelivers
// it on a later connection, and the decrypted event buffer (configureClient)
// hands the plaintext to the handler again.
type bridgeEvents struct {
	client *whatsmeow.Client
	store  *MessageStore
	logger waLog.Logger
}

func newBridgeEvents(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) *bridgeEvents {
	return &bridgeEvents{client: client, store: store, logger: logger}
}

// handle processes one event and reports whether it may be acknowledged.
func (b *bridgeEvents) handle(evt any) bool {
	switch v := evt.(type) {
	case *events.Message:
		return handleMessage(b.client, b.store, v, b.logger)

	case *events.HistorySync:
		handleHistorySync(b.client, b.store, v, b.logger)

	case *events.Connected:
		b.logger.Infof("Connected to WhatsApp")
		go backfillChatNames(b.client, b.store, b.logger)

	case *events.LoggedOut:
		b.logger.Warnf("Device logged out, please scan QR code to log in again")
	}
	return true
}

// configureClient sets the whatsmeow options the bridge relies on.
func configureClient(client *whatsmeow.Client) {
	// Keep decrypted messages until the handler succeeded, so a message whose
	// store failed (handler returned false, no receipt sent) is replayed from
	// the buffer when the server redelivers it. Without it the redelivered
	// ciphertext can't be decrypted a second time.
	client.EnableDecryptedEventBuffer = true
	// A retryable network error on the first Connect is retried in the
	// background instead of failing startup (see main).
	client.EnableAutoReconnect = true
	client.InitialAutoReconnect = true
}

func main() {
	flags, err := parseFlags(os.Args[1:])
	if err != nil {
		os.Exit(2)
	}
	debugLogging = flags.debug
	health := newBridgeHealth(time.Now()) // for /api/health, see health.go

	// Set up logger
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", "INFO", true)

	// Create directory for database if it doesn't exist (owner-only: it holds
	// the device session keys, message history and the API token)
	if err := os.MkdirAll("store", 0700); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}

	container, err := sqlstore.New(context.Background(), "sqlite3", "file:store/whatsapp.db?_foreign_keys=on", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	// Initialize message store (runs pending schema migrations; see migrate.go)
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		os.Exit(1)
	}
	defer messageStore.Close()
	messageStore.purgeDeleted = flags.purgeDeleted

	// Migrations that need our JIDs and the LID map. The device store is
	// loaded, so this works before connecting.
	if client.Store.ID != nil {
		if _, err := messageStore.MigrateIdentity(clientIdentity(client)); err != nil {
			logger.Errorf("Failed to migrate message store: %v", err)
			messageStore.Close()
			os.Exit(1)
		}
	} else if pending, err := messageStore.PendingIdentityMigrations(); err == nil && pending {
		logger.Warnf("Not logged in yet: chat/sender identity migrations will run on the next start after login")
	}

	// API token and send allowlist (see security.go, allowlist.go)
	token, err := ensureBridgeToken("store")
	if err != nil {
		logger.Errorf("Failed to set up API token: %v", err)
		return
	}
	allowedDirs, err := sendAllowedDirs("store")
	if err != nil {
		logger.Errorf("Failed to set up outbox: %v", err)
		return
	}
	if err := hardenStorePermissions("store"); err != nil {
		logger.Errorf("Failed to restrict store permissions: %v", err)
		return
	}
	// Bind the REST port before connecting: if another bridge already holds
	// it, stop here rather than connect and kick that bridge's session.
	restListener, err := listenREST(8080)
	if err != nil {
		logger.Errorf("Failed to start REST API server (is another bridge running?): %v", err)
		messageStore.Close()
		os.Exit(1)
	}
	defer restListener.Close()

	if debugLogging {
		logger.Warnf("Debug logging on: message content, names and file paths will be logged")
	}

	// Every event counts as a sign of life for /api/health.
	client.AddEventHandler(health.eventHandler)

	// Messages, history sync and connection events. A message that fails to
	// store is not acknowledged (see bridgeEvents).
	configureClient(client)
	client.AddEventHandlerWithSuccessStatus(newBridgeEvents(client, messageStore, logger).handle)

	// Create channel to track connection success
	connected := make(chan bool, 1)

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		// Print QR code for pairing with phone
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				connected <- true
				break
			}
		}

		// Wait for connection
		select {
		case <-connected:
			fmt.Println("\nSuccessfully connected and authenticated!")
		case <-time.After(3 * time.Minute):
			logger.Errorf("Timeout waiting for QR code scan")
			return
		}
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
		connected <- true
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp! Type 'help' for commands.")

	// Start REST API server. Clients must send the token from store/bridge_token.
	logger.Infof("Files can be sent from: %s", strings.Join(allowedDirs, ", "))
	api := newAPIServer(client, messageStore)
	api.token = token
	api.allowedDirs = allowedDirs
	api.health = health
	serveREST(api, restListener)

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for termination signal
	<-exitChan

	fmt.Println("Disconnecting...")
	// Disconnect client
	client.Disconnect()
}
