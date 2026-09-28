package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// apiStore backs the REST handler tests: a store in a temp dir that lives for
// the whole package run (not tied to any one test's TempDir). API tests pass a
// nil client, so they must only exercise paths that don't touch it.
var apiStore *MessageStore

// TestMain creates apiStore and removes it after the run.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bridge-api-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	apiStore, err = NewMessageStoreAt(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	code := m.Run()

	apiStore.Close()
	os.RemoveAll(dir)
	os.Exit(code)
}

const (
	testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testPort  = 8080
)

// sentMessage records one call to the stubbed sender.
type sentMessage struct{ recipient, message, mediaPath string }

// newTestAPIServer returns an apiServer backed by apiStore with a nil client
// and a sender stub that records calls and reports success.
func newTestAPIServer(t *testing.T) (*apiServer, *[]sentMessage) {
	t.Helper()
	s := newAPIServer(nil, apiStore)
	s.token = testToken
	s.port = testPort
	var sent []sentMessage
	s.send = func(recipient, message, mediaPath string, mediaData []byte) (bool, string) {
		sent = append(sent, sentMessage{recipient, message, mediaPath})
		return true, "stub sent"
	}
	return s, &sent
}

// apiHandler returns the REST handlers of newTestAPIServer.
func apiHandler(t *testing.T) http.Handler {
	t.Helper()
	s, _ := newTestAPIServer(t)
	return s.handler()
}

// validRequest builds a request that passes the API guard: loopback Host with
// the right port, the token header and a JSON content type (for POST).
func validRequest(method, path, body string) *http.Request {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Host = fmt.Sprintf("127.0.0.1:%d", testPort)
	req.Header.Set("X-Bridge-Token", testToken)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestAPIRequestValidation(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		path     string
		body     string
		wantCode int
	}{
		{"send GET", http.MethodGet, "/api/send", "", http.StatusMethodNotAllowed},
		{"send bad json", http.MethodPost, "/api/send", "{", http.StatusBadRequest},
		{"send no recipient", http.MethodPost, "/api/send", `{"message":"hi"}`, http.StatusBadRequest},
		{"send no message or media", http.MethodPost, "/api/send", `{"recipient":"15550000001"}`, http.StatusBadRequest},
		{"download GET", http.MethodGet, "/api/download", "", http.StatusMethodNotAllowed},
		{"download bad json", http.MethodPost, "/api/download", "not json", http.StatusBadRequest},
		{"download missing chat", http.MethodPost, "/api/download", `{"message_id":"m1"}`, http.StatusBadRequest},
	}
	h := apiHandler(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, validRequest(tt.method, tt.path, tt.body))
			if rec.Code != tt.wantCode {
				t.Errorf("%s %s -> %d, want %d (body %q)", tt.method, tt.path, rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}

func TestAPIGuard(t *testing.T) {
	const sendBody = `{"recipient":"15550000001","message":"hi"}`
	tests := []struct {
		name     string
		modify   func(r *http.Request)
		wantCode int
		wantSent bool
	}{
		{"valid request", func(r *http.Request) {}, http.StatusOK, true},
		{"valid with localhost host", func(r *http.Request) { r.Host = fmt.Sprintf("localhost:%d", testPort) }, http.StatusOK, true},
		{"valid with charset", func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") }, http.StatusOK, true},
		{"missing token", func(r *http.Request) { r.Header.Del("X-Bridge-Token") }, http.StatusUnauthorized, false},
		{"wrong token", func(r *http.Request) { r.Header.Set("X-Bridge-Token", strings.Repeat("0", 64)) }, http.StatusUnauthorized, false},
		{"token prefix only", func(r *http.Request) { r.Header.Set("X-Bridge-Token", testToken[:10]) }, http.StatusUnauthorized, false},
		{"text/plain body", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType, false},
		{"form body", func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, http.StatusUnsupportedMediaType, false},
		{"no content type", func(r *http.Request) { r.Header.Del("Content-Type") }, http.StatusUnsupportedMediaType, false},
		{"foreign host", func(r *http.Request) { r.Host = fmt.Sprintf("evil.example:%d", testPort) }, http.StatusForbidden, false},
		{"rebinding host without port", func(r *http.Request) { r.Host = "evil.example" }, http.StatusForbidden, false},
		{"loopback wrong port", func(r *http.Request) { r.Host = "127.0.0.1:9999" }, http.StatusForbidden, false},
		{"loopback no port", func(r *http.Request) { r.Host = "127.0.0.1" }, http.StatusForbidden, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, sent := newTestAPIServer(t)
			req := validRequest(http.MethodPost, "/api/send", sendBody)
			tt.modify(req)
			rec := httptest.NewRecorder()
			s.handler().ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Errorf("-> %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if got := len(*sent) > 0; got != tt.wantSent {
				t.Errorf("sender called = %v, want %v", got, tt.wantSent)
			}
		})
	}
}

func TestAPIGuardCoversDownload(t *testing.T) {
	req := validRequest(http.MethodPost, "/api/download", `{"message_id":"m1","chat_jid":"15550000001@s.whatsapp.net"}`)
	req.Header.Del("X-Bridge-Token")
	rec := httptest.NewRecorder()
	apiHandler(t).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("download without token -> %d, want 401", rec.Code)
	}
}

func TestAPIGuardEmptyServerTokenRejectsAll(t *testing.T) {
	// A server that somehow has no token must not accept an empty header.
	s, sent := newTestAPIServer(t)
	s.token = ""
	req := validRequest(http.MethodPost, "/api/send", `{"recipient":"15550000001","message":"hi"}`)
	req.Header.Set("X-Bridge-Token", "")
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || len(*sent) != 0 {
		t.Errorf("empty token -> %d (sent %d), want 401 and no send", rec.Code, len(*sent))
	}
}
