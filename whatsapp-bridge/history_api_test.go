package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
)

// --- #20: POST /api/history ---------------------------------------------------

type peerCall struct{ msg *waE2E.Message }

// historyTestServer is newTestAPIServer with a recording peer-message sender.
func historyTestServer(t *testing.T, sendErr error) (*apiServer, *[]peerCall) {
	t.Helper()
	s, _ := newTestAPIServer(t)
	var calls []peerCall
	s.sendPeer = func(_ context.Context, msg *waE2E.Message) (string, error) {
		calls = append(calls, peerCall{msg})
		if sendErr != nil {
			return "", sendErr
		}
		return "REQ-1", nil
	}
	return s, &calls
}

// seedHistoryChat stores three messages; the oldest (H1, from me) is the anchor.
func seedHistoryChat(t *testing.T, chat string) time.Time {
	t.Helper()
	t1 := time.Date(2023, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := apiStore.StoreChat(chat, "History Chat", t1.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i, m := range []struct {
		id     string
		at     time.Time
		fromMe bool
	}{{"H2", t1.Add(time.Hour), false}, {"H1", t1, true}, {"H3", t1.Add(2 * time.Hour), false}} {
		if err := apiStore.StoreMessage(m.id, chat, chat, "msg", m.at, m.fromMe, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	return t1
}

func TestAPIHistoryBuildsOnDemandRequest(t *testing.T) {
	chat := "15550000123@s.whatsapp.net"
	t1 := seedHistoryChat(t, chat)
	s, calls := historyTestServer(t, nil)

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, validRequest(http.MethodPost, "/api/history", `{"chat_jid":"`+chat+`","count":20}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("-> %d %s, want 202", rec.Code, rec.Body)
	}
	var resp HistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Success || resp.RequestID != "REQ-1" || resp.OldestMessageID != "H1" || resp.Count != 20 {
		t.Errorf("response %+v", resp)
	}
	if len(*calls) != 1 {
		t.Fatalf("sent %d peer messages, want 1", len(*calls))
	}
	pm := (*calls)[0].msg.GetProtocolMessage()
	if pm.GetType() != waE2E.ProtocolMessage_PEER_DATA_OPERATION_REQUEST_MESSAGE {
		t.Errorf("protocol message type %v", pm.GetType())
	}
	op := pm.GetPeerDataOperationRequestMessage()
	if op.GetPeerDataOperationRequestType() != waE2E.PeerDataOperationRequestType_HISTORY_SYNC_ON_DEMAND {
		t.Errorf("request type %v", op.GetPeerDataOperationRequestType())
	}
	req := op.GetHistorySyncOnDemandRequest()
	if req.GetChatJID() != chat || req.GetOldestMsgID() != "H1" || !req.GetOldestMsgFromMe() ||
		req.GetOnDemandMsgCount() != 20 || req.GetOldestMsgTimestampMS() != t1.Unix() {
		t.Errorf("on-demand request %+v, want anchor H1 (from me, %d) count 20 in %s", req, t1.Unix(), chat)
	}
}

func TestAPIHistoryDefaultCount(t *testing.T) {
	chat := "15550000124@s.whatsapp.net"
	seedHistoryChat(t, chat)
	s, calls := historyTestServer(t, nil)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, validRequest(http.MethodPost, "/api/history", `{"chat_jid":"`+chat+`"}`))
	if rec.Code != http.StatusAccepted || len(*calls) != 1 {
		t.Fatalf("-> %d %s", rec.Code, rec.Body)
	}
	if n := (*calls)[0].msg.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest().GetOnDemandMsgCount(); n != 50 {
		t.Errorf("default count %d, want 50", n)
	}
}

func TestAPIHistoryValidation(t *testing.T) {
	chat := "15550000125@s.whatsapp.net"
	seedHistoryChat(t, chat)
	tests := []struct {
		name, method, body string
		want               int
	}{
		{"GET", http.MethodGet, "", http.StatusMethodNotAllowed},
		{"bad json", http.MethodPost, "{", http.StatusBadRequest},
		{"no chat", http.MethodPost, `{"count":10}`, http.StatusBadRequest},
		{"not a JID", http.MethodPost, `{"chat_jid":"hello"}`, http.StatusBadRequest},
		{"count too big", http.MethodPost, `{"chat_jid":"` + chat + `","count":51}`, http.StatusBadRequest},
		{"negative count", http.MethodPost, `{"chat_jid":"` + chat + `","count":-1}`, http.StatusBadRequest},
		{"unknown chat", http.MethodPost, `{"chat_jid":"15550009999@s.whatsapp.net"}`, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, calls := historyTestServer(t, nil)
			rec := httptest.NewRecorder()
			s.handler().ServeHTTP(rec, validRequest(tt.method, "/api/history", tt.body))
			if rec.Code != tt.want {
				t.Errorf("-> %d %s, want %d", rec.Code, rec.Body, tt.want)
			}
			if len(*calls) != 0 {
				t.Errorf("peer message sent for an invalid request")
			}
		})
	}
}

func TestAPIHistoryGuarded(t *testing.T) {
	chat := "15550000126@s.whatsapp.net"
	seedHistoryChat(t, chat)
	s, calls := historyTestServer(t, nil)
	req := validRequest(http.MethodPost, "/api/history", `{"chat_jid":"`+chat+`"}`)
	req.Header.Del("X-Bridge-Token")
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || len(*calls) != 0 {
		t.Errorf("without token -> %d (sent %d), want 401 and nothing sent", rec.Code, len(*calls))
	}
	req = validRequest(http.MethodPost, "/api/history", `{"chat_jid":"`+chat+`"}`)
	req.Header.Set("Content-Type", "text/plain")
	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType || len(*calls) != 0 {
		t.Errorf("text/plain -> %d, want 415", rec.Code)
	}
}

func TestAPIHistorySendFailure(t *testing.T) {
	chat := "15550000127@s.whatsapp.net"
	seedHistoryChat(t, chat)
	s, _ := historyTestServer(t, errors.New("not logged in"))
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, validRequest(http.MethodPost, "/api/history", `{"chat_jid":"`+chat+`"}`))
	var resp HistoryResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusBadGateway || resp.Success {
		t.Errorf("-> %d %+v, want 502", rec.Code, resp)
	}
}

// Without a client (not connected) the default sender refuses cleanly.
func TestAPIHistoryWithoutClient(t *testing.T) {
	chat := "15550000128@s.whatsapp.net"
	seedHistoryChat(t, chat)
	s, _ := newTestAPIServer(t)
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, validRequest(http.MethodPost, "/api/history", `{"chat_jid":"`+chat+`"}`))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("-> %d %s, want 502", rec.Code, rec.Body)
	}
}
