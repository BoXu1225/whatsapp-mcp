package main

import (
	"fmt"
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

// apiHandler returns the REST handlers backed by apiStore and a nil client.
func apiHandler(t *testing.T) http.Handler {
	t.Helper()
	return newAPIServer(nil, apiStore).handler()
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
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Errorf("%s %s -> %d, want %d (body %q)", tt.method, tt.path, rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}
