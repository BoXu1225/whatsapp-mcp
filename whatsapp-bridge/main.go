package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
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
	media  *mediaService // handles media retry answers; may be nil

	// ready is closed once the store's migrations are complete (right after
	// pairing on a first run). Message events wait for it.
	ready     chan struct{}
	readyOnce sync.Once

	// connected is closed on the first Connected event; fatal receives
	// errLoggedOut when WhatsApp ends the session (main then exits 1).
	connected     chan struct{}
	connectedOnce sync.Once
	fatal         chan error
}

func newBridgeEvents(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) *bridgeEvents {
	return &bridgeEvents{client: client, store: store, logger: logger, ready: make(chan struct{}),
		connected: make(chan struct{}), fatal: make(chan error, 1)}
}

// markReady lets message events through. Safe to call more than once.
func (b *bridgeEvents) markReady() {
	b.readyOnce.Do(func() { close(b.ready) })
}

// handle processes one event and reports whether it may be acknowledged.
func (b *bridgeEvents) handle(evt any) bool {
	switch v := evt.(type) {
	case *events.Message:
		// Status updates are not a chat; skip them entirely (#21).
		if v.Info.Chat.String() == statusBroadcastJID {
			return true
		}
		<-b.ready
		return handleMessage(b.client, b.store, v, b.logger)

	case *events.HistorySync:
		<-b.ready
		dropStatusConversations(v)
		handleHistorySync(b.client, b.store, v, b.logger)

	case *events.MediaRetry:
		if b.media != nil {
			go b.media.handleRetry(v)
		}

	case *events.Connected:
		b.logger.Infof("Connected to WhatsApp")
		b.connectedOnce.Do(func() { close(b.connected) })
		if b.client != nil {
			go backfillChatNames(b.client, b.store, b.logger)
		}

	case *events.LoggedOut:
		// whatsmeow has already deleted the device's session from
		// store/whatsapp.db; nothing more can be received or sent.
		b.logger.Errorf("%v (reason: %s). The bridge will exit. %s", errLoggedOut, v.Reason, repairHint)
		select {
		case b.fatal <- fmt.Errorf("%w (reason: %s)", errLoggedOut, v.Reason):
		default: // already reported
		}
	}
	return true
}

// dropStatusConversations removes status updates from a history sync, so no
// name lookup or store work is spent on them (#21).
func dropStatusConversations(evt *events.HistorySync) {
	if evt.Data == nil {
		return
	}
	kept := evt.Data.Conversations[:0]
	for _, c := range evt.Data.Conversations {
		if c.GetID() != statusBroadcastJID {
			kept = append(kept, c)
		}
	}
	evt.Data.Conversations = kept
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
	os.Exit(run())
}

// run starts the bridge and returns the process exit status: 0 after
// SIGINT/SIGTERM, 1 on a startup failure, a logout or a REST server error.
func run() int {
	flags, err := parseFlags(os.Args[1:])
	if err != nil {
		return 2
	}
	debugLogging = flags.debug
	health := newBridgeHealth(time.Now()) // for /api/health, see health.go

	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")
	dbLog := waLog.Stdout("Database", "INFO", true)

	// Owner-only: it holds the device session keys, message history and the API token.
	if err := os.MkdirAll("store", 0700); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return 1
	}

	container, err := sqlstore.New(context.Background(), "sqlite3", "file:store/whatsapp.db?_foreign_keys=on&_busy_timeout=5000", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return 1
	}
	defer container.Close()

	// The device store holds the session. Without one, GetFirstDevice
	// returns a new device, which needs pairing.
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		logger.Errorf("Failed to get device: %v", err)
		return 1
	}
	if deviceStore.ID == nil {
		logger.Infof("No linked device yet: a QR code will be shown for pairing")
	}

	client := whatsmeow.NewClient(deviceStore, logger)
	configureClient(client)

	// Message store; runs pending schema migrations (see migrate.go).
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return 1
	}
	messageStore.purgeDeleted = flags.purgeDeleted
	// On every return: stop the REST server, disconnect, close the DB.
	var httpServer *http.Server
	var restListener net.Listener
	defer func() {
		var srv httpShutdowner
		if httpServer != nil {
			srv = httpServer
		} else if restListener != nil {
			restListener.Close()
		}
		shutdown(srv, client, messageStore, 10*time.Second)
	}()

	// Migrations that need our JIDs and the LID map. The device store is
	// loaded, so this works before connecting.
	if client.Store.ID != nil {
		if _, err := messageStore.MigrateIdentity(clientIdentity(client)); err != nil {
			logger.Errorf("Failed to migrate message store: %v", err)
			return 1
		}
	} else if pending, err := messageStore.PendingMigrations(); err == nil && pending {
		logger.Infof("Not logged in yet: the remaining migrations run right after pairing")
	}

	// API token and send allowlist (see security.go, allowlist.go)
	token, err := ensureBridgeToken("store")
	if err != nil {
		logger.Errorf("Failed to set up API token: %v", err)
		return 1
	}
	allowedDirs, err := sendAllowedDirs("store")
	if err != nil {
		logger.Errorf("Failed to set up outbox: %v", err)
		return 1
	}
	if err := hardenStorePermissions("store"); err != nil {
		logger.Errorf("Failed to restrict store permissions: %v", err)
		return 1
	}
	// Bind the REST port before connecting: if another bridge already holds
	// it, stop here rather than connect and kick that bridge's session.
	restListener, err = listenREST(8080)
	if err != nil {
		logger.Errorf("Failed to start REST API server (is another bridge running?): %v", err)
		return 1
	}

	if debugLogging {
		logger.Warnf("Debug logging on: message content, names and file paths will be logged")
	}

	// Every event counts as a sign of life for /api/health.
	client.AddEventHandler(health.eventHandler)

	// Messages, history sync and connection events. A message that fails to
	// store is not acknowledged (see bridgeEvents).
	media := newMediaService(clientFetcher(client), messageStore)
	bridge := newBridgeEvents(client, messageStore, logger)
	bridge.media = media
	if client.Store.ID != nil {
		bridge.markReady() // identity migrations ran above
	}
	client.AddEventHandlerWithSuccessStatus(bridge.handle)

	// stop receives the reason to exit: a signal (errInterrupted, exit 0) or
	// a logout (exit 1).
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	stop := make(chan error, 2)
	go func() { sig := <-sigs; stop <- fmt.Errorf("%w (%v)", errInterrupted, sig) }()
	go func() { stop <- <-bridge.fatal }()

	if client.Store.ID == nil {
		// New device: pair with the phone by QR code.
		// Background reconnects need a session, so fail fast while pairing.
		client.InitialAutoReconnect = false
		qrChan, _ := client.GetQRChannel(context.Background())
		if err := client.Connect(); err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return 1
		}
		paired := false
	qrLoop:
		for {
			select {
			case evt, ok := <-qrChan:
				if !ok {
					break qrLoop
				}
				switch evt.Event {
				case "code":
					fmt.Println("\nScan this QR code with your WhatsApp app:")
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				case "success":
					paired = true
					break qrLoop
				default:
					logger.Errorf("Pairing ended: %s", evt.Event)
					break qrLoop
				}
			case err := <-stop:
				return exitStatus(logger, err)
			}
		}
		if !paired {
			logger.Errorf("Pairing failed or timed out; run the bridge again to get a new QR code")
			return 1
		}
		fmt.Println("\nSuccessfully paired!")
		// Now that we know our JIDs, finish the migrations before any
		// message is stored (message events wait for markReady).
		if _, err := messageStore.MigrateIdentity(clientIdentity(client)); err != nil {
			logger.Errorf("Failed to migrate message store: %v", err)
			return 1
		}
		bridge.markReady()
	} else {
		// A retryable network error here is retried in the background
		// (InitialAutoReconnect); anything else is fatal.
		if err := client.Connect(); err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return 1
		}
	}

	logger.Infof("Waiting up to %v for the WhatsApp connection...", connectTimeout)
	if err := waitForConnection(bridge.connected, stop, connectTimeout); err != nil {
		return exitStatus(logger, err)
	}
	fmt.Println("\n✓ Connected to WhatsApp!")

	// Start REST API server. Clients must send the token from store/bridge_token.
	logger.Infof("Files can be sent from: %s", strings.Join(allowedDirs, ", "))
	api := newAPIServer(client, messageStore)
	api.token = token
	api.allowedDirs = allowedDirs
	api.health = health
	api.media = media
	srv, restErrs := serveREST(api, restListener)
	httpServer = srv

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	select {
	case err := <-stop:
		return exitStatus(logger, err)
	case err, ok := <-restErrs:
		if !ok {
			err = errors.New("REST API server stopped")
		}
		logger.Errorf("%v; exiting so the bridge can be restarted", err)
		return 1
	}
}
