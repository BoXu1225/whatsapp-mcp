package main

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

func TestStartRESTServerRecordsPortAndGuards(t *testing.T) {
	s, sent := newTestAPIServer(t)
	s.port = 0
	if err := startRESTServer(s, 0); err != nil {
		t.Fatalf("startRESTServer: %v", err)
	}
	if s.port == 0 {
		t.Fatal("port not recorded")
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/send", s.port)

	post := func(token string) int {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"recipient":"15550000001","message":"hi"}`))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set(bridgeTokenHeader, token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(""); code != http.StatusUnauthorized {
		t.Errorf("no token over TCP -> %d, want 401", code)
	}
	if code := post(testToken); code != http.StatusOK || len(*sent) != 1 {
		t.Errorf("valid request over TCP -> %d (sent %d), want 200 and one send", code, len(*sent))
	}
}
