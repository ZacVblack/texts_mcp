package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

func TestPlanSend(t *testing.T) {
	ok := []OutboxItem{
		{ID: "s1", ConversationID: "c1", Text: " hi "},
		{ID: "s2", NewNumber: "+17345550101", Text: "hi"},
	}
	for _, it := range ok {
		p, err := planSend(&it)
		if err != nil {
			t.Fatalf("%+v: %v", it, err)
		}
		if p.text != "hi" {
			t.Fatalf("text not trimmed: %q", p.text)
		}
	}
	bad := []OutboxItem{
		{ID: "b1", ConversationID: "c1", Text: "  "},
		{ID: "b2", ConversationID: "c1", NewNumber: "+17345550101", Text: "hi"},
		{ID: "b3", Text: "hi"},
		{ID: "b4", NewNumber: "7345550101", Text: "hi"},
		{ID: "b5", NewNumber: "+1734abc0101", Text: "hi"},
	}
	for _, it := range bad {
		if _, err := planSend(&it); err == nil {
			t.Fatalf("%+v should be refused", it)
		}
	}
}

func TestBuildSendRequestUsesConversationAndSIM(t *testing.T) {
	conv := &gmproto.Conversation{ConversationID: "c9", DefaultOutgoingID: "p-me"}
	sim := &gmproto.SIMCard{SIMData: &gmproto.SIMData{SIMPayload: &gmproto.SIMPayload{SIMNumber: 2}}}
	req := buildSendRequest(conv, "On my way", "tmp-1", sim)
	if req.GetConversationID() != "c9" || req.GetMessagePayload().GetConversationID() != "c9" {
		t.Fatal("wrong conversation")
	}
	if req.GetMessagePayload().GetParticipantID() != "p-me" {
		t.Fatal("must send as the conversation's own outgoing participant")
	}
	if req.GetTmpID() != "tmp-1" || req.GetMessagePayload().GetTmpID() != "tmp-1" || req.GetMessagePayload().GetTmpID2() != "tmp-1" {
		t.Fatal("tmp IDs not set")
	}
	if got := req.GetMessagePayload().GetMessageInfo()[0].GetMessageContent().GetContent(); got != "On my way" {
		t.Fatalf("text %q", got)
	}
	if req.GetSIMPayload().GetSIMNumber() != 2 {
		t.Fatal("SIM payload not used")
	}
	if buildSendRequest(conv, "x", "t", nil).GetSIMPayload() != nil {
		t.Fatal("no SIM known should send no SIM payload")
	}
}

func TestCannotSendTo(t *testing.T) {
	if cannotSendTo(&gmproto.Conversation{Status: gmproto.ConversationStatus_ACTIVE}) != "" {
		t.Fatal("active conversation refused")
	}
	if cannotSendTo(&gmproto.Conversation{Status: gmproto.ConversationStatus_ARCHIVED}) != "" {
		t.Fatal("archived conversation refused")
	}
	if cannotSendTo(&gmproto.Conversation{Status: gmproto.ConversationStatus_SPAM_FOLDER}) == "" {
		t.Fatal("spam conversation allowed")
	}
	if cannotSendTo(&gmproto.Conversation{ReadOnly: true}) == "" {
		t.Fatal("read-only conversation allowed")
	}
}

func TestSendLedgerNeverTwice(t *testing.T) {
	var l sendLedger
	if !l.claim("s1") || l.claim("s1") {
		t.Fatal("a send must be claimable exactly once")
	}
	if !l.claim("s2") {
		t.Fatal("another send must be claimable")
	}
}

func TestEchoWaitersDontBlock(t *testing.T) {
	var e echoWaiters
	ch := e.add("tmp-1")
	for i := 0; i < 100; i++ { // more than the buffer: must not block
		e.deliver(&gmproto.Message{TmpID: "tmp-1", MessageID: "m"})
	}
	e.deliver(&gmproto.Message{TmpID: "other"})
	e.deliver(nil)
	if m := <-ch; m.GetMessageID() != "m" {
		t.Fatal("echo not delivered")
	}
	e.remove("tmp-1")
	e.deliver(&gmproto.Message{TmpID: "tmp-1"})
}

func TestSIMCardsByParticipant(t *testing.T) {
	var s simCards
	s.set([]*gmproto.SIMCard{{SIMParticipant: &gmproto.SIMParticipant{ID: "p1"}}})
	if s.get("p1") == nil || s.get("p2") != nil {
		t.Fatal("SIM lookup wrong")
	}
	s.set(nil) // an empty settings update keeps what we know
	if s.get("p1") == nil {
		t.Fatal("empty update dropped the SIM")
	}
}

func TestOutboxClient(t *testing.T) {
	token := deriveToken(testPass, purposeBridge)
	var results []OutboxResult
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/bridge/outbox":
			if r.URL.Query().Get("wait") != "25" {
				t.Errorf("wait %q", r.URL.Query().Get("wait"))
			}
			_, _ = w.Write([]byte(`{"item":{"id":"s1","conversation_id":"c1","new_number":"","text":"hi"},"sending_enabled":true}`))
		case "/bridge/outbox/result":
			var res OutboxResult
			_ = json.NewDecoder(r.Body).Decode(&res)
			results = append(results, res)
			_, _ = w.Write([]byte(`{"ok":true,"updated":true}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	wc := newWorkerClient(&Config{WorkerURL: srv.URL, Passphrase: testPass})
	next, err := wc.NextOutbox(context.Background(), 25)
	if err != nil || next.Item == nil || next.Item.ID != "s1" || next.Item.ConversationID != "c1" || !next.SendingEnabled {
		t.Fatalf("next %+v %v", next, err)
	}
	if err := wc.PostOutboxResult(context.Background(), &OutboxResult{ID: "s1", State: "sent", MessageID: "m1"}); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].State != "sent" || results[0].MessageID != "m1" {
		t.Fatalf("results %+v", results)
	}

	// A Worker from before sending existed answers 404.
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer old.Close()
	_, err = newWorkerClient(&Config{WorkerURL: old.URL, Passphrase: testPass}).NextOutbox(context.Background(), 25)
	if !errors.Is(err, errOutboxMissing) {
		t.Fatalf("want errOutboxMissing, got %v", err)
	}
}

func TestHeartbeatSaysCanSend(t *testing.T) {
	data, _ := json.Marshal(&Heartbeat{State: "connected", CanSend: true})
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	if m["can_send"] != true {
		t.Fatalf("heartbeat %s", data)
	}
}
