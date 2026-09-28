package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Bridge lifecycle (#21): startup waits for a connection, a logout or a REST
// server failure ends the process with a non-zero status, and shutdown stops
// things in order.

// errLoggedOut is reported when WhatsApp ends this device's session.
var errLoggedOut = errors.New("WhatsApp logged this device out")

// errInterrupted is reported for SIGINT/SIGTERM.
var errInterrupted = errors.New("interrupted")

// repairHint tells the user how to link the bridge again.
const repairHint = "Link the bridge again by running `scripts/bridge.sh fg` and scanning the QR code " +
	"(WhatsApp > Settings > Linked devices). Message history in store/messages.db is kept."

// connectTimeout is how long startup waits for the first Connected event while
// whatsmeow retries in the background.
const connectTimeout = 60 * time.Second

// waitForConnection waits until connected is closed, a fatal error arrives
// (e.g. logged out) or timeout passes.
func waitForConnection(connected <-chan struct{}, fatal <-chan error, timeout time.Duration) error {
	select {
	case <-connected:
		return nil
	case err := <-fatal:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("not connected to WhatsApp after %v; check the network and try again", timeout)
	}
}

// exitStatus logs why the bridge stops and returns its exit status: 0 for a
// signal, 1 for anything else.
func exitStatus(logger interface{ Errorf(string, ...any) }, err error) int {
	switch {
	case errors.Is(err, errInterrupted):
		fmt.Printf("%v: shutting down...\n", err)
		return 0
	case errors.Is(err, errLoggedOut):
		logger.Errorf("%v. %s", err, repairHint)
	default:
		logger.Errorf("%v", err)
	}
	return 1
}

type httpShutdowner interface {
	Shutdown(ctx context.Context) error
}

type disconnecter interface {
	Disconnect()
}

// shutdown stops the REST server (waiting up to timeout for requests in
// flight), then disconnects from WhatsApp, then closes the database, so no
// request or event handler writes to a closed store. nil parts are skipped.
func shutdown(srv httpShutdowner, client disconnecter, store io.Closer, timeout time.Duration) {
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		if err := srv.Shutdown(ctx); err != nil {
			fmt.Printf("REST API server shutdown: %v\n", err)
		}
		cancel()
	}
	if client != nil {
		client.Disconnect()
	}
	if store != nil {
		if err := store.Close(); err != nil {
			fmt.Printf("Closing messages.db: %v\n", err)
		}
	}
}
