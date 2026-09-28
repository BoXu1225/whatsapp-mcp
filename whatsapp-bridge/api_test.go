package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

var registerHandlersOnce sync.Once

// apiHandler returns the mux startRESTServer registers its handlers on.
// startRESTServer uses http.DefaultServeMux and also starts a listener; port 0
// makes that an unused ephemeral port. Only request-validation paths are
// exercised here, so the nil client is never touched.
func apiHandler(t *testing.T) http.Handler {
	t.Helper()
	registerHandlersOnce.Do(func() {
		startRESTServer(nil, newTestStore(t), 0)
	})
	return http.DefaultServeMux
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
