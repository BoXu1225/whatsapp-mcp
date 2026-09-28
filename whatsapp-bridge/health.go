package main

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// version is reported by /api/health. Override at build time with
// -ldflags "-X main.version=..." (scripts/bridge.sh uses git describe).
var version = "0.2.0"

// bridgeHealth tracks when the bridge started and when it last received a
// whatsmeow event, so clients can tell a quiet inbox from a dead bridge.
type bridgeHealth struct {
	startedAt     time.Time
	lastEventNano atomic.Int64 // UnixNano of the latest event; 0 = none yet
}

func newBridgeHealth(startedAt time.Time) *bridgeHealth {
	return &bridgeHealth{startedAt: startedAt}
}

// recordEvent notes an event at t. It only moves forward, so concurrent
// handlers finishing out of order can't make the bridge look staler.
func (h *bridgeHealth) recordEvent(t time.Time) {
	n := t.UnixNano()
	for {
		cur := h.lastEventNano.Load()
		if n <= cur || h.lastEventNano.CompareAndSwap(cur, n) {
			return
		}
	}
}

// eventHandler is registered with client.AddEventHandler; every whatsmeow
// event (messages, receipts, presence, connection changes) counts.
func (h *bridgeHealth) eventHandler(any) {
	h.recordEvent(time.Now())
}

// lastEvent returns the time of the latest event, if there was one.
func (h *bridgeHealth) lastEvent() (time.Time, bool) {
	n := h.lastEventNano.Load()
	if n == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, n), true
}

// HealthResponse is the body of GET /api/health. Times are RFC 3339 UTC;
// last_event is null until the first event.
type HealthResponse struct {
	Connected bool    `json:"connected"`
	LoggedIn  bool    `json:"logged_in"`
	LastEvent *string `json:"last_event"`
	StartedAt string  `json:"started_at"`
	Version   string  `json:"version"`
}

func (s *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := HealthResponse{
		Connected: s.isConnected(),
		LoggedIn:  s.isLoggedIn(),
		StartedAt: s.health.startedAt.UTC().Format(time.RFC3339),
		Version:   version,
	}
	if t, ok := s.health.lastEvent(); ok {
		v := t.UTC().Format(time.RFC3339)
		resp.LastEvent = &v
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
