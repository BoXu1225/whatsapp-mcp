package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureBridgeTokenCreatesPrivateRandomToken(t *testing.T) {
	dir := t.TempDir()
	tok, err := ensureBridgeToken(dir)
	if err != nil {
		t.Fatalf("ensureBridgeToken: %v", err)
	}
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(tok))
	}
	if _, err := hex.DecodeString(tok); err != nil {
		t.Errorf("token is not hex: %v", err)
	}
	path := filepath.Join(dir, bridgeTokenFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}
	data, _ := os.ReadFile(path)
	if string(data) != tok {
		t.Errorf("file holds %q, returned %q", data, tok)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("store dir has %d entries, want only the token file", len(entries))
	}
}

func TestEnsureBridgeTokenReusesExisting(t *testing.T) {
	dir := t.TempDir()
	first, err := ensureBridgeToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ensureBridgeToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Errorf("second call returned %q, want the existing %q", second, first)
	}
}

func TestEnsureBridgeTokenDiffersPerStore(t *testing.T) {
	a, _ := ensureBridgeToken(t.TempDir())
	b, _ := ensureBridgeToken(t.TempDir())
	if a == "" || a == b {
		t.Errorf("tokens %q and %q should be random and distinct", a, b)
	}
}

func TestEnsureBridgeTokenReplacesInvalidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, bridgeTokenFile)
	if err := os.WriteFile(path, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tok, err := ensureBridgeToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 {
		t.Errorf("token = %q, want a fresh 64-char token", tok)
	}
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}
}
