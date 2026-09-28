package main

import "testing"

// newTestStore opens a MessageStore backed by a fresh messages.db in a
// temporary directory that is removed when the test finishes.
func newTestStore(t *testing.T) *MessageStore {
	t.Helper()
	store, err := NewMessageStoreAt(t.TempDir())
	if err != nil {
		t.Fatalf("NewMessageStoreAt: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
