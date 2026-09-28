package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

// healthBody is /api/health's JSON, decoded loosely so a missing field or a
// wrong type shows up in the test rather than being zero-filled.
type healthBody map[string]any

func getHealth(t *testing.T, s *apiServer, modify func(r *http.Request)) (*httptest.ResponseRecorder, healthBody) {
	t.Helper()
	req := validRequest(http.MethodGet, "/api/health", "")
	if modify != nil {
		modify(req)
	}
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	var body healthBody
	if rec.Code == http.StatusOK {
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode health: %v (body %q)", err, rec.Body.String())
		}
	}
	return rec, body
}

func TestHealthGuarded(t *testing.T) {
	tests := []struct {
		name     string
		modify   func(r *http.Request)
		wantCode int
	}{
		{"valid", nil, http.StatusOK},
		{"missing token", func(r *http.Request) { r.Header.Del("X-Bridge-Token") }, http.StatusUnauthorized},
		{"wrong token", func(r *http.Request) { r.Header.Set("X-Bridge-Token", testToken[:63]+"0") }, http.StatusUnauthorized},
		{"foreign host", func(r *http.Request) { r.Host = fmt.Sprintf("evil.example:%d", testPort) }, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestAPIServer(t)
			rec, _ := getHealth(t, s, tt.modify)
			if rec.Code != tt.wantCode {
				t.Errorf("-> %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}

func TestHealthOnlyGET(t *testing.T) {
	s, _ := newTestAPIServer(t)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, validRequest(http.MethodPost, "/api/health", "{}"))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/health -> %d, want 405", rec.Code)
	}
}

func TestHealthNilClientReportsDisconnected(t *testing.T) {
	// newTestAPIServer has a nil client, like a bridge that has not connected.
	s, _ := newTestAPIServer(t)
	rec, body := getHealth(t, s, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("-> %d, want 200", rec.Code)
	}
	if body["connected"] != false || body["logged_in"] != false {
		t.Errorf("connected/logged_in = %v/%v, want false/false", body["connected"], body["logged_in"])
	}
	if v, ok := body["last_event"]; !ok || v != nil {
		t.Errorf("last_event = %v (present %v), want null before any event", v, ok)
	}
}

func TestHealthReportsState(t *testing.T) {
	s, _ := newTestAPIServer(t)
	started := time.Date(2024, 1, 2, 10, 0, 0, 0, time.FixedZone("CET", 3600))
	s.health = newBridgeHealth(started)
	s.isConnected = func() bool { return true }
	s.isLoggedIn = func() bool { return true }

	event := time.Date(2024, 1, 2, 11, 30, 15, 500, time.FixedZone("CET", 3600))
	s.health.recordEvent(event)

	rec, body := getHealth(t, s, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("-> %d, want 200", rec.Code)
	}
	want := healthBody{
		"connected":  true,
		"logged_in":  true,
		"last_event": "2024-01-02T10:30:15Z",
		"started_at": "2024-01-02T09:00:00Z",
		"version":    version,
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %#v, want %#v", k, body[k], v)
		}
	}
	if len(body) != len(want) {
		t.Errorf("health has fields %v, want exactly %v", body, want)
	}
}

func TestHealthConnectedButLoggedOut(t *testing.T) {
	s, _ := newTestAPIServer(t)
	s.isConnected = func() bool { return true }
	s.isLoggedIn = func() bool { return false }
	_, body := getHealth(t, s, nil)
	if body["connected"] != true || body["logged_in"] != false {
		t.Errorf("connected/logged_in = %v/%v, want true/false", body["connected"], body["logged_in"])
	}
}

func TestEventHandlerRecordsEveryEvent(t *testing.T) {
	h := newBridgeHealth(time.Now())
	if _, ok := h.lastEvent(); ok {
		t.Fatal("new health reports an event")
	}
	for _, evt := range []any{&events.Message{}, &events.Connected{}, &events.Receipt{}, "anything"} {
		before := time.Now()
		h.eventHandler(evt)
		got, ok := h.lastEvent()
		if !ok || got.Before(before) || got.After(time.Now()) {
			t.Errorf("after %T: lastEvent = %v, %v; want between %v and now", evt, got, ok, before)
		}
	}
}

func TestRecordEventNeverGoesBackwards(t *testing.T) {
	h := newBridgeHealth(time.Now())
	later := time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC)
	h.recordEvent(later)
	h.recordEvent(later.Add(-time.Hour))
	if got, _ := h.lastEvent(); !got.Equal(later) {
		t.Errorf("lastEvent = %v, want %v", got, later)
	}
}

func TestRecordEventConcurrent(t *testing.T) {
	h := newBridgeHealth(time.Now())
	base := time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.recordEvent(base.Add(time.Duration(i) * time.Second))
		}()
	}
	wg.Wait()
	if got, _ := h.lastEvent(); !got.Equal(base.Add(49 * time.Second)) {
		t.Errorf("lastEvent = %v, want the latest recorded time", got)
	}
}
