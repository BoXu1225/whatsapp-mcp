package main

// TODO(#21) stubs so the tests compile.

import (
	"context"
	"errors"
	"io"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

var errLoggedOut = errors.New("stub")

func waitForConnection(connected <-chan struct{}, fatal <-chan error, timeout time.Duration) error {
	return nil
}

type groupNameCache struct {
	fetch func(*whatsmeow.Client, types.JID) (string, error)
	now   func() time.Time
}

const groupNameRetryAfter = time.Hour

var groupNames = newGroupNameCache()

func newGroupNameCache() *groupNameCache { return &groupNameCache{} }
func (c *groupNameCache) lookup(client *whatsmeow.Client, jid types.JID) (string, bool) {
	return "", false
}

type httpShutdowner interface{ Shutdown(context.Context) error }
type disconnecter interface{ Disconnect() }

func shutdown(srv httpShutdowner, client disconnecter, store io.Closer, timeout time.Duration) {}
